package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Embed describes how to embed a page: youtube reuses youtube's own player,
// everything else gets a progressive mp4 served by this instance.
type Embed struct {
	Type      string  `json:"type"`
	Provider  string  `json:"provider"`
	URL       string  `json:"url"`
	ID        string  `json:"id,omitempty"`
	Title     string  `json:"title,omitempty"`
	Author    string  `json:"author,omitempty"`
	Thumbnail string  `json:"thumbnail,omitempty"`
	Duration  float64 `json:"duration,omitempty"`
	Width     int     `json:"width,omitempty"`
	Height    int     `json:"height,omitempty"`
	EmbedURL  string  `json:"embed_url"`
	PlayerURL string  `json:"player_url"`
	VideoURL  string  `json:"video_url,omitempty"`
	Native    bool    `json:"native"`
	HTML      string  `json:"html"`
}

type httpErr struct {
	status int
	code   string
	msg    string
	retry  int
}

func (e *httpErr) Error() string { return e.msg }

func (e *httpErr) write(w http.ResponseWriter) {
	if e.retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.retry))
	}
	writeErr(w, e.status, e.code, e.msg)
}

var (
	schemeFix  = regexp.MustCompile(`^(https?):/+`)
	youtubeIDs = regexp.MustCompile(`(?i)(?:youtu\.be/|/(?:watch\?(?:.*&)?v=|shorts/|embed/|live/|v/))([\w-]{11})`)
)

func youtubeID(raw string) string {
	if !isYouTube(raw) {
		return ""
	}
	if m := youtubeIDs.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	return ""
}

