package main

// Reserved names (Predator04/Tracerfall#318). Anyone can join a game server
// under anyone's name, so classic Soldat servers ran "AntiFake" scripts. Here
// a player reserves their name against their profile and game servers ask
// before letting someone play under it.
//
//	POST /names/reserve {id, name}         reserve / change (one name per profile)
//	POST /names/reserve {id, release:true} give the name up
//	POST /names/mine    {id}               this profile's reservation
//	GET  /names/check?name=&pid=           is a name reserved, and by whom
//
// `id` is the game's random 32-hex profile id. Only its sha256 is kept: the
// same hash a game server computes from the id the player sends when joining
// (the game's Net.peer_profiles, also the leaderboard key), so a server
// compares the joiner's hash with `owner`. Names are compared after the
// filter's normalisation (filterNorm: case, leetspeak and accents folded,
// everything but a-z dropped), so "B0b", "bob" and "B.O.B" are one name.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	nameMinKey     = 3              // normalised letters a reservable name needs
	nameMaxLen     = 24             // the game's own name limit
	nameCooldown   = 72 * time.Hour // between changes (a release counts as one)
	maxNamesPerIP  = 5              // live reservations made from one address
	maxNamesTotal  = 200000         // whole store (memory bound)
	namesSaveEvery = 20 * time.Second
)

// Reservation is one profile's reserved name. A released name keeps the
// record (Name / Key empty) so the cooldown still applies to the next one.
type Reservation struct {
	Owner string `json:"owner"`          // sha256(profile id), 64 hex
	Name  string `json:"name,omitempty"` // as typed (cleaned)
	Key   string `json:"key,omitempty"`  // filterNorm(Name)
	SetMS int64  `json:"set_ms"`         // last reserve / change / release
	IP    string `json:"ip,omitempty"`   // short hash of the address it was made from
}

var (
	namesMu    sync.Mutex
	namesByKey = map[string]*Reservation{} // key -> reservation
	namesByOwn = map[string]*Reservation{} // owner -> reservation (also released ones)
	namesDirty bool
	namesFile  string
	rawIDRe    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// Names nobody may reserve: the server's own chat author, the names the
	// game gives (impostors are renamed "Player####", bots are "*_Bot_*").
	nameBlockedKeys     = []string{"server", "host", "admin", "moderator", "system", "console", "tracerfall", "soldat", "spectator"}
	nameBlockedPrefixes = []string{"player", "bluebot", "redbot"}
	// "Bot 12" normalises to "boti" (digits fold to letters or drop).
	botKeyRe = regexp.MustCompile(`^bot[oiesatbg]{0,2}$`)
)

// Rate limits (per source address, like levels.go): game servers check every
// joining name (they cache), the settings screen checks and reserves.
var (
	nameReadsPerIP  = newIPLimiter(240, time.Minute)
	nameWritesPerIP = newIPLimiter(10, time.Minute)
)

// profileHash is what a game server stores for a joiner: sha256 of the
// lower-cased profile id, hex.
func profileHash(id string) string {
	s := sha256.Sum256([]byte(strings.ToLower(id)))
	return hex.EncodeToString(s[:])
}

// nameKey strips a clan tag ("«TAG» Name") and normalises.
func nameKey(name string) string {
	return filterNorm(stripTag(name))
}

func stripTag(name string) string {
	name = strings.TrimSpace(name)
	if strings.HasPrefix(name, "«") {
		if i := strings.Index(name, "»"); i > 0 {
			name = strings.TrimSpace(name[i+len("»"):])
		}
	}
	return name
}

// nameProblem says why a name can't be reserved ("" = it can).
func nameProblem(name string) string {
	key := nameKey(name)
	if len(key) < nameMinKey {
		return "short"
	}
	if _, bad := filterText(name); bad || wordIsBad(key) {
		return "offensive"
	}
	for _, b := range nameBlockedKeys {
		if key == b {
			return "blocked"
		}
	}
	if botKeyRe.MatchString(key) {
		return "blocked"
	}
	for _, p := range nameBlockedPrefixes {
		if strings.HasPrefix(key, p) {
			return "blocked"
		}
	}
	return ""
}

