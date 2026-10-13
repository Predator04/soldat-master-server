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

# The Go server binds 0.0.0.0:8080; keep it loopback-only once Caddy fronts it.
# (Leave 8080 open for one game release so old clients transition, then drop
# the Oracle ingress rule for 8080.)
mkdir -p /etc/systemd/system/soldat-master.service.d
cat > /etc/systemd/system/soldat-master.service.d/tls.conf <<UNIT
[Service]
ExecStart=
ExecStart=/opt/soldat/soldat-master -port 8080 -addr 127.0.0.1
UNIT

ufw allow 80/tcp 2>/dev/null || true
ufw allow 443/tcp 2>/dev/null || true
systemctl daemon-reload
systemctl enable --now caddy
systemctl restart caddy soldat-master
sleep 3
systemctl is-active caddy soldat-master
curl -fsS "https://$DOMAIN/health" && echo
echo "TLS is live on https://$DOMAIN"
echo "REMAINING MANUAL STEP: in the Oracle console add ingress rules for"
echo "TCP 80 and TCP 443 (0.0.0.0/0) to the VCN's default security list."
