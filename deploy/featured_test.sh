#!/usr/bin/env bash
# Script-level test for the weekly featured server (#264): game-server.sh's
# ExecStart generator and featured-rotate.sh, in a temp dir (no root, nothing
# installed).   bash deploy/featured_test.sh   -> "FEATURED-SCRIPT-TEST ok"
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
bad=()
gen() { SOLDAT_GAME_DIR="$T" bash "$HERE/game-server.sh" --print-exec; }
expect() { # label line want
  case "$2" in *"$3"*) ;; *) bad+=("$1: '$2' lacks '$3'");; esac
}
reject() {
  case "$2" in *"$3"*) bad+=("$1: '$2' has '$3'");; esac
}

# Missing featured.json -> plain CTF, bans cleared.
l=$(gen)
expect missing "$l" "--dedicated --port 7777 --mode ctf --bans= --register http://127.0.0.1:8080 \"--name=Tracerfall Official\""

# Active week with bans (one has a space -> quoted for systemd).
tomorrow=$(date -u -d tomorrow +%F)
printf '{"mode":"rm","map":"Voland","note":"Rambo week","until":"%s","bans":["Minigun","Steyr AUG"]}' "$tomorrow" > "$T/featured.json"
l=$(gen)
expect active "$l" "--mode ctf --mode rm --map Voland \"--bans=Minigun,Steyr AUG\" --register"

# Epoch-ms until in the future, mode name with spaces.
fut=$(( ($(date +%s) + 3600) * 1000 ))
printf '{"mode":"Capture the Flag","map":"Ash","until":%d}' "$fut" > "$T/featured.json"
l=$(gen)
expect ms "$l" "--mode \"Capture the Flag\" --map Ash --bans= --register"

# Expired -> default.
printf '{"mode":"rm","map":"Voland","until":"2001-01-01","bans":["Minigun"]}' > "$T/featured.json"
l=$(gen)
expect expired "$l" "--mode ctf --bans= --register"
reject expired "$l" "Voland"

# Garbage / injection attempts never reach the unit.
printf '{"mode":"rm\\"; rm -rf / #","map":"$(id)%%h\\\\n","bans":"Minigun,%%n"}' > "$T/featured.json"
l=$(gen)
for c in '$(' '%' ';' '\'; do reject sanitize "$l" "$c"; done
expect sanitize "$l" "--mode \"rm rm -rf\" --map id"
printf 'not json' > "$T/featured.json"
expect garbage "$(gen)" "--mode ctf --bans= --register"

# Rotation: week N picks plan[N % len]; manual featured.json is kept.
printf '[{"mode":"rm","map":"Voland"},{"mode":"ctf","map":"Ash","bans":["Barrett"]},{"mode":"dm","map":"Arena"}]' > "$T/featured-plan.json"
rm -f "$T/featured.json"
SOLDAT_FEATURED_WEEK=4 bash "$HERE/featured-rotate.sh" "$T" > /dev/null
m=$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d["mode"], d["map"], d["until"] >= sys.argv[2])' "$T/featured.json" "$(date -u +%F)")
[ "$m" = "ctf Ash True" ] || bad+=("rotate week 4: $m")
expect rotated "$(gen)" "--mode ctf --mode ctf --map Ash --bans=Barrett --register"
SOLDAT_FEATURED_WEEK=6 bash "$HERE/featured-rotate.sh" "$T" > /dev/null
expect rotated6 "$(gen)" "--mode rm --map Voland --bans= --register"
printf '{"mode":"gg","map":"Abel","manual":true}' > "$T/featured.json"
SOLDAT_FEATURED_WEEK=4 bash "$HERE/featured-rotate.sh" "$T" > /dev/null
expect manual "$(gen)" "--mode gg --map Abel"

if [ ${#bad[@]} = 0 ]; then echo "FEATURED-SCRIPT-TEST ok"; else printf 'FEATURED-SCRIPT-TEST FAIL\n'; printf '  %s\n' "${bad[@]}"; exit 1; fi
