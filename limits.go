// limits.go — per-IP abuse controls (issue #1).
//
// The master sits on a public IP; everything here exists so one source can't
// crowd out everyone else: register rate + count caps, relay connection caps,
// and a general sliding-window limiter.
package main

import (
	"sync"
	"time"
)

// ipLimiter is a sliding-window rate limiter: at most `limit` hits per IP per
// `window`. Bookkeeping is O(recent hits) per call and the map is swept once
// it grows past idleIPs, so a scan of unique addresses can't leak memory.
type ipLimiter struct {
	mu      sync.Mutex
	seen    map[string][]time.Time
	limit   int
	window  time.Duration
	idleIPs int
}

func newIPLimiter(limit int, window time.Duration) *ipLimiter {
	return &ipLimiter{seen: map[string][]time.Time{}, limit: limit, window: window, idleIPs: 1024}
}

// allow records a hit for ip and reports whether it fits the limit.
func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	keep := l.seen[ip][:0]
	for _, t := range l.seen[ip] {
		if now.Sub(t) < l.window {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.limit {
		l.seen[ip] = keep
		return false
	}
	l.seen[ip] = append(keep, now)
	if len(l.seen) > l.idleIPs {
		for k, ts := range l.seen {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > l.window {
				delete(l.seen, k)
			}
		}
	}
	return true
}

var (
	// /register: heartbeats arrive every ~30 s (2/min per host). 16/min allows
	// a full NAT of hosts behind one address without grief room for floods.
	registerPerIP = newIPLimiter(16, time.Minute)
	// /relay: new WebSocket connections per IP per minute. Covers join-code
	// brute force and reconnect floods; real play uses a fraction of this.
	relayConnsPerIP = newIPLimiter(30, time.Minute)
)

const (
	maxServersPerIP = 8    // distinct registered game hosts per source IP
	relayMaxConnsIP = 16   // simultaneously open relay sockets per IP
	relayMaxConns   = 1000 // whole server — under the default 1024 fd limit
)

// relayLiveConns counts open relay sockets (global + per IP), issue #1.
var (
	relayConnsMu sync.Mutex
	relayConns   = map[string]int{}
	relayConnsN  int
)

// relayConnStart reserves a relay connection slot; false = over the cap.
func relayConnStart(ip string) bool {
	relayConnsMu.Lock()
	defer relayConnsMu.Unlock()
	if relayConnsN >= relayMaxConns || relayConns[ip] >= relayMaxConnsIP {
		return false
	}
	relayConnsN++
	relayConns[ip]++
	return true
}

func relayConnEnd(ip string) {
	relayConnsMu.Lock()
	defer relayConnsMu.Unlock()
	if relayConns[ip] > 0 {
		relayConns[ip]--
		if relayConns[ip] == 0 {
			delete(relayConns, ip)
		}
	}
	if relayConnsN > 0 {
		relayConnsN--
	}
}
