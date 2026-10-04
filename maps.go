package main

// Shared maps: players upload maps from the in-game editor and browse /
// download everyone else's. Stored as files next to the binary (maps/).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

type MapEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Author    string `json:"author"`
	Size      int    `json:"size"`
	Platforms int    `json:"platforms"`
	Modes     string `json:"modes"` // "ctf inf htf dom" objectives present
	UpMS      int64  `json:"uploaded_ms"`
	Downloads int    `json:"downloads"`
}

type uploadReq struct {
	Name   string          `json:"name"`
	Author string          `json:"author"`
	Map    json.RawMessage `json:"map"`
}

var (
	mapsMu      sync.Mutex
	mapIndex    = map[string]*MapEntry{}
	mapsDir     string
	mapsDirty   bool
	uploadsByIP = map[string][]time.Time{}
	mapIDRe     = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

const (
	maxMapBytes   = 512 << 10
	maxMaps       = 5000
	uploadsPerDay = 20
)

func loadMaps() {
	mapsDir = filepath.Join(dataDir(), "maps")
	_ = os.MkdirAll(mapsDir, 0o755)
	b, err := os.ReadFile(filepath.Join(mapsDir, "index.json"))
	if err == nil {
		var list []*MapEntry
		if json.Unmarshal(b, &list) == nil {
			for _, e := range list {
				if e != nil && mapIDRe.MatchString(e.ID) {
					mapIndex[e.ID] = e
				}
			}
		}
	}
	log.Printf("maps: %d shared maps in %s", len(mapIndex), mapsDir)
	go func() {
		for {
			time.Sleep(30 * time.Second)
			saveMapIndex()
		}
	}()
}

func saveMapIndex() {
	mapsMu.Lock()
	if !mapsDirty {
		mapsMu.Unlock()
		return
	}
	list := make([]*MapEntry, 0, len(mapIndex))
	for _, e := range mapIndex {
		list = append(list, e)
	}
	b, _ := json.Marshal(list)
	mapsDirty = false
	mapsMu.Unlock()
	p := filepath.Join(mapsDir, "index.json")
	if os.WriteFile(p+".tmp", b, 0o644) == nil {
		_ = os.Rename(p+".tmp", p)
	}
}

func cleanText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '[' || r == ']' {
			return -1
		}
		return r
	}, s)
	return clip(strings.TrimSpace(s), n)
}

func handleMapUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := sourceIP(r)
	now := time.Now()
	mapsMu.Lock()
	recent := uploadsByIP[ip][:0]
	for _, t := range uploadsByIP[ip] {
		if now.Sub(t) < 24*time.Hour {
			recent = append(recent, t)
		}
	}
	uploadsByIP[ip] = recent
	if len(recent) >= uploadsPerDay {
		mapsMu.Unlock()
		http.Error(w, "upload limit reached, try tomorrow", http.StatusTooManyRequests)
		return
	}
	mapsMu.Unlock()

	var req uploadReq
	r.Body = http.MaxBytesReader(w, r.Body, maxMapBytes+8<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json or map too big", http.StatusBadRequest)
		return
	}
	var m map[string]any
	if len(req.Map) == 0 || len(req.Map) > maxMapBytes || json.Unmarshal(req.Map, &m) != nil {
		http.Error(w, "map must be a JSON object under 512 KB", http.StatusBadRequest)
		return
	}
	pls, _ := m["platforms"].([]any)
	if len(pls) < 1 || len(pls) > 20000 {
		http.Error(w, "map needs at least one platform", http.StatusBadRequest)
		return
	}
	name := cleanText(req.Name, 32)
	if name == "" {
		name = cleanText(func() string { s, _ := m["name"].(string); return s }(), 32)
	}
	if name == "" {
		name = "Untitled"
	}
	m["name"] = name
	body, _ := json.Marshal(m)
	sum := sha256.Sum256(body)
	id := hex.EncodeToString(sum[:])[:16]
	modes := []string{}
	for _, k := range []string{"ctf_flags", "inf_flag", "htf_flag", "dom_points"} {
		if v, ok := m[k]; ok && v != nil {
			if a, isArr := v.([]any); !isArr || len(a) > 0 {
				modes = append(modes, strings.SplitN(k, "_", 2)[0])
			}
		}
	}

	mapsMu.Lock()
	defer mapsMu.Unlock()
	if e, ok := mapIndex[id]; ok {
		writeJSON(w, map[string]any{"ok": true, "id": id, "existing": true, "name": e.Name})
		return
	}
	if len(mapIndex) >= maxMaps {
		http.Error(w, "map library is full", http.StatusInsufficientStorage)
		return
	}
	if err := os.WriteFile(filepath.Join(mapsDir, id+".json"), body, 0o644); err != nil {
		http.Error(w, "could not store map", http.StatusInternalServerError)
		return
	}
	mapIndex[id] = &MapEntry{ID: id, Name: name, Author: cleanText(req.Author, 24), Size: len(body),
		Platforms: len(pls), Modes: strings.Join(modes, " "), UpMS: now.UnixMilli()}
	uploadsByIP[ip] = append(uploadsByIP[ip], now)
	mapsDirty = true
	writeJSON(w, map[string]any{"ok": true, "id": id})
}

func handleMapList(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	sortBy := r.URL.Query().Get("sort")
	mapsMu.Lock()
	list := make([]MapEntry, 0, len(mapIndex))
	for _, e := range mapIndex {
		if q != "" && !strings.Contains(strings.ToLower(e.Name+" "+e.Author), q) {
			continue
		}
		list = append(list, *e)
	}
	mapsMu.Unlock()
	sort.Slice(list, func(i, j int) bool {
		if sortBy == "top" && list[i].Downloads != list[j].Downloads {
			return list[i].Downloads > list[j].Downloads
		}
		return list[i].UpMS > list[j].UpMS
	})
	if len(list) > 100 {
		list = list[:100]
	}
	writeJSON(w, map[string]any{"maps": list})
}

func handleMapGet(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.URL.Query().Get("id"))
	if !mapIDRe.MatchString(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	mapsMu.Lock()
	e, ok := mapIndex[id]
	if ok {
		e.Downloads++
		mapsDirty = true
	}
	mapsMu.Unlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	b, err := os.ReadFile(filepath.Join(mapsDir, id+".json"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}
