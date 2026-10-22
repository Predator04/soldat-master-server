package main

// Level sharing (Predator04/Soldat-Reborn#288): the shared maps (maps.go)
// grown into a Mario-Maker-style level library. Every stored map is a level
// with a share code ("7KQ2-MX9P-4HDA"), mode hints, a thumbnail outline,
// plays and likes, and a "hot this week" score. The old /maps routes keep
// working for older game builds; they read and write the same store.
//
//	GET  /levels?sort=hot|new|plays|likes&q=&page=0&per=24&pid=&ids=
//	GET  /levels/get?id=<16 hex>|code=<share code>[&pid=]
//	POST /levels/upload {title, author, map, theme}
//	POST /levels/like   {id, pid, like}
//	POST /levels/play   {id, pid}
//
// pid is an anonymous per-install id (hex, 16-64 chars) the game derives
// from its random profile id; only a hash of it is stored. There are no
// accounts, so bookmarks live in the game.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	levelRecordV    = 2   // MapEntry.V once the level fields are filled in
	thumbW          = 320 // thumbnail box (aspect kept, fits inside)
	thumbH          = 128
	maxThumbPolys   = 600
	maxThumbPoints  = 3600
	levelsPerPage   = 24
	levelsPerPageMx = 48
	maxLevelPages   = 1000
	// One address (a household / LAN behind NAT) can like a level, or count
	// a play of it per day, at most this many times with different pids.
	maxMarksPerIP = 4
	hotDays       = 7                                  // "hot this week"
	keepDays      = 14                                 // per-day buckets kept for the hot score
	codeAlphabet  = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base 32
)

// Mark is one player's like or play: the day it happened and a short hash of
// the address it came from (the per-address caps above).
type Mark struct {
	D  int    `json:"d"`
	IP string `json:"ip"`
}

// Thumb is a level's outline for the browser cards, in thumbnail pixels
// (0..W, 0..H): platform rects, polygons with a colour (0xRRGGBB, -1 = the
// game's default ground colour) and objective markers.
type Thumb struct {
	W int      `json:"w"`
	H int      `json:"h"`
	C int      `json:"c"`           // colour of the rects (the map's terrain colour, -1 = default)
	R [][4]int `json:"r,omitempty"` // [x, y, w, h]
	P [][]int  `json:"p,omitempty"` // [colour, x0, y0, x1, y1, ...]
	F [][3]int `json:"f,omitempty"` // [x, y, kind] 0 blue flag, 1 red flag, 2 neutral flag, 3 dom point
}

// LevelView is what the API shows of a level (never the like / play marks).
type LevelView struct {
	ID        string  `json:"id"`
	Code      string  `json:"code"`
	Title     string  `json:"title"`
	Name      string  `json:"name"` // = title; the field older builds read
	Author    string  `json:"author"`
	Created   int64   `json:"created_ms"`
	UpMS      int64   `json:"uploaded_ms"`
	Size      int     `json:"size"`
	Platforms int     `json:"platforms"`
	Modes     string  `json:"modes"`
	Theme     string  `json:"theme,omitempty"`
	Plays     int     `json:"plays"`
	Likes     int     `json:"likes"`
	Downloads int     `json:"downloads"`
	Hot       float64 `json:"hot"`
	Liked     bool    `json:"liked,omitempty"`
	Thumb     *Thumb  `json:"thumb,omitempty"`
}

var (
	codeIndex = map[string]string{} // normalised code -> id (mapsMu)
	pidRe     = regexp.MustCompile(`^[0-9a-f]{16,64}$`)
	themes    = map[string]bool{"rock": true, "urban": true, "desert": true, "snow": true, "jungle": true, "ruins": true}
)

func dayOf(t time.Time) int { return int(t.Unix() / 86400) }

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// ── Share codes ─────────────────────────────────────────────────────────────

// makeCode derives a 12-symbol code from the level id (salt bumps on a
// collision or an unlucky word), shown as XXXX-XXXX-XXXX.
func makeCode(id string, salt int) string {
	sum := sha256.Sum256([]byte("tracerfall-level:" + id + ":" + strconv.Itoa(salt)))
	var v uint64
	for i := 0; i < 8; i++ {
		v = v<<8 | uint64(sum[i])
	}
	b := make([]byte, 0, 14)
	for i := 0; i < 12; i++ {
		if i == 4 || i == 8 {
			b = append(b, '-')
		}
		b = append(b, codeAlphabet[v&31])
		v >>= 5
	}
	return string(b)
}

