// soldat-master-server — standalone HTTP master/lobby server for Soldat Reborn.
//
// Dedicated game hosts POST /register every ~30s; the master records their
// PUBLIC ip (from the TCP source address, so it works behind NAT) plus the
// game port they report. Clients GET /list to browse live servers.
//
// Build (any platform, no deps):
//
//	go build -o soldat-master .
//
// Run:
//
//	./soldat-master -port 8080
//
// Cross-compile (see build.sh):
//
//	GOOS=linux GOARCH=amd64 go build -o soldat-master-linux-amd64 .
//	GOOS=windows GOARCH=amd64 go build -o soldat-master-windows-amd64.exe .
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is one registered dedicated game host.
type Server struct {
	ID         string `json:"id"`              // "ip:port" — stable key
	Name       string `json:"name"`            // host-configured server name
	IP         string `json:"ip"`              // public IP captured from the register request
	Port       int    `json:"port"`            // game (ENet UDP) port
	Map        string `json:"map"`             // current map name
	Mode       string `json:"mode"`            // current game mode
	Players    int    `json:"players"`         // current player count
	Max        int    `json:"max"`             // max players
	Password   bool   `json:"password"`        // requires password
	Version    string `json:"version"`         // game version string
	Relay      string `json:"relay,omitempty"` // "R-XXXXXX" when hosted through /relay
	LastSeenMS int64  `json:"last_seen_ms"`    // epoch ms heartbeat stamp

	lastSeen time.Time `json:"-"`
}

type registerReq struct {
	Name     string `json:"name"`
	Port     int    `json:"port"`
	Map      string `json:"map"`
	Mode     string `json:"mode"`
	Players  int    `json:"players"`
	Max      int    `json:"max"`
	Password bool   `json:"password"`
	Version  string `json:"version"`
	Relay    string `json:"relay"`
}

var (
	mu      sync.RWMutex
	servers = map[string]*Server{}

	// buildVersion is injected at build time via -ldflags "-X main.buildVersion=...".
	buildVersion = "dev"
)

func main() {
	port := flag.Int("port", defaultPort(), "HTTP listen port (overrides SOLDAT_MASTER_PORT / PORT env)")
	addr := flag.String("addr", "0.0.0.0", "bind address")
	ttl := flag.Duration("ttl", 90*time.Second, "server heartbeat expiry window")
	flag.Parse()

	http.HandleFunc("/register", handleRegister)
	http.HandleFunc("/list", handleList)
	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/relay", handleRelay)
	http.HandleFunc("/", handleDashboard)

	go func() {
		for {
			time.Sleep(10 * time.Second)
			prune(*ttl)
		}
	}()

	host := net.JoinHostPort(*addr, fmt.Sprintf("%d", *port))
	log.Printf("soldat-master-server listening on http://%s (ttl=%s)", host, *ttl)
	hs := &http.Server{Addr: host, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 16 << 10}
	if err := hs.ListenAndServe(); err != nil {
		log.Fatalf("listen failed: %v", err)
	}
}

// defaultPort resolves the listen port from env (SOLDAT_MASTER_PORT, then PORT)
// so it can be configured without a flag (systemd Environment=, docker -e, etc.).
// A -port flag always wins; otherwise 8080.
func defaultPort() int {
	for _, key := range []string{"SOLDAT_MASTER_PORT", "PORT"} {
		if v := os.Getenv(key); v != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 && n <= 65535 {
				return n
			}
		}
	}
	return 8080
}

// sourceIP returns the client's public IP (IPv4 preferred) from the request,
// stripping any port. This is the address OTHER clients should connect to,
// so NAT'd hosts advertise their external IP automatically.
func sourceIP(r *http.Request) string {
	// Reverse proxies can pass the real client IP here.
	// Only trusted when running behind one (SOLDAT_TRUST_PROXY=1); otherwise
	// anyone could spoof their address in the list.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" && os.Getenv("SOLDAT_TRUST_PROXY") == "1" {
		parts := strings.Split(xff, ",")
		ip := strings.TrimSpace(parts[0])
		if net.ParseIP(ip) != nil {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr may lack a port in some tests.
		host = r.RemoteAddr
	}
	// A game server on the same machine registers over loopback; advertise the
	// machine's public address instead (SOLDAT_PUBLIC_IP), or players would be
	// told to connect to 127.0.0.1.
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if pub := strings.TrimSpace(os.Getenv("SOLDAT_PUBLIC_IP")); pub != "" {
			return pub
		}
	}
	// Prefer IPv4 for NAT simplicity.
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
	}
	return host
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req registerReq
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	relay := strings.ToUpper(strings.TrimSpace(req.Relay))
	if relay != "" && !relayRoomExists(relay) {
		http.Error(w, "unknown relay code", http.StatusBadRequest)
		return
	}
	if relay == "" && (req.Port <= 0 || req.Port > 65535) {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}
	ip := sourceIP(r)
	key := fmt.Sprintf("%s:%d", ip, req.Port)
	if relay != "" {
		key = relay // relay games are joined by code, not address
	}

	mu.Lock()
	srv, ok := servers[key]
	if !ok && len(servers) >= 1000 {
		mu.Unlock()
		http.Error(w, "server list full", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		srv = &Server{ID: key, IP: ip, Port: req.Port}
		servers[key] = srv
	}
	srv.Name = clip(req.Name, 48)
	srv.Map = clip(req.Map, 48)
	srv.Mode = clip(req.Mode, 24)
	srv.Players = req.Players
	srv.Max = req.Max
	srv.Password = req.Password
	srv.Version = req.Version
	srv.Relay = relay
	srv.lastSeen = time.Now()
	srv.LastSeenMS = srv.lastSeen.UnixMilli()
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": key})
}

