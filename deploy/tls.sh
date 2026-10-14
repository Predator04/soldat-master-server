#!/usr/bin/env bash
# TLS for the soldat master server (#252). Run ON THE SERVER (ubuntu@...):
#   sudo DOMAIN=play.example.com bash tls.sh
# Prerequisites (the only human steps):
#   1. Own a domain (any registrar) or a subdomain
#   2. Create an A record  <sub>.yourdomain.tld  ->  161.153.9.69
#   3. Wait for DNS to propagate (dig +short <domain> shows the IP)
# What this does:
#   - installs Caddy (Let's Encrypt, auto-renew)
#   - proxies https://<domain> -> 127.0.0.1:8080 (and wss:// for /relay)
#   - opens 80/443 in the Ubuntu firewall (Oracle's security list needs the
#     same two rules added by hand in the console — see the echo at the end)
#   - rewrites the systemd unit's master URL bits? No — the game's default URL
#     changes in a game release (Settings.DEFAULT_MASTER_URL), this only does
#     the server side.
set -euo pipefail
DOMAIN="${DOMAIN:?set DOMAIN=your.domain.example}"

apt-get update -qq
apt-get install -yqq debian-keyring debian-archive-keyring apt-transport-https curl >/dev/null
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --batch --yes --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null
apt-get update -qq && apt-get install -yqq caddy >/dev/null

cat > /etc/caddy/Caddyfile <<CADDY
$DOMAIN {
	reverse_proxy 127.0.0.1:8080
}
CADDY

# Firewall: Oracle Ubuntu images use RAW iptables with a catch-all REJECT —
# `ufw allow` silently does nothing there (found the hard way). Insert ACCEPT
# rules ahead of the REJECT and persist.
iptables -C INPUT -p tcp --dport 80 -j ACCEPT 2>/dev/null || iptables -I INPUT 1 -p tcp --dport 80 -j ACCEPT
iptables -C INPUT -p tcp --dport 443 -j ACCEPT 2>/dev/null || iptables -I INPUT 2 -p tcp --dport 443 -j ACCEPT
mkdir -p /etc/iptables
iptables-save > /etc/iptables/rules.v4

# Optional: bind the Go server to loopback only (plain 8080 goes away) once
# every player build defaults to https — run with LOOPBACK=1 when that day
# comes, and drop the 8080 Oracle ingress rule too.
if [ "${LOOPBACK:-0}" = "1" ]; then
  mkdir -p /etc/systemd/system/soldat-master.service.d
  cat > /etc/systemd/system/soldat-master.service.d/tls.conf <<UNIT
[Service]
ExecStart=
ExecStart=/opt/soldat/soldat-master -port 8080 -addr 127.0.0.1
UNIT
fi
systemctl daemon-reload
systemctl enable --now caddy
systemctl restart caddy soldat-master
sleep 3
systemctl is-active caddy soldat-master
curl -fsS "https://$DOMAIN/health" && echo
echo "TLS is live on https://$DOMAIN"
echo "REMAINING MANUAL STEP: in the Oracle console add ingress rules for"
echo "TCP 80 and TCP 443 (0.0.0.0/0) to the VCN's default security list."
