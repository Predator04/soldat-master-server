package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetNames(t *testing.T) {
	t.Helper()
	namesMu.Lock()
	namesByKey = map[string]*Reservation{}
	namesByOwn = map[string]*Reservation{}
	namesDirty = false
	namesFile = filepath.Join(t.TempDir(), "names.json")
	namesMu.Unlock()
	nameReadsPerIP = newIPLimiter(240, time.Minute)
	nameWritesPerIP = newIPLimiter(10, time.Minute)
}

func rawID(n int) string { return strings.Repeat("0", 31) + string("0123456789abcdef"[n%16]) }

func reserve(t *testing.T, id, name, ip string) (int, map[string]any) {
	t.Helper()
	rec, m := call(t, handleNameReserve, "POST", "/names/reserve", map[string]any{"id": id, "name": name}, ip)
	return rec.Code, m
}

func check(t *testing.T, name, pid string) map[string]any {
	t.Helper()
	u := "/names/check?name=" + url.QueryEscape(name)
	if pid != "" {
		u += "&pid=" + pid
	}
	_, m := call(t, handleNameCheck, "GET", u, nil, "")
	return m
}

func TestNameKeyNormalisation(t *testing.T) {
	for _, c := range [][2]string{
		{"Bob", "bob"}, {"B0B", "bob"}, {"b.o.b", "bob"}, {"«ABC» Bob", "bob"},
		{"Sn1p3r", "sniper"}, {"Zoë", "zoe"},
	} {
		if got := nameKey(c[0]); got != c[1] {
			t.Errorf("nameKey(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestReserveCheckAndImpostor(t *testing.T) {
	resetNames(t)
	alice, mallory := rawID(1), rawID(2)
	if code, m := reserve(t, alice, "Alice", ""); code != 200 || m["status"] != "reserved" || m["name"] != "Alice" {
		t.Fatalf("reserve: %d %v", code, m)
	}
	m := check(t, "4L1CE", "")
	if m["reserved"] != true || m["owner"] != profileHash(alice) || m["display"] != "Alice" {
		t.Fatalf("check leet spelling: %v", m)
	}
	if st := check(t, "alice", profileHash(alice))["status"]; st != "mine" {
		t.Fatalf("owner status %v", st)
	}
	if st := check(t, "ALICE", profileHash(mallory))["status"]; st != "taken" {
		t.Fatalf("impostor status %v", st)
	}
	if st := check(t, "Nobody", profileHash(mallory))["status"]; st != "free" {
		t.Fatalf("free status %v", st)
	}
	// The hash a game server computes from the id the player sends.
	if profileHash(strings.ToUpper(alice)) != profileHash(alice) {
		t.Fatal("hash must ignore case")
	}
	if code, m := reserve(t, mallory, "a.l.i.c.e", ""); code != http.StatusConflict || m["status"] != "taken" || m["taken_by"] != "Alice" {
		t.Fatalf("second owner: %d %v", code, m)
	}
	// Owner re-spelling the same name: no cooldown.
	if code, m := reserve(t, alice, "ALICE", ""); code != 200 || m["name"] != "ALICE" {
		t.Fatalf("respell: %d %v", code, m)
	}
}

func TestOneNamePerProfileAndCooldown(t *testing.T) {
	resetNames(t)
	id := rawID(3)
	now := time.Now()
	if r := reserveName(profileHash(id), "First", "198.51.100.1", now); r.Status != "reserved" {
		t.Fatal(r)
	}
	if r := reserveName(profileHash(id), "Second", "198.51.100.1", now.Add(time.Hour)); r.Status != "cooldown" || r.WaitS <= 0 {
		t.Fatalf("change inside the cooldown: %+v", r)
	}
	if r := reserveName(profileHash(id), "Second", "198.51.100.1", now.Add(nameCooldown+time.Minute)); r.Status != "reserved" {
		t.Fatalf("change after the cooldown: %+v", r)
	}
	if check(t, "First", "")["reserved"] != false {
		t.Fatal("old name must be free after a change")
	}
	if check(t, "Second", "")["reserved"] != true {
		t.Fatal("new name reserved")
	}
	// Release, then a new name waits out the cooldown again.
	t2 := now.Add(nameCooldown + 2*time.Minute)
	if r := reserveName(profileHash(id), "", "198.51.100.1", t2); r.Status != "released" {
		t.Fatal(r)
	}
	if check(t, "Second", "")["reserved"] != false {
		t.Fatal("released name must be free")
	}
	if r := reserveName(profileHash(id), "Third", "198.51.100.1", t2.Add(time.Minute)); r.Status != "cooldown" {
		t.Fatalf("release must not skip the cooldown: %+v", r)
	}
}

func TestReserveRefusals(t *testing.T) {
	resetNames(t)
	for name, want := range map[string]string{
		"Sh1tlord": "offensive", "fuck you": "offensive", "ab": "short", "12": "short",
		"Player1234": "blocked", "SERVER": "blocked", "Bot 12": "blocked", "Blue Bot 3": "blocked",
	} {
		if code, m := reserve(t, rawID(4), name, ""); code != http.StatusBadRequest || m["status"] != want {
			t.Errorf("%q: %d %v (want %s)", name, code, m["status"], want)
		}
	}
	// Ordinary names that only look like the blocked ones.
	for i, name := range []string{"Botswana", "Cockpit Ace", "Hoedown"} {
		_ = name
		if r := reserveName(profileHash(rawID(5+i)), name, "203.0.113.9", time.Now()); r.Status == "blocked" || r.Status == "offensive" {
			t.Errorf("%q refused: %+v", name, r)
		}
	}
	// Bad ids / methods.
	if rec, _ := call(t, handleNameReserve, "POST", "/names/reserve", map[string]any{"id": "nothex", "name": "Zed"}, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", rec.Code)
	}
	if rec, _ := call(t, handleNameReserve, "GET", "/names/reserve", nil, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET reserve: %d", rec.Code)
	}
}

func TestNameAbuseLimits(t *testing.T) {
	resetNames(t)
	ip := "203.0.113.50"
	now := time.Now()
	for i := 0; i < maxNamesPerIP; i++ {
		if r := reserveName(profileHash(rawID(i)), "Squat"+strings.Repeat("x", i+1), ip, now); r.Status != "reserved" {
			t.Fatalf("reservation %d: %+v", i, r)
		}
	}
	if r := reserveName(profileHash(rawID(9)), "Squatmore", ip, now); r.Status != "limit" {
		t.Fatalf("per-address cap: %+v", r)
	}
	if r := reserveName(profileHash(rawID(9)), "Squatmore", "203.0.113.51", now); r.Status != "reserved" {
		t.Fatalf("other address: %+v", r)
	}
	// Write rate limit per address.
	resetNames(t)
	got429 := false
	for i := 0; i < 12; i++ {
		rec, _ := call(t, handleNameReserve, "POST", "/names/reserve", map[string]any{"id": rawID(1), "name": "Rate"}, "198.51.100.77")
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
		}
	}
	if !got429 {
		t.Fatal("no rate limit on /names/reserve")
	}
}

func TestNamesPersistAndMine(t *testing.T) {
	resetNames(t)
	id := rawID(7)
	if code, _ := reserve(t, id, "Keeper", ""); code != 200 {
		t.Fatal(code)
	}
	rec, m := call(t, handleNameMine, "POST", "/names/mine", map[string]any{"id": id}, "")
	if rec.Code != 200 || m["name"] != "Keeper" || m["owner"] != profileHash(id) {
		t.Fatalf("mine: %d %v", rec.Code, m)
	}
	if cd, _ := m["cooldown_s"].(float64); cd <= 0 {
		t.Fatalf("mine cooldown: %v", m)
	}
	saveNames()
	if _, err := os.Stat(namesFile); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(namesFile)
	if strings.Contains(string(b), id) {
		t.Fatal("raw profile id must never be stored")
	}
	f := namesFile
	namesMu.Lock()
	namesByKey = map[string]*Reservation{}
	namesByOwn = map[string]*Reservation{}
	namesMu.Unlock()
	t.Setenv("SOLDAT_DATA_DIR", filepath.Dir(f))
	loadNames()
	if check(t, "keeper", "")["owner"] != profileHash(id) {
		t.Fatal("reservation lost on reload")
	}
}