// base returns the public origin of this instance for absolute links.
func (s *server) base(r *http.Request) string {
	if s.cfg.publicURL != "" {
		return s.cfg.publicURL
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *server) embedBase(r *http.Request) string {
	if s.cfg.embedHost != "" {
		return "https://" + s.cfg.embedHost
	}
	return s.base(r) + "/embed"
}

func hostOf(r *http.Request) string {
	h := strings.ToLower(r.Host)
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return h
}

// embedTarget rebuilds the url that follows the prefix in the request path,
// tolerating a missing scheme and the slashes the client may have collapsed.
func embedTarget(r *http.Request, prefix string) string {
	if u := r.URL.Query().Get("url"); u != "" && strings.TrimPrefix(r.URL.Path, prefix) == "" {
		return u
	}
	raw := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	if r.URL.RawQuery != "" {
		raw += "?" + r.URL.RawQuery
	}
	return normalizeTarget(raw)
}

// normalizeTarget adds a scheme when missing and repairs collapsed slashes.
func normalizeTarget(raw string) string {
	raw = strings.TrimLeft(strings.TrimSpace(raw), "/")
	if raw == "" {
		return ""
	}
	if m := schemeFix.FindStringSubmatch(raw); m != nil {
		return m[1] + "://" + raw[len(m[0]):]
	}
	return "https://" + raw
}

func embedOptions(u string) Options {
	return Options{URL: u, Quality: "1080", Codec: "h264", Container: "mp4"}
}

// resolveEmbed builds the Embed for a url: youtube via oembed, native
// extractors when available, yt-dlp metadata otherwise.
func (s *server) resolveEmbed(ctx context.Context, r *http.Request, raw string) (Embed, *httpErr) {
	if err := checkURL(raw, s.cfg.allowPrivate); err != nil {
		return Embed{}, &httpErr{status: 400, code: "invalid_request", msg: err.Error()}
	}
	eb := s.embedBase(r)
	e := Embed{Type: "video", URL: raw, EmbedURL: eb + "/" + raw}
	if id := youtubeID(raw); id != "" {
		e.Type, e.Provider, e.ID, e.Native = "youtube", "YouTube", id, true
		e.URL = "https://www.youtube.com/watch?v=" + id
		e.EmbedURL = eb + "/youtu.be/" + id
		e.PlayerURL = "https://www.youtube.com/embed/" + id
		e.Width, e.Height = 1280, 720
		e.Thumbnail = "https://i.ytimg.com/vi/" + id + "/hqdefault.jpg"
		if o, err := s.youtubeOEmbed(ctx, id); err == nil {
			e.Title, e.Author = o.Title, o.Author
			if o.Thumbnail != "" {
				e.Thumbnail = o.Thumbnail
			}
		}
		e.HTML = fmt.Sprintf(`<iframe width="%d" height="%d" src="%s" frameborder="0" allow="autoplay; encrypted-media; picture-in-picture" allowfullscreen></iframe>`, e.Width, e.Height, e.PlayerURL)
		return e, nil
	}
	e.PlayerURL = e.EmbedURL
	e.VideoURL = eb + "/media?url=" + url.QueryEscape(raw)
	if n, ok := resolveNative(ctx, raw); ok {
		e.Native = true
		e.Provider, e.ID, e.Title, e.Author, e.Thumbnail, e.Duration = n.Provider, n.ID, n.Title, n.Author, n.Thumbnail, n.Duration
		if n.WebpageURL != "" {
			e.URL = n.WebpageURL
		}
		if v := n.best(1080); v != nil {
			e.Width, e.Height = v.Width, v.Height
		}
	} else {
		body, herr := s.ytdlpInfo(ctx, raw, false)
		if herr != nil {
			return e, herr
		}
		var in info
		json.Unmarshal(body, &in)
		e.Provider, e.ID, e.Title, e.Author, e.Thumbnail, e.Duration = in.Extractor, in.ID, in.Title, in.Uploader, in.Thumbnail, in.Duration
		if e.Author == "" {
			e.Author = in.Channel
		}
		if in.WebpageURL != "" {
			e.URL = in.WebpageURL
		}
		for _, f := range in.Formats {
			if f.Height > 0 && f.Height <= 1080 && f.Height >= e.Height && f.VCodec != "none" {
				e.Width, e.Height = f.Width, f.Height
			}
		}
	}
	if e.Width == 0 || e.Height == 0 {
		e.Width, e.Height = 1280, 720
	}
	e.HTML = fmt.Sprintf(`<video controls playsinline preload="metadata" width="%d" height="%d" poster="%s" src="%s"></video>`, e.Width, e.Height, template.HTMLEscapeString(e.Thumbnail), template.HTMLEscapeString(e.VideoURL))
	return e, nil
}

type oembed struct {
	Title     string `json:"title"`
	Author    string `json:"author_name"`
	Thumbnail string `json:"thumbnail_url"`
}

var embedClient = &http.Client{Timeout: 15 * time.Second}

func (s *server) youtubeOEmbed(ctx context.Context, id string) (oembed, error) {
	key := "oembed|" + id
	if b, ok := s.ctl.cache.get(key); ok {
		var o oembed
		return o, json.Unmarshal(b, &o)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.youtube.com/oembed?format=json&url="+url.QueryEscape("https://www.youtube.com/watch?v="+id), nil)
	res, err := embedClient.Do(req)
	if err != nil {
		return oembed{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return oembed{}, fmt.Errorf("oembed %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return oembed{}, err
	}
	var o oembed
	if err := json.Unmarshal(b, &o); err != nil {
		return oembed{}, err
	}
	s.ctl.cache.put(key, b, time.Hour)
	return o, nil
}

// ytdlpInfo runs yt-dlp -J once per url and caches the raw output.
func (s *server) ytdlpInfo(ctx context.Context, u string, playlist bool) ([]byte, *httpErr) {
	key := fmt.Sprintf("J|%t|%s", playlist, u)
	if body, ok := s.ctl.cache.get(key); ok {
		s.ctl.stats.add("info_cache_hits", 1)
		return body, nil
	}
	if !s.info.acquire(ctx) {
		return nil, &httpErr{status: 503, code: "busy", msg: "too many extractions in flight", retry: 2}
	}
	defer s.info.release()
	account, cookies := "", ""
	if isYouTube(u) {
		id, path, wait, err := s.pool.Acquire()
		if err != nil {
			return nil, &httpErr{status: 503, code: "upstream_cooldown", msg: err.Error(), retry: max(1, int(wait.Seconds()))}
		}
		account, cookies = id, path
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
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
			return nil, &httpErr{status: 504, code: "timeout", msg: "extraction timed out"}
		}
		msg := stderr.lastError()
		s.pool.NoteFailure(account, msg)
		return nil, &httpErr{status: 422, code: "ytdlp_error", msg: msg}
	}
	s.ctl.cache.put(key, out, time.Duration(s.ctl.get().InfoCacheTTL)*time.Second)
	return out, nil
}

// embedRouter serves the embed host (or /embed on the main host).
func (s *server) embedRouter(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
			return
		}
		switch strings.TrimPrefix(r.URL.Path, prefix) {
		case "/media":
			s.embedMedia(w, r)
		case "/oembed":
			s.embedOEmbed(w, r)
		case "", "/":
			if r.URL.Query().Get("url") == "" {
				http.Redirect(w, r, s.base(r)+"/#embed", http.StatusFound)
				return
			}
			s.embedPage(w, r, prefix)
		default:
			s.embedPage(w, r, prefix)
		}
	})
}

