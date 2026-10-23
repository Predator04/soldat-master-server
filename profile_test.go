package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetCloud(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SOLDAT_DATA_DIR", dir)
	loadCloud()
	cloudReadsPerIP = newIPLimiter(30, time.Minute)
	cloudWritesPerIP = newIPLimiter(12, time.Minute)
	cloudNewPerIP = newIPLimiter(cloudNewPerIPDay, 24*time.Hour)
}

func cloudK(code string) string {
	s := sha256.Sum256([]byte("tracerfall-cloud-id:" + code))
	return hex.EncodeToString(s[:])
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestCloudPutGetConflict(t *testing.T) {
	resetCloud(t)
	k := cloudK("ABCD-EFGH")
	rec, m := call(t, handleCloudGet, "POST", "/cloud/get", map[string]any{"k": k}, "")
	if rec.Code != http.StatusNotFound || m["status"] != "none" {
		t.Fatalf("get before put: %d %v", rec.Code, m)
	}
	rec, m = call(t, handleCloudPut, "POST", "/cloud/put", map[string]any{"k": k, "base": 0, "fmt": 1, "blob": b64("one")}, "")
	if rec.Code != 200 || m["ver"].(float64) != 1 {
		t.Fatalf("first put: %d %v", rec.Code, m)
	}
	rec, m = call(t, handleCloudPut, "POST", "/cloud/put", map[string]any{"k": k, "base": 1, "fmt": 1, "blob": b64("two")}, "")
	if rec.Code != 200 || m["ver"].(float64) != 2 {
		t.Fatalf("second put: %d %v", rec.Code, m)
	}
	// A device that last saw version 1 must merge first.
	rec, m = call(t, handleCloudPut, "POST", "/cloud/put", map[string]any{"k": k, "base": 1, "fmt": 1, "blob": b64("stale")}, "")
	if rec.Code != http.StatusConflict || m["status"] != "conflict" || m["ver"].(float64) != 2 || m["blob"] != b64("two") {
		t.Fatalf("stale put: %d %v", rec.Code, m)
	}
	rec, m = call(t, handleCloudGet, "POST", "/cloud/get", map[string]any{"k": k}, "")
	if rec.Code != 200 || m["blob"] != b64("two") || m["ver"].(float64) != 2 || m["fmt"].(float64) != 1 {
		t.Fatalf("get: %d %v", rec.Code, m)
	}
	// The file name is a hash of k, never k itself.
	ents, _ := os.ReadDir(cloudDir)
	for _, e := range ents {
		if strings.Contains(e.Name(), k) {
			t.Fatalf("save stored under its address: %s", e.Name())
		}
	}
	if cloudCount != 1 {
		t.Fatalf("count %d", cloudCount)
	}
}

func TestCloudLimits(t *testing.T) {
	resetCloud(t)
	k := cloudK("BIG")
	big := strings.Repeat("A", cloudMaxBlob+4)
	rec, m := call(t, handleCloudPut, "POST", "/cloud/put", map[string]any{"k": k, "base": 0, "fmt": 1, "blob": big}, "")
	if rec.Code != http.StatusRequestEntityTooLarge || m["status"] != "big" {
		t.Fatalf("big: %d %v", rec.Code, m)
	}
	rec, _ = call(t, handleCloudPut, "POST", "/cloud/put", map[string]any{"k": "nothex", "base": 0, "blob": b64("x")}, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad k: %d", rec.Code)
	}
	rec, _ = call(t, handleCloudPut, "POST", "/cloud/put", map[string]any{"k": k, "base": 0, "blob": "not base64!"}, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad blob: %d", rec.Code)
	}
	// New saves per address per day.
	for i := 0; i < cloudNewPerIPDay+2; i++ {
		cloudWritesPerIP = newIPLimiter(1000, time.Minute)
		rec, m = call(t, handleCloudPut, "POST", "/cloud/put", map[string]any{"k": cloudK(string(rune('a' + i))), "base": 0, "blob": b64("x")}, "198.51.100.7")
		if i < cloudNewPerIPDay && rec.Code != 200 {
			t.Fatalf("save %d refused: %d %v", i, rec.Code, m)
		}
		if i >= cloudNewPerIPDay && (rec.Code != http.StatusTooManyRequests || m["status"] != "limit") {
			t.Fatalf("save %d over the daily cap: %d %v", i, rec.Code, m)
		}
	}
	// Writes per minute.
	cloudWritesPerIP = newIPLimiter(12, time.Minute)
	n429 := 0
	for i := 0; i < 15; i++ {
		rec, _ = call(t, handleCloudPut, "POST", "/cloud/put", map[string]any{"k": k, "base": int64(i), "blob": b64("x")}, "203.0.113.9")
		if rec.Code == http.StatusTooManyRequests {
			n429++
		}
	}
	if n429 != 3 {
		t.Fatalf("write rate limit: %d refused, want 3", n429)
	}
}

func TestCloudSurvivesRestart(t *testing.T) {
	resetCloud(t)
	k := cloudK("KEEP")
	call(t, handleCloudPut, "POST", "/cloud/put", map[string]any{"k": k, "base": 0, "fmt": 1, "blob": b64("kept")}, "")
	loadCloud() // same SOLDAT_DATA_DIR
	if cloudCount != 1 {
		t.Fatalf("count after reload %d", cloudCount)
	}
	_, m := call(t, handleCloudGet, "POST", "/cloud/get", map[string]any{"k": k}, "")
	if m["blob"] != b64("kept") {
		t.Fatalf("after reload: %v", m)
	}
	// Stored back-end lost the save: a device's put continues above its base.
	os.Remove(cloudPath(k))
	rec, m := call(t, handleCloudPut, "POST", "/cloud/put", map[string]any{"k": k, "base": 7, "fmt": 1, "blob": b64("again")}, "")
	if rec.Code != 200 || m["ver"].(float64) != 8 {
		t.Fatalf("put after loss: %d %v", rec.Code, m)
	}
}

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	setTicketKey(k)
	ticketsPerIP = newIPLimiter(60, time.Minute)
	return k
}

