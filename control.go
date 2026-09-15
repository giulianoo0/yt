package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Settings struct {
	Paused          bool    `json:"paused"`
	MaxJobs         int     `json:"max_jobs"`
	MaxQueue        int     `json:"max_queue"`
	GlobalPerMin    float64 `json:"global_per_min"`
	GlobalBurst     int     `json:"global_burst"`
	ClientPerMin    float64 `json:"client_per_min"`
	ClientBurst     int     `json:"client_burst"`
	InfoCacheTTL    int     `json:"info_cache_ttl"`
	SleepRequests   float64 `json:"sleep_requests"`
	YouTubeCooldown int     `json:"youtube_cooldown"`
	AccountPerHour  int     `json:"account_per_hour"`
	GuestPerHour    int     `json:"guest_per_hour"`
	UseGuest        bool    `json:"use_guest"`
	MaxFilesize     string  `json:"max_filesize"`
	AllowArgs       bool    `json:"allow_args"`
}

func defaultSettings(cfg config) Settings {
	return Settings{
		MaxJobs:         max(1, envInt("MAX_JOBS", 3)),
		MaxQueue:        max(1, envInt("MAX_QUEUE", 64)),
		GlobalPerMin:    float64(envInt("GLOBAL_PER_MIN", 4)),
		GlobalBurst:     envInt("GLOBAL_BURST", 10),
		ClientPerMin:    float64(envInt("CLIENT_PER_MIN", 2)),
		ClientBurst:     envInt("CLIENT_BURST", 4),
		InfoCacheTTL:    envInt("INFO_CACHE_TTL", 600),
		SleepRequests:   0,
		YouTubeCooldown: envInt("YOUTUBE_COOLDOWN", 1800),
		AccountPerHour:  envInt("ACCOUNT_PER_HOUR", 1000),
		GuestPerHour:    envInt("GUEST_PER_HOUR", 250),
		UseGuest:        envBool("USE_GUEST", true),
		MaxFilesize:     cfg.maxFilesize,
		AllowArgs:       cfg.allowArgs,
	}
}

func (s *Settings) validate() error {
	switch {
	case s.MaxJobs < 1 || s.MaxJobs > 64:
		return errors.New("max_jobs must be 1-64")
	case s.MaxQueue < 0 || s.MaxQueue > 10000:
		return errors.New("max_queue must be 0-10000")
	case s.GlobalPerMin < 0 || s.ClientPerMin < 0:
		return errors.New("per_min must be >= 0 (0 disables)")
	case s.GlobalBurst < 1 || s.ClientBurst < 1:
		return errors.New("burst must be >= 1")
	case s.InfoCacheTTL < 0 || s.YouTubeCooldown < 0:
		return errors.New("ttl and cooldown must be >= 0")
	case s.SleepRequests < 0 || s.SleepRequests > 10:
		return errors.New("sleep_requests must be 0-10")
	case s.AccountPerHour < 0 || s.GuestPerHour < 0:
		return errors.New("per_hour limits must be >= 0 (0 disables)")
	case s.MaxFilesize != "" && !sizeRe.MatchString(s.MaxFilesize):
		return errors.New("max_filesize must look like 500M or 4G")
	}
	return nil
}

type Control struct {
	cfg      config
	path     string
	settings atomic.Pointer[Settings]
	onChange []func(Settings)

	global  *bucket
	clients *clientLimiter

	stats   *Stats
	cache   *infoCache
	started time.Time
}

func newControl(cfg config) *Control {
	c := &Control{
		cfg:     cfg,
		path:    filepath.Join(cfg.dataDir, "settings.json"),
		global:  &bucket{},
		clients: newClientLimiter(),
		stats:   newStats(filepath.Join(cfg.dataDir, "stats.json")),
		cache:   &infoCache{items: map[string]cacheItem{}},
		started: time.Now(),
	}
	s := defaultSettings(cfg)
	if b, err := os.ReadFile(c.path); err == nil {
		saved := s
		if json.Unmarshal(b, &saved) == nil && saved.validate() == nil {
			s = saved
		}
	}
	c.settings.Store(&s)
	return c
}

func (c *Control) get() Settings { return *c.settings.Load() }

func (c *Control) update(patch []byte) (Settings, error) {
	next := c.get()
	dec := json.NewDecoder(strings.NewReader(string(patch)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next); err != nil {
		return c.get(), err
	}
	if err := next.validate(); err != nil {
		return c.get(), err
	}
	c.settings.Store(&next)
	if b, err := json.MarshalIndent(next, "", "  "); err == nil {
		tmp := c.path + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			os.Rename(tmp, c.path)
		}
	}
	for _, fn := range c.onChange {
		fn(next)
	}
	return next, nil
}

var (
	errPaused = errors.New("paused")
	sizeRe    = regexp.MustCompile(`^\d+(\.\d+)?[KMGT]?$`)
)

// admit charges one request against the global and per-client buckets.
func (c *Control) admit(r *http.Request) (time.Duration, error) {
	s := c.get()
	if s.Paused {
		return time.Minute, errPaused
	}
	now := time.Now()
	if wait := c.clients.take(clientKey(r, c.cfg.clientIPHeader), s.ClientPerMin, s.ClientBurst, now); wait > 0 {
		return wait, errRateLimited
	}
	if wait := c.global.take(s.GlobalPerMin, s.GlobalBurst, now); wait > 0 {
		return wait, errRateLimited
	}
	return 0, nil
}