// normCode turns what a player typed into the canonical 12 symbols, or "".
// Case, spaces and dashes don't matter; O reads as 0 and I / L as 1.
func normCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch r {
		case ' ', '-', '_', '.':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if !strings.ContainsRune(codeAlphabet, r) {
			return ""
		}
		b.WriteRune(r)
	}
	if b.Len() != 12 {
		return ""
	}
	return b.String()
}

// assignCode gives e a unique code (mapsMu held).
func assignCode(e *MapEntry) {
	if e.Code != "" {
		if n := normCode(e.Code); n != "" && (codeIndex[n] == "" || codeIndex[n] == e.ID) {
			codeIndex[n] = e.ID
			return
		}
	}
	for salt := 0; ; salt++ {
		c := makeCode(e.ID, salt)
		n := normCode(c)
		if _, bad := filterText(strings.ReplaceAll(c, "-", "")); bad {
			continue
		}
		if codeIndex[n] == "" {
			codeIndex[n] = e.ID
			e.Code = c
			return
		}
	}
}

// ── Map analysis: mode hints + thumbnail ───────────────────────────────────

func num(v any) (float64, bool) {
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func vec(v any) (float64, float64, bool) {
	a, ok := v.([]any)
	if !ok || len(a) < 2 {
		return 0, 0, false
	}
	x, ok1 := num(a[0])
	y, ok2 := num(a[1])
	return x, y, ok1 && ok2
}

func vecList(v any) [][2]float64 {
	a, _ := v.([]any)
	var out [][2]float64
	for _, it := range a {
		if x, y, ok := vec(it); ok {
			out = append(out, [2]float64{x, y})
		}
	}
	return out
}

// modeHints lists the modes a level is set up for (space separated, lower
// case): dm always; tdm with two or more spawns; ctf / inf / htf / rm / dom /
// bomb when the author placed that mode's objects. Every mode still runs on any
// map (the game falls back to default spots); these say which ones the
// author built for.
func modeHints(m map[string]any) string {
	out := []string{"dm"}
	spawns := len(vecList(m["bot_spawns"]))
	if _, _, ok := vec(m["player_spawn"]); ok {
		spawns++
	}
	_, teams := m["team_spawns"].(map[string]any)
	flags := len(vecList(m["ctf_flags"]))
	if spawns >= 2 || teams || flags >= 2 {
		out = append(out, "tdm")
	}
	if flags >= 2 {
		out = append(out, "ctf")
	}
	if _, _, ok := vec(m["inf_flag"]); ok {
		out = append(out, "inf")
	}
	if _, _, ok := vec(m["htf_flag"]); ok {
		out = append(out, "htf")
	}
	if _, _, ok := vec(m["rambo_pos"]); ok {
		out = append(out, "rm")
	}
	if len(vecList(m["dom_points"])) > 0 {
		out = append(out, "dom")
	}
	// Bomb Defusal (game #304): two bomb sites [{name, rect: [x, y, w, h]}].
	if sites, ok := m["bomb_sites"].([]any); ok && len(sites) >= 2 {
		out = append(out, "bomb")
	}
	return strings.Join(out, " ")
}

func hexColour(s string) (int, bool) {
	if len(s) < 6 {
		return 0, false
	}
	v, err := strconv.ParseUint(s[:6], 16, 32)
	return int(v), err == nil
}

func polyColour(p map[string]any, def int) int {
	if c, ok := p["color"].([]any); ok && len(c) >= 3 {
		r, _ := num(c[0])
		g, _ := num(c[1])
		b, _ := num(c[2])
		cl := func(f float64) int { return int(math.Max(0, math.Min(255, math.Round(f*255)))) }
		return cl(r)<<16 | cl(g)<<8 | cl(b)
	}
	if vc, ok := p["vc"].([]any); ok && len(vc) > 0 {
		var r, g, b, n int
		for _, it := range vc {
			s, _ := it.(string)
			if v, ok := hexColour(s); ok {
				r += v >> 16 & 255
				g += v >> 8 & 255
				b += v & 255
				n++
			}
		}
		if n > 0 {
			return (r/n)<<16 | (g/n)<<8 | b/n
		}
	}
	return def
}

// buildThumb draws the level's outline into the thumbnail box: platforms,
// polygons (largest first when over budget, then back in file order so
// overlaps paint like the game), the visible floor and objective markers.
func buildThumb(m map[string]any) *Thumb {
	type shape struct {
		rect bool
		col  int
		pts  []float64
		area float64
		idx  int
	}
	var shapes []shape
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	grow := func(x, y float64) {
		minX, minY = math.Min(minX, x), math.Min(minY, y)
		maxX, maxY = math.Max(maxX, x), math.Max(maxY, y)
	}
	def := -1
	if tc, ok := m["terrain_color"].([]any); ok && len(tc) >= 3 {
		def = polyColour(map[string]any{"color": tc}, -1)
	}
	pls, _ := m["platforms"].([]any)
	for _, it := range pls {
		p, _ := it.(map[string]any)
		x, y, ok1 := vec(p["p"])
		w, h, ok2 := vec(p["s"])
		if !ok1 || !ok2 || w <= 0 || h <= 0 {
			continue
		}
		grow(x, y)
		grow(x+w, y+h)
		shapes = append(shapes, shape{rect: true, col: def, pts: []float64{x, y, w, h}, area: w * h, idx: len(shapes)})
	}
	polys, _ := m["polys"].([]any)
	for _, it := range polys {
		p, _ := it.(map[string]any)
		flat, _ := p["points"].([]any)
		if len(flat) < 6 {
			continue
		}
		pts := make([]float64, 0, len(flat))
		for i := 0; i+1 < len(flat); i += 2 {
			x, ok1 := num(flat[i])
			y, ok2 := num(flat[i+1])
			if ok1 && ok2 {
				pts = append(pts, x, y)
			}
		}
		if len(pts) < 6 {
			continue
		}
		a := 0.0
		for i := 0; i < len(pts); i += 2 {
			j := (i + 2) % len(pts)
			a += pts[i]*pts[j+1] - pts[j]*pts[i+1]
		}
		for i := 0; i < len(pts); i += 2 {
			grow(pts[i], pts[i+1])
		}
		shapes = append(shapes, shape{col: polyColour(p, def), pts: pts, area: math.Abs(a) / 2, idx: len(shapes)})
	}
	// Editor maps (no "world") and ported maps with floor_visible stand on the
	// game's baseline floor.
	floorY, floorW := 0.0, 0.0
	if wd, ok := m["world"].(map[string]any); ok {
		if fv, _ := wd["floor_visible"].(bool); fv {
			floorY, _ = num(wd["ground_y"])
			floorW, _ = num(wd["w"])
		}
	} else {
		floorY, floorW = 1900, 4800
	}
	if floorW > 0 && floorY > 0 {
		grow(0, floorY)
		grow(floorW, floorY+100)
		shapes = append(shapes, shape{rect: true, col: def, pts: []float64{0, floorY, floorW, 100}, area: floorW * 100, idx: len(shapes)})
	}
	if len(shapes) == 0 || maxX-minX < 1 || maxY-minY < 1 {
		return nil
	}
	// Objective markers widen the box (a flag stands above the ground it's
	// on) unless they're far outside the geometry (bad data).
	type marker struct {
		x, y float64
		kind int
	}
	var marks []marker
	for i, f := range vecList(m["ctf_flags"]) {
		if i < 2 {
			marks = append(marks, marker{f[0], f[1], i})
		}
	}
	for _, k := range []string{"inf_flag", "htf_flag"} {
		if x, y, ok := vec(m[k]); ok {
			marks = append(marks, marker{x, y, 2})
			break
		}
	}
	for _, d := range vecList(m["dom_points"]) {
		marks = append(marks, marker{d[0], d[1], 3})
	}
	ex, ey := (maxX-minX)*0.25, (maxY-minY)*0.25
	kept := marks[:0]
	for _, mk := range marks {
		if mk.x >= minX-ex && mk.x <= maxX+ex && mk.y >= minY-ey && mk.y <= maxY+ey {
			grow(mk.x, mk.y-60)
			grow(mk.x, mk.y)
			kept = append(kept, mk)
		}
	}
	padX, padY := (maxX-minX)*0.02, (maxY-minY)*0.02
	minX, minY, maxX, maxY = minX-padX, minY-padY, maxX+padX, maxY+padY
	sc := math.Min(thumbW/(maxX-minX), thumbH/(maxY-minY))
	t := &Thumb{W: int(math.Ceil((maxX - minX) * sc)), H: int(math.Ceil((maxY - minY) * sc)), C: def}
	q := func(v, o float64) int { return int(math.Round((v - o) * sc)) }

	// Budget: biggest shapes first, then back to file order.
	order := make([]int, len(shapes))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return shapes[order[a]].area > shapes[order[b]].area })
	keep := make([]bool, len(shapes))
	nPolys, nPts := 0, 0
	for _, i := range order {
		s := shapes[i]
		if s.area*sc*sc < 0.5 {
			continue // under half a thumbnail pixel
		}
		if !s.rect {
			if nPolys >= maxThumbPolys || nPts+len(s.pts)/2 > maxThumbPoints {
				continue
			}
			nPolys++
			nPts += len(s.pts) / 2
		}
		keep[i] = true
	}
	for i, s := range shapes {
		if !keep[i] {
			continue
		}
		if s.rect {
			w := int(math.Max(1, math.Round(s.pts[2]*sc)))
			h := int(math.Max(1, math.Round(s.pts[3]*sc)))
			t.R = append(t.R, [4]int{q(s.pts[0], minX), q(s.pts[1], minY), w, h})
			continue
		}
		row := []int{s.col}
		lx, ly := math.MinInt, math.MinInt
		for k := 0; k < len(s.pts); k += 2 {
			x, y := q(s.pts[k], minX), q(s.pts[k+1], minY)
			if x == lx && y == ly {
				continue
			}
			row = append(row, x, y)
			lx, ly = x, y
		}
		if len(row) >= 7 {
			t.P = append(t.P, row)
		}
	}
	for _, mk := range kept {
		t.F = append(t.F, [3]int{q(mk.x, minX), q(mk.y, minY), mk.kind})
	}
	return t
}

