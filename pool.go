package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const guestID = "guest"

// Account is one YouTube identity: a cookies.txt, or the cookie-less guest.
type Account struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Disabled      bool      `json:"disabled"`
	AddedAt       time.Time `json:"added_at"`
	CooldownUntil time.Time `json:"cooldown_until"`
	Total         int64     `json:"total"`
	Blocks        int64     `json:"blocks"`
	LastBlock     time.Time `json:"last_block"`
	uses          []time.Time
}

type AccountView struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Guest           bool      `json:"guest"`
	Disabled        bool      `json:"disabled"`
	UsesLastHour    int       `json:"uses_last_hour"`
	LimitPerHour    int       `json:"limit_per_hour"`
	Total           int64     `json:"total"`
	Blocks          int64     `json:"blocks"`
	CooldownSeconds int       `json:"cooldown_seconds"`
	LastBlock       time.Time `json:"last_block,omitzero"`
	AddedAt         time.Time `json:"added_at,omitzero"`
}

type Pool struct {
	mu       sync.Mutex
	dir      string
	accounts map[string]*Account
	control  *Control
}

var errPoolExhausted = errors.New("all youtube accounts are cooling down or at their hourly limit")

func newPool(dataDir string, c *Control) *Pool {
	p := &Pool{dir: filepath.Join(dataDir, "accounts"), accounts: map[string]*Account{}, control: c}
	os.MkdirAll(p.dir, 0o700)
	var saved []*Account
	if b, err := os.ReadFile(filepath.Join(p.dir, "accounts.json")); err == nil {
		json.Unmarshal(b, &saved)
	}
	for _, a := range saved {
		if a.ID == guestID {
			p.accounts[guestID] = a
			continue
		}
		if _, err := os.Stat(p.cookieFile(a.ID)); err == nil {
			p.accounts[a.ID] = a
		}
	}
	if p.accounts[guestID] == nil {
		p.accounts[guestID] = &Account{ID: guestID, Name: "guest"}
	}
	return p
}

func (p *Pool) cookieFile(id string) string { return filepath.Join(p.dir, id+".txt") }

func (p *Pool) saveLocked() {
	list := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		list = append(list, a)
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	tmp := filepath.Join(p.dir, "accounts.json.tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, filepath.Join(p.dir, "accounts.json"))
	}
}

func (a *Account) usesLastHour(now time.Time) int {
	cut := now.Add(-time.Hour)
	i := sort.Search(len(a.uses), func(i int) bool { return a.uses[i].After(cut) })
	a.uses = a.uses[i:]
	return len(a.uses)
}

func (p *Pool) limit(a *Account, s Settings) int {
	if a.ID == guestID {
		return s.GuestPerHour
	}
	return s.AccountPerHour
}

// Acquire picks the least used available account and charges one use.
// It returns the account id and its cookies path ("" for guest).
func (p *Pool) Acquire() (id, cookies string, wait time.Duration, err error) {
	return p.pick(true)
}

// Wait reports how long until any account is available, without charging.
func (p *Pool) Wait() time.Duration {
	_, _, wait, _ := p.pick(false)
	return wait
}

func (p *Pool) pick(charge bool) (id, cookies string, wait time.Duration, err error) {
	s := p.control.get()
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()

	var best *Account
	bestLoad := 2.0
	soonest := time.Duration(0)
	consider := func(a *Account) {
		lim := p.limit(a, s)
		if a.Disabled || lim <= 0 {
			return
		}
		if a.CooldownUntil.After(now) {
			if d := a.CooldownUntil.Sub(now); soonest == 0 || d < soonest {
				soonest = d
			}
			return
		}
		used := a.usesLastHour(now)
		if used >= lim {
			if d := a.uses[0].Add(time.Hour).Sub(now); soonest == 0 || d < soonest {
				soonest = d
			}
			return
		}
		if load := float64(used) / float64(lim); load < bestLoad {
			best, bestLoad = a, load
		}
	}
	for _, a := range p.accounts {
		if a.ID != guestID {
			consider(a)
		}
	}
	if best == nil && s.UseGuest {
		consider(p.accounts[guestID])
	}
	if best == nil {
		if soonest == 0 {
			soonest = 15 * time.Minute
		}
		return "", "", soonest, errPoolExhausted
	}
	if charge {
		best.uses = append(best.uses, now)
		best.Total++
		p.saveLocked()
	}
	if best.ID == guestID {
		return guestID, "", 0, nil
	}
	return best.ID, p.cookieFile(best.ID), 0, nil
}

var blockMarkers = []string{"try again later", "confirm you're not a bot", "confirm you’re not a bot", "rate-limited", "http error 429", "too many requests"}