var errRateLimited = errors.New("rate limited")

func isYouTube(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	return h == "youtu.be" || h == "youtube.com" || strings.HasSuffix(h, ".youtube.com") || h == "youtube-nocookie.com"
}

// Token bucket; perMin 0 disables it.
type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	init   bool
}

func (b *bucket) take(perMin float64, burst int, now time.Time) time.Duration {
	if perMin <= 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rate := perMin / 60
	if !b.init {
		b.tokens, b.last, b.init = float64(burst), now, true
	}
	b.tokens = math.Min(float64(burst), b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return 0
	}
	return time.Duration((1 - b.tokens) / rate * float64(time.Second))
}

// Per-client buckets keyed by an HMAC of the ip with a random in-memory salt,
// rotated daily. Nothing here is persisted or logged.
type clientLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	seen    map[string]time.Time
	salt    []byte
	rotated time.Time
}

func newClientLimiter() *clientLimiter {
	l := &clientLimiter{buckets: map[string]*bucket{}, seen: map[string]time.Time{}}
	l.rotate(time.Now())
	return l
}

func (l *clientLimiter) rotate(now time.Time) {
	l.salt = make([]byte, 32)
	rand.Read(l.salt)
	l.rotated = now
	l.buckets = map[string]*bucket{}
	l.seen = map[string]time.Time{}
}

func (l *clientLimiter) take(ip string, perMin float64, burst int, now time.Time) time.Duration {
	if perMin <= 0 || ip == "" {
		return 0
	}
	l.mu.Lock()
	if now.Sub(l.rotated) > 24*time.Hour {
		l.rotate(now)
	}
	mac := hmac.New(sha256.New, l.salt)
	mac.Write([]byte(ip))
	key := string(mac.Sum(nil))
	b := l.buckets[key]
	if b == nil {
		b = &bucket{}
		l.buckets[key] = b
	}
	l.seen[key] = now
	if len(l.seen) > 4096 {
		for k, t := range l.seen {
			if now.Sub(t) > 10*time.Minute {
				delete(l.seen, k)
				delete(l.buckets, k)
			}
		}
	}
	l.mu.Unlock()
	return b.take(perMin, burst, now)
}

func (l *clientLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

func clientKey(r *http.Request, header string) string {
	if header != "" {
		if v := strings.TrimSpace(strings.Split(r.Header.Get(header), ",")[0]); v != "" {
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Aggregate counters only: totals, hourly buckets for 48h, per-extractor counts.
type Stats struct {
	mu         sync.Mutex
	path       string
	Totals     map[string]int64 `json:"totals"`
	Extractors map[string]int64 `json:"extractors"`
	Hours      []hourBucket     `json:"hours"`
	dirty      bool
}

type hourBucket struct {
	Hour   int64            `json:"hour"`
	Counts map[string]int64 `json:"counts"`
}

func newStats(path string) *Stats {
	s := &Stats{path: path, Totals: map[string]int64{}, Extractors: map[string]int64{}}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, s)
		if s.Totals == nil {
			s.Totals = map[string]int64{}
		}
		if s.Extractors == nil {
			s.Extractors = map[string]int64{}
		}
	}
	return s
}

func (s *Stats) add(key string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Totals[key] += n
	h := time.Now().Unix() / 3600
	if len(s.Hours) == 0 || s.Hours[len(s.Hours)-1].Hour != h {
		s.Hours = append(s.Hours, hourBucket{Hour: h, Counts: map[string]int64{}})
		if len(s.Hours) > 48 {
			s.Hours = s.Hours[len(s.Hours)-48:]
		}
	}
	s.Hours[len(s.Hours)-1].Counts[key] += n
	s.dirty = true
}

func (s *Stats) extractor(name string) {
	if name == "" {
		return
	}
	s.mu.Lock()
	s.Extractors[name]++
	s.dirty = true
	s.mu.Unlock()
}

func (s *Stats) window(hours int64) map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int64{}
	from := time.Now().Unix()/3600 - hours + 1
	for _, b := range s.Hours {
		if b.Hour >= from {
			for k, v := range b.Counts {
				out[k] += v
			}
		}
	}
	return out
}

func (s *Stats) topExtractors(n int) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	type kv struct {
		k string
		v int64
	}
	var list []kv
	for k, v := range s.Extractors {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].v > list[j].v })
	var out []map[string]any
	for i, e := range list {
		if i == n {
			break
		}
		out = append(out, map[string]any{"name": e.k, "count": e.v})
	}
	return out
}

func (s *Stats) flushLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flush()
			return
		case <-t.C:
			s.flush()
		}
	}
}

func (s *Stats) flush() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	b, _ := json.Marshal(s)
	s.dirty = false
	s.mu.Unlock()
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, s.path)
	}
}

type cacheItem struct {
	body    []byte
	expires time.Time
}

type infoCache struct {
	mu    sync.Mutex
	items map[string]cacheItem
}

func (c *infoCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[key]
	if !ok || time.Now().After(it.expires) {
		delete(c.items, key)
		return nil, false
	}
	return it.body, true
}

func (c *infoCache) put(key string, body []byte, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) >= 512 {
		now := time.Now()
		for k, it := range c.items {
			if now.After(it.expires) || len(c.items) >= 512 {
				delete(c.items, k)
			}
		}
	}
	c.items[key] = cacheItem{body, time.Now().Add(ttl)}
}

func (c *infoCache) clear() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.items)
	c.items = map[string]cacheItem{}
	return n
}