func loadNames() {
	namesFile = filepath.Join(dataDir(), "names.json")
	b, err := os.ReadFile(namesFile)
	if err != nil {
		return
	}
	var list []*Reservation
	if json.Unmarshal(b, &list) != nil {
		log.Printf("names: %s unreadable, starting empty", namesFile)
		return
	}
	namesMu.Lock()
	for _, r := range list {
		if r == nil || !idRe.MatchString(r.Owner) {
			continue
		}
		namesByOwn[r.Owner] = r
		if r.Key != "" {
			namesByKey[r.Key] = r
		}
	}
	n := len(namesByKey)
	namesMu.Unlock()
	log.Printf("names: %d reserved names from %s", n, namesFile)
}

func saveNamesLoop() {
	for {
		time.Sleep(namesSaveEvery)
		saveNames()
	}
}

func saveNames() {
	namesMu.Lock()
	defer namesMu.Unlock()
	if !namesDirty || namesFile == "" {
		return
	}
	list := make([]*Reservation, 0, len(namesByOwn))
	for _, r := range namesByOwn {
		list = append(list, r)
	}
	b, _ := json.Marshal(list)
	tmp := namesFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("names: save failed: %v", err)
		return
	}
	if err := os.Rename(tmp, namesFile); err != nil {
		log.Printf("names: save failed: %v", err)
		return
	}
	namesDirty = false
}

type nameResult struct {
	Status string // reserved | released | taken | cooldown | short | offensive | blocked | limit | full
	Name   string
	WaitS  int64
	Code   int
}

// reserveName reserves `name` for owner (or releases with name == "").
func reserveName(owner, name, ip string, now time.Time) nameResult {
	name = cleanText(stripTag(name), nameMaxLen)
	namesMu.Lock()
	defer namesMu.Unlock()
	cur := namesByOwn[owner]
	if name == "" { // release
		if cur == nil || cur.Key == "" {
			return nameResult{Status: "released", Code: http.StatusOK}
		}
		delete(namesByKey, cur.Key)
		cur.Name, cur.Key, cur.SetMS = "", "", now.UnixMilli()
		namesDirty = true
		return nameResult{Status: "released", Code: http.StatusOK}
	}
	if p := nameProblem(name); p != "" {
		return nameResult{Status: p, Code: http.StatusBadRequest}
	}
	key := nameKey(name)
	if other := namesByKey[key]; other != nil && other.Owner != owner {
		return nameResult{Status: "taken", Name: other.Name, Code: http.StatusConflict}
	}
	if cur != nil && cur.Key == key {
		// Same name (maybe new spelling / case): no cooldown for that.
		if cur.Name != name {
			cur.Name = name
			namesDirty = true
		}
		return nameResult{Status: "reserved", Name: cur.Name, Code: http.StatusOK}
	}
	if cur != nil {
		if wait := time.UnixMilli(cur.SetMS).Add(nameCooldown).Sub(now); wait > 0 {
			return nameResult{Status: "cooldown", Name: cur.Name, WaitS: int64(wait.Seconds()) + 1, Code: http.StatusTooManyRequests}
		}
	}
	ipk := shortHash("ip:" + ip)[:8]
	n := 0
	for _, r := range namesByKey {
		if r.IP == ipk && r.Owner != owner {
			n++
		}
	}
	if n >= maxNamesPerIP {
		return nameResult{Status: "limit", Code: http.StatusTooManyRequests}
	}
	if cur == nil && len(namesByOwn) >= maxNamesTotal {
		return nameResult{Status: "full", Code: http.StatusServiceUnavailable}
	}
	if cur == nil {
		cur = &Reservation{Owner: owner}
		namesByOwn[owner] = cur
	} else if cur.Key != "" {
		delete(namesByKey, cur.Key)
	}
	cur.Name, cur.Key, cur.SetMS, cur.IP = name, key, now.UnixMilli(), ipk
	namesByKey[key] = cur
	namesDirty = true
	return nameResult{Status: "reserved", Name: name, Code: http.StatusOK}
}

