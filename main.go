package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type config struct {
	addr           string
	apiKey         string
	dataDir        string
	bin            string
	cookies        string
	proxy          string
	cors           string
	maxFilesize    string
	adminToken     string
	clientIPHeader string
	jobTTL         time.Duration
	timeout        time.Duration
	allowArgs      bool
	allowPrivate   bool
	extraArgs      []string
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return n
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return def
}

func envBool(key string, def bool) bool {
	if b, err := strconv.ParseBool(os.Getenv(key)); err == nil {
		return b
	}
	return def
}

func loadConfig() config {
	return config{
		addr:           env("ADDR", ":8080"),
		apiKey:         os.Getenv("API_KEY"),
		dataDir:        env("DATA_DIR", filepath.Join(os.TempDir(), "yt")),
		bin:            env("YTDLP_BIN", "yt-dlp"),
		cookies:        os.Getenv("COOKIES_FILE"),
		proxy:          os.Getenv("PROXY"),
		cors:           env("CORS_ORIGIN", "*"),
		maxFilesize:    os.Getenv("MAX_FILESIZE"),
		adminToken:     os.Getenv("ADMIN_TOKEN"),
		clientIPHeader: os.Getenv("CLIENT_IP_HEADER"),
		jobTTL:         envDur("JOB_TTL", time.Hour),
		timeout:        envDur("JOB_TIMEOUT", 30*time.Minute),
		allowArgs:      envBool("ALLOW_ARGS", true),
		allowPrivate:   envBool("ALLOW_PRIVATE", false),
		extraArgs:      strings.Fields(os.Getenv("YTDLP_ARGS")),
	}
}

func main() {
	cfg := loadConfig()
	if err := os.MkdirAll(cfg.dataDir, 0o755); err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctl := newControl(cfg)
	pool := newPool(cfg.dataDir, ctl)
	m := newManager(cfg, ctl, pool)
	go m.janitor(ctx)
	go ctl.stats.flushLoop(ctx)

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           newServer(cfg, m),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("listening on %s", cfg.addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	m.shutdown()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}
