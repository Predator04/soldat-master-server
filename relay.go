// relay.go — WebSocket game relay (v1.25).
//
// Lets two players play online when neither can open a port (CGNAT, phone
// hotspot, strict router): the host and every client open an outgoing
// WebSocket to this server, which forwards game packets between them.
//
// Endpoint: GET /relay (WebSocket upgrade). Protocol, all JSON text frames
// except game data:
//
//	client -> {"op":"host","version":"1.25.0","name":"William's game"}
//	server -> {"op":"hosted","code":"R7KQ2M","id":1}
//	client -> {"op":"join","code":"R7KQ2M"}
//	server -> {"op":"joined","id":123456}            (to the joiner)
//	          {"op":"peer_connected","id":123456}    (to the host)
//	          {"op":"peer_disconnected","id":123456} (to the host)
//	host   -> {"op":"kick","id":123456}
//	error  -> {"op":"error","msg":"..."} and the socket closes.
//
// Game data are binary frames. Host -> server: 4-byte little-endian target
// peer id + payload (0 = every client, -N = every client except N).
// Server -> host: 4-byte little-endian source id + payload. Client <-> server:
// the bare payload (clients only ever talk to the host; Godot's SceneMultiplayer
// relays client-to-client traffic through the host itself).
//
// Standard library only (hand-rolled RFC 6455 framing), like the rest of the
// master server.
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
const relayMaxFrame = 1 << 20 // 1 MiB per message is plenty for game packets
const relayMaxClients = 16
const relayMaxRooms = 300    // whole server (a 1 GB VM)
const relayMaxRoomsPerIP = 8 // a household or a mobile carrier NAT

type wsConn struct {
	c    net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
	dead bool
}

func (w *wsConn) writeFrame(op byte, data []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.dead {
		return errors.New("closed")
	}
	hdr := []byte{0x80 | op}
	n := len(data)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n < 65536:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	w.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	w.c.SetWriteDeadline(time.Now().Add(5 * time.Second)) // a stalled peer must not block the room
	if _, err := w.c.Write(append(hdr, data...)); err != nil {
		w.dead = true
		return err
	}
	return nil
}

func (w *wsConn) text(v interface{}) {
	b, _ := json.Marshal(v)
	w.writeFrame(0x1, b)
}

func (w *wsConn) close() {
	w.writeFrame(0x8, []byte{0x03, 0xE8})
	w.wmu.Lock()
	w.dead = true
	w.wmu.Unlock()
	w.c.Close()
}

// readMessage returns (opcode, payload) of the next complete data message,
// answering pings and reassembling fragments.
func (w *wsConn) readMessage() (byte, []byte, error) {
	var msg []byte
	var msgOp byte
	for {
		w.c.SetReadDeadline(time.Now().Add(90 * time.Second))
		h := make([]byte, 2)
		if _, err := io.ReadFull(w.br, h); err != nil {
			return 0, nil, err
		}
		fin := h[0]&0x80 != 0
		op := h[0] & 0x0F
		masked := h[1]&0x80 != 0
		n := uint64(h[1] & 0x7F)
		if n == 126 {
			b := make([]byte, 2)
			if _, err := io.ReadFull(w.br, b); err != nil {
				return 0, nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b))
		} else if n == 127 {
			b := make([]byte, 8)
			if _, err := io.ReadFull(w.br, b); err != nil {
				return 0, nil, err
			}
			n = binary.BigEndian.Uint64(b)
		}
		if n > relayMaxFrame || uint64(len(msg))+n > relayMaxFrame {
			return 0, nil, errors.New("frame too big")
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(w.br, mask[:]); err != nil {
				return 0, nil, err
			}
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(w.br, p); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range p {
				p[i] ^= mask[i%4]
			}
		}
		switch op {
		case 0x8:
			return 0x8, nil, io.EOF
		case 0x9:
			w.writeFrame(0xA, p)
			continue
		case 0xA:
			continue
		case 0x0:
			msg = append(msg, p...)
		default:
			msgOp = op
			msg = p
		}
		if fin {
			return msgOp, msg, nil
		}
	}
}

func upgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "websocket only", http.StatusBadRequest)
		return nil, errors.New("not a websocket request")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return nil, errors.New("missing key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("no hijack")
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n"
	if _, err := c.Write([]byte(resp)); err != nil {
		c.Close()
		return nil, err
	}
	return &wsConn{c: c, br: brw.Reader}, nil
}

type relayRoom struct {
	code    string
	ip      string
	name    string
	version string
	host    *wsConn
	mu      sync.Mutex
	clients map[int32]*wsConn
	created time.Time
}

var (
	roomsMu sync.Mutex
	rooms   = map[string]*relayRoom{}
)

const codeAlpha = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

func newCode() string {
	b := make([]byte, 6)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(codeAlpha))))
		b[i] = codeAlpha[n.Int64()]
	}
	return string(b)
}

