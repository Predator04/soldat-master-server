package main

// Profile codes + cloud saves (Predator04/Tracerfall#323) and join tickets
// (Predator04/Tracerfall#324).
//
// Cloud saves
//
//	POST /cloud/get {k}                    the save stored under k
//	POST /cloud/put {k, base, fmt, blob}   store a new version (optimistic)
//
// The game shows the player a random profile code (the only secret of the
// cloud save) and derives two things from it on the device: k =
// sha256("tracerfall-cloud-id:" + code), the address of the save here, and an
// encryption key the master never sees. `blob` is ciphertext (base64): the
// master can't read it, and nothing in it or in k is known to game servers
// (they only ever get the public profile hash, and from 1.41 only inside a
// ticket). Versions are optimistic: a put whose `base` isn't the stored
// version gets 409 with the stored blob; the game merges (larger progress
// wins per counter) and puts again.
//
// Join tickets
//
//	GET  /auth/key                 the master's RSA public key (PEM)
//	POST /auth/ticket {id, n, srv} a signed ticket for one join
//
// A game server sends each joining client a random nonce `n`; the client
// asks here for a ticket with its raw profile id (which the master already
// receives for /names) and hands only the ticket to the game server. The
// ticket says "profile hash pub joined with nonce n" (+ the address the
// client dialled and a timestamp), signed with the master's key, so the
// server learns the player's stable public id without ever seeing anything
// it could reuse elsewhere: another server's nonce is different.

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
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
	cloudMaxBlob      = 48 << 10 // base64 characters of one save
	cloudMaxSaves     = 200000   // whole store (disk bound: ~10 GB worst case)
	cloudNewPerIPDay  = 20       // new saves created from one address per day
	ticketTTL         = 5 * time.Minute
	ticketKeyBits     = 2048
	ticketNonceMinLen = 16
	ticketNonceMaxLen = 64
)

var (
	cloudReadsPerIP   = newIPLimiter(30, time.Minute)
	cloudWritesPerIP  = newIPLimiter(12, time.Minute)
	cloudNewPerIP     = newIPLimiter(cloudNewPerIPDay, 24*time.Hour)
	ticketsPerIP      = newIPLimiter(60, time.Minute)
	authKeyReadsPerIP = newIPLimiter(60, time.Minute)

	cloudMu    sync.Mutex
	cloudDir   string
	cloudCount int

	cloudKeyRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	nonceRe    = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

	ticketMu   sync.RWMutex
	ticketKey  *rsa.PrivateKey
	ticketPEM  string // public key, PKIX PEM
	ticketKID  string // first 16 hex of sha256(public DER)
	errNoCloud = errors.New("no save")
)

// cloudSave is one stored save (cloud/<sha256("cloud:"+k)>.json).
type cloudSave struct {
	Ver   int64  `json:"ver"`
	Fmt   int    `json:"fmt"`
	SetMS int64  `json:"set_ms"`
	IP    string `json:"ip,omitempty"` // short hash of the address that made it
	Blob  string `json:"blob"`
}

// ── storage ──────────────────────────────────────────────────────────────────

func loadCloud() {
	cloudDir = filepath.Join(dataDir(), "cloud")
	if err := os.MkdirAll(cloudDir, 0o755); err != nil {
		log.Printf("cloud: %v", err)
	}
	ents, _ := os.ReadDir(cloudDir)
	n := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	cloudMu.Lock()
	cloudCount = n
	cloudMu.Unlock()
	log.Printf("cloud: %d saves in %s", n, cloudDir)
}

// cloudPath: the file for save address k. The name is a hash of k, so a copy
// of the directory doesn't hand out the addresses themselves.
func cloudPath(k string) string {
	s := sha256.Sum256([]byte("cloud:" + k))
	return filepath.Join(cloudDir, hex.EncodeToString(s[:])+".json")
}

func readCloud(k string) (*cloudSave, error) {
	b, err := os.ReadFile(cloudPath(k))
	if err != nil {
		return nil, errNoCloud
	}
	var s cloudSave
	if json.Unmarshal(b, &s) != nil {
		return nil, errNoCloud
	}
	return &s, nil
}

