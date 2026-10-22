package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// resetLevels gives one test an empty level store in a temp dir and fresh
// rate limiters.
func resetLevels(t *testing.T) {
	t.Helper()
	mapsMu.Lock()
	mapsDir = t.TempDir()
	mapIndex = map[string]*MapEntry{}
	codeIndex = map[string]string{}
	uploadsByIP = map[string][]time.Time{}
	mapsDirty = false
	mapsMu.Unlock()
	uploadAttemptsPerIP = newIPLimiter(30, time.Minute)
	levelReadsPerIP = newIPLimiter(240, time.Minute)
	levelLikesPerIP = newIPLimiter(60, time.Minute)
	levelPlaysPerIP = newIPLimiter(30, time.Minute)
}

func call(t *testing.T, h http.HandlerFunc, method, url string, body any, ip string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, url, rd)
	if ip == "" {
		ip = "192.0.2.10"
	}
	req.RemoteAddr = ip + ":5555"
	rec := httptest.NewRecorder()
	h(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec, m
}

func sampleMap(name string, extra map[string]any) map[string]any {
	m := map[string]any{
		"name":         name,
		"platforms":    []any{map[string]any{"p": []any{0.0, 1500.0}, "s": []any{2000.0, 60.0}}},
		"player_spawn": []any{100.0, 1400.0},
		"bot_spawns":   []any{[]any{300.0, 1400.0}, []any{900.0, 1400.0}},
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func upload(t *testing.T, title, author string, extra map[string]any, ip string) map[string]any {
	t.Helper()
	rec, m := call(t, handleMapUpload, http.MethodPost, "/levels/upload",
		map[string]any{"title": title, "author": author, "theme": "jungle", "map": sampleMap(title, extra)}, ip)
	if rec.Code != 200 {
		t.Fatalf("upload %q: %d %s", title, rec.Code, rec.Body.String())
	}
	return m
}

var codeRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`)

func TestFilterMasksAndSparesInnocentWords(t *testing.T) {
	cases := map[string]string{
		"Fuck Valley":      "**** Valley",
		"Sh1t Hole":        "**** Hole",
		"f u c k this":     "* * * * this",
		"FUUUUCK":          "*******",
		"a$$hole arena":    "******* arena",
		"Classic Ass":      "Classic ***",
		"b.i.t.c.h":        "*.*.*.*.*",
		"boooobs":          "*******",
		"Bullshit Bridge":  "******** Bridge",
		"Cockpit Run":      "Cockpit Run",
		"Grape Escape":     "Grape Escape",
		"Title Fight":      "Title Fight",
		"Spicy Map":        "Spicy Map",
		"Ash Itadel":       "Ash Itadel",
		"Bob's Base":       "Bob's Base",
		"Niigata Docks":    "Niigata Docks",
		"Fire Retardant":   "Fire Retardant",
		"Assault Course":   "Assault Course",
		"Classic CTF 2":    "Classic CTF 2",
		"Scunthorpe Steel": "********** Steel", // the classic false positive of in-word matching; accepted
	}
	for in, want := range cases {
		got, changed := filterText(in)
		if got != want {
			t.Errorf("filterText(%q) = %q, want %q", in, got, want)
		}
		if changed != (in != want) {
			t.Errorf("filterText(%q) changed=%v", in, changed)
		}
	}
}

func TestShareCodes(t *testing.T) {
	resetLevels(t)
	seen := map[string]bool{}
	for i := 0; i < 3000; i++ {
		e := &MapEntry{ID: fmt.Sprintf("%016x", i*7919+13)}
		assignCode(e)
		if !codeRe.MatchString(e.Code) {
			t.Fatalf("bad code %q", e.Code)
		}
		if seen[e.Code] {
			t.Fatalf("duplicate code %q", e.Code)
		}
		seen[e.Code] = true
		if _, bad := filterText(strings.ReplaceAll(e.Code, "-", "")); bad {
			t.Fatalf("code spells a filtered word: %q", e.Code)
		}
	}
	// Deterministic, and typed any way it still resolves.
	if makeCode("00000000000000ab", 0) != makeCode("00000000000000ab", 0) {
		t.Fatal("codes not deterministic")
	}
	c := makeCode("00000000000000ab", 0)
	typed := strings.ToLower(strings.ReplaceAll(c, "-", " "))
	typed = strings.ReplaceAll(strings.ReplaceAll(typed, "0", "o"), "1", "l")
	if normCode(typed) != strings.ReplaceAll(c, "-", "") {
		t.Fatalf("normCode(%q) = %q, want %q", typed, normCode(typed), c)
	}
	for _, bad := range []string{"", "ABCD", "ABCD-EFGH-JKLMN", "ABCD-EFGH-JKU!"} {
		if normCode(bad) != "" {
			t.Fatalf("normCode(%q) should be empty", bad)
		}
	}
	// An entry keeps the code it already has.
	e := &MapEntry{ID: "00000000000000cd", Code: "ZZZZ-ZZZZ-ZZZZ"}
	assignCode(e)
	if e.Code != "ZZZZ-ZZZZ-ZZZZ" || codeIndex["ZZZZZZZZZZZZ"] != e.ID {
		t.Fatalf("existing code not kept: %q", e.Code)
	}
}

func TestSortingAndHot(t *testing.T) {
	now := time.Now()
	today := dayOf(now)
	old := now.Add(-30 * 24 * time.Hour).UnixMilli()
	es := []*MapEntry{
		{ID: "000000000000000a", UpMS: old, Plays: 50, Likes: 1, PlayDays: map[int]int{today - 6: 50}},
		{ID: "000000000000000b", UpMS: old + 1, Plays: 10, Likes: 9, PlayDays: map[int]int{today: 10}},
		{ID: "000000000000000c", UpMS: old + 2, Plays: 20, Likes: 3, LikeDays: map[int]int{today: 3}},
		{ID: "000000000000000d", UpMS: now.UnixMilli(), Plays: 0, Likes: 0},
	}
	views := func() []LevelView {
		var out []LevelView
		for _, e := range es {
			out = append(out, e.view("", now, false))
		}
		return out
	}
	order := func(by string) string {
		l := views()
		sortLevels(l, by)
		s := ""
		for _, v := range l {
			s += v.ID[15:]
		}
		return s
	}
	if got := order("new"); got != "dcba" {
		t.Errorf("new: %s", got)
	}
	if got := order("plays"); got != "acbd" {
		t.Errorf("plays: %s", got)
	}
	if got := order("likes"); got != "bcad" {
		t.Errorf("likes: %s", got)
	}
	// hot: 10 plays today (10) > 3 likes today (9) > 50 plays 6 days ago
	// (50 * 0.125 = 6.25) > a brand-new level with nothing yet (2).
	if got := order("hot"); got != "bcad" {
		t.Errorf("hot: %s (scores %v)", got, views())
	}
	if h := hotScore(es[3], now); h < 1.9 || h > 2.0 {
		t.Errorf("new-level boost %v", h)
	}
	// Outside the week counts nothing.
	e := &MapEntry{UpMS: old, PlayDays: map[int]int{today - 8: 1000}}
	if hotScore(e, now) != 0 {
		t.Errorf("week-old plays still hot: %v", hotScore(e, now))
	}
}

func TestLikesIdempotent(t *testing.T) {
	resetLevels(t)
	id := upload(t, "Like Me", "Ace", nil, "")["id"].(string)
	pid := "0123456789abcdef0123456789abcdef"
	like := func(p string, on bool, ip string) (int, map[string]any) {
		rec, m := call(t, handleLevelLike, http.MethodPost, "/levels/like", map[string]any{"id": id, "pid": p, "like": on}, ip)
		return rec.Code, m
	}
	for i := 0; i < 3; i++ {
		if code, m := like(pid, true, ""); code != 200 || m["likes"].(float64) != 1 || m["liked"] != true {
			t.Fatalf("like #%d: %d %v", i, code, m)
		}
	}
	if _, m := like("fedcba9876543210", true, "198.51.100.7"); m["likes"].(float64) != 2 {
		t.Fatalf("second player: %v", m)
	}
	// The list shows the caller's own like.
	_, l := call(t, handleLevelList, http.MethodGet, "/levels?sort=likes&pid="+pid, nil, "")
	if lv := l["levels"].([]any)[0].(map[string]any); lv["liked"] != true || lv["likes"].(float64) != 2 {
		t.Fatalf("list liked flag: %v", lv)
	}
	for i := 0; i < 2; i++ {
		if _, m := like(pid, false, ""); m["likes"].(float64) != 1 || m["liked"] != false {
			t.Fatalf("unlike #%d: %v", i, m)
		}
	}
	// One address can't stuff likes with made-up ids.
	for i := 0; i < maxMarksPerIP; i++ {
		if code, _ := like(fmt.Sprintf("%032x", 1000+i), true, "203.0.113.5"); code != 200 {
			t.Fatalf("like from shared address #%d refused", i)
		}
	}
	if code, _ := like(fmt.Sprintf("%032x", 99999), true, "203.0.113.5"); code != http.StatusTooManyRequests {
		t.Fatalf("like over the per-address cap: %d", code)
	}
	if code, _ := like("nothex!", true, ""); code != 400 {
		t.Fatalf("bad pid accepted: %d", code)
	}
	if code, _ := call(t, handleLevelLike, http.MethodPost, "/levels/like", map[string]any{"id": "ffffffffffffffff", "pid": pid}, ""); code.Code != 404 {
		t.Fatalf("unknown level: %d", code.Code)
	}
}

func TestPlayCounting(t *testing.T) {
	resetLevels(t)
	id := upload(t, "Play Me", "Ace", nil, "")["id"].(string)
	now := time.Now()
	pid := "00112233445566778899aabbccddeeff"
	for i := 0; i < 3; i++ {
		plays, counted, ok := playLevel(id, pid, "192.0.2.1", now)
		if !ok || plays != 1 || counted != (i == 0) {
			t.Fatalf("same day play #%d: plays=%d counted=%v", i, plays, counted)
		}
	}
	if plays, counted, _ := playLevel(id, pid, "192.0.2.1", now.Add(24*time.Hour)); plays != 2 || !counted {
		t.Fatalf("next day: %d %v", plays, counted)
	}
	if plays, counted, _ := playLevel(id, "aaaaaaaaaaaaaaaa", "198.51.100.1", now.Add(24*time.Hour)); plays != 3 || !counted {
		t.Fatalf("other player: %d %v", plays, counted)
	}
	// Per-address daily cap, then the next day it counts again.
	day := now.Add(48 * time.Hour)
	for i := 0; i < maxMarksPerIP+2; i++ {
		_, counted, _ := playLevel(id, fmt.Sprintf("%032x", i), "203.0.113.9", day)
		if counted != (i < maxMarksPerIP) {
			t.Fatalf("shared address play #%d counted=%v", i, counted)
		}
	}
	mapsMu.Lock()
	e := mapIndex[id]
	if len(e.Played) != maxMarksPerIP || e.PlayDays[dayOf(day)] != maxMarksPerIP {
		t.Fatalf("played marks %d, day bucket %d", len(e.Played), e.PlayDays[dayOf(day)])
	}
	mapsMu.Unlock()
	// Handler round trip.
	rec, m := call(t, handleLevelPlay, http.MethodPost, "/levels/play", map[string]any{"id": id, "pid": "bbbbbbbbbbbbbbbb"}, "198.51.100.77")
	if rec.Code != 200 || m["counted"] != true {
		t.Fatalf("play handler: %d %v", rec.Code, m)
	}
	if rec, _ := call(t, handleLevelPlay, http.MethodGet, "/levels/play", nil, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET play: %d", rec.Code)
	}
}

func TestUploadListSearchGet(t *testing.T) {
	resetLevels(t)
	flags := map[string]any{"ctf_flags": []any{[]any{100.0, 1450.0}, []any{1900.0, 1450.0}}, "dom_points": []any{[]any{1000.0, 1400.0}}}
	a := upload(t, "Alpha Ridge", "Ace", flags, "")
	time.Sleep(2 * time.Millisecond)
	b := upload(t, "Bravo Fuck Base", "Sh1tlord", nil, "")
	time.Sleep(2 * time.Millisecond)
	if !codeRe.MatchString(a["code"].(string)) || a["modes"] != "dm tdm ctf dom" {
		t.Fatalf("upload reply: %v", a)
	}
	if b["title"] != "Bravo **** Base" {
		t.Fatalf("title not masked: %v", b)
	}
	// Same map again: existing, no new code, no quota used.
	_, again := call(t, handleMapUpload, http.MethodPost, "/levels/upload",
		map[string]any{"title": "Alpha Ridge", "author": "Ace", "map": sampleMap("Alpha Ridge", flags)}, "")
	if again["existing"] != true || again["code"] != a["code"] {
		t.Fatalf("re-upload: %v", again)
	}
	// Legacy route + polygon-only map (ported style, no platforms).
	rec, legacy := call(t, handleMapUpload, http.MethodPost, "/maps/upload", map[string]any{"name": "Poly Land", "author": "Old",
		"map": map[string]any{"name": "Poly Land", "platforms": []any{}, "world": map[string]any{"w": 3000.0, "h": 1500.0},
			"polys": []any{map[string]any{"points": []any{0.0, 1000.0, 3000.0, 1000.0, 1500.0, 1400.0}, "vc": []any{"336633ff", "336633ff", "113311ff"}}}}}, "")
	if rec.Code != 200 || legacy["code"] == nil {
		t.Fatalf("legacy upload: %d %s", rec.Code, rec.Body.String())
	}

	list := func(q string) []any {
		rec, m := call(t, handleLevelList, http.MethodGet, "/levels"+q, nil, "")
		if rec.Code != 200 {
			t.Fatalf("list %s: %d", q, rec.Code)
		}
		return m["levels"].([]any)
	}
	l := list("?sort=new")
	if len(l) != 3 || l[0].(map[string]any)["title"] != "Poly Land" {
		t.Fatalf("new: %v", l)
	}
	if th, ok := l[0].(map[string]any)["thumb"].(map[string]any); !ok || th["w"].(float64) > thumbW || th["h"].(float64) > thumbH || len(th["p"].([]any)) != 1 {
		t.Fatalf("thumb: %v", l[0])
	}
	if p := l[0].(map[string]any)["thumb"].(map[string]any)["p"].([]any)[0].([]any); int(p[0].(float64)) != 0x275527 {
		t.Fatalf("poly colour from vc: %x", int(p[0].(float64)))
	}
	if got := list("?q=ridge"); len(got) != 1 || got[0].(map[string]any)["id"] != a["id"] {
		t.Fatalf("search title: %v", got)
	}
	if got := list("?q=OLD"); len(got) != 1 || got[0].(map[string]any)["title"] != "Poly Land" {
		t.Fatalf("search author: %v", got)
	}
	typed := strings.ToLower(strings.ReplaceAll(a["code"].(string), "-", " "))
	if got := list("?q=" + strings.ReplaceAll(typed, " ", "+")); len(got) != 1 || got[0].(map[string]any)["id"] != a["id"] {
		t.Fatalf("search code %q: %v", typed, got)
	}
	// Paging.
	p0, p1 := list("?sort=new&per=2&page=0"), list("?sort=new&per=2&page=1")
	if len(p0) != 2 || len(p1) != 1 {
		t.Fatalf("paging %d %d", len(p0), len(p1))
	}
	_, pm := call(t, handleLevelList, http.MethodGet, "/levels?per=2", nil, "")
	if pm["more"] != true || pm["total"].(float64) != 3 {
		t.Fatalf("paging meta: %v", pm)
	}
	// Bookmarks: exactly these, in this order.
	if got := list("?ids=" + b["id"].(string) + ",ffffffffffffffff," + a["id"].(string)); len(got) != 2 || got[0].(map[string]any)["id"] != b["id"] {
		t.Fatalf("ids: %v", got)
	}
	// Get by code (any spelling) returns the level and its map.
	rec, g := call(t, handleLevelGet, http.MethodGet, "/levels/get?code="+strings.ReplaceAll(typed, " ", ""), nil, "")
	if rec.Code != 200 {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	if g["level"].(map[string]any)["id"] != a["id"] || len(g["map"].(map[string]any)["platforms"].([]any)) != 1 {
		t.Fatalf("get body: %v", g)
	}
	if rec, _ := call(t, handleLevelGet, http.MethodGet, "/levels/get?code=0000-0000-0000", nil, ""); rec.Code != 404 {
		t.Fatalf("unknown code: %d", rec.Code)
	}
	// The old list keeps its shape and never leaks like / play marks.
	call(t, handleLevelLike, http.MethodPost, "/levels/like", map[string]any{"id": a["id"], "pid": "0123456789abcdef"}, "")
	rec = httptest.NewRecorder()
	handleMapList(rec, httptest.NewRequest(http.MethodGet, "/maps?sort=top", nil))
	if body := rec.Body.String(); strings.Contains(body, "likers") || strings.Contains(body, `"ip"`) || !strings.Contains(body, `"downloads"`) || !strings.Contains(body, `"name":"Alpha Ridge"`) {
		t.Fatalf("legacy /maps body: %s", body)
	}
	// Title cap.
	_, long := call(t, handleMapUpload, http.MethodPost, "/levels/upload",
		map[string]any{"title": strings.Repeat("Z", 80), "map": sampleMap("x", map[string]any{"medkits": []any{}})}, "")
	if n := len([]rune(long["title"].(string))); n != maxTitleLen {
		t.Fatalf("title length %d", n)
	}
	// Empty / platform-less maps are refused.
	if rec, _ := call(t, handleMapUpload, http.MethodPost, "/levels/upload", map[string]any{"title": "x", "map": map[string]any{"platforms": []any{}}}, ""); rec.Code != 400 {
		t.Fatalf("empty map: %d", rec.Code)
	}
}

func TestMigrationOfOldIndex(t *testing.T) {
	resetLevels(t)
	dir := t.TempDir()
	t.Setenv("SOLDAT_DATA_DIR", dir)
	md := filepath.Join(dir, "maps")
	_ = os.MkdirAll(md, 0o755)
	old := `[{"id":"0123456789abcdef","name":"Old Map","author":"Vet","size":10,"platforms":1,"modes":"ctf","uploaded_ms":1700000000000,"downloads":7}]`
	_ = os.WriteFile(filepath.Join(md, "index.json"), []byte(old), 0o644)
	mb, _ := json.Marshal(sampleMap("Old Map", map[string]any{"ctf_flags": []any{[]any{50.0, 1450.0}, []any{1950.0, 1450.0}}, "htf_flag": []any{1000.0, 1450.0}}))
	_ = os.WriteFile(filepath.Join(md, "0123456789abcdef.json"), mb, 0o644)
	mapsMu.Lock()
	mapIndex = map[string]*MapEntry{}
	mapsMu.Unlock()
	loadMaps()
	mapsMu.Lock()
	e := mapIndex["0123456789abcdef"]
	mapsMu.Unlock()
	if e == nil || e.V != levelRecordV || !codeRe.MatchString(e.Code) || e.Thumb == nil || e.Modes != "dm tdm ctf htf" || e.Downloads != 7 {
		t.Fatalf("migrated entry: %+v", e)
	}
	code := e.Code
	saveMapIndex()
	// Reload: nothing left to migrate and the code is unchanged.
	mapsMu.Lock()
	mapIndex = map[string]*MapEntry{}
	codeIndex = map[string]string{}
	mapsMu.Unlock()
	loadMaps()
	mapsMu.Lock()
	e = mapIndex["0123456789abcdef"]
	mapsMu.Unlock()
	if e == nil || e.Code != code || codeIndex[normCode(code)] != e.ID {
		t.Fatalf("code after reload: %+v (want %s)", e, code)
	}
	// The saved index still reads as the old struct shape (older binaries).
	b, _ := os.ReadFile(filepath.Join(md, "index.json"))
	var legacy []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Downloads int    `json:"downloads"`
	}
	if json.Unmarshal(b, &legacy) != nil || len(legacy) != 1 || legacy[0].Name != "Old Map" || legacy[0].Downloads != 7 {
		t.Fatalf("index no longer readable as the old shape: %s", b)
	}
}

func TestThumbBudget(t *testing.T) {
	polys := []any{}
	for i := 0; i < 5000; i++ {
		x := float64(i%100) * 40
		y := float64(i/100) * 40
		polys = append(polys, map[string]any{"points": []any{x, y, x + 39, y, x, y + 39}, "color": []any{0.5, 0.25, 1.0}})
	}
	// One tiny sliver that must be dropped.
	polys = append(polys, map[string]any{"points": []any{0.0, 0.0, 0.01, 0.0, 0.0, 0.01}})
	th := buildThumb(map[string]any{"polys": polys, "world": map[string]any{"w": 4000.0, "h": 2000.0}})
	if th == nil || th.W > thumbW || th.H > thumbH {
		t.Fatalf("thumb box %+v", th)
	}
	pts := 0
	for _, p := range th.P {
		pts += (len(p) - 1) / 2
		if p[0] != 0x8040ff {
			t.Fatalf("colour %x", p[0])
		}
		for k := 1; k < len(p); k++ {
			if p[k] < 0 || p[k] > thumbW {
				t.Fatalf("point out of box: %v", p)
			}
		}
	}
	if len(th.P) > maxThumbPolys || pts > maxThumbPoints {
		t.Fatalf("over budget: %d polys %d points", len(th.P), pts)
	}
	// Editor maps stand on the baseline floor.
	ed := buildThumb(sampleMap("ed", nil))
	if ed == nil || len(ed.R) != 2 || ed.C != -1 {
		t.Fatalf("editor thumb: %+v", ed)
	}
	// Flags above the ground still make it into the box.
	fl := buildThumb(sampleMap("f", map[string]any{"ctf_flags": []any{[]any{50.0, 1450.0}, []any{1950.0, 1450.0}}}))
	if fl == nil || len(fl.F) != 2 || fl.F[0][2] != 0 || fl.F[1][2] != 1 || fl.F[0][1] < 0 {
		t.Fatalf("flag markers: %+v", fl)
	}
	if buildThumb(map[string]any{"world": map[string]any{"w": 100.0}}) != nil {
		t.Fatal("empty map has a thumb")
	}
}

// A level with two bomb sites is hinted for Bomb Defusal (game #304).
func TestModeHintsBomb(t *testing.T) {
	m := map[string]any{"player_spawn": []any{100.0, 100.0}, "bot_spawns": []any{[]any{200.0, 100.0}},
		"bomb_sites": []any{map[string]any{"name": "A", "rect": []any{0.0, 0.0, 200.0, 150.0}},
			map[string]any{"name": "B", "rect": []any{900.0, 0.0, 200.0, 150.0}}}}
	if got := modeHints(m); got != "dm tdm bomb" {
		t.Fatalf("modeHints = %q, want \"dm tdm bomb\"", got)
	}
	delete(m, "bomb_sites")
	if got := modeHints(m); got != "dm tdm" {
		t.Fatalf("modeHints without sites = %q", got)
	}
}
