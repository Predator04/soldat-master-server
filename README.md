# soldat-master-server

Standalone HTTP master/lobby server for **Soldat Reborn**. Run it anywhere with a
public URL — dedicated game hosts register against it, and the in-game "Browse
Servers" menu lists them. One static binary, zero dependencies.

## How it works

```
dedicated host ──POST /register (every 30s)──▶ master server ──GET /list──▶ game client
```

- A dedicated host (`SoldatReborn.exe --dedicated --register <url>`) POSTs its
  name/map/mode/player-count to `/register` every 30s.
- The master records the host's **public IP** from the TCP source address (so it
  works behind NAT — no port-forward config needed on the master's side; the host
  still forwards its *game* port).
- Servers expire automatically after 90s of silence.
- Clients fetch `/list` and join the address shown.

## Run

Download a binary from [Releases](../../releases) or build from source (Go 1.21+):

```bash
go build -o soldat-master .
./soldat-master -port 8080
```

The port is configurable three ways (in priority order: `-port` flag, then
`SOLDAT_MASTER_PORT` / `PORT` env vars, then the 8080 default):

```bash
./soldat-master -port 9000          # flag
SOLDAT_MASTER_PORT=9000 ./soldat-master   # env
```

Flags:

| Flag | Default | Meaning |
|------|---------|---------|
| `-port` | `SOLDAT_MASTER_PORT`/`PORT`/`8080` | HTTP listen port |
| `-addr` | `0.0.0.0` | bind address |
| `-ttl` | `90s` | heartbeat expiry window |

## Relay (v1.25): online play with no port forwarding

The same binary also runs a **game relay** at `/relay` (WebSocket). When a
player ticks *Host through the relay* in the game, the host and every
joining player open an **outgoing** connection to this server, which passes
the game traffic between them. Nobody has to open a port, and it works behind
CGNAT, phone hotspots and strict routers. The host gets a join code like
`R-7KQ2MX`; friends type it into Join Game.

- The server must be reachable from the internet (that's the one place a port
  is open, 8080 by default). Put it on any small VPS or container host.
- Set its address in the game under **Join → Master Server URL** (e.g.
  `http://your-server:8080` or `https://relay.example.com`). The game uses
  the same URL for the server list and the relay (`ws://…/relay`, or `wss://`
  for https).
- Relay games also show in *Find Games* (marked RELAY) if the host ticks
  *List on the master server*.
- Each room closes when its host leaves. Up to 16 players per room.
- Bandwidth: a busy 8-player match is roughly 30–60 KB/s through the server.

### Deploying it

Any of these work; all you need is one public port.

**Docker (any VPS):**
```bash
docker build -t soldat-master .
docker run -d --restart unless-stopped -p 8080:8080 soldat-master
```

**Plain binary (Linux VPS):**
```bash
./dist/soldat-master-linux-amd64 -port 8080
```
(keep it running with `systemd`, `tmux`, or your host's process manager).

**Behind HTTPS** (recommended for a public server): put Caddy or nginx in
front and proxy `/` (including WebSocket upgrades) to port 8080; players then
use `https://your-domain` as the master URL.

## Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/register` | host heartbeat (JSON body below) |
| GET | `/list` | JSON list of live servers |
| GET | `/` | HTML dashboard (auto-refresh) |
| GET | `/health` | `{"ok":true,"servers":N}` for uptime checks |

`/register` body:

```json
{
  "name": "My Server",
  "port": 7777,
  "map": "Ascent",
  "mode": "Deathmatch",
  "players": 3,
  "max": 16,
  "password": false,
  "version": "1.11.0"
}
```

`/list` returns:

```json
{
  "count": 1,
  "servers": [
    {"id":"1.2.3.4:7777","name":"My Server","ip":"1.2.3.4","port":7777,
     "map":"Ascent","mode":"Deathmatch","players":3,"max":16,
     "password":false,"version":"1.11.0","last_seen_ms":1200}
  ]
}
```

## Deploy (systemd)

```ini
# /etc/systemd/system/soldat-master.service
[Unit]
Description=Soldat Reborn master server
After=network.target

[Service]
ExecStart=/opt/soldat-master/soldat-master -port 8080
# Or configure the port via env instead of the flag:
# Environment=SOLDAT_MASTER_PORT=9000
Restart=always
RestartSec=3
User=nobody

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now soldat-master
```

## Deploy (Docker)

```bash
docker compose up -d
```

Then point the game's master URL at `http://<your-host>:8080`.