// fillLevel computes the derived level fields from the stored map JSON
// (upload, and the one-time migration of maps stored before #288).
func fillLevel(e *MapEntry, m map[string]any) {
	e.Modes = modeHints(m)
	e.Thumb = buildThumb(m)
	pls, _ := m["platforms"].([]any)
	e.Platforms = len(pls)
	e.V = levelRecordV
}

// ── Scores ──────────────────────────────────────────────────────────────────

// hotScore: plays + 3x likes per day over the last week, each day worth half
// as much every two days back, plus a short boost for brand-new levels so
// they get seen at all.
func hotScore(e *MapEntry, now time.Time) float64 {
	today := dayOf(now)
	s := 0.0
	for d := 0; d < hotDays; d++ {
		w := math.Pow(0.5, float64(d)/2.0)
		s += w * (float64(e.PlayDays[today-d]) + 3*float64(e.LikeDays[today-d]))
	}
	if age := now.Sub(time.UnixMilli(e.UpMS)).Hours() / 24; age >= 0 && age < 3 {
		s += 2 * math.Pow(0.5, age)
	}
	return math.Round(s*1000) / 1000
}

func pruneDays(m map[int]int, today int) {
	for d := range m {
		if d <= today-keepDays {
			delete(m, d)
		}
	}
}