func newPeerID() int32 {
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<31-3))
	return int32(n.Int64()) + 2
}

type relayHello struct {
	Op      string `json:"op"`
	Code    string `json:"code"`
	Version string `json:"version"`
	Name    string `json:"name"`
	ID      int32  `json:"id"`
}

func handleRelay(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrade(w, r)
	if err != nil {
		return
	}
	defer ws.c.Close()
	op, msg, err := ws.readMessage()
	if err != nil || op != 0x1 {
		return
	}
	var hello relayHello
	if json.Unmarshal(msg, &hello) != nil {
		ws.text(map[string]string{"op": "error", "msg": "bad hello"})
		return
	}
	switch hello.Op {
	case "host":
		relayHost(ws, hello, sourceIP(r))
	case "join":
		relayJoin(ws, strings.ToUpper(strings.TrimSpace(hello.Code)))
	default:
		ws.text(map[string]string{"op": "error", "msg": "unknown op"})
	}
}

func relayHost(ws *wsConn, hello relayHello, ip string) {
	room := &relayRoom{host: ws, ip: ip, clients: map[int32]*wsConn{}, name: clip(hello.Name, 48), version: clip(hello.Version, 24), created: time.Now()}
	roomsMu.Lock()
	perIP := 0
	for _, rm := range rooms {
		if rm.ip == ip {
			perIP++
		}
	}
	if len(rooms) >= relayMaxRooms || perIP >= relayMaxRoomsPerIP {
		roomsMu.Unlock()
		ws.text(map[string]string{"op": "error", "msg": "the relay is full right now, try again in a bit"})
		return
	}
	for {
		room.code = newCode()
		if _, taken := rooms[room.code]; !taken {
			break
		}
	}
	rooms[room.code] = room
	roomsMu.Unlock()
	log.Printf("relay: room %s hosted (%s)", room.code, hello.Name)
	ws.text(map[string]interface{}{"op": "hosted", "code": room.code, "id": 1})
	defer func() {
		roomsMu.Lock()
		delete(rooms, room.code)
		roomsMu.Unlock()
		room.mu.Lock()
		for _, c := range room.clients {
			c.close()
		}
		room.mu.Unlock()
		log.Printf("relay: room %s closed", room.code)
	}()
	for {
		op, msg, err := ws.readMessage()
		if err != nil {
			return
		}
		if op == 0x1 {
			var m relayHello
			if json.Unmarshal(msg, &m) == nil && m.Op == "kick" {
				room.mu.Lock()
				if c, ok := room.clients[m.ID]; ok {
					c.close()
				}
				room.mu.Unlock()
			}
			continue
		}
		if len(msg) < 4 {
			continue
		}
		target := int32(binary.LittleEndian.Uint32(msg[:4]))
		payload := msg[4:]
		room.mu.Lock()
		for id, c := range room.clients {
			if target == 0 || target == id || (target < 0 && -target != id) {
				c.writeFrame(0x2, payload)
			}
		}
		room.mu.Unlock()
	}
}

func relayJoin(ws *wsConn, code string) {
	roomsMu.Lock()
	room := rooms[code]
	roomsMu.Unlock()
	if room == nil {
		ws.text(map[string]string{"op": "error", "msg": "no game with that code (it may have ended)"})
		return
	}
	room.mu.Lock()
	if len(room.clients) >= relayMaxClients {
		room.mu.Unlock()
		ws.text(map[string]string{"op": "error", "msg": "that game is full"})
		return
	}
	id := newPeerID()
	for room.clients[id] != nil {
		id = newPeerID()
	}
	room.clients[id] = ws
	room.mu.Unlock()
	ws.text(map[string]interface{}{"op": "joined", "id": id, "version": room.version})
	room.host.text(map[string]interface{}{"op": "peer_connected", "id": id})
	log.Printf("relay: peer %d joined %s", id, code)
	defer func() {
		room.mu.Lock()
		delete(room.clients, id)
		room.mu.Unlock()
		room.host.text(map[string]interface{}{"op": "peer_disconnected", "id": id})
	}()
	hdr := make([]byte, 4)
	binary.LittleEndian.PutUint32(hdr, uint32(id))
	for {
		op, msg, err := ws.readMessage()
		if err != nil {
			return
		}
		if op != 0x2 {
			continue
		}
		room.host.writeFrame(0x2, append(append([]byte{}, hdr...), msg...))
	}
}

// relayRooms is shown on the dashboard / used by /health.
func relayRooms() int {
	roomsMu.Lock()
	defer roomsMu.Unlock()
	return len(rooms)
}

// relayRoomExists reports whether a relay code is a live room (so /register
// can't list made-up relay games).
func relayRoomExists(code string) bool {
	roomsMu.Lock()
	defer roomsMu.Unlock()
	_, ok := rooms[code]
	return ok
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > n {
		return string([]rune(s)[:n])
	}
	return s
}
