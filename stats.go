package main

// Player profiles and the leaderboard.
//
// Only trusted game servers report results: a server on the same machine
// (loopback, i.e. the official one) or one that sends the SOLDAT_STATS_KEY
// secret. Players are keyed by a hash of a random id their game generates
// once; the master never sees the id itself.

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Profile struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Kills   int    `json:"k"`
	Deaths  int    `json:"d"`
	Caps    int    `json:"c"`
	Wins    int    `json:"w"`
	Matches int    `json:"m"`
	XP      int    `json:"xp"`
	LastMS  int64  `json:"last_ms"`
}

type reportPlayer struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kills  int    `json:"k"`
	Deaths int    `json:"d"`
	Caps   int    `json:"c"`
	Win    bool   `json:"win"`
	Played bool   `json:"played"` // finished a round (counts as a match)
}

type reportReq struct {
	Players []reportPlayer `json:"players"`
}

var (
	statsMu    sync.Mutex
	profiles   = map[string]*Profile{}
	statsDirty bool
	statsFile  string
	idRe       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func dataDir() string {
	if d := strings.TrimSpace(os.Getenv("SOLDAT_DATA_DIR")); d != "" {
		return d
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

func xpFor(k, c, w, m int) int { return k*10 + c*50 + w*100 + m*20 }

func loadStats() {
	statsFile = filepath.Join(dataDir(), "stats.json")
	b, err := os.ReadFile(statsFile)
	if err != nil {
		return
	}
	var list []*Profile
	if json.Unmarshal(b, &list) == nil {
		for _, p := range list {
			if p != nil && idRe.MatchString(p.ID) {
				profiles[p.ID] = p
			}
		}
	}
	log.Printf("stats: %d profiles from %s", len(profiles), statsFile)
}

func saveStatsLoop() {
	for {
		time.Sleep(20 * time.Second)
		saveStats()
	}
}

func saveStats() {
	statsMu.Lock()
	if !statsDirty {
		statsMu.Unlock()
		return
	}
	list := make([]*Profile, 0, len(profiles))
	for _, p := range profiles {
		list = append(list, p)
	}
	b, _ := json.Marshal(list)
	statsDirty = false
	statsMu.Unlock()
	tmp := statsFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("stats: save failed: %v", err)
		return
	}
	_ = os.Rename(tmp, statsFile)
}

// trustedReporter: loopback (game server on this box) or the shared key.
func trustedReporter(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	key := strings.TrimSpace(os.Getenv("SOLDAT_STATS_KEY"))
	got := r.Header.Get("X-Soldat-Key")
	return key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(got)) == 1
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func handleReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !trustedReporter(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req reportReq
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if len(req.Players) > 64 {
		req.Players = req.Players[:64]
	}
	now := time.Now().UnixMilli()
	statsMu.Lock()
	n := 0
	for _, rp := range req.Players {
		id := strings.ToLower(strings.TrimSpace(rp.ID))
		if !idRe.MatchString(id) {
			continue
		}
		p := profiles[id]
		if p == nil {
			if len(profiles) >= 200000 {
				continue
			}
			p = &Profile{ID: id}
			profiles[id] = p
		}
		if nm := clip(strings.TrimSpace(rp.Name), 24); nm != "" {
			p.Name = nm
		}
		p.Kills += clampInt(rp.Kills, 0, 1000)
		p.Deaths += clampInt(rp.Deaths, 0, 1000)
		p.Caps += clampInt(rp.Caps, 0, 100)
		if rp.Played {
			p.Matches++
			if rp.Win {
				p.Wins++
			}
		}
		p.XP = xpFor(p.Kills, p.Caps, p.Wins, p.Matches)
		p.LastMS = now
		n++
	}
	statsDirty = statsDirty || n > 0
	statsMu.Unlock()
	writeJSON(w, map[string]int{"ok": n})
}

type boardRow struct {
	Rank int `json:"rank"`
	Profile
}

func sortedProfiles() []*Profile {
	list := make([]*Profile, 0, len(profiles))
	for _, p := range profiles {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].XP != list[j].XP {
			return list[i].XP > list[j].XP
		}
		return list[i].LastMS < list[j].LastMS
	})
	return list
}

func handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	n = clampInt(n, 1, 100)
	if r.URL.Query().Get("n") == "" {
		n = 50
	}
	statsMu.Lock()
	list := sortedProfiles()
	out := make([]boardRow, 0, n)
	for i, p := range list {
		if i >= n {
			break
		}
		row := boardRow{Rank: i + 1, Profile: *p}
		row.ID = "" // ids are private-ish; the board shows names
		out = append(out, row)
	}
	total := len(list)
	statsMu.Unlock()
	writeJSON(w, map[string]any{"total": total, "players": out})
}

func handleProfile(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.URL.Query().Get("id"))
	if !idRe.MatchString(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	statsMu.Lock()
	defer statsMu.Unlock()
	p := profiles[id]
	if p == nil {
		writeJSON(w, map[string]any{"found": false, "total": len(profiles)})
		return
	}
	rank := 1
	for _, o := range profiles {
		if o.XP > p.XP || (o.XP == p.XP && o.LastMS < p.LastMS) {
			rank++
		}
	}
	writeJSON(w, map[string]any{"found": true, "rank": rank, "total": len(profiles), "profile": p})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(v)
}
