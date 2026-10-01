#!/usr/bin/env bash
# Update the live master server from GitHub and restart it.
# Run on the server:  curl -fsSL https://raw.githubusercontent.com/Predator04/soldat-master-server/master/deploy/update.sh | sudo bash
set -euo pipefail
cd /opt/soldat
rm -rf src.new && mkdir src.new
curl -fsSL https://codeload.github.com/Predator04/soldat-master-server/tar.gz/refs/heads/master | tar xz -C src.new --strip-components=1
cd src.new
HOME=/root GOCACHE=/root/.gocache GOTOOLCHAIN=local go build -o ../soldat-master.new .
cd ..
mv -f soldat-master.new soldat-master
systemctl restart soldat-master
sleep 1
systemctl is-active soldat-master && curl -s localhost:8080/health; echo
