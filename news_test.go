package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setNewsFiles points /news at fresh temp files for one test.
func setNewsFiles(t *testing.T) (news, featured string) {
	t.Helper()
	dir := t.TempDir()
	news = filepath.Join(dir, "news.json")
	featured = filepath.Join(dir, "featured.json")
	newsOnce.Do(func() {})
	newsFile = &watchedFile{path: news}
	featuredFile = &watchedFile{path: featured}
	return
}

func writeFile(t *testing.T, p, s string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	// Distinct mtimes so the change check sees every rewrite.
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func getNews(t *testing.T) (map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	handleNews(rec, httptest.NewRequest(http.MethodGet, "/news", nil))
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad json %q: %v", rec.Body.String(), err)
	}
	return m, rec
}

func TestNewsMissingFileIsEmpty(t *testing.T) {
	setNewsFiles(t)
	m, rec := getNews(t)
	if len(m) != 0 {
		t.Fatalf("want {}, got %v", m)
	}
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "max-age=") {
		t.Fatalf("code %d cache %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

func TestNewsServesAndReloads(t *testing.T) {
	news, _ := setNewsFiles(t)
	base := time.Now().Add(-time.Hour)
	writeFile(t, news, `{"text":"Hello\u0007 [b]world[/b]\n","url":"https://example.com/x","until_ms":0}`, base)
	m, _ := getNews(t)
	if m["text"] != "Hello bworld/b" || m["url"] != "https://example.com/x" {
		t.Fatalf("got %v", m)
	}
	// Raw control bytes from a hand edit don't invalidate the file.
	writeFile(t, news, "{\"text\":\"Tab\there\x07\nnow\"}", base.Add(30*time.Second))
	if m, _ = getNews(t); m["text"] != "Tab here now" {
		t.Fatalf("raw control bytes: %v", m)
	}
	if _, ok := m["until_ms"]; ok {
		t.Fatalf("until_ms 0 should be omitted: %v", m)
	}
	// Edited on disk -> picked up without a restart.
	long := strings.Repeat("x", 300)
	writeFile(t, news, `{"text":"`+long+`","url":"javascript:alert(1)"}`, base.Add(time.Minute))
	m, _ = getNews(t)
	if len([]rune(m["text"].(string))) != newsMaxText {
		t.Fatalf("text not capped: %d", len(m["text"].(string)))
	}
	if _, ok := m["url"]; ok {
		t.Fatalf("non-http url kept: %v", m)
	}
}

func TestNewsExpiredAndInvalid(t *testing.T) {
	news, _ := setNewsFiles(t)
	base := time.Now().Add(-time.Hour)
	past := time.Now().Add(-time.Minute).UnixMilli()
	writeFile(t, news, `{"text":"old","until_ms":`+itoa(past)+`}`, base)
	if m, _ := getNews(t); len(m) != 0 {
		t.Fatalf("expired served: %v", m)
	}
	future := time.Now().Add(time.Hour).UnixMilli()
	writeFile(t, news, `{"text":"new","until_ms":`+itoa(future)+`}`, base.Add(time.Minute))
	if m, _ := getNews(t); m["text"] != "new" || int64(m["until_ms"].(float64)) != future {
		t.Fatalf("future not served: %v", m)
	}
	writeFile(t, news, `{not json`, base.Add(2*time.Minute))
	if m, _ := getNews(t); len(m) != 0 {
		t.Fatalf("invalid served: %v", m)
	}
}

func TestNewsFallsBackToFeatured(t *testing.T) {
	news, featured := setNewsFiles(t)
	base := time.Now().Add(-time.Hour)
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02")
	writeFile(t, featured, `{"mode":"rm","map":"Voland","until":"`+tomorrow+`"}`, base)
	m, _ := getNews(t)
	if m["text"] != "This week on the official server: Rambomatch on Voland" {
		t.Fatalf("featured fallback: %v", m)
	}
	// A note replaces the generated line.
	writeFile(t, featured, `{"mode":"ctf","map":"Ash","note":"Knife-only CTF week!","until":0}`, base.Add(time.Minute))
	if m, _ := getNews(t); m["text"] != "Knife-only CTF week!" {
		t.Fatalf("featured note: %v", m)
	}
	// Explicit news wins over featured.
	writeFile(t, news, `{"text":"Server maintenance at 18:00 UTC"}`, base)
	if m, _ := getNews(t); m["text"] != "Server maintenance at 18:00 UTC" {
		t.Fatalf("news should win: %v", m)
	}
	// Expired news -> featured again; expired featured -> nothing.
	writeFile(t, news, `{"text":"gone","until_ms":1}`, base.Add(time.Minute))
	writeFile(t, featured, `{"mode":"ctf","map":"Ash","until":"2001-01-01"}`, base.Add(2*time.Minute))
	if m, _ := getNews(t); len(m) != 0 {
		t.Fatalf("all expired should be empty: %v", m)
	}
}

func TestParseUntil(t *testing.T) {
	cases := map[string]int64{`0`: 0, `null`: 0, `""`: 0, `1760000000000`: 1760000000000, `"1760000000000"`: 1760000000000, `"bogus"`: -1}
	for in, want := range cases {
		if got := parseUntil(json.RawMessage(in)); got != want {
			t.Errorf("parseUntil(%s)=%d want %d", in, got, want)
		}
	}
	d := parseUntil(json.RawMessage(`"2026-10-26"`))
	if d != time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC).UnixMilli() {
		t.Errorf("date until %d", d)
	}
}

func TestNewsMethod(t *testing.T) {
	setNewsFiles(t)
	rec := httptest.NewRecorder()
	handleNews(rec, httptest.NewRequest(http.MethodPost, "/news", strings.NewReader("x")))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST code %d", rec.Code)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