func (s *server) embedJSON(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("url")
	s.ctl.stats.add("embed_requests", 1)
	e, herr := s.resolveEmbed(r.Context(), r, u)
	if herr != nil {
		herr.write(w)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, e)
}

func (s *server) embedPage(w http.ResponseWriter, r *http.Request, prefix string) {
	raw := embedTarget(r, prefix)
	s.ctl.stats.add("embed_pages", 1)
	e, herr := s.resolveEmbed(r.Context(), r, raw)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if herr != nil {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(herr.status)
		embedErrTpl.Execute(w, map[string]any{"URL": raw, "Error": herr.msg, "Base": s.base(r)})
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Robots-Tag", "noindex")
	desc := e.Provider
	if e.Author != "" {
		desc = e.Author + " · " + e.Provider
	}
	embedTpl.Execute(w, map[string]any{
		"E": e, "Description": desc, "Base": s.base(r),
		"OEmbed":   s.embedBase(r) + "/oembed?url=" + url.QueryEscape(e.EmbedURL),
		"Download": s.base(r) + "/v1/download?url=" + url.QueryEscape(e.URL),
		"Duration": fmtDuration(e.Duration),
	})
}

func fmtDuration(d float64) string {
	if d <= 0 {
		return ""
	}
	n := int(d + 0.5)
	if n >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", n/3600, n%3600/60, n%60)
	}
	return fmt.Sprintf("%d:%02d", n/60, n%60)
}

func (s *server) embedOEmbed(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	eb := s.embedBase(r) + "/"
	if strings.HasPrefix(raw, eb) {
		raw = normalizeTarget(strings.TrimPrefix(raw, eb))
	}
	e, herr := s.resolveEmbed(r.Context(), r, raw)
	if herr != nil {
		herr.write(w)
		return
	}
	out := map[string]any{
		"version": "1.0", "type": "video", "provider_name": e.Provider, "provider_url": e.URL,
		"title": e.Title, "author_name": e.Author, "author_url": e.URL,
		"html": e.HTML, "width": e.Width, "height": e.Height,
	}
	if e.Thumbnail != "" {
		out["thumbnail_url"], out["thumbnail_width"], out["thumbnail_height"] = e.Thumbnail, e.Width, e.Height
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, out)
}

