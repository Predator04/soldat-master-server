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

Flags:

| Flag | Default | Meaning |
|------|---------|---------|
| `-port` | `8080` | HTTP listen port |
| `-addr` | `0.0.0.0` | bind address |
| `-ttl` | `90s` | heartbeat expiry window |

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