func handleList(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	list := make([]*Server, 0, len(servers))
	now := time.Now()
	for _, s := range servers {
		cp := *s
		// expose age as seconds so clients can sort/ping-ish
		cp.LastSeenMS = now.Sub(s.lastSeen).Milliseconds()
		list = append(list, &cp)
	}
	mu.RUnlock()

	sort.Slice(list, func(i, j int) bool {
		if list[i].Players != list[j].Players {
			return list[i].Players > list[j].Players // most populated first
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]any{"servers": list, "count": len(list)})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	n := len(servers)
	mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "servers": n})
}

func prune(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl)
	mu.Lock()
	for k, s := range servers {
		if s.lastSeen.Before(cutoff) {
			delete(servers, k)
		}
	}
	mu.Unlock()
}

var dashboardTmpl = template.Must(template.New("dash").Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Soldat Reborn — Server List</title>
<meta http-equiv="refresh" content="15">
<style>
body{font-family:system-ui,sans-serif;background:#0b0e1a;color:#d7dbe8;margin:0;padding:2rem}
h1{font-size:1.4rem;font-weight:600}
.sub{color:#7c8398;font-size:.9rem;margin-bottom:1.5rem}
table{border-collapse:collapse;width:100%;max-width:900px}
th,td{text-align:left;padding:.55rem .8rem;border-bottom:1px solid #1c2133;font-size:.92rem}
th{color:#7c8398;font-weight:600;font-size:.8rem;text-transform:uppercase;letter-spacing:.05em}
tr:hover td{background:#141828}
.name{font-weight:600;color:#fff}
.pill{background:#1c2133;border-radius:6px;padding:.15rem .5rem;font-size:.8rem}
.lock{color:#d9a441}
a{color:#5b8cff;text-decoration:none}
.empty{color:#7c8398;padding:1rem 0}
</style></head><body>
<h1>Soldat Reborn</h1>
<div class="sub">Live dedicated servers (auto-refresh 15s) · {{.Now}}</div>
{{if .Servers}}<table><tr><th>Name</th><th>Players</th><th>Map</th><th>Mode</th><th>Address</th><th>Age</th></tr>
{{range .Servers}}<tr>
<td class="name">{{.Name}}{{if .Password}} <span class="lock" title="password required">🔒</span>{{end}}</td>
<td>{{.Players}}/{{.Max}}</td>
<td><span class="pill">{{.Map}}</span></td>
<td>{{.Mode}}</td>
<td>{{.IP}}:{{.Port}}</td>
<td>{{.Age}}s</td>
</tr>{{end}}</table>
{{else}}<div class="empty">No servers registered yet. Start a dedicated host with <code>--register http://this-host:8080</code>.</div>{{end}}
</body></html>`))

type dashServer struct {
	Name     string
	Players  int
	Max      int
	Map      string
	Mode     string
	IP       string
	Port     int
	Password bool
	Age      int64
}

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	mu.RLock()
	now := time.Now()
	out := make([]dashServer, 0, len(servers))
	for _, s := range servers {
		out = append(out, dashServer{
			Name: s.Name, Players: s.Players, Max: s.Max, Map: s.Map,
			Mode: s.Mode, IP: s.IP, Port: s.Port, Password: s.Password,
			Age: int64(now.Sub(s.lastSeen).Seconds()),
		})
	}
	mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Players > out[j].Players })
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = dashboardTmpl.Execute(w, map[string]any{"Servers": out, "Now": now.Format(time.RFC1123)})
}
