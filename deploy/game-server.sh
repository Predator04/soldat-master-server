#!/usr/bin/env bash
# Install / update the always-on official Soldat Reborn game server on this box
# (next to the master server). Safe to run again; it is also what the hourly
# update timer runs.
#   curl -fsSL https://raw.githubusercontent.com/Predator04/soldat-master-server/master/deploy/game-server.sh | sudo bash
set -euo pipefail
GODOT_VER="4.7.2"
REPO="Predator04/Soldat-Reborn"
PUBLIC_IP="${PUBLIC_IP:-161.153.9.69}"
NAME="${SERVER_NAME:-Soldat Reborn Official}"
DIR=/opt/soldat-game
mkdir -p "$DIR"; cd "$DIR"

# 1 GB VM: a little swap so a spike never gets the game killed.
if ! swapon --show | grep -q /swapfile; then
  fallocate -l 1G /swapfile && chmod 600 /swapfile && mkswap /swapfile >/dev/null && swapon /swapfile
  grep -q /swapfile /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
fi
command -v unzip >/dev/null || DEBIAN_FRONTEND=noninteractive apt-get install -y unzip >/dev/null

# Godot (headless runs the exported game pack).
if [ ! -x "godot-$GODOT_VER" ]; then
  curl -fsSL -o godot.zip "https://github.com/godotengine/godot/releases/download/${GODOT_VER}-stable/Godot_v${GODOT_VER}-stable_linux.x86_64.zip"
  unzip -o -q godot.zip && rm godot.zip
  mv -f "Godot_v${GODOT_VER}-stable_linux.x86_64" "godot-$GODOT_VER"; chmod +x "godot-$GODOT_VER"
fi

# Latest release's game pack; only download when it changed.
URL=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(next((a["browser_download_url"]+" "+a["updated_at"] for a in d.get("assets",[]) if a["name"]=="SoldatReborn.pck"),""))')
CHANGED=0
if [ -n "$URL" ]; then
  set -- $URL
  if [ "$(cat pck.stamp 2>/dev/null)" != "$2" ]; then
    curl -fsSL -o SoldatReborn.pck.new "$1" && mv -f SoldatReborn.pck.new SoldatReborn.pck && echo "$2" > pck.stamp && CHANGED=1
  fi
fi
[ -f SoldatReborn.pck ] || { echo "no SoldatReborn.pck in the latest release yet"; exit 1; }

cat > /etc/systemd/system/soldat-game.service <<UNIT
[Unit]
Description=Soldat Reborn official game server
After=network-online.target soldat-master.service
Wants=network-online.target
[Service]
WorkingDirectory=$DIR
ExecStart=$DIR/godot-$GODOT_VER --headless --main-pack $DIR/SoldatReborn.pck -- --dedicated --port 7777 --mode ctf --register http://127.0.0.1:8080 --name=$NAME
Restart=always
RestartSec=5
DynamicUser=yes
StateDirectory=soldat-game
Environment=HOME=/var/lib/soldat-game
Nice=5
MemoryMax=600M
[Install]
WantedBy=multi-user.target
UNIT
sed -i "s|--name=$NAME|\"--name=$NAME\"|" /etc/systemd/system/soldat-game.service

# Master advertises this box's public IP for the loopback registration.
mkdir -p /etc/systemd/system/soldat-master.service.d
DROPIN=/etc/systemd/system/soldat-master.service.d/public-ip.conf
WANT=$(printf '[Service]\nEnvironment=SOLDAT_PUBLIC_IP=%s' "$PUBLIC_IP")
MASTER_RESTART=0
if [ "$(cat "$DROPIN" 2>/dev/null)" != "$WANT" ]; then echo "$WANT" > "$DROPIN"; MASTER_RESTART=1; fi

# Game (ENet) + server query ports, UDP.
for p in 7777 7778; do
  iptables -C INPUT -p udp --dport $p -j ACCEPT 2>/dev/null || iptables -I INPUT 1 -p udp --dport $p -j ACCEPT
done
command -v netfilter-persistent >/dev/null && netfilter-persistent save >/dev/null || iptables-save > /etc/iptables/rules.v4

# Hourly: pick up a new release automatically.
cat > /etc/systemd/system/soldat-game-update.service <<UNIT
[Unit]
Description=Update the Soldat Reborn game server from the latest release
[Service]
Type=oneshot
ExecStart=/bin/bash -c 'curl -fsSL https://raw.githubusercontent.com/Predator04/soldat-master-server/master/deploy/game-server.sh | bash'
UNIT
cat > /etc/systemd/system/soldat-game-update.timer <<UNIT
[Unit]
Description=Hourly Soldat Reborn game server update check
[Timer]
OnBootSec=10min
OnUnitActiveSec=1h
[Install]
WantedBy=timers.target
UNIT

systemctl daemon-reload
systemctl enable --now soldat-game-update.timer >/dev/null
[ "$MASTER_RESTART" = 1 ] && systemctl restart soldat-master  # only when the setting changed: a restart drops live relay games
if [ "$CHANGED" = 1 ] || ! systemctl is-active -q soldat-game; then
  systemctl enable soldat-game >/dev/null 2>&1; systemctl restart soldat-game
fi
sleep 3
systemctl is-active soldat-game soldat-master