// embedMedia streams a progressive mp4 for the url: proxied from the origin
// when a native extractor found one, otherwise produced by a (shared) job.
func (s *server) embedMedia(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if err := checkURL(raw, s.cfg.allowPrivate); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	s.ctl.stats.add("embed_media", 1)
	if n, ok := resolveNative(r.Context(), raw); ok {
		if v := n.best(1080); v != nil {
			s.ctl.stats.add("native_streams", 1)
			proxyMedia(w, r, v.URL, v.ContentType, n.filename(v))
			return
		}
	}
	o := embedOptions(raw)
	if err := o.validate(s.cfg, s.ctl.get()); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	j := s.m.find(o)
	if j == nil {
		if !s.admit(w, r, raw) {
			return
		}
		var err error
		if j, err = s.m.create(o); errors.Is(err, errQueueFull) {
			writeErr(w, http.StatusTooManyRequests, "queue_full", "too many jobs in flight, retry later")
			return
		} else if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	for {
		v, changed := j.snapshot()
		if v.terminal() {
			if v.Status != StatusDone {
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": v.Error, "job": v})
				return
			}
			serveFile(w, r, j, v, true)
			return
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
	}
}

// proxyMedia streams an upstream file, passing range requests through.
func proxyMedia(w http.ResponseWriter, r *http.Request, src, ctype, name string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, src, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream", err.Error())
		return
	}
	req.Header.Set("User-Agent", browserUA)
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	res, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream", err.Error())
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 200 && res.StatusCode != 206 {
		writeErr(w, http.StatusBadGateway, "upstream", fmt.Sprintf("origin answered %d", res.StatusCode))
		return
	}
	h := w.Header()
	for _, k := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if v := res.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	if ctype == "" {
		ctype = res.Header.Get("Content-Type")
	}
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", fmt.Sprintf("inline; filename=%q; filename*=UTF-8''%s", asciiName(name), url.PathEscape(name)))
	h.Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(res.StatusCode)
	if r.Method != http.MethodHead {
		io.Copy(w, res.Body)
	}
}

var embedTpl = template.Must(template.New("embed").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{if .E.Title}}{{.E.Title}}{{else}}{{.E.Provider}}{{end}}</title>
<meta name="theme-color" content="#0f0f15">
<meta name="robots" content="noindex">
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><text y='.9em' font-size='90'>📼</text></svg>">
<link rel="canonical" href="{{.E.URL}}">
<link rel="alternate" type="application/json+oembed" href="{{.OEmbed}}">
<meta property="og:site_name" content="{{.E.Provider}}">
<meta property="og:type" content="video.other">
<meta property="og:url" content="{{.E.EmbedURL}}">
<meta property="og:title" content="{{if .E.Title}}{{.E.Title}}{{else}}{{.E.Provider}}{{end}}">
<meta property="og:description" content="{{.Description}}">
{{if .E.Thumbnail}}<meta property="og:image" content="{{.E.Thumbnail}}">
<meta property="og:image:width" content="{{.E.Width}}">
<meta property="og:image:height" content="{{.E.Height}}">{{end}}
{{if eq .E.Type "youtube"}}<meta property="og:video" content="{{.E.PlayerURL}}">
<meta property="og:video:url" content="{{.E.PlayerURL}}">
<meta property="og:video:secure_url" content="{{.E.PlayerURL}}">
<meta property="og:video:type" content="text/html">
<meta property="og:video:width" content="{{.E.Width}}">
<meta property="og:video:height" content="{{.E.Height}}">
<meta name="twitter:card" content="player">
<meta name="twitter:player" content="{{.E.PlayerURL}}">
<meta name="twitter:player:width" content="{{.E.Width}}">
<meta name="twitter:player:height" content="{{.E.Height}}">{{else}}<meta property="og:video" content="{{.E.VideoURL}}">
<meta property="og:video:url" content="{{.E.VideoURL}}">
<meta property="og:video:secure_url" content="{{.E.VideoURL}}">
<meta property="og:video:type" content="video/mp4">
<meta property="og:video:width" content="{{.E.Width}}">
<meta property="og:video:height" content="{{.E.Height}}">
<meta name="twitter:card" content="player">
<meta name="twitter:player" content="{{.E.PlayerURL}}">
<meta name="twitter:player:width" content="{{.E.Width}}">
<meta name="twitter:player:height" content="{{.E.Height}}">
<meta name="twitter:player:stream" content="{{.E.VideoURL}}">
<meta name="twitter:player:stream:content_type" content="video/mp4">{{end}}
<meta name="twitter:title" content="{{if .E.Title}}{{.E.Title}}{{else}}{{.E.Provider}}{{end}}">
{{if .E.Thumbnail}}<meta name="twitter:image" content="{{.E.Thumbnail}}">{{end}}
<style>
:root { color-scheme: dark; }
* { box-sizing: border-box; }
html, body { height: 100%; }
body { margin: 0; background: #000; color: #ededed; font: 13px/1.6 "Geist Mono", ui-monospace, SFMono-Regular, Menlo, monospace; display: grid; place-items: center; padding: 16px; }
main { width: min(100%, 960px); display: grid; gap: 12px; animation: in .6s cubic-bezier(.22,1,.36,1) both; }
@keyframes in { from { opacity: 0; transform: translateY(6px); filter: blur(6px); } }
.frame { position: relative; width: 100%; aspect-ratio: {{.E.Width}} / {{.E.Height}}; max-height: 80vh; margin: 0 auto; border-radius: 14px; overflow: hidden; background: #0f0f15; border: 1px solid #223249; box-shadow: 0 30px 80px rgba(0,0,0,.6); }
.frame iframe, .frame video { position: absolute; inset: 0; width: 100%; height: 100%; border: 0; background: #000; }
.bar { display: flex; flex-wrap: wrap; gap: 6px 16px; align-items: baseline; color: #737373; padding: 0 4px; }
.bar strong { color: #ededed; font-weight: 500; flex: 1 1 100%; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.bar a { color: #a3a3a3; text-decoration: none; }
.bar a:hover { color: #fff; }
.bar .r { margin-left: auto; display: flex; gap: 16px; }
</style>
</head>
<body>
<main>
  <div class="frame">{{if eq .E.Type "youtube"}}<iframe src="{{.E.PlayerURL}}?autoplay=1&rel=0" allow="autoplay; encrypted-media; picture-in-picture" allowfullscreen></iframe>{{else}}<video controls autoplay playsinline preload="metadata"{{if .E.Thumbnail}} poster="{{.E.Thumbnail}}"{{end}} src="{{.E.VideoURL}}"></video>{{end}}</div>
  <div class="bar">
    <strong>{{if .E.Title}}{{.E.Title}}{{else}}{{.E.Provider}}{{end}}</strong>
    <span>{{.Description}}{{if .Duration}} · {{.Duration}}{{end}}</span>
    <span class="r"><a href="{{.E.URL}}" rel="noopener">open original ↗</a>{{if ne .E.Type "youtube"}}<a href="{{.Download}}">download ↓</a>{{end}}</span>
  </div>
</main>
</body>
</html>
`))

var embedErrTpl = template.Must(template.New("err").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>embed</title>
<meta name="robots" content="noindex">
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#000;color:#ededed;font:13px/1.6 "Geist Mono",ui-monospace,Menlo,monospace;padding:16px}main{max-width:560px;display:grid;gap:8px}b{font-weight:500;color:#c34043}code{color:#e6c384;word-break:break-all}p{margin:0;color:#737373}a{color:#a3a3a3}</style>
</head><body><main>
<b>{{if .URL}}couldn’t embed{{else}}x · embed anything{{end}}</b>
{{if .URL}}<code>{{.URL}}</code><p>{{.Error}}</p>{{else}}<p>put a link after the slash: <code>x.mnl.rocks/https://x.com/…/status/…</code></p>{{end}}
<p><a href="{{.Base}}">{{.Base}}</a></p>
</main></body></html>
`))
