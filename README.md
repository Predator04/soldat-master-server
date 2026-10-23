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

## Production deployment (the live server)

The public instance runs on Oracle Cloud free tier at
`https://161-153-9-69.sslip.io` with:

- **TLS** — Caddy in front (Let's Encrypt, auto-renew), proxying 443 → 127.0.0.1:8080
  and upgrading `/relay` to `wss://`. One-command setup: `deploy/tls.sh` (opens the
  firewall with raw iptables — Oracle images REJECT by default and `ufw` is inert
  there; optional `LOOPBACK=1` retires plain 8080 once clients are on HTTPS).
- **Abuse limits** (`limits.go`) — register 24/min per IP and ≤8 servers per IP;
  relay 30 new connections/min per IP, ≤16 open per IP / 1000 global, 10 s hello
  deadline; map uploads 20/day per IP with attempt throttling; level browser
  240 list/get, 60 likes and 30 play reports per minute per IP, at most 4
  likes / daily plays per address per level; reserved names 10 writes and
  240 checks per minute per IP, at most 5 live reservations per address;
  cloud saves 30 reads / 12 writes per minute and 20 new saves per day per
  IP (48 KB each, 200k in all); join tickets 60 per minute per IP;
  HTTP read timeouts.
- **Backups** — daily cron (`/usr/local/bin/soldat-backup.sh`) tars stats.json
  (add names.json beside it — see "Reserved names"),
  the maps dir and the TLS/firewall config, 14-day rotation.
- **Updates** — `deploy/update.sh` rebuilds from this repo's master and restarts
  the systemd unit.

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
| GET | `/news` | the main menu's news line (below) |
| GET | `/levels` | level browser list: sort, search, paging (below) |
| GET | `/levels/get` | one level + its map, by `id` or share `code` |
| POST | `/levels/upload` | publish a level (`/maps/upload` still works) |
| POST | `/levels/like` | like / unlike (one per player id) |
| POST | `/levels/play` | play report (once per player id per day) |
| GET | `/maps`, `/maps/get` | the classic map library (older game builds) |
| POST | `/names/reserve` | reserve / change / release a player name (below) |
| POST | `/names/mine` | a profile's reserved name + cooldown |
| GET | `/names/check` | is a name reserved, and by whom (game servers ask on join) |
| POST | `/cloud/get` | a profile-code cloud save (below) |
| POST | `/cloud/put` | store a new version of a cloud save |
| GET | `/auth/key` | the public key game servers verify join tickets with |
| POST | `/auth/ticket` | a signed one-join ticket for a player |

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

## Level sharing (`/levels`, game issue Predator04/Soldat-Reborn#288)

Every shared map is a **level**: a title, author, share code, the modes it
was built for, a thumbnail outline, plays and likes. The game's LEVELS
browser and the map editor's Publish use these routes; `/maps` keeps working
for older builds and reads / writes the same store (`maps/`).

- `GET /levels?sort=hot|new|plays|likes&q=&page=0&per=24&pid=&ids=`
  → `{"levels":[...], "page":0, "per":24, "total":N, "more":bool, "sort":"hot"}`.
  `q` matches title / author (substring, any case) or a share code typed any
  way (an exact code hit is listed first). `per` ≤ 48. `ids=a,b,c` (≤ 48)
  returns exactly those levels in that order (the game's bookmarks; there
  are no accounts). With `pid`, each level says `"liked": true` if that
  player liked it.
- Level fields: `id`, `code` (`7KQ2-MX9P-4HDA`), `title` (+ `name`, the same,
  for old builds), `author`, `created_ms`, `size`, `platforms`, `modes`
  (`"dm tdm ctf dom"`: dm always, tdm with ≥ 2 spawns, ctf / inf / htf / rm /
  dom when the author placed that mode's objects), `theme`, `plays`,
  `likes`, `downloads`, `hot`, `thumb`
  (`{"w","h","c","r":[[x,y,w,h]],"p":[[rgb,x0,y0,...]],"f":[[x,y,kind]]}` in
  a ≤ 320×128 box; colour `-1` = the game's default ground; kind 0/1 = blue /
  red flag, 2 = neutral flag, 3 = domination point).
- `GET /levels/get?id=<16 hex>` or `?code=<share code>` → `{"level":{...},"map":{...}}`
  (counts a download), 404 if unknown.
- `POST /levels/upload` `{"title","author","theme","map"}` → `{"ok":true,"id","code","title","modes"}`
  (`"existing":true` for an identical map). Map ≤ 512 KB with ≥ 1 platform or
  terrain polygon; title ≤ 32 characters, author ≤ 24; same daily quota as
  before (20 per IP).
- `POST /levels/like` `{"id","pid","like":true|false}` → `{"ok":true,"likes","liked"}`;
  idempotent per player id.
- `POST /levels/play` `{"id","pid"}` → `{"ok":true,"plays","counted"}`;
  one play per player id per level per UTC day.
- `pid` = an anonymous per-install id (16–64 hex characters) the game derives
  from its random profile id; only a hash of it is stored, with a short hash
  of the sender's address for the per-address caps.
- **Hot this week** = for each of the last 7 days, (plays + 3 × likes) ×
  0.5^(days ago / 2), plus a fading boost (2 → 0 over 3 days) for brand-new
  levels so they get seen.
- **Share codes**: 12 Crockford base-32 symbols derived from the level id
  (no I, L, O, U; typed O reads as 0, I / L as 1; case, spaces and dashes
  don't matter); a code that would spell a filtered word is skipped.
- **Name filter** (`filter.go`): titles and author names are checked against
  a small built-in list after leetspeak / accent normalisation ("Sh1t",
  "f.u.c.k", "FUUUCK"); hits are masked with `*` (the game asks for another
  title before it uploads — `scripts/name_filter.gd` there has the same
  rules; keep the two lists in step).
- **Storage**: still `maps/index.json` + `maps/<id>.json`. The level fields
  are extra keys on each index entry, so an older binary can still read the
  index (it ignores them, and drops them on its next save — roll back with a
  copy of `index.json` if you care about the counts). Maps stored before
  this version are filled in once on load (thumbnail, modes, code; logged
  as "N migrated to level records").

`go test ./...` covers sorting / hot, like idempotency, play counting,
the filter, share codes, the upload → list → search → get flow, the legacy
`/maps` shape and the index migration.

## Reserved names (`/names`, game issue Predator04/Tracerfall#318)

Anyone can join a game server under anyone's name. A player can reserve
their name against their profile; game servers that list here ask before
letting someone play under it and rename an impostor to `Player####`.

- `POST /names/reserve` `{"id":"<32 hex profile id>","name":"Alice"}` →
  `{"ok","status","name","owner","cooldown_s"[,"taken_by"]}`. `status`:
  `reserved` (200), `taken` (409, `taken_by` = the reserved spelling),
  `cooldown` (429, `cooldown_s` until a change is allowed), `short` /
  `offensive` / `blocked` (400), `limit` (429, address cap), `full` (503).
  `{"id","release":true}` gives the name up (`released`).
- `POST /names/mine` `{"id"}` → `{"ok","name","owner","cooldown_s","set_ms"}`.
- `GET /names/check?name=<name>[&pid=<64 hex profile hash>]` →
  `{"name","key","reserved"[,"owner","display"][,"status"]}`; with `pid`,
  `status` = `free` / `mine` / `taken` / `invalid` (+ `problem`).
- **Identity**: `id` is the game's random per-install profile id. Only
  `sha256(lower(id))` is stored — the public id game servers learn from a
  join ticket (`/auth`, below) and the leaderboard key, so a server compares
  the joiner's verified id with `owner`.
- **Rules**: one name per profile; a change (or a release) starts a 72 h
  cooldown, re-spelling the same name doesn't. Names compare after the
  filter's normalisation (`filterNorm`: case, leetspeak, accents folded,
  non-letters dropped — "B0b", "b.o.b" and "bob" are one name), a clan tag
  prefix `«TAG» ` is ignored. Refused: under 3 letters after normalisation,
  anything `filter.go` masks, and the names the game hands out or uses
  itself (`Player…`, `Bot N`, `Red Bot…` / `Blue Bot…`, SERVER, Host, Admin…).
- **Storage**: `names.json` next to `stats.json` (`SOLDAT_DATA_DIR`), saved
  every 20 s and on SIGTERM.

## Profile codes + cloud saves (`/cloud`, game issue Predator04/Tracerfall#323)

The game shows each player a random **profile code** (24 symbols,
`XXXX-XXXX-XXXX-XXXX-XXXX-XXXX`). Entering it on another device restores the
profile there; this server keeps the backup. The code itself never arrives
here.

- `k` = `sha256("tracerfall-cloud-id:" + code)` (64 hex) is the save's
  address. The game encrypts the save (AES-256 + HMAC, keys derived from the
  code) before sending it, so `blob` is opaque base64 to this server: stats,
  cosmetics, name, carried settings and the player's raw profile id are all
  inside, readable only with the code. Nothing here is known to game servers.
- `POST /cloud/get {"k"}` → `{"ok":true,"ver","fmt","set_ms","blob"}`, or
  404 `{"status":"none"}`.
- `POST /cloud/put {"k","base","fmt","blob"}` → `{"ok":true,"ver"}`.
  Optimistic versions: `base` must be the stored `ver` (0 for a new save),
  otherwise 409 `{"status":"conflict","ver","fmt","set_ms","blob"}` — the game
  merges (per counter the larger progress wins) and puts again. 413 `big`
  (blob > 48 KB of base64), 429 `limit` (20 new saves per address per day) or
  rate limit, 503 `full` (200k saves).
- **Storage**: one file per save, `cloud/<sha256("cloud:" + k)>.json` next to
  stats.json (`SOLDAT_DATA_DIR`); file names are hashes of `k`, so a copy of
  the directory doesn't hand out the addresses.

## Join tickets (`/auth`, game issue Predator04/Tracerfall#324)

Game servers used to receive each player's raw profile id, so a modified
server could collect ids and play elsewhere as someone else. Now a joining
game gets a ticket from here instead:

1. the game server sends the joiner a random nonce (16–64 hex);
2. the game `POST /auth/ticket {"id":"<32 hex profile id>","n":"<nonce>","srv":"<address it dialled>"}`
   → `{"ok":true,"ticket","pub","exp_ms"}`;
3. the game server checks the ticket with `GET /auth/key`
   (`{"alg":"RS256","kid","pem"}`, PKIX public key) and the nonce it sent.

`ticket` = `base64(payload) + "." + base64(signature)`: payload JSON
`{"v":1,"pub","n","srv","iat","exp","kid"}` (`pub` = `sha256(lower(id))`, the
leaderboard key / reserved-name owner; 5-minute expiry), signature = RSA
PKCS#1 v1.5 over SHA-256 of the base64 payload text. A ticket only works for
the connection whose nonce it carries, so collecting them is pointless.

- **Key**: `ticket_key.pem` (RSA 2048, mode 0600) in the data dir, or
  `SOLDAT_TICKET_KEY=<path>`; made on first start. Losing / replacing it is
  harmless: tickets are short-lived and game servers fetch the key again when
  a ticket stops verifying.
- If this server can't be reached, game servers let players in unverified
  (no reserved-name rights, not ranked, no admin by id).

`go test ./...` covers cloud put / get / conflict / limits / restart and
ticket signing, nonce / tamper / wrong key / expiry checks.

## News line (`/news`)

The game's main menu shows one dismissible line under the version button,
fetched from `GET /news` (at most every 10 minutes per player):

```json
{"text":"v1.35 is out: weekly featured server!","url":"https://github.com/Predator04/Soldat-Reborn/releases","until_ms":1761609600000}
```

or `{}` when there is nothing to say. Edit `news.json` next to the binary
(`SOLDAT_DATA_DIR` moves it with stats.json and maps/, `SOLDAT_NEWS_FILE`
points at a specific file) — same fields as above; `until_ms` is the expiry
in epoch ms, `0` or absent = no expiry. Changes are picked up without a
restart. Text is plain (control characters and `[ ]` stripped, 120 chars),
the link must be http(s). Responses carry `Cache-Control: max-age=60`.
Players who hide a message with × don't see that text again; a new text
shows for everyone.

With no active news text, `/news` falls back to the official server's
featured week (`SOLDAT_FEATURED_FILE`, default
`/opt/soldat-game/featured.json`): its `note` if set, else
"This week on the official server: Rambomatch on Voland".

## Weekly featured mode/map (official game server)

`deploy/game-server.sh` (the hourly update timer runs it) reads
`/opt/soldat-game/featured.json`:

```json
{"mode":"rm","map":"Voland","note":"","until":"2026-10-25","bans":["Minigun"]}
```

- `mode` / `map`: names, aliases (`ctf`, `rm`, `gg`...) or indices the game
  accepts (`scripts/game_lists.gd`). They are passed as `--mode`/`--map`
  after the default `--mode ctf`; an unknown name is logged by the game and
  the default kept, so a typo can't take the server down.
- `bans`: weapon names (as in the host's weapon-ban grid) → `--bans=...`.
  Every start passes `--bans=` so last week's bans never linger.
- `until`: `YYYY-MM-DD` (through that day, UTC), RFC 3339, or epoch ms;
  `0`/absent = no expiry. Expired or missing = plain CTF as before.
- Any change (new week, edit, expiry) restarts `soldat-game`, through the
  same path as a new game release (`featured.stamp` holds the hash).
- The master's `/news` advertises the featured week automatically.

**Rotation:** put a plan in `/opt/soldat-game/featured-plan.json` — a list
of weeks (or `{"weeks":[...]}`) with the same fields, minus `until`:

```json
[{"mode":"rm","map":"Voland"},
 {"mode":"ctf","map":"Ash","bans":["Barrett","Minigun"]},
 {"mode":"gg","map":"Arena","note":"Gun Game week: climb the ladder on Arena"}]
```

`deploy/featured-rotate.sh [DIR]` (run by `game-server.sh` before it builds
the unit) writes `featured.json` from entry *ISO week number mod list length*,
valid through Sunday (UTC). A hand-written `featured.json` with
`"manual": true` (special events) is left alone until you remove it.

Dry runs and the script test (no root needed):

```bash
SOLDAT_GAME_DIR=/tmp/x bash deploy/game-server.sh --print-exec   # the ExecStart line
bash deploy/featured_test.sh                                     # FEATURED-SCRIPT-TEST ok
go test ./...                                                    # incl. /news
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
