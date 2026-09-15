package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const discordAPI = "https://discord.com/api/v10"

type bot struct {
	appID  string
	token  string
	key    ed25519.PublicKey
	owners []string
	api    string
	admin  string
	client *http.Client
}

func main() {
	key, err := hex.DecodeString(os.Getenv("DISCORD_PUBLIC_KEY"))
	if err != nil || len(key) != ed25519.PublicKeySize {
		log.Fatal("DISCORD_PUBLIC_KEY must be the hex public key")
	}
	b := &bot{
		appID:  os.Getenv("DISCORD_APP_ID"),
		token:  os.Getenv("DISCORD_BOT_TOKEN"),
		key:    key,
		api:    strings.TrimRight(envOr("YT_API", "http://yt:8080"), "/"),
		admin:  os.Getenv("ADMIN_TOKEN"),
		client: &http.Client{Timeout: 20 * time.Second},
	}
	for _, id := range strings.Split(os.Getenv("DISCORD_OWNER_IDS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			b.owners = append(b.owners, id)
		}
	}
	if b.appID == "" || b.admin == "" {
		log.Fatal("DISCORD_APP_ID and ADMIN_TOKEN are required")
	}
	if b.token != "" {
		if err := b.register(); err != nil {
			log.Printf("register commands: %v", err)
		}
		if len(b.owners) == 0 {
			if err := b.loadOwners(); err != nil {
				log.Printf("load owners: %v", err)
			}
		}
	}
	log.Printf("owners: %d", len(b.owners))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /discord/interactions", b.interactions)
	mux.HandleFunc("GET /discord/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	srv := &http.Server{Addr: envOr("ADDR", ":8081"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Printf("listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// discord types

type interaction struct {
	Type   int    `json:"type"`
	Token  string `json:"token"`
	Member *struct {
		User user `json:"user"`
	} `json:"member"`
	User *user `json:"user"`
	Data struct {
		Name     string   `json:"name"`
		Options  []option `json:"options"`
		Resolved struct {
			Attachments map[string]struct {
				URL      string `json:"url"`
				Filename string `json:"filename"`
				Size     int    `json:"size"`
			} `json:"attachments"`
		} `json:"resolved"`
	} `json:"data"`
}

type user struct {
	ID string `json:"id"`
}

type option struct {
	Name    string          `json:"name"`
	Type    int             `json:"type"`
	Value   json.RawMessage `json:"value"`
	Focused bool            `json:"focused"`
	Options []option        `json:"options"`
}

func (o option) str() string {
	var s string
	if json.Unmarshal(o.Value, &s) == nil {
		return s
	}
	return strings.Trim(string(o.Value), `"`)
}

type embed struct {
	Title       string  `json:"title,omitempty"`
	Description string  `json:"description,omitempty"`
	Color       int     `json:"color,omitempty"`
	Fields      []field `json:"fields,omitempty"`
	Footer      *footer `json:"footer,omitempty"`
}

type field struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type footer struct {
	Text string `json:"text"`
}

const (
	colorOK   = 0x7fb4ca
	colorWarn = 0xe6c384
	colorErr  = 0xc34043
)

func (b *bot) interactions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sig, err := hex.DecodeString(r.Header.Get("X-Signature-Ed25519"))
	ts := r.Header.Get("X-Signature-Timestamp")
	if err != nil || ts == "" || !ed25519.Verify(b.key, append([]byte(ts), body...), sig) {
		http.Error(w, "invalid request signature", http.StatusUnauthorized)
		return
	}
	var in interaction
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if in.Type == 1 {
		reply(w, map[string]any{"type": 1})
		return
	}
	uid := ""
	if in.Member != nil {
		uid = in.Member.User.ID
	} else if in.User != nil {
		uid = in.User.ID
	}
	if !slices.Contains(b.owners, uid) {
		if in.Type == 4 {
			reply(w, map[string]any{"type": 8, "data": map[string]any{"choices": []any{}}})
			return
		}
		respond(w, embed{Title: "Not allowed", Description: "Only the owner can control this instance.", Color: colorErr})
		return
	}
	if len(in.Data.Options) == 0 {
		respond(w, embed{Title: "Unknown command", Color: colorErr})
		return
	}
	sub := in.Data.Options[0]
	opts := map[string]option{}
	for _, o := range sub.Options {
		opts[o.Name] = o
	}

	if in.Type == 4 {
		b.autocomplete(w, sub)
		return
	}

	switch sub.Name {
	case "stats":
		b.stats(w)
	case "settings":
		b.showSettings(w)
	case "set":
		b.set(w, opts["key"].str(), opts["value"].str())
	case "pause":
		b.patchSettings(w, `{"paused":true}`, "Paused", "New jobs and info requests are rejected with 503.")
	case "resume":
		b.patchSettings(w, `{"paused":false}`, "Resumed", "Accepting requests again.")
	case "accounts":
		b.accounts(w)
	case "account-add":
		att, ok := in.Data.Resolved.Attachments[opts["file"].str()]
		if !ok {
			respond(w, embed{Title: "Attach a cookies.txt", Color: colorErr})
			return
		}
		reply(w, map[string]any{"type": 5, "data": map[string]any{"flags": 64}})
		go b.addAccount(in.Token, att.URL, att.Size, opts["name"].str())
	case "account-enable", "account-disable":
		disabled := sub.Name == "account-disable"
		b.accountCall(w, http.MethodPatch, opts["id"].str(), fmt.Sprintf(`{"disabled":%t}`, disabled), map[bool]string{true: "Account disabled", false: "Account enabled"}[disabled])
	case "account-reset":
		b.accountCall(w, http.MethodPatch, opts["id"].str(), `{"reset_cooldown":true}`, "Cooldown and hourly usage cleared")
	case "account-remove":
		b.accountCall(w, http.MethodDelete, opts["id"].str(), "", "Account removed")
	case "cooldown-reset":
		if _, err := b.call(http.MethodPost, "/admin/cooldown/reset", "", nil); err != nil {
			respondErr(w, err)
			return
		}
		respond(w, embed{Title: "All cooldowns cleared", Color: colorOK})
	case "jobs":
		b.jobs(w)
	case "cancel-all":
		var out struct{ Canceled int }
		if _, err := b.call(http.MethodPost, "/admin/jobs/cancel", "", &out); err != nil {
			respondErr(w, err)
			return
		}
		respond(w, embed{Title: fmt.Sprintf("Canceled %d jobs", out.Canceled), Color: colorWarn})
	case "cache-clear":
		var out struct{ Cleared int }
		if _, err := b.call(http.MethodPost, "/admin/cache/clear", "", &out); err != nil {
			respondErr(w, err)
			return
		}
		respond(w, embed{Title: fmt.Sprintf("Cleared %d cached info entries", out.Cleared), Color: colorOK})
	default:
		respond(w, embed{Title: "Unknown command", Color: colorErr})
	}
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func respond(w http.ResponseWriter, e embed) {
	reply(w, map[string]any{"type": 4, "data": map[string]any{"flags": 64, "embeds": []embed{e}}})
}

func respondErr(w http.ResponseWriter, err error) {
	respond(w, embed{Title: "Request failed", Description: err.Error(), Color: colorErr})
}

// yt admin api

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (b *bot) call(method, path, body string, out any) (int, error) {
	return b.callRaw(method, path, "application/json", strings.NewReader(body), out)
}

func (b *bot) callRaw(method, path, ctype string, body io.Reader, out any) (int, error) {
	req, err := http.NewRequest(method, b.api+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+b.admin)
	req.Header.Set("Content-Type", ctype)
	res, err := b.client.Do(req)
	if err != nil {
		return 0, errors.New("api unreachable")
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 400 {
		var e apiError
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			return res.StatusCode, errors.New(e.Error.Message)
		}
		return res.StatusCode, fmt.Errorf("api answered %d", res.StatusCode)
	}
	if out != nil && len(data) > 0 {
		return res.StatusCode, json.Unmarshal(data, out)
	}
	return res.StatusCode, nil
}

type stats struct {
	Uptime     int              `json:"uptime_seconds"`
	YtDlp      string           `json:"yt_dlp"`
	Paused     bool             `json:"paused"`
	Jobs       map[string]int   `json:"jobs"`
	LastHour   map[string]int64 `json:"last_hour"`
	Last24h    map[string]int64 `json:"last_24h"`
	Totals     map[string]int64 `json:"totals"`
	Extractors []struct {
		Name  string `json:"name"`
		Count int64  `json:"count"`
	} `json:"extractors"`
	Accounts map[string]int `json:"accounts"`
	Clients  int            `json:"clients_tracked"`
	Disk     int64          `json:"disk_bytes"`
}

func counts(m map[string]int64) string {
	return fmt.Sprintf("created **%d** · done **%d** · failed **%d**\nrate limited **%d** · yt blocks **%d**\ninfo **%d** (%d cached) · %s",
		m["jobs_created"], m["jobs_done"], m["jobs_failed"], m["rate_limited"], m["youtube_blocks"],
		m["info_requests"], m["info_cache_hits"], bytesStr(m["bytes_produced"]))
}

func (b *bot) stats(w http.ResponseWriter) {
	var s stats
	if _, err := b.call(http.MethodGet, "/admin/stats", "", &s); err != nil {
		respondErr(w, err)
		return
	}
	var top []string
	for _, e := range s.Extractors {
		top = append(top, fmt.Sprintf("%s %d", e.Name, e.Count))
	}
	if len(top) == 0 {
		top = []string{"none yet"}
	}
	color, state := colorOK, "running"
	if s.Paused {
		color, state = colorWarn, "paused"
	}
	respond(w, embed{
		Title: "yt · " + state,
		Color: color,
		Fields: []field{
			{Name: "Now", Value: fmt.Sprintf("running **%d** · queued **%d**\nclients in window **%d**", s.Jobs["running"], s.Jobs["queued"], s.Clients), Inline: true},
			{Name: "Accounts", Value: fmt.Sprintf("available **%d** · cooling **%d**\ntotal **%d**", s.Accounts["available"], s.Accounts["cooling"], s.Accounts["total"]), Inline: true},
			{Name: "Last hour", Value: counts(s.LastHour)},
			{Name: "Last 24h", Value: counts(s.Last24h)},
			{Name: "All time", Value: counts(s.Totals)},
			{Name: "Top extractors", Value: strings.Join(top, " · ")},
		},
		Footer: &footer{Text: fmt.Sprintf("yt-dlp %s · up %s · disk %s", s.YtDlp, durStr(s.Uptime), bytesStr(s.Disk))},
	})
}

var settingKeys = []struct {
	key, kind, help string
}{
	{"paused", "bool", "reject new work"},
	{"max_jobs", "int", "concurrent downloads"},
	{"max_queue", "int", "queued jobs before 429"},
	{"global_per_min", "float", "requests/min for everyone, 0 off"},
	{"global_burst", "int", "global burst"},
	{"client_per_min", "float", "requests/min per client, 0 off"},
	{"client_burst", "int", "per client burst"},
	{"account_per_hour", "int", "youtube uses/hour per account"},
	{"guest_per_hour", "int", "youtube uses/hour without cookies"},
	{"use_guest", "bool", "fall back to no cookies"},
	{"youtube_cooldown", "int", "seconds an account rests after a block"},
	{"sleep_requests", "float", "seconds between yt-dlp requests"},
	{"info_cache_ttl", "int", "seconds info responses stay cached"},
	{"max_filesize", "string", "like 2G, empty for none"},
	{"allow_args", "bool", "accept raw yt-dlp args"},
}

func (b *bot) showSettings(w http.ResponseWriter) {
	var s map[string]any
	if _, err := b.call(http.MethodGet, "/admin/settings", "", &s); err != nil {
		respondErr(w, err)
		return
	}
	var lines []string
	for _, k := range settingKeys {
		v := fmt.Sprint(s[k.key])
		if v == "" {
			v = "none"
		}
		lines = append(lines, fmt.Sprintf("`%s` **%s** · %s", k.key, v, k.help))
	}
	respond(w, embed{Title: "Settings", Description: strings.Join(lines, "\n"), Color: colorOK, Footer: &footer{Text: "change with /yt set"}})
}

func (b *bot) set(w http.ResponseWriter, key, value string) {
	value = strings.TrimSpace(value)
	var v any
	for _, k := range settingKeys {
		if k.key != key {
			continue
		}
		switch k.kind {
		case "bool":
			switch strings.ToLower(value) {
			case "true", "on", "yes", "1":
				v = true
			case "false", "off", "no", "0":
				v = false
			default:
				respond(w, embed{Title: "Expected true or false", Color: colorErr})
				return
			}
		case "int":
			n, err := strconv.Atoi(value)
			if err != nil {
				respond(w, embed{Title: "Expected a whole number", Color: colorErr})
				return
			}
			v = n
		case "float":
			f, err := strconv.ParseFloat(value, 64)
			if err != nil {
				respond(w, embed{Title: "Expected a number", Color: colorErr})
				return
			}
			v = f
		default:
			v = value
		}
	}
	if v == nil {
		respond(w, embed{Title: "Unknown setting", Color: colorErr})
		return
	}
	patch, _ := json.Marshal(map[string]any{key: v})
	b.patchSettings(w, string(patch), "Updated", fmt.Sprintf("`%s` is now **%v**", key, v))
}

func (b *bot) patchSettings(w http.ResponseWriter, patch, title, desc string) {
	if _, err := b.call(http.MethodPatch, "/admin/settings", patch, nil); err != nil {
		respondErr(w, err)
		return
	}
	respond(w, embed{Title: title, Description: desc, Color: colorOK})
}

type account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Guest    bool   `json:"guest"`
	Disabled bool   `json:"disabled"`
	Uses     int    `json:"uses_last_hour"`
	Limit    int    `json:"limit_per_hour"`
	Total    int64  `json:"total"`
	Blocks   int64  `json:"blocks"`
	Cooldown int    `json:"cooldown_seconds"`
}

func (b *bot) listAccounts() ([]account, error) {
	var out struct{ Accounts []account }
	_, err := b.call(http.MethodGet, "/admin/accounts", "", &out)
	return out.Accounts, err
}

func accountLine(a account) string {
	state := "🟢"
	switch {
	case a.Disabled:
		state = "⚫"
	case a.Cooldown > 0:
		state = "🔴"
	case a.Limit > 0 && a.Uses >= a.Limit:
		state = "🟡"
	}
	extra := ""
	if a.Cooldown > 0 {
		extra = " · cooling " + durStr(a.Cooldown)
	}
	id := "`" + a.ID + "`"
	return fmt.Sprintf("%s **%s** %s\n%d/%d this hour · %d total · %d blocks%s", state, a.Name, id, a.Uses, a.Limit, a.Total, a.Blocks, extra)
}

func (b *bot) accounts(w http.ResponseWriter) {
	list, err := b.listAccounts()
	if err != nil {
		respondErr(w, err)
		return
	}
	var lines []string
	for _, a := range list {
		lines = append(lines, accountLine(a))
	}
	respond(w, embed{Title: "YouTube accounts", Description: strings.Join(lines, "\n\n"), Color: colorOK, Footer: &footer{Text: "🟢 ready · 🟡 hourly limit · 🔴 cooling · ⚫ disabled"}})
}

func (b *bot) accountCall(w http.ResponseWriter, method, id, body, title string) {
	if id == "" {
		respond(w, embed{Title: "Pick an account", Color: colorErr})
		return
	}
	if _, err := b.call(method, "/admin/accounts/"+id, body, nil); err != nil {
		respondErr(w, err)
		return
	}
	respond(w, embed{Title: title, Description: "`" + id + "`", Color: colorOK})
}

func (b *bot) addAccount(token, url string, size int, name string) {
	e := b.doAddAccount(url, size, name)
	body, _ := json.Marshal(map[string]any{"embeds": []embed{e}})
	req, _ := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/webhooks/%s/%s/messages/@original", discordAPI, b.appID, token), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if res, err := b.client.Do(req); err == nil {
		res.Body.Close()
	}
}