func TestTicketRoundTrip(t *testing.T) {
	k := testKey(t)
	id := strings.Repeat("ab", 16)
	nonce := strings.Repeat("0f", 16)
	rec, m := call(t, handleAuthTicket, "POST", "/auth/ticket", map[string]any{"id": id, "n": nonce, "srv": "1.2.3.4:7777"}, "")
	if rec.Code != 200 || m["pub"] != profileHash(id) {
		t.Fatalf("ticket: %d %v", rec.Code, m)
	}
	tk := m["ticket"].(string)
	if strings.Contains(tk, id) {
		t.Fatal("raw id inside the ticket")
	}
	p, err := verifyTicket(&k.PublicKey, tk, nonce, time.Now())
	if err != nil || p.Pub != profileHash(id) || p.Srv != "1.2.3.4:7777" || p.Exp-p.IAT != ticketTTL.Milliseconds() {
		t.Fatalf("verify: %v %+v", err, p)
	}
	// Another server's nonce: refused (a ticket can't be replayed elsewhere).
	if _, err := verifyTicket(&k.PublicKey, tk, strings.Repeat("1e", 16), time.Now()); err == nil || err.Error() != "nonce" {
		t.Fatalf("other nonce: %v", err)
	}
	// Tampered payload (someone else's hash): signature fails.
	parts := strings.Split(tk, ".")
	raw, _ := base64.StdEncoding.DecodeString(parts[0])
	forged := strings.Replace(string(raw), profileHash(id), profileHash(strings.Repeat("cd", 16)), 1)
	if _, err := verifyTicket(&k.PublicKey, b64(forged)+"."+parts[1], nonce, time.Now()); err == nil || err.Error() != "signature" {
		t.Fatalf("forged: %v", err)
	}
	// Signed by another key.
	other, _ := rsa.GenerateKey(rand.Reader, 1024)
	if _, err := verifyTicket(&other.PublicKey, tk, nonce, time.Now()); err == nil {
		t.Fatal("verified with the wrong key")
	}
	// Way past its expiry.
	if _, err := verifyTicket(&k.PublicKey, tk, nonce, time.Now().Add(time.Hour)); err == nil || err.Error() != "expired" {
		t.Fatalf("expired: %v", err)
	}
	// Bad inputs.
	for _, body := range []map[string]any{
		{"id": "short", "n": nonce},
		{"id": id, "n": "zz"},
		{"id": id, "n": strings.Repeat("a", 65)},
	} {
		if rec, _ := call(t, handleAuthTicket, "POST", "/auth/ticket", body, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad %v: %d", body, rec.Code)
		}
	}
}

func TestAuthKeyServesPublicPEM(t *testing.T) {
	k := testKey(t)
	authKeyReadsPerIP = newIPLimiter(60, time.Minute)
	rec, m := call(t, handleAuthKey, "GET", "/auth/key", nil, "")
	if rec.Code != 200 {
		t.Fatalf("key: %d", rec.Code)
	}
	p := m["pem"].(string)
	if strings.Contains(p, "PRIVATE") {
		t.Fatal("private key served")
	}
	blk, _ := pem.Decode([]byte(p))
	pk, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil || !pk.(*rsa.PublicKey).Equal(&k.PublicKey) {
		t.Fatalf("pem: %v", err)
	}
}

func TestTicketKeyPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.pem")
	t.Setenv("SOLDAT_TICKET_KEY", path)
	loadTicketKey()
	first := ticketKID
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", err, st)
	}
	loadTicketKey()
	if ticketKID != first || first == "" {
		t.Fatalf("key changed across restarts: %s -> %s", first, ticketKID)
	}
}
