package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type server struct {
	cfg  config
	m    *Manager
	ctl  *Control
	pool *Pool
	info *slots
}

func newServer(cfg config, m *Manager) http.Handler {
	s := &server{cfg: cfg, m: m, ctl: m.ctl, pool: m.pool}
	s.info = newSlots(func() int { return s.ctl.get().MaxJobs * 2 })
	web, _ := fs.Sub(webFS, "web")
	static := func(name, ctype string) http.HandlerFunc {
		b, _ := fs.ReadFile(web, name)
		etag := fmt.Sprintf(`"%x"`, sha256.Sum256(b))
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Write(b)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", static("index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /llms.txt", static("llms.txt", "text/plain; charset=utf-8"))
	mux.HandleFunc("GET /docs.txt", static("llms.txt", "text/plain; charset=utf-8"))
	mux.HandleFunc("GET /openapi.json", static("openapi.json", "application/json"))
	mux.HandleFunc("GET /healthz", s.health)

	v1 := http.NewServeMux()
	v1.HandleFunc("GET /v1/info", s.handleInfo)
	v1.HandleFunc("GET /v1/extractors", s.extractors)
	v1.HandleFunc("POST /v1/jobs", s.createJob)
	v1.HandleFunc("GET /v1/jobs/{id}", s.getJob)
	v1.HandleFunc("DELETE /v1/jobs/{id}", s.deleteJob)
	v1.HandleFunc("GET /v1/jobs/{id}/events", s.events)
	v1.HandleFunc("GET /v1/jobs/{id}/file", s.file)
	v1.HandleFunc("GET /v1/download", s.download)
	mux.Handle("/v1/", s.auth(v1))
	mux.Handle("/admin/", s.adminRoutes())

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "no route for "+r.Method+" "+r.URL.Path)
	})
	return s.cors(logged(mux))
}

func (s *server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", s.cfg.cors)
		h.Set("Access-Control-Expose-Headers", "Content-Disposition, Content-Length, Content-Range, Location")
		if r.Method == http.MethodOptions {
			h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Range")
			h.Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) auth(next http.Handler) http.Handler {
	if s.cfg.apiKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "" {
			key = r.URL.Query().Get("key")
		}
		if subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.apiKey)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid api key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{w, 200}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}

func jsonDecode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

var ytdlpVersion = sync.OnceValue(func() string {
	out, err := exec.Command(loadConfig().bin, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
})

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	v := ytdlpVersion()
	status := http.StatusOK
	if v == "" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ok": v != "", "yt_dlp": v})
}

// admit applies pause, rate limits and the youtube pool before any work.
func (s *server) admit(w http.ResponseWriter, r *http.Request, rawURL string) bool {
	wait, err := s.ctl.admit(r)
	if err == nil && isYouTube(rawURL) {
		if d := s.pool.Wait(); d > 0 {
			wait, err = d, errPoolExhausted
		}
	}
	if err == nil {
		return true
	}
	secs := max(1, int(wait.Round(time.Second).Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	switch err {
	case errPaused:
		s.ctl.stats.add("paused_rejects", 1)
		writeErr(w, http.StatusServiceUnavailable, "paused", "this instance is paused, try again later")
	case errPoolExhausted:
		s.ctl.stats.add("youtube_exhausted", 1)
		writeErr(w, http.StatusServiceUnavailable, "upstream_cooldown", fmt.Sprintf("youtube is cooling down, retry in %ds", secs))
	default:
		s.ctl.stats.add("rate_limited", 1)
		writeErr(w, http.StatusTooManyRequests, "rate_limited", fmt.Sprintf("slow down, retry in %ds", secs))
	}
	return false
}

func (s *server) createJob(w http.ResponseWriter, r *http.Request) {
	o, err := decodeOptions(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "invalid body: "+err.Error())
		return
	}
	if err := o.validate(s.cfg, s.ctl.get()); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !s.admit(w, r, o.URL) {
		return
	}
	j, err := s.m.create(o)
	if errors.Is(err, errQueueFull) {
		writeErr(w, http.StatusTooManyRequests, "queue_full", "too many jobs in flight, retry later")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	v, _ := j.snapshot()
	w.Header().Set("Location", v.Links.Self)
	writeJSON(w, http.StatusAccepted, v)
}

func (s *server) job(w http.ResponseWriter, r *http.Request) *Job {
	j := s.m.get(r.PathValue("id"))
	if j == nil {
		writeErr(w, http.StatusNotFound, "not_found", "job not found or expired")
	}
	return j
}

func (s *server) getJob(w http.ResponseWriter, r *http.Request) {
	if j := s.job(w, r); j != nil {
		v, _ := j.snapshot()
		writeJSON(w, http.StatusOK, v)
	}
}

func (s *server) deleteJob(w http.ResponseWriter, r *http.Request) {
	if !s.m.cancelJob(r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "not_found", "job not found or expired")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	j := s.job(w, r)
	if j == nil {
		return
	}
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 2000\n\n")

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		v, changed := j.snapshot()
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: job\ndata: %s\n\n", b)
		if v.terminal() {
			fmt.Fprint(w, "event: end\ndata: {}\n\n")
			rc.Flush()
			return
		}
		if rc.Flush() != nil {
			return
		}
		for waiting := true; waiting; {
			select {
			case <-changed:
				waiting = false
			case <-ping.C:
				fmt.Fprint(w, ": ping\n\n")
				if rc.Flush() != nil {
					return
				}
			case <-r.Context().Done():
				return
			}
		}
	}
}

func (s *server) file(w http.ResponseWriter, r *http.Request) {
	j := s.job(w, r)
	if j == nil {
		return
	}
	v, _ := j.snapshot()
	if v.Status != StatusDone {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": apiError{"not_ready", "job is " + v.Status},
			"job":   v,
		})
		return
	}
	serveFile(w, r, j, v)
}