func (b *bot) doAddAccount(url string, size int, name string) embed {
	if size > 256<<10 {
		return embed{Title: "File too large", Description: "cookies.txt should be a few KB.", Color: colorErr}
	}
	if !strings.HasPrefix(url, "https://cdn.discordapp.com/") && !strings.HasPrefix(url, "https://media.discordapp.net/") {
		return embed{Title: "Unexpected attachment host", Color: colorErr}
	}
	res, err := b.client.Get(url)
	if err != nil {
		return embed{Title: "Could not read the attachment", Color: colorErr}
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 256<<10))
	var a account
	path := "/admin/accounts?name=" + strings.ReplaceAll(strings.TrimSpace(name), " ", "%20")
	if _, err := b.callRaw(http.MethodPost, path, "text/plain", bytes.NewReader(data), &a); err != nil {
		return embed{Title: "Account rejected", Description: err.Error(), Color: colorErr}
	}
	return embed{Title: "Account added", Description: accountLine(a) + "\n\nDelete the message with the file if you pasted it anywhere else.", Color: colorOK}
}

func (b *bot) jobs(w http.ResponseWriter) {
	var out struct {
		Jobs []struct {
			ID        string  `json:"id"`
			Status    string  `json:"status"`
			Stage     string  `json:"stage"`
			Extractor string  `json:"extractor"`
			Percent   float64 `json:"percent"`
			Age       int     `json:"age_seconds"`
		} `json:"jobs"`
	}
	if _, err := b.call(http.MethodGet, "/admin/jobs", "", &out); err != nil {
		respondErr(w, err)
		return
	}
	if len(out.Jobs) == 0 {
		respond(w, embed{Title: "No active jobs", Color: colorOK})
		return
	}
	sort.Slice(out.Jobs, func(i, j int) bool { return out.Jobs[i].Age > out.Jobs[j].Age })
	var lines []string
	for i, j := range out.Jobs {
		if i == 20 {
			lines = append(lines, fmt.Sprintf("…and %d more", len(out.Jobs)-20))
			break
		}
		ex := j.Extractor
		if ex == "" {
			ex = "?"
		}
		lines = append(lines, fmt.Sprintf("`%s` %s %s · %s · %.0f%% · %s", j.ID[:8], j.Status, j.Stage, ex, j.Percent, durStr(j.Age)))
	}
	respond(w, embed{Title: fmt.Sprintf("%d active jobs", len(out.Jobs)), Description: strings.Join(lines, "\n"), Color: colorOK})
}

