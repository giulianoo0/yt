package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *server) adminRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/stats", s.adminStats)
	mux.HandleFunc("GET /admin/settings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.ctl.get())
	})
	mux.HandleFunc("PATCH /admin/settings", s.adminPatchSettings)
	mux.HandleFunc("GET /admin/accounts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"accounts": s.pool.List()})
	})
	mux.HandleFunc("POST /admin/accounts", s.adminAddAccount)
	mux.HandleFunc("PATCH /admin/accounts/{id}", s.adminPatchAccount)
	mux.HandleFunc("DELETE /admin/accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.pool.Remove(r.PathValue("id")); err != nil {
			adminErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /admin/cooldown/reset", func(w http.ResponseWriter, r *http.Request) {
		s.pool.ResetAll()
		writeJSON(w, http.StatusOK, map[string]any{"accounts": s.pool.List()})
	})
	mux.HandleFunc("GET /admin/jobs", s.adminJobs)
	mux.HandleFunc("POST /admin/jobs/cancel", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]int{"canceled": s.m.cancelAll()})
	})
	mux.HandleFunc("POST /admin/cache/clear", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]int{"cleared": s.ctl.cache.clear()})
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.cfg.adminToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.adminToken)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid admin token")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func adminErr(w http.ResponseWriter, err error) {
	if errors.Is(err, os.ErrNotExist) {
		writeErr(w, http.StatusNotFound, "not_found", "account not found")
		return
	}
	writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
}

func (s *server) adminPatchSettings(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	next, err := s.ctl.update(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, next)
}

func (s *server) adminAddAccount(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "cookies file is too large")
		return
	}
	v, err := s.pool.Add(r.URL.Query().Get("name"), data)
	if err != nil {
		adminErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *server) adminPatchAccount(w http.ResponseWriter, r *http.Request) {
	var patch struct {
		Name     *string `json:"name"`
		Disabled *bool   `json:"disabled"`
		Reset    bool    `json:"reset_cooldown"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := s.pool.Update(r.PathValue("id"), patch); err != nil {
		adminErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": s.pool.List()})
}

// Jobs without urls or titles: only what is needed to see load.
func (s *server) adminJobs(w http.ResponseWriter, r *http.Request) {
	type row struct {
		ID        string  `json:"id"`
		Status    string  `json:"status"`
		Stage     string  `json:"stage,omitempty"`
		Extractor string  `json:"extractor,omitempty"`
		Percent   float64 `json:"percent"`
		AgeSec    int     `json:"age_seconds"`
	}
	s.m.mu.RLock()
	out := []row{}
	for _, j := range s.m.jobs {
		v, _ := j.snapshot()
		if v.terminal() {
			continue
		}
		rw := row{ID: v.ID, Status: v.Status, Stage: v.Stage, Percent: v.Progress.Percent, AgeSec: int(time.Since(v.CreatedAt).Seconds())}
		if v.Media != nil {
			rw.Extractor = v.Media.Extractor
		}
		out = append(out, rw)
	}
	s.m.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

func (s *server) adminStats(w http.ResponseWriter, r *http.Request) {
	running, queued := s.m.active()
	accounts := s.pool.List()
	available, cooling := 0, 0
	for _, a := range accounts {
		switch {
		case a.Disabled:
		case a.CooldownSeconds > 0:
			cooling++
		case a.LimitPerHour > 0 && a.UsesLastHour < a.LimitPerHour:
			available++
		}
	}
	var disk int64
	filepath.WalkDir(filepath.Join(s.cfg.dataDir, "jobs"), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				disk += info.Size()
			}
		}
		return nil
	})
	st := s.ctl.stats
	st.mu.Lock()
	totals := make(map[string]int64, len(st.Totals))
	for k, v := range st.Totals {
		totals[k] = v
	}
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"uptime_seconds":  int(time.Since(s.ctl.started).Seconds()),
		"yt_dlp":          ytdlpVersion(),
		"paused":          s.ctl.get().Paused,
		"jobs":            map[string]int{"running": running, "queued": queued},
		"last_hour":       st.window(1),
		"last_24h":        st.window(24),
		"totals":          totals,
		"extractors":      st.topExtractors(10),
		"accounts":        map[string]int{"total": len(accounts), "available": available, "cooling": cooling},
		"clients_tracked": s.ctl.clients.size(),
		"disk_bytes":      disk,
	})
}
