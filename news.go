package main

// News line (#261) + weekly featured mode/map tie-in (#264).
//
// GET /news returns one short message for the game's main menu:
//
//	{"text":"...","url":"https://...","until_ms":1760000000000}
//
// or {} when there is nothing to show. The text comes from news.json next
// to the binary (SOLDAT_NEWS_FILE overrides the path, SOLDAT_DATA_DIR moves
// the default like stats.json / maps). With no active news text it falls
// back to the official server's featured week (featured.json, written by
// deploy/game-server.sh; SOLDAT_FEATURED_FILE, default
// /opt/soldat-game/featured.json): "This week on the official server:
// Rambomatch on Voland". Both files are re-read when they change on disk
// (mtime + size), so editing them never needs a restart.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	newsMaxText   = 120
	newsMaxURL    = 300
	newsMaxFile   = 16 << 10 // a news doc is a few hundred bytes; ignore anything huge
	newsCacheSecs = 60
)

// newsDoc is what /news serves (and the shape of news.json).
type newsDoc struct {
	Text    string `json:"text,omitempty"`
	URL     string `json:"url,omitempty"`
	UntilMS int64  `json:"until_ms,omitempty"`
}

// featuredDoc is featured.json (see deploy/game-server.sh). Until may be epoch
// ms (number or digit string), a date ("2026-10-26", valid through that day,
// UTC) or an RFC 3339 time.
type featuredDoc struct {
	Mode  string          `json:"mode"`
	Map   string          `json:"map"`
	Note  string          `json:"note"`
	URL   string          `json:"url"`
	Until json.RawMessage `json:"until"`
	Bans  json.RawMessage `json:"bans"`
}

// watchedFile caches one small JSON file and re-reads it when it changes.
type watchedFile struct {
	mu    sync.Mutex
	path  string
	mod   time.Time
	size  int64
	ok    bool
	raw   []byte
	known bool // stat has run at least once
}

// load returns the current file contents (nil when missing / unreadable /
// too big). The stat per call is cheap; /news responses are cacheable anyway.
func (f *watchedFile) load() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, err := os.Stat(f.path)
	if err != nil || st.IsDir() || st.Size() > newsMaxFile {
		f.known, f.ok, f.raw = true, false, nil
		return nil
	}
	if f.known && f.ok && st.ModTime().Equal(f.mod) && st.Size() == f.size {
		return f.raw
	}
	b, err := os.ReadFile(f.path)
	f.known, f.mod, f.size = true, st.ModTime(), st.Size()
	if err != nil || len(b) > newsMaxFile {
		f.ok, f.raw = false, nil
		return nil
	}
	// Hand-edited files: a raw tab / newline / control byte inside a JSON
	// string is invalid JSON; blank them (cleanText drops what's left).
	// (Whitespace becomes a space, other control bytes go.)
	clean := make([]byte, 0, len(b))
	for _, c := range b {
		switch {
		case c == '\t' || c == '\n' || c == '\r':
			clean = append(clean, ' ')
		case c >= 0x20:
			clean = append(clean, c)
		}
	}
	f.ok, f.raw = true, clean
	return clean
}

var (
	newsOnce     sync.Once
	newsFile     *watchedFile
	featuredFile *watchedFile
)

func newsPath() string {
	if p := strings.TrimSpace(os.Getenv("SOLDAT_NEWS_FILE")); p != "" {
		return p
	}
	return filepath.Join(dataDir(), "news.json")
}

func featuredPath() string {
	if p := strings.TrimSpace(os.Getenv("SOLDAT_FEATURED_FILE")); p != "" {
		return p
	}
	return "/opt/soldat-game/featured.json"
}

func newsFiles() (*watchedFile, *watchedFile) {
	newsOnce.Do(func() {
		newsFile = &watchedFile{path: newsPath()}
		featuredFile = &watchedFile{path: featuredPath()}
	})
	return newsFile, featuredFile
}

// cleanURL keeps only plain http(s) links (the game hands them to the OS).
func cleanURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > newsMaxURL {
		return ""
	}
	for _, r := range s {
		if r < 0x21 || r == 0x7f {
			return ""
		}
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return s
}

// parseUntil: 0 = no expiry, -1 = unparseable (treated as expired).
func parseUntil(raw json.RawMessage) int64 {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == `""` || s == "0" {
		return 0
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(n)
	}
	var str string
	if json.Unmarshal(raw, &str) != nil {
		return -1
	}
	str = strings.TrimSpace(str)
	if str == "" {
		return 0
	}
	if n, err := strconv.ParseInt(str, 10, 64); err == nil {
		return n
	}
	if d, err := time.Parse("2006-01-02", str); err == nil {
		return d.Add(24 * time.Hour).UnixMilli() // through the end of that day (UTC)
	}
	if t, err := time.Parse(time.RFC3339, str); err == nil {
		return t.UnixMilli()
	}
	return -1
}

func activeUntil(until, nowMS int64) bool {
	return until == 0 || until > nowMS
}

// modeDisplay: featured.json may use the game's aliases or indices
// (scripts/game_lists.gd MODE_NAMES); show the real name in the news line.
var modeNames = []string{"Deathmatch", "Teammatch", "Capture the Flag", "Infiltration",
	"Hold the Flag", "Rambomatch", "Pointmatch", "Domination", "Battle Royale", "Gun Game"}
var modeAliases = map[string]int{"dm": 0, "tdm": 1, "ctf": 2, "inf": 3, "htf": 4, "rm": 5, "pm": 6, "dom": 7, "br": 8, "gg": 9}

func modeDisplay(m string) string {
	m = strings.TrimSpace(m)
	if i, err := strconv.Atoi(m); err == nil && i >= 0 && i < len(modeNames) {
		return modeNames[i]
	}
	if i, ok := modeAliases[strings.ToLower(m)]; ok {
		return modeNames[i]
	}
	for _, n := range modeNames {
		if strings.EqualFold(n, m) {
			return n
		}
	}
	return m
}

// currentNews resolves what /news should say right now.
func currentNews(nowMS int64) newsDoc {
	nf, ff := newsFiles()
	if b := nf.load(); b != nil {
		var d struct {
			Text  string          `json:"text"`
			URL   string          `json:"url"`
			Until json.RawMessage `json:"until_ms"`
		}
		if json.Unmarshal(b, &d) == nil {
			text := cleanText(d.Text, newsMaxText)
			until := parseUntil(d.Until)
			if text != "" && activeUntil(until, nowMS) {
				return newsDoc{Text: text, URL: cleanURL(d.URL), UntilMS: until}
			}
		}
	}
	if b := ff.load(); b != nil {
		var f featuredDoc
		if json.Unmarshal(b, &f) == nil {
			until := parseUntil(f.Until)
			mode := cleanText(modeDisplay(f.Mode), 32)
			mp := cleanText(f.Map, 32)
			if activeUntil(until, nowMS) && (mode != "" || mp != "") {
				// The note is the line itself when given; otherwise say it plainly.
				text := cleanText(f.Note, newsMaxText)
				if text == "" {
					text = "This week on the official server: "
					switch {
					case mode != "" && mp != "":
						text += fmt.Sprintf("%s on %s", mode, mp)
					case mode != "":
						text += mode
					default:
						text += mp
					}
				}
				return newsDoc{Text: cleanText(text, newsMaxText), URL: cleanURL(f.URL), UntilMS: until}
			}
		}
	}
	return newsDoc{}
}

func handleNews(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	d := currentNews(time.Now().UnixMilli())
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", newsCacheSecs))
	writeJSON(w, d)
}
