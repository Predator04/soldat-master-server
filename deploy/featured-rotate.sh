#!/usr/bin/env bash
# Weekly featured mode/map rotation for the official game server (#264).
#   bash featured-rotate.sh [DIR]          (DIR default /opt/soldat-game)
# Reads DIR/featured-plan.json — a list of weeks:
#   [{"mode":"rm","map":"Voland","note":"","bans":[]},
#    {"mode":"ctf","map":"Ash","bans":["Minigun","Barrett"]}, ...]
# (or {"weeks":[...]}) and writes DIR/featured.json with the entry for the
# current ISO week (entry = ISO week number mod list length), valid until the
# end of that week (Sunday, UTC). game-server.sh runs this every hour before
# it builds the systemd unit, so a new week switches the server by itself.
# An entry may set "until" itself; a featured.json with "manual": true is
# left alone (hand-set special events win over the plan).
# SOLDAT_FEATURED_WEEK=N overrides the week number (tests).
set -euo pipefail
DIR="${1:-${SOLDAT_GAME_DIR:-/opt/soldat-game}}"
PLAN="$DIR/featured-plan.json"
OUTF="$DIR/featured.json"
[ -f "$PLAN" ] || { echo "featured-rotate: no $PLAN"; exit 0; }
python3 - "$PLAN" "$OUTF" <<'PY'
import datetime, json, os, sys
plan_path, out_path = sys.argv[1], sys.argv[2]
try:
    plan = json.load(open(plan_path))
except Exception as e:
    print("featured-rotate: bad plan (%s), leaving featured.json as is" % e)
    sys.exit(0)
if isinstance(plan, dict):
    plan = plan.get("weeks", [])
plan = [e for e in plan if isinstance(e, dict)] if isinstance(plan, list) else []
if not plan:
    print("featured-rotate: empty plan")
    sys.exit(0)
try:
    cur = json.load(open(out_path))
    if isinstance(cur, dict) and cur.get("manual"):
        print("featured-rotate: featured.json is manual, not touching it")
        sys.exit(0)
except Exception:
    pass
today = datetime.datetime.now(datetime.timezone.utc).date()
year, week, wday = today.isocalendar()
week = int(os.environ.get("SOLDAT_FEATURED_WEEK", week))
entry = dict(plan[week % len(plan)])
sunday = today + datetime.timedelta(days=7 - wday)
entry.setdefault("until", sunday.isoformat())   # through Sunday (UTC)
entry["week"] = "%d-W%02d" % (year, week)
entry.pop("manual", None)
data = json.dumps(entry, indent=2, sort_keys=True) + "\n"
try:
    if open(out_path).read() == data:
        sys.exit(0)
except Exception:
    pass
tmp = out_path + ".tmp"
with open(tmp, "w") as fh:
    fh.write(data)
os.chmod(tmp, 0o644)
os.replace(tmp, out_path)
print("featured-rotate: week %s -> %s on %s" % (entry["week"], entry.get("mode", "?"), entry.get("map", "?")))
PY