func writeCloud(k string, s *cloudSave) error {
	b, _ := json.Marshal(s)
	p := cloudPath(k)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

type cloudReq struct {
	K    string `json:"k"`
	Base int64  `json:"base"`
	Fmt  int    `json:"fmt"`
	Blob string `json:"blob"`
}

func readCloudReq(w http.ResponseWriter, r *http.Request) (cloudReq, bool) {
	var req cloudReq
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return req, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, cloudMaxBlob+(4<<10))
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeStatus(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "status": "big", "max": cloudMaxBlob})
			return req, false
		}
		http.Error(w, "bad json", http.StatusBadRequest)
		return req, false
	}
	req.K = strings.ToLower(strings.TrimSpace(req.K))
	if !cloudKeyRe.MatchString(req.K) {
		http.Error(w, "bad k", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

func writeStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func saveView(s *cloudSave) map[string]any {
	return map[string]any{"ver": s.Ver, "fmt": s.Fmt, "set_ms": s.SetMS, "blob": s.Blob}
}

// POST /cloud/get {k} → {ok, ver, fmt, set_ms, blob} | 404 {status:"none"}.
func handleCloudGet(w http.ResponseWriter, r *http.Request) {
	if !cloudReadsPerIP.allow(sourceIP(r)) {
		http.Error(w, "too many requests, slow down", http.StatusTooManyRequests)
		return
	}
	req, ok := readCloudReq(w, r)
	if !ok {
		return
	}
	cloudMu.Lock()
	s, err := readCloud(req.K)
	cloudMu.Unlock()
	if err != nil {
		writeStatus(w, http.StatusNotFound, map[string]any{"ok": false, "status": "none"})
		return
	}
	out := saveView(s)
	out["ok"] = true
	writeJSON(w, out)
}

// POST /cloud/put {k, base, fmt, blob} → {ok, ver} | 409 {status:"conflict",
// ver, fmt, set_ms, blob} | 413 big | 429 limit | 503 full.
func handleCloudPut(w http.ResponseWriter, r *http.Request) {
	ip := sourceIP(r)
	if !cloudWritesPerIP.allow(ip) {
		http.Error(w, "too many requests, slow down", http.StatusTooManyRequests)
		return
	}
	req, ok := readCloudReq(w, r)
	if !ok {
		return
	}
	if len(req.Blob) > cloudMaxBlob {
		writeStatus(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "status": "big", "max": cloudMaxBlob})
		return
	}
	if req.Blob == "" {
		http.Error(w, "no blob", http.StatusBadRequest)
		return
	}
	if _, err := base64.StdEncoding.DecodeString(req.Blob); err != nil {
		http.Error(w, "blob is not base64", http.StatusBadRequest)
		return
	}
	res, code := putCloud(req, ip, time.Now())
	writeStatus(w, code, res)
}

func putCloud(req cloudReq, ip string, now time.Time) (map[string]any, int) {
	cloudMu.Lock()
	defer cloudMu.Unlock()
	cur, err := readCloud(req.K)
	if err == nil && req.Base != cur.Ver {
		out := saveView(cur)
		out["ok"] = false
		out["status"] = "conflict"
		return out, http.StatusConflict
	}
	next := &cloudSave{Fmt: req.Fmt, SetMS: now.UnixMilli(), Blob: req.Blob, IP: shortHash("ip:" + ip)[:8]}
	if err == nil {
		next.Ver = cur.Ver + 1
		next.IP = cur.IP
	} else {
		if cloudCount >= cloudMaxSaves {
			return map[string]any{"ok": false, "status": "full"}, http.StatusServiceUnavailable
		}
		if !cloudNewPerIP.allow(ip) {
			return map[string]any{"ok": false, "status": "limit"}, http.StatusTooManyRequests
		}
		// A save the store lost (restored from an older backup) continues
		// above the version the device last saw.
		next.Ver = maxI64(req.Base, 0) + 1
	}
	if werr := writeCloud(req.K, next); werr != nil {
		log.Printf("cloud: write failed: %v", werr)
		return map[string]any{"ok": false, "status": "error"}, http.StatusInternalServerError
	}
	if err != nil {
		cloudCount++
	}
	return map[string]any{"ok": true, "status": "saved", "ver": next.Ver}, http.StatusOK
}

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ── join tickets ─────────────────────────────────────────────────────────────

// loadTicketKey reads the signing key (SOLDAT_TICKET_KEY, default
// <data dir>/ticket_key.pem) or makes one. Losing it is harmless: tickets
// live 5 minutes and game servers fetch /auth/key again when a ticket stops
// verifying.
func loadTicketKey() {
	path := strings.TrimSpace(os.Getenv("SOLDAT_TICKET_KEY"))
	if path == "" {
		path = filepath.Join(dataDir(), "ticket_key.pem")
	}
	var key *rsa.PrivateKey
	if b, err := os.ReadFile(path); err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
				key = k
			} else if k8, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
				key, _ = k8.(*rsa.PrivateKey)
			}
		}
		if key == nil {
			log.Printf("auth: %s unreadable, making a new key", path)
		}
	}
	if key == nil {
		k, err := rsa.GenerateKey(rand.Reader, ticketKeyBits)
		if err != nil {
			log.Printf("auth: key generation failed: %v", err)
			return
		}
		key = k
		b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
		if err := os.WriteFile(path, b, 0o600); err != nil {
			log.Printf("auth: can't save %s (%v); this key lasts until a restart", path, err)
		} else {
			log.Printf("auth: new ticket key saved to %s", path)
		}
	}
	setTicketKey(key)
}