func (e *MapEntry) view(pid string, now time.Time, thumb bool) LevelView {
	v := LevelView{ID: e.ID, Code: e.Code, Title: e.Name, Name: e.Name, Author: e.Author,
		Created: e.UpMS, UpMS: e.UpMS, Size: e.Size, Platforms: e.Platforms, Modes: e.Modes,
		Theme: e.Theme, Plays: e.Plays, Likes: e.Likes, Downloads: e.Downloads, Hot: hotScore(e, now)}
	if pid != "" {
		_, v.Liked = e.Likers[shortHash(pid)]
	}
	if thumb {
		v.Thumb = e.Thumb
	}
	return v
}

// sortLevels orders views for a browser tab; ties go to the newest.
func sortLevels(list []LevelView, by string) {
	key := func(v LevelView) float64 {
		switch by {
		case "hot":
			return v.Hot
		case "plays":
			return float64(v.Plays)
		case "likes":
			return float64(v.Likes)
		}
		return float64(v.UpMS)
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := key(list[i]), key(list[j])
		if a != b {
			return a > b
		}
		if list[i].UpMS != list[j].UpMS {
			return list[i].UpMS > list[j].UpMS
		}
		return list[i].ID < list[j].ID
	})
}

// levelMatches: title / author substring, or the share code (any spelling).
func levelMatches(e *MapEntry, q, code string) bool {
	if code != "" && normCode(e.Code) == code {
		return true
	}
	return strings.Contains(strings.ToLower(e.Name+"\n"+e.Author), q)
}