// NoteFailure cools the account down when yt-dlp reports a block on it.
func (p *Pool) NoteFailure(id, msg string) bool {
	lower := strings.ToLower(msg)
	hit := false
	for _, m := range blockMarkers {
		if strings.Contains(lower, m) {
			hit = true
			break
		}
	}
	if !hit || id == "" {
		return false
	}
	cd := time.Duration(p.control.get().YouTubeCooldown) * time.Second
	p.mu.Lock()
	if a := p.accounts[id]; a != nil {
		a.Blocks++
		a.LastBlock = time.Now()
		if cd > 0 {
			a.CooldownUntil = time.Now().Add(cd)
		}
		p.saveLocked()
	}
	p.mu.Unlock()
	p.control.stats.add("youtube_blocks", 1)
	return true
}

func (p *Pool) List() []AccountView {
	s := p.control.get()
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]AccountView, 0, len(p.accounts))
	for _, a := range p.accounts {
		v := AccountView{
			ID: a.ID, Name: a.Name, Guest: a.ID == guestID, Disabled: a.Disabled,
			UsesLastHour: a.usesLastHour(now), LimitPerHour: p.limit(a, s),
			Total: a.Total, Blocks: a.Blocks, LastBlock: a.LastBlock, AddedAt: a.AddedAt,
		}
		if a.ID == guestID {
			v.Disabled = a.Disabled || !s.UseGuest
		}
		if a.CooldownUntil.After(now) {
			v.CooldownSeconds = int(a.CooldownUntil.Sub(now).Seconds())
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Guest != out[j].Guest {
			return !out[i].Guest
		}
		return out[i].AddedAt.Before(out[j].AddedAt)
	})
	return out
}

// Add validates a Netscape cookies.txt with YouTube auth cookies and stores it.
func (p *Pool) Add(name string, data []byte) (AccountView, error) {
	if len(data) > 256<<10 {
		return AccountView{}, errors.New("cookies file is too large")
	}
	if err := validateCookies(data); err != nil {
		return AccountView{}, err
	}
	b := make([]byte, 4)
	rand.Read(b)
	id := hex.EncodeToString(b)
	name = strings.TrimSpace(name)
	if name == "" {
		name = "account-" + id
	}
	if len(name) > 40 {
		name = name[:40]
	}
	if err := os.WriteFile(p.cookieFile(id), data, 0o600); err != nil {
		return AccountView{}, err
	}
	p.mu.Lock()
	p.accounts[id] = &Account{ID: id, Name: name, AddedAt: time.Now()}
	p.saveLocked()
	p.mu.Unlock()
	for _, v := range p.List() {
		if v.ID == id {
			return v, nil
		}
	}
	return AccountView{}, errors.New("account vanished")
}

func validateCookies(data []byte) error {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	lines, youtube, auth := 0, 0, false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimPrefix(line, "#HttpOnly_")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 7 {
			return errors.New("not a netscape cookies.txt (expected 7 tab separated fields)")
		}
		lines++
		if strings.HasSuffix(f[0], "youtube.com") {
			youtube++
			switch f[5] {
			case "LOGIN_INFO", "SAPISID", "__Secure-3PAPISID", "__Secure-1PSID", "SID":
				auth = true
			}
		}
	}
	switch {
	case lines == 0:
		return errors.New("cookies file is empty")
	case youtube == 0:
		return errors.New("no youtube.com cookies found")
	case !auth:
		return errors.New("youtube cookies are not from a logged in session")
	}
	return nil
}

func (p *Pool) Update(id string, patch struct {
	Name     *string `json:"name"`
	Disabled *bool   `json:"disabled"`
	Reset    bool    `json:"reset_cooldown"`
}) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.accounts[id]
	if a == nil {
		return os.ErrNotExist
	}
	if patch.Name != nil && id != guestID {
		n := strings.TrimSpace(*patch.Name)
		if n == "" || len(n) > 40 {
			return errors.New("name must be 1-40 chars")
		}
		a.Name = n
	}
	if patch.Disabled != nil {
		a.Disabled = *patch.Disabled
	}
	if patch.Reset {
		a.CooldownUntil = time.Time{}
		a.uses = nil
	}
	p.saveLocked()
	return nil
}

func (p *Pool) Remove(id string) error {
	if id == guestID {
		return errors.New("guest can be disabled but not removed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.accounts[id] == nil {
		return os.ErrNotExist
	}
	delete(p.accounts, id)
	os.Remove(p.cookieFile(id))
	p.saveLocked()
	return nil
}

func (p *Pool) ResetAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		a.CooldownUntil = time.Time{}
	}
	p.saveLocked()
}
