package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"mime"
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
	root   string
	mu     sync.RWMutex
	jobs   map[string]*Job
	sem    chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var errQueueFull = errors.New("queue is full")

func newManager(cfg config) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:    cfg,
		root:   filepath.Join(cfg.dataDir, "jobs"),
		jobs:   map[string]*Job{},
		sem:    make(chan struct{}, cfg.maxJobs),
		ctx:    ctx,
		cancel: cancel,
	}
	os.MkdirAll(m.root, 0o755)
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

func (m *Manager) create(o Options) (*Job, error) {
	m.mu.Lock()
	active := 0
	for _, j := range m.jobs {
		if v, _ := j.snapshot(); !v.terminal() {
			active++
		}
	}
	if active >= m.cfg.maxJobs+m.cfg.maxQueue {
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
	t := time.NewTicker(time.Minute)
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
	j.update(true, func(v *JobView) {
		if v.Status != StatusCanceled {
			v.Status, v.Stage = StatusFailed, ""
			v.Error = &apiError{code, msg}
		}
	})
}

func (m *Manager) run(ctx context.Context, j *Job) {
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		m.fail(j, "timeout", "job expired while queued")
		return
	}
	if ctx.Err() != nil {
		return
	}
	v, _ := j.snapshot()
	j.update(true, func(v *JobView) { v.Status, v.Stage = StatusRunning, "resolving" })

	args := append(v.Options.args(m.cfg),
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
		m.fail(j, "ytdlp_error", stderr.lastError())
		return
	}
	st, err := os.Stat(file)
	if file == "" || err != nil || filepath.Dir(file) != filepath.Clean(j.dir) {
		msg := stderr.lastError()
		if msg == "" {
			msg = "yt-dlp finished without producing a file (too large or unavailable?)"
		}
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