// ── Handlers ────────────────────────────────────────────────────────────────

func queryPid(r *http.Request) string {
	p := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("pid")))
	if pidRe.MatchString(p) {
		return p
	}
	return ""
}

func handleLevelList(w http.ResponseWriter, r *http.Request) {
	if !levelReadsPerIP.allow(sourceIP(r)) {
		http.Error(w, "too many requests, slow down", http.StatusTooManyRequests)
		return
	}
	qs := r.URL.Query()
	q := strings.ToLower(strings.TrimSpace(qs.Get("q")))
	if len(q) > 64 {
		q = q[:64]
	}
	code := normCode(q)
	by := qs.Get("sort")
	switch by {
	case "hot", "new", "plays", "likes":
	default:
		by = "hot"
	}
	page := clampInt(atoiOr(qs.Get("page"), 0), 0, maxLevelPages)
	per := clampInt(atoiOr(qs.Get("per"), levelsPerPage), 1, levelsPerPageMx)
	pid := queryPid(r)
	now := time.Now()
	var ids []string
	if raw := strings.TrimSpace(qs.Get("ids")); raw != "" {
		for _, id := range strings.Split(strings.ToLower(raw), ",") {
			if mapIDRe.MatchString(id) && len(ids) < levelsPerPageMx {
				ids = append(ids, id)
			}
		}
	}

	list := []LevelView{}
	mapsMu.Lock()
	if ids != nil {
		// Bookmarks: exactly these levels, in the order asked.
		for _, id := range ids {
			if e, ok := mapIndex[id]; ok {
				list = append(list, e.view(pid, now, true))
			}
		}
	} else {
		for _, e := range mapIndex {
			if q == "" || levelMatches(e, q, code) {
				list = append(list, e.view(pid, now, false))
			}
		}
	}
	mapsMu.Unlock()
	total := len(list)
	if ids == nil {
		sortLevels(list, by)
		// An exact code hit goes first whatever the sort.
		if code != "" {
			for i, v := range list {
				if normCode(v.Code) == code {
					copy(list[1:i+1], list[:i])
					list[0] = v
					break
				}
			}
		}
		lo := page * per
		if lo > len(list) {
			lo = len(list)
		}
		hi := lo + per
		if hi > len(list) {
			hi = len(list)
		}
		list = list[lo:hi]
		mapsMu.Lock()
		for i := range list {
			if e, ok := mapIndex[list[i].ID]; ok {
				list[i].Thumb = e.Thumb
			}
		}
		mapsMu.Unlock()
	}
	writeJSON(w, map[string]any{"levels": list, "page": page, "per": per, "total": total,
		"more": ids == nil && (page+1)*per < total, "sort": by})
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func handleLevelGet(w http.ResponseWriter, r *http.Request) {
	if !levelReadsPerIP.allow(sourceIP(r)) {
		http.Error(w, "too many requests, slow down", http.StatusTooManyRequests)
		return
	}
	qs := r.URL.Query()
	id := strings.ToLower(strings.TrimSpace(qs.Get("id")))
	mapsMu.Lock()
	if id == "" {
		if c := normCode(qs.Get("code")); c != "" {
			id = codeIndex[c]
		}
	}
	e, ok := mapIndex[id]
	var v LevelView
	if ok {
		e.Downloads++
		mapsDirty = true
		v = e.view(queryPid(r), time.Now(), true)
	}
	mapsMu.Unlock()
	if !ok || !mapIDRe.MatchString(id) {
		http.Error(w, "no level with that id or code", http.StatusNotFound)
		return
	}
	body, err := readMapFile(id)
	if err != nil {
		http.Error(w, "no level with that id or code", http.StatusNotFound)
		return
	}
	head, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = w.Write([]byte(`{"level":`))
	_, _ = w.Write(head)
	_, _ = w.Write([]byte(`,"map":`))
	_, _ = w.Write(body)
	_, _ = w.Write([]byte("}\n"))
}

type markReq struct {
	ID   string `json:"id"`
	Pid  string `json:"pid"`
	Like *bool  `json:"like"`
}

func readMarkReq(w http.ResponseWriter, r *http.Request) (markReq, bool) {
	var req markReq
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return req, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return req, false
	}
	req.ID = strings.ToLower(strings.TrimSpace(req.ID))
	req.Pid = strings.ToLower(strings.TrimSpace(req.Pid))
	if !mapIDRe.MatchString(req.ID) || !pidRe.MatchString(req.Pid) {
		http.Error(w, "bad id or pid", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// likeLevel sets / clears pid's like (idempotent). err is "" or a reason.
func likeLevel(id, pid, ip string, like bool, now time.Time) (likes int, liked bool, status int, err string) {
	mapsMu.Lock()
	defer mapsMu.Unlock()
	e, ok := mapIndex[id]
	if !ok {
		return 0, false, http.StatusNotFound, "no such level"
	}
	key, ipk, today := shortHash(pid), shortHash("ip:" + ip)[:8], dayOf(now)
	cur, has := e.Likers[key]
	switch {
	case like && !has:
		n := 0
		for _, mk := range e.Likers {
			if mk.IP == ipk {
				n++
			}
		}
		if n >= maxMarksPerIP {
			return e.Likes, false, http.StatusTooManyRequests, "too many likes from this address"
		}
		if e.Likers == nil {
			e.Likers = map[string]Mark{}
		}
		if e.LikeDays == nil {
			e.LikeDays = map[int]int{}
		}
		e.Likers[key] = Mark{D: today, IP: ipk}
		e.Likes++
		e.LikeDays[today]++
		pruneDays(e.LikeDays, today)
		mapsDirty = true
	case !like && has:
		delete(e.Likers, key)
		if e.Likes > 0 {
			e.Likes--
		}
		if e.LikeDays[cur.D] > 0 {
			e.LikeDays[cur.D]--
		}
		mapsDirty = true
	}
	_, liked = e.Likers[key]
	return e.Likes, liked, http.StatusOK, ""
}

// playLevel counts pid's play once per level per UTC day.
func playLevel(id, pid, ip string, now time.Time) (plays int, counted bool, ok bool) {
	mapsMu.Lock()
	defer mapsMu.Unlock()
	e, found := mapIndex[id]
	if !found {
		return 0, false, false
	}
	key, ipk, today := shortHash(pid), shortHash("ip:" + ip)[:8], dayOf(now)
	n := 0
	for k, mk := range e.Played {
		if mk.D != today {
			delete(e.Played, k) // only today's plays matter for the once-a-day rule
		} else if mk.IP == ipk {
			n++
		}
	}
	if mk, has := e.Played[key]; (has && mk.D == today) || n >= maxMarksPerIP {
		return e.Plays, false, true
	}
	if e.Played == nil {
		e.Played = map[string]Mark{}
	}
	if e.PlayDays == nil {
		e.PlayDays = map[int]int{}
	}
	e.Played[key] = Mark{D: today, IP: ipk}
	e.Plays++
	e.PlayDays[today]++
	pruneDays(e.PlayDays, today)
	mapsDirty = true
	return e.Plays, true, true
}

func handleLevelLike(w http.ResponseWriter, r *http.Request) {
	ip := sourceIP(r)
	if !levelLikesPerIP.allow(ip) {
		http.Error(w, "too many likes, slow down", http.StatusTooManyRequests)
		return
	}
	req, ok := readMarkReq(w, r)
	if !ok {
		return
	}
	like := req.Like == nil || *req.Like
	likes, liked, code, msg := likeLevel(req.ID, req.Pid, ip, like, time.Now())
	if msg != "" {
		http.Error(w, msg, code)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": req.ID, "likes": likes, "liked": liked})
}

func handleLevelPlay(w http.ResponseWriter, r *http.Request) {
	ip := sourceIP(r)
	if !levelPlaysPerIP.allow(ip) {
		http.Error(w, "too many play reports, slow down", http.StatusTooManyRequests)
		return
	}
	req, ok := readMarkReq(w, r)
	if !ok {
		return
	}
	plays, counted, found := playLevel(req.ID, req.Pid, ip, time.Now())
	if !found {
		http.Error(w, "no such level", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": req.ID, "plays": plays, "counted": counted})
}