func (b *bot) autocomplete(w http.ResponseWriter, sub option) {
	query := ""
	for _, o := range sub.Options {
		if o.Focused {
			query = strings.ToLower(o.str())
		}
	}
	var choices []map[string]string
	if list, err := b.listAccounts(); err == nil {
		for _, a := range list {
			if sub.Name == "account-remove" && a.Guest {
				continue
			}
			label := fmt.Sprintf("%s (%s) %d/%d", a.Name, a.ID, a.Uses, a.Limit)
			if query == "" || strings.Contains(strings.ToLower(label), query) {
				choices = append(choices, map[string]string{"name": label, "value": a.ID})
			}
			if len(choices) == 25 {
				break
			}
		}
	}
	reply(w, map[string]any{"type": 8, "data": map[string]any{"choices": choices}})
}

// command registration

func (b *bot) discord(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, discordAPI+path, rd)
	req.Header.Set("Authorization", "Bot "+b.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("discord %d: %s", res.StatusCode, data)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (b *bot) loadOwners() error {
	var app struct {
		Owner struct {
			ID string `json:"id"`
		} `json:"owner"`
		Team *struct {
			Members []struct {
				User user `json:"user"`
			} `json:"members"`
		} `json:"team"`
	}
	if err := b.discord(http.MethodGet, "/applications/@me", nil, &app); err != nil {
		return err
	}
	if app.Team != nil {
		for _, m := range app.Team.Members {
			b.owners = append(b.owners, m.User.ID)
		}
	} else if app.Owner.ID != "" {
		b.owners = append(b.owners, app.Owner.ID)
	}
	return nil
}

func (b *bot) register() error {
	sub := func(name, desc string, opts ...map[string]any) map[string]any {
		o := map[string]any{"type": 1, "name": name, "description": desc}
		if len(opts) > 0 {
			o["options"] = opts
		}
		return o
	}
	accountID := map[string]any{"type": 3, "name": "id", "description": "Account", "required": true, "autocomplete": true}
	var keyChoices []map[string]string
	for _, k := range settingKeys {
		keyChoices = append(keyChoices, map[string]string{"name": k.key, "value": k.key})
	}
	cmd := []map[string]any{{
		"name":                       "yt",
		"description":                "Control the yt api",
		"type":                       1,
		"default_member_permissions": "0",
		"contexts":                   []int{0, 1, 2},
		"integration_types":          []int{0, 1},
		"options": []map[string]any{
			sub("stats", "Usage, limits and account health"),
			sub("settings", "Show every setting"),
			sub("set", "Change a setting",
				map[string]any{"type": 3, "name": "key", "description": "Setting", "required": true, "choices": keyChoices},
				map[string]any{"type": 3, "name": "value", "description": "New value", "required": true}),
			sub("pause", "Reject new work"),
			sub("resume", "Accept new work"),
			sub("accounts", "List YouTube accounts"),
			sub("account-add", "Import a cookies.txt",
				map[string]any{"type": 11, "name": "file", "description": "Netscape cookies.txt", "required": true},
				map[string]any{"type": 3, "name": "name", "description": "Label", "max_length": 40}),
			sub("account-enable", "Put an account back in rotation", accountID),
			sub("account-disable", "Take an account out of rotation", accountID),
			sub("account-reset", "Clear an account cooldown and hourly usage", accountID),
			sub("account-remove", "Delete an account and its cookies", accountID),
			sub("cooldown-reset", "Clear every cooldown"),
			sub("jobs", "Active jobs"),
			sub("cancel-all", "Cancel every active job"),
			sub("cache-clear", "Drop cached info responses"),
		},
	}}
	return b.discord(http.MethodPut, "/applications/"+b.appID+"/commands", cmd, nil)
}

func bytesStr(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func durStr(sec int) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", sec)
}