type nameReq struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Release bool   `json:"release"`
}

func readNameReq(w http.ResponseWriter, r *http.Request) (nameReq, bool) {
	var req nameReq
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return req, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return req, false
	}
	req.ID = strings.ToLower(strings.TrimSpace(req.ID))
	if !rawIDRe.MatchString(req.ID) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// mineView is a profile's reservation as /names/mine and /names/reserve show it.
func mineView(owner string, now time.Time) map[string]any {
	namesMu.Lock()
	defer namesMu.Unlock()
	out := map[string]any{"name": "", "owner": owner, "cooldown_s": 0}
	if cur := namesByOwn[owner]; cur != nil {
		out["name"] = cur.Name
		out["set_ms"] = cur.SetMS
		if wait := time.UnixMilli(cur.SetMS).Add(nameCooldown).Sub(now); wait > 0 {
			out["cooldown_s"] = int64(wait.Seconds()) + 1
		}
	}
	return out
}

func handleNameReserve(w http.ResponseWriter, r *http.Request) {
	ip := sourceIP(r)
	if !nameWritesPerIP.allow(ip) {
		http.Error(w, "too many requests, slow down", http.StatusTooManyRequests)
		return
	}
	req, ok := readNameReq(w, r)
	if !ok {
		return
	}
	owner := profileHash(req.ID)
	name := req.Name
	if req.Release {
		name = ""
	} else if strings.TrimSpace(name) == "" {
		http.Error(w, "no name", http.StatusBadRequest)
		return
	}
	now := time.Now()
	res := reserveName(owner, name, ip, now)
	out := mineView(owner, now)
	out["ok"] = res.Code == http.StatusOK
	out["status"] = res.Status
	if res.Status == "taken" {
		out["taken_by"] = res.Name
	}
	if res.WaitS > 0 {
		out["cooldown_s"] = res.WaitS
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(res.Code)
	_ = json.NewEncoder(w).Encode(out)
}

func handleNameMine(w http.ResponseWriter, r *http.Request) {
	if !nameReadsPerIP.allow(sourceIP(r)) {
		http.Error(w, "too many requests, slow down", http.StatusTooManyRequests)
		return
	}
	req, ok := readNameReq(w, r)
	if !ok {
		return
	}
	out := mineView(profileHash(req.ID), time.Now())
	out["ok"] = true
	writeJSON(w, out)
}

// GET /names/check?name=<name>[&pid=<64-hex profile hash>]
// → {name, key, reserved, owner?, display?, status?}. status (with pid):
// free | mine | taken; "invalid" for a name that can't be reserved at all.
func handleNameCheck(w http.ResponseWriter, r *http.Request) {
	if !nameReadsPerIP.allow(sourceIP(r)) {
		http.Error(w, "too many requests, slow down", http.StatusTooManyRequests)
		return
	}
	name := r.URL.Query().Get("name")
	if len(name) > 200 {
		http.Error(w, "name too long", http.StatusBadRequest)
		return
	}
	pid := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("pid")))
	key := nameKey(name)
	out := map[string]any{"name": name, "key": key, "reserved": false}
	namesMu.Lock()
	res := namesByKey[key]
	if res != nil {
		out["reserved"] = true
		out["owner"] = res.Owner
		out["display"] = res.Name
	}
	namesMu.Unlock()
	if idRe.MatchString(pid) {
		switch {
		case res != nil && res.Owner == pid:
			out["status"] = "mine"
		case res != nil:
			out["status"] = "taken"
		case nameProblem(name) != "":
			out["status"] = "invalid"
			out["problem"] = nameProblem(name)
		default:
			out["status"] = "free"
		}
	}
	writeJSON(w, out)
}