func setTicketKey(key *rsa.PrivateKey) {
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	sum := sha256.Sum256(der)
	ticketMu.Lock()
	ticketKey = key
	ticketPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	ticketKID = hex.EncodeToString(sum[:8])
	ticketMu.Unlock()
}

type ticketPayload struct {
	V   int    `json:"v"`
	Pub string `json:"pub"` // sha256(lower(profile id)): leaderboard key / name owner
	N   string `json:"n"`   // the game server's nonce for this connection
	Srv string `json:"srv"` // the address the client dialled (informative)
	IAT int64  `json:"iat"` // ms
	Exp int64  `json:"exp"` // ms
	KID string `json:"kid"`
}

// makeTicket: base64(payload JSON) + "." + base64(RSA PKCS#1 v1.5 SHA-256
// signature over the base64 payload text). Standard base64 (the game's
// Marshalls), no "." in either part.
func makeTicket(pub, nonce, srv string, now time.Time) (string, int64, error) {
	ticketMu.RLock()
	key, kid := ticketKey, ticketKID
	ticketMu.RUnlock()
	if key == nil {
		return "", 0, errors.New("no key")
	}
	p := ticketPayload{V: 1, Pub: pub, N: nonce, Srv: srv, IAT: now.UnixMilli(), Exp: now.Add(ticketTTL).UnixMilli(), KID: kid}
	b, _ := json.Marshal(p)
	body := base64.StdEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", 0, err
	}
	return body + "." + base64.StdEncoding.EncodeToString(sig), p.Exp, nil
}

// verifyTicket is the game server's check, in Go (tests; the game has its
// own copy in scripts/join_ticket.gd).
func verifyTicket(pubKey *rsa.PublicKey, t, nonce string, now time.Time) (*ticketPayload, error) {
	parts := strings.Split(t, ".")
	if len(parts) != 2 {
		return nil, errors.New("shape")
	}
	sig, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("sig encoding")
	}
	sum := sha256.Sum256([]byte(parts[0]))
	if rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, sum[:], sig) != nil {
		return nil, errors.New("signature")
	}
	raw, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("payload encoding")
	}
	var p ticketPayload
	if json.Unmarshal(raw, &p) != nil || p.V != 1 || !idRe.MatchString(p.Pub) {
		return nil, errors.New("payload")
	}
	if p.N != nonce {
		return nil, errors.New("nonce")
	}
	if now.UnixMilli() > p.Exp+int64(10*time.Minute/time.Millisecond) {
		return nil, errors.New("expired")
	}
	return &p, nil
}

// GET /auth/key → {alg, kid, pem}.
func handleAuthKey(w http.ResponseWriter, r *http.Request) {
	if !authKeyReadsPerIP.allow(sourceIP(r)) {
		http.Error(w, "too many requests, slow down", http.StatusTooManyRequests)
		return
	}
	ticketMu.RLock()
	p, kid := ticketPEM, ticketKID
	ticketMu.RUnlock()
	if p == "" {
		writeStatus(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "status": "nokey"})
		return
	}
	w.Header().Set("Cache-Control", "max-age=600")
	writeJSON(w, map[string]any{"ok": true, "alg": "RS256", "kid": kid, "pem": p})
}

type ticketReq struct {
	ID  string `json:"id"`
	N   string `json:"n"`
	Srv string `json:"srv"`
}

// POST /auth/ticket {id, n, srv} → {ok, ticket, pub, exp_ms}.
func handleAuthTicket(w http.ResponseWriter, r *http.Request) {
	if !ticketsPerIP.allow(sourceIP(r)) {
		http.Error(w, "too many requests, slow down", http.StatusTooManyRequests)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req ticketReq
	r.Body = http.MaxBytesReader(w, r.Body, 2<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	id := strings.ToLower(strings.TrimSpace(req.ID))
	n := strings.ToLower(strings.TrimSpace(req.N))
	if !rawIDRe.MatchString(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if len(n) < ticketNonceMinLen || len(n) > ticketNonceMaxLen || !nonceRe.MatchString(n) {
		http.Error(w, "bad nonce", http.StatusBadRequest)
		return
	}
	pub := profileHash(id)
	t, exp, err := makeTicket(pub, n, cleanText(req.Srv, 80), time.Now())
	if err != nil {
		writeStatus(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "status": "nokey"})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "ticket": t, "pub": pub, "exp_ms": exp})
}