func serveFile(w http.ResponseWriter, r *http.Request, j *Job, v JobView) {
	f, err := os.Open(j.path)
	if err != nil {
		writeErr(w, http.StatusGone, "gone", "file is no longer available")
		return
	}
	defer f.Close()
	disp := "attachment"
	if r.URL.Query().Get("inline") == "1" {
		disp = "inline"
	}
	w.Header().Set("Content-Type", v.File.ContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q; filename*=UTF-8''%s", disp, asciiName(v.File.Name), url.PathEscape(v.File.Name)))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "", v.UpdatedAt, f)
}

func asciiName(s string) string {
	b := make([]rune, 0, len(s))
	for _, c := range s {
		if c < 32 || c > 126 || c == '"' || c == '\\' {
			c = '_'
		}
		b = append(b, c)
	}
	return string(b)
}

func (s *server) download(w http.ResponseWriter, r *http.Request) {
	o := optionsFromQuery(r.URL.Query())
	if err := o.validate(s.cfg, s.ctl.get()); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !s.admit(w, r, o.URL) {
		return
	}
	j, err := s.m.create(o)
	if errors.Is(err, errQueueFull) {
		writeErr(w, http.StatusTooManyRequests, "queue_full", "too many jobs in flight, retry later")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for {
		v, changed := j.snapshot()
		if v.terminal() {
			if v.Status != StatusDone {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": v.Error, "job": v})
				return
			}
			serveFile(w, r, j, v)
			return
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			s.m.cancelJob(v.ID)
			return
		}
	}
}

type infoFormat struct {
	FormatID       string  `json:"format_id"`
	Ext            string  `json:"ext,omitempty"`
	Resolution     string  `json:"resolution,omitempty"`
	Width          int     `json:"width,omitempty"`
	Height         int     `json:"height,omitempty"`
	FPS            float64 `json:"fps,omitempty"`
	VCodec         string  `json:"vcodec,omitempty"`
	ACodec         string  `json:"acodec,omitempty"`
	Filesize       int64   `json:"filesize,omitempty"`
	FilesizeApprox int64   `json:"filesize_approx,omitempty"`
	TBR            float64 `json:"tbr,omitempty"`
	Protocol       string  `json:"protocol,omitempty"`
	FormatNote     string  `json:"format_note,omitempty"`
}

type thumb struct {
	URL string `json:"url"`
}

type infoEntry struct {
	ID         string  `json:"id,omitempty"`
	Title      string  `json:"title,omitempty"`
	URL        string  `json:"url,omitempty"`
	WebpageURL string  `json:"webpage_url,omitempty"`
	Duration   float64 `json:"duration,omitempty"`
	Uploader   string  `json:"uploader,omitempty"`
	Thumbnail  string  `json:"thumbnail,omitempty"`
	Thumbnails []thumb `json:"thumbnails,omitempty"`
}

type info struct {
	Type          string       `json:"_type,omitempty"`
	ID            string       `json:"id,omitempty"`
	Title         string       `json:"title,omitempty"`
	Description   string       `json:"description,omitempty"`
	Uploader      string       `json:"uploader,omitempty"`
	Channel       string       `json:"channel,omitempty"`
	ChannelURL    string       `json:"channel_url,omitempty"`
	UploadDate    string       `json:"upload_date,omitempty"`
	Duration      float64      `json:"duration,omitempty"`
	ViewCount     int64        `json:"view_count,omitempty"`
	LikeCount     int64        `json:"like_count,omitempty"`
	IsLive        bool         `json:"is_live,omitempty"`
	Thumbnail     string       `json:"thumbnail,omitempty"`
	Extractor     string       `json:"extractor_key,omitempty"`
	WebpageURL    string       `json:"webpage_url,omitempty"`
	PlaylistCount int          `json:"playlist_count,omitempty"`
	Formats       []infoFormat `json:"formats,omitempty"`
	Entries       []infoEntry  `json:"entries,omitempty"`
}

func (s *server) handleInfo(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	u := q.Get("url")
	if err := checkURL(u, s.cfg.allowPrivate); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	playlist := q.Get("playlist") == "true" || q.Get("playlist") == "1"
	raw := q.Get("raw") == "1" || q.Get("raw") == "true"
	s.ctl.stats.add("info_requests", 1)
	key := fmt.Sprintf("%t|%t|%s", playlist, raw, u)
	if body, ok := s.ctl.cache.get(key); ok {
		s.ctl.stats.add("info_cache_hits", 1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Cache", "hit")
		w.Write(body)
		return
	}
	if !s.admit(w, r, u) {
		return
	}
	if !s.info.acquire(r.Context()) {
		return
	}
	defer s.info.release()
	account, cookies := "", ""
	if isYouTube(u) {
		id, path, wait, err := s.pool.Acquire()
		if err != nil {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(wait.Seconds()))))
			writeErr(w, http.StatusServiceUnavailable, "upstream_cooldown", err.Error())
			return
		}
		account, cookies = id, path
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	args := append([]string{"-J", "--no-warnings", "--flat-playlist"}, commonArgs(s.cfg, s.ctl.get(), cookies)...)
	if !playlist {
		args = append(args, "--no-playlist")
	}
	args = append(args, "--", u)
	cmd := exec.CommandContext(ctx, s.cfg.bin, args...)
	stderr := &tailBuffer{max: 8 << 10}
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			writeErr(w, http.StatusGatewayTimeout, "timeout", "extraction timed out")
			return
		}
		msg := stderr.lastError()
		s.pool.NoteFailure(account, msg)
		writeErr(w, http.StatusUnprocessableEntity, "ytdlp_error", msg)
		return
	}
	ttl := time.Duration(s.ctl.get().InfoCacheTTL) * time.Second
	if raw {
		s.ctl.cache.put(key, out, ttl)
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
		return
	}
	var in info
	if err := json.Unmarshal(out, &in); err != nil {
		writeErr(w, http.StatusBadGateway, "ytdlp_error", "unreadable yt-dlp output")
		return
	}
	if in.Type == "" {
		in.Type = "video"
	}
	for i := range in.Entries {
		e := &in.Entries[i]
		if e.Thumbnail == "" && len(e.Thumbnails) > 0 {
			e.Thumbnail = e.Thumbnails[len(e.Thumbnails)-1].URL
		}
		e.Thumbnails = nil
		if e.WebpageURL == "" {
			e.WebpageURL = e.URL
		}
	}
	if in.Type == "playlist" && in.PlaylistCount == 0 {
		in.PlaylistCount = len(in.Entries)
	}
	if body, err := json.Marshal(in); err == nil {
		s.ctl.cache.put(key, append(body, '\n'), ttl)
	}
	writeJSON(w, http.StatusOK, in)
}

var (
	extractorsOnce sync.Once
	extractorList  []string
)

func (s *server) extractors(w http.ResponseWriter, r *http.Request) {
	extractorsOnce.Do(func() {
		out, err := exec.Command(s.cfg.bin, "--ignore-config", "--list-extractors").Output()
		if err != nil {
			return
		}
		for _, l := range strings.Split(string(out), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.Contains(l, "(CURRENTLY BROKEN)") {
				extractorList = append(extractorList, l)
			}
		}
	})
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, http.StatusOK, map[string]any{"count": len(extractorList), "extractors": extractorList})
}
