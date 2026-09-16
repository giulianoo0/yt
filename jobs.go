package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	StatusQueued   = "queued"
	StatusRunning  = "running"
	StatusDone     = "done"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
)

type Progress struct {
	Percent         float64 `json:"percent"`
	DownloadedBytes int64   `json:"downloaded_bytes"`
	TotalBytes      int64   `json:"total_bytes,omitempty"`
	Speed           float64 `json:"speed,omitempty"`
	ETA             int     `json:"eta,omitempty"`
	Part            int     `json:"part,omitempty"`
	Parts           int     `json:"parts,omitempty"`
}

type Media struct {
	ID         string  `json:"id,omitempty"`
	Title      string  `json:"title,omitempty"`
	Extractor  string  `json:"extractor,omitempty"`
	WebpageURL string  `json:"webpage_url,omitempty"`
	Thumbnail  string  `json:"thumbnail,omitempty"`
	Duration   float64 `json:"duration,omitempty"`
}

type FileInfo struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

type Links struct {
	Self   string `json:"self"`
	Events string `json:"events"`
	File   string `json:"file"`
}

type JobView struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Stage     string    `json:"stage,omitempty"`
	Options   Options   `json:"options"`
	Media     *Media    `json:"media,omitempty"`
	Progress  Progress  `json:"progress"`
	File      *FileInfo `json:"file,omitempty"`
	Error     *apiError `json:"error,omitempty"`
	Links     Links     `json:"links"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (v JobView) terminal() bool {
	return v.Status == StatusDone || v.Status == StatusFailed || v.Status == StatusCanceled
}

type Job struct {
	mu       sync.Mutex
	v        JobView
	path     string
	dir      string
	cancel   context.CancelFunc
	changed  chan struct{}
	lastPush time.Time
}

func (j *Job) snapshot() (JobView, <-chan struct{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.v, j.changed
}

func (j *Job) update(force bool, fn func(v *JobView)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	fn(&j.v)
	now := time.Now()
	j.v.UpdatedAt = now
	if force || now.Sub(j.lastPush) >= 150*time.Millisecond {
		j.lastPush = now
		close(j.changed)
		j.changed = make(chan struct{})
	}
}

type persisted struct {
	Job  JobView `json:"job"`
	Path string  `json:"path,omitempty"`
}

func (j *Job) persist() {
	j.mu.Lock()
	b, _ := json.Marshal(persisted{j.v, j.path})
	j.mu.Unlock()
	tmp := filepath.Join(j.dir, ".job.json")
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, filepath.Join(j.dir, "job.json"))
	}
}

type Manager struct {
	cfg    config
	ctl    *Control
	pool   *Pool
	slots  *slots
	root   string
	mu     sync.RWMutex
	jobs   map[string]*Job
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var errQueueFull = errors.New("queue is full")

func newManager(cfg config, ctl *Control, pool *Pool) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:    cfg,
		ctl:    ctl,
		pool:   pool,
		slots:  newSlots(func() int { return ctl.get().MaxJobs }),
		root:   filepath.Join(cfg.dataDir, "jobs"),
		jobs:   map[string]*Job{},
		ctx:    ctx,
		cancel: cancel,
	}
	os.MkdirAll(m.root, 0o755)
	ctl.onChange = append(ctl.onChange, func(Settings) { m.slots.wake() })
	m.restore()
	return m
}

func (m *Manager) restore() {
	entries, _ := os.ReadDir(m.root)
	for _, e := range entries {
		dir := filepath.Join(m.root, e.Name())
		var p persisted
		b, err := os.ReadFile(filepath.Join(dir, "job.json"))
		if err != nil || json.Unmarshal(b, &p) != nil || p.Job.ID != e.Name() {
			os.RemoveAll(dir)
			continue
		}
		if p.Job.Status == StatusDone {
			if _, err := os.Stat(p.Path); err != nil {
				os.RemoveAll(dir)
				continue
			}
		}
		if !p.Job.terminal() {
			p.Job.Status, p.Job.Stage = StatusFailed, ""
			p.Job.Error = &apiError{"interrupted", "server restarted while the job was running"}
		}
		j := &Job{v: p.Job, path: p.Path, dir: dir, changed: make(chan struct{})}
		m.jobs[j.v.ID] = j
		j.persist()
	}
	if len(m.jobs) > 0 {
		log.Printf("restored %d jobs", len(m.jobs))
	}
}

func newID() string {
	b := make([]byte, 9)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Manager) get(id string) *Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.jobs[id]
}

// find returns a queued, running or done job with exactly these options, if any.
func (m *Manager) find(o Options) *Job {
	key, _ := json.Marshal(o)
	m.mu.RLock()
	defer m.mu.RUnlock()
	var best *Job
	for _, j := range m.jobs {
		v, _ := j.snapshot()
		if v.Status == StatusFailed || v.Status == StatusCanceled {
			continue
		}
		if b, _ := json.Marshal(v.Options); string(b) != string(key) {
			continue
		}
		if v.Status == StatusDone {
			if _, err := os.Stat(j.path); err != nil {
				continue
			}
		}
		if best == nil || v.CreatedAt.After(best.v.CreatedAt) {
			best = j
		}
	}
	return best
}

func (m *Manager) create(o Options) (*Job, error) {
	m.mu.Lock()
	active := 0
	for _, j := range m.jobs {
		if v, _ := j.snapshot(); !v.terminal() {
			active++
		}
	}
	if s := m.ctl.get(); active >= s.MaxJobs+s.MaxQueue {
		m.mu.Unlock()
		return nil, errQueueFull
	}
	id := newID()
	now := time.Now()
	j := &Job{
		dir:     filepath.Join(m.root, id),
		changed: make(chan struct{}),
		v: JobView{
			ID: id, Status: StatusQueued, Stage: "queued", Options: o, CreatedAt: now, UpdatedAt: now,
			Links: Links{Self: "/v1/jobs/" + id, Events: "/v1/jobs/" + id + "/events", File: "/v1/jobs/" + id + "/file"},
		},
	}
	m.jobs[id] = j
	m.mu.Unlock()
	m.ctl.stats.add("jobs_created", 1)

	if err := os.MkdirAll(j.dir, 0o755); err != nil {
		m.remove(id)
		return nil, err
	}
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.timeout)
	j.cancel = cancel
	j.persist()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		m.run(ctx, j)
		if m.get(id) == nil {
			os.RemoveAll(j.dir)
			return
		}
		j.persist()
	}()
	return j, nil
}

func (m *Manager) cancelJob(id string) bool {
	j := m.get(id)
	if j == nil {
		return false
	}
	j.update(true, func(v *JobView) {
		if !v.terminal() {
			v.Status, v.Stage = StatusCanceled, ""
		}
	})
	if j.cancel != nil {
		j.cancel()
	}
	m.remove(id)
	return true
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	j := m.jobs[id]
	delete(m.jobs, id)
	m.mu.Unlock()
	if j != nil {
		os.RemoveAll(j.dir)
	}
}

func (m *Manager) janitor(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.mu.RLock()
			var stale []string
			for id, j := range m.jobs {
				if v, _ := j.snapshot(); v.terminal() && time.Since(v.UpdatedAt) > m.cfg.jobTTL {
					stale = append(stale, id)
				}
			}
			m.mu.RUnlock()
			for _, id := range stale {
				m.remove(id)
			}
		}
	}
}

func (m *Manager) shutdown() {
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) fail(j *Job, code, msg string) {
	failed := false
	j.update(true, func(v *JobView) {
		if v.Status != StatusCanceled {
			v.Status, v.Stage = StatusFailed, ""
			v.Error = &apiError{code, msg}
			failed = true
		}
	})
	if failed {
		m.ctl.stats.add("jobs_failed", 1)
	} else {
		m.ctl.stats.add("jobs_canceled", 1)
	}
}

func (m *Manager) active() (running, queued int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, j := range m.jobs {
		v, _ := j.snapshot()
		switch v.Status {
		case StatusRunning:
			running++
		case StatusQueued:
			queued++
		}
	}
	return
}

func (m *Manager) cancelAll() int {
	m.mu.RLock()
	var ids []string
	for id, j := range m.jobs {
		if v, _ := j.snapshot(); !v.terminal() {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range ids {
		m.cancelJob(id)
	}
	return len(ids)
}

// slots is a concurrency gate whose limit can change at runtime.
type slots struct {
	mu    sync.Mutex
	used  int
	limit func() int
	ch    chan struct{}
}

func newSlots(limit func() int) *slots {
	return &slots{limit: limit, ch: make(chan struct{})}
}

func (s *slots) acquire(ctx context.Context) bool {
	for {
		s.mu.Lock()
		if s.used < s.limit() {
			s.used++
			s.mu.Unlock()
			return true
		}
		ch := s.ch
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

func (s *slots) release() {
	s.mu.Lock()
	s.used--
	s.mu.Unlock()
	s.wake()
}

func (s *slots) wake() {
	s.mu.Lock()
	close(s.ch)
	s.ch = make(chan struct{})
	s.mu.Unlock()
}

func (m *Manager) run(ctx context.Context, j *Job) {
	if !m.slots.acquire(ctx) {
		m.fail(j, "timeout", "job expired while queued")
		return
	}
	defer m.slots.release()
	if ctx.Err() != nil {
		return
	}
	v, _ := j.snapshot()
	if v.Options.plain() {
		if n, ok := resolveNative(ctx, v.Options.URL); ok {
			j.update(true, func(v *JobView) { v.Status, v.Stage = StatusRunning, "resolving" })
			if m.runNative(ctx, j, n) {
				return
			}
		}
	}
	account, cookies := "", ""
	if isYouTube(v.Options.URL) {
		id, path, wait, err := m.pool.Acquire()
		if err != nil {
			m.ctl.stats.add("youtube_exhausted", 1)
			m.fail(j, "upstream_cooldown", fmt.Sprintf("%s, retry in %s", err, wait.Round(time.Second)))
			return
		}
		account, cookies = id, path
	}
	j.update(true, func(v *JobView) { v.Status, v.Stage = StatusRunning, "resolving" })

	args := append(v.Options.args(m.cfg, m.ctl.get(), cookies),
		"--newline", "--progress", "--no-simulate", "--no-mtime",
		"-P", j.dir, "-o", "%(title).120B [%(id)s].%(ext)s",
		"--print", "before_dl:META %(.{id,title,duration,extractor_key,webpage_url,thumbnail,filesize,filesize_approx})j",
		"--print", "before_dl:PARTS %(requested_formats.:.{filesize,filesize_approx})j",
		"--print", "after_move:FILE %(filepath)s",
		"--progress-template", "download:PROG %(progress.status)s|%(progress.downloaded_bytes)s|%(progress.total_bytes)s|%(progress.total_bytes_estimate)s|%(progress.speed)s|%(progress.eta)s|%(info.format_id)s",
		"--progress-template", "postprocess:POST %(progress.status)s|%(progress.postprocessor)s",
		"--", v.Options.URL,
	)
	cmd := exec.CommandContext(ctx, m.cfg.bin, args...)
	cmd.Dir = j.dir
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.fail(j, "internal", err.Error())
		return
	}
	stderr := &tailBuffer{max: 16 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		m.fail(j, "internal", "cannot start yt-dlp: "+err.Error())
		return
	}

	var (
		file      string
		parts     []int64
		part      int
		curFmt    string
		curTotal  int64
		doneBytes int64
	)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		kind, rest, _ := strings.Cut(line, " ")
		switch kind {
		case "META":
			var meta struct {
				Media
				Extractor      string `json:"extractor_key"`
				Filesize       int64  `json:"filesize"`
				FilesizeApprox int64  `json:"filesize_approx"`
			}
			if json.Unmarshal([]byte(rest), &meta) == nil {
				meta.Media.Extractor = meta.Extractor
				if len(parts) == 0 {
					parts = []int64{max(meta.Filesize, meta.FilesizeApprox)}
				}
				media := meta.Media
				j.update(true, func(v *JobView) { v.Media = &media })
			}
		case "PARTS":
			var ps []struct {
				Filesize       int64 `json:"filesize"`
				FilesizeApprox int64 `json:"filesize_approx"`
			}
			if json.Unmarshal([]byte(rest), &ps) == nil && len(ps) > 0 {
				parts = parts[:0]
				for _, p := range ps {
					parts = append(parts, max(p.Filesize, p.FilesizeApprox))
				}
			}
		case "PROG":
			f := strings.Split(rest, "|")
			if len(f) < 7 {
				continue
			}
			if f[6] != curFmt {
				if curFmt != "" {
					doneBytes += curTotal
					part++
				}
				curFmt, curTotal = f[6], 0
			}
			downloaded := atoi(f[1])
			if t := max(atoi(f[2]), atoi(f[3])); t > 0 {
				curTotal = t
			} else if part < len(parts) {
				curTotal = parts[part]
			}
			total := doneBytes + max(curTotal, downloaded)
			for i := part + 1; i < len(parts); i++ {
				total += parts[i]
			}
			p := Progress{
				DownloadedBytes: doneBytes + downloaded,
				TotalBytes:      total,
				Speed:           atof(f[4]),
				ETA:             int(atof(f[5])),
				Part:            part + 1,
				Parts:           max(len(parts), part+1),
			}
			if total > 0 {
				p.Percent = min(99.9, float64(p.DownloadedBytes)*100/float64(total))
				p.Percent = float64(int(p.Percent*10)) / 10
			}
			j.update(false, func(v *JobView) { v.Stage, v.Progress = "downloading", p })
		case "POST":
			if strings.HasPrefix(rest, "started") {
				j.update(true, func(v *JobView) { v.Stage = "processing" })
			}
		case "FILE":
			file = rest
		}
	}
	err = cmd.Wait()

	switch {
	case ctx.Err() == context.DeadlineExceeded:
		m.fail(j, "timeout", "job exceeded the time limit")
		return
	case ctx.Err() != nil:
		m.fail(j, "canceled", "job was canceled")
		return
	case err != nil:
		msg := stderr.lastError()
		m.pool.NoteFailure(account, msg)
		m.fail(j, "ytdlp_error", msg)
		return
	}
	st, err := os.Stat(file)
	if file == "" || err != nil || filepath.Dir(file) != filepath.Clean(j.dir) {
		msg := stderr.lastError()
		if msg == "" {
			msg = "yt-dlp finished without producing a file (too large or unavailable?)"
		}
		m.pool.NoteFailure(account, msg)
		m.fail(j, "no_file", msg)
		return
	}
	j.update(true, func(v *JobView) {
		j.path = file
		v.Status, v.Stage = StatusDone, ""
		v.Progress.Percent = 100
		v.Progress.DownloadedBytes = st.Size()
		v.Progress.TotalBytes = st.Size()
		v.Progress.Speed, v.Progress.ETA = 0, 0
		v.File = &FileInfo{Name: filepath.Base(file), Size: st.Size(), ContentType: contentType(file)}
	})
	m.ctl.stats.add("jobs_done", 1)
	m.ctl.stats.add("bytes_produced", st.Size())
	if v, _ := j.snapshot(); v.Media != nil {
		m.ctl.stats.extractor(v.Media.Extractor)
	}
}

// runNative downloads a progressive file found by a native extractor with
// plain http. It reports false to fall back to yt-dlp on any upstream error.
func (m *Manager) runNative(ctx context.Context, j *Job, n *NativeMedia) bool {
	o, _ := j.snapshot()
	v := n.best(o.Options.maxHeight())
	if v == nil {
		return false
	}
	media := Media{ID: n.ID, Title: n.Title, Extractor: n.Provider, WebpageURL: n.WebpageURL, Thumbnail: n.Thumbnail, Duration: n.Duration}
	j.update(true, func(v *JobView) { v.Media = &media })

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.URL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", browserUA)
	res, err := (&http.Client{Timeout: m.cfg.timeout}).Do(req)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("native download %s: %v", v.URL, err)
		}
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		log.Printf("native download %s: http %d", v.URL, res.StatusCode)
		return false
	}
	total := res.ContentLength
	if limit := parseSize(m.ctl.get().MaxFilesize); limit > 0 && total > limit {
		m.fail(j, "no_file", fmt.Sprintf("file is larger than the %s limit", m.ctl.get().MaxFilesize))
		return true
	}
	name := n.filename(v)
	if ct := res.Header.Get("Content-Type"); ct != "" && v.ContentType == "" {
		v.ContentType = ct
	}
	file := filepath.Join(j.dir, name)
	f, err := os.Create(file)
	if err != nil {
		m.fail(j, "internal", err.Error())
		return true
	}
	start := time.Now()
	last, lastBytes := start, int64(0)
	speed := 0.0
	var done int64
	buf := make([]byte, 256<<10)
	for {
		nr, rerr := res.Body.Read(buf)
		if nr > 0 {
			if _, werr := f.Write(buf[:nr]); werr != nil {
				f.Close()
				m.fail(j, "internal", werr.Error())
				return true
			}
			done += int64(nr)
			now := time.Now()
			if dt := now.Sub(last).Seconds(); dt >= 0.5 {
				inst := float64(done-lastBytes) / dt
				if speed == 0 {
					speed = inst
				} else {
					speed = speed*0.6 + inst*0.4
				}
				last, lastBytes = now, done
			}
			p := Progress{DownloadedBytes: done, TotalBytes: max(total, done), Speed: speed, Part: 1, Parts: 1}
			if total > 0 {
				p.Percent = min(99.9, float64(int(float64(done)*1000/float64(total)))/10)
				if speed > 0 {
					p.ETA = int(float64(total-done) / speed)
				}
			}
			j.update(false, func(v *JobView) { v.Stage, v.Progress = "downloading", p })
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(file)
			if ctx.Err() != nil {
				if ctx.Err() == context.DeadlineExceeded {
					m.fail(j, "timeout", "job exceeded the time limit")
				} else {
					m.fail(j, "canceled", "job was canceled")
				}
				return true
			}
			log.Printf("native download %s: %v", v.URL, rerr)
			return false
		}
	}
	if err := f.Close(); err != nil {
		m.fail(j, "internal", err.Error())
		return true
	}
	if total > 0 && done != total {
		os.Remove(file)
		return false
	}
	ctype := v.ContentType
	if ctype == "" {
		ctype = contentType(file)
	}
	j.update(true, func(v *JobView) {
		j.path = file
		v.Status, v.Stage = StatusDone, ""
		v.Progress = Progress{Percent: 100, DownloadedBytes: done, TotalBytes: done, Part: 1, Parts: 1}
		v.File = &FileInfo{Name: name, Size: done, ContentType: ctype}
	})
	m.ctl.stats.add("jobs_done", 1)
	m.ctl.stats.add("jobs_native", 1)
	m.ctl.stats.add("bytes_produced", done)
	m.ctl.stats.extractor(n.Provider)
	return true
}

func parseSize(s string) int64 {
	if s == "" || !sizeRe.MatchString(s) {
		return 0
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K':
		mult = 1 << 10
	case 'M':
		mult = 1 << 20
	case 'G':
		mult = 1 << 30
	case 'T':
		mult = 1 << 40
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	f, _ := strconv.ParseFloat(s, 64)
	return int64(f * float64(mult))
}

func atoi(s string) int64 {
	f := atof(s)
	return int64(f)
}

func atof(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

var mimeTypes = map[string]string{
	".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm", ".mkv": "video/x-matroska", ".mov": "video/quicktime",
	".m4a": "audio/mp4", ".mp3": "audio/mpeg", ".opus": "audio/ogg", ".ogg": "audio/ogg", ".flac": "audio/flac",
	".wav": "audio/wav", ".aac": "audio/aac", ".jpg": "image/jpeg", ".png": "image/png", ".webp": "image/webp",
}

func contentType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if t, ok := mimeTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) lastError() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if msg, ok := strings.CutPrefix(lines[i], "ERROR: "); ok {
			return msg
		}
	}
	if len(lines) > 0 {
		return lines[len(lines)-1]
	}
	return ""
}
