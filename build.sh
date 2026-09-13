#!/usr/bin/env bash
# Cross-compile the master server for common targets. No deps beyond Go.
set -euo pipefail
cd "$(dirname "$0")"

VERSION="${1:-$(git describe --tags --always 2>/dev/null || echo dev)}"
mkdir -p dist

build() {
	local os="$1" arch="$2" ext="$3"
	local out="dist/soldat-master-${os}-${arch}${ext}"
	echo "building ${out} (${VERSION})"
	CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" go build \
		-ldflags "-s -w -X main.buildVersion=${VERSION}" -o "${out}" .
}

build linux amd64 ""
build linux arm64 ""
build linux arm ""
build windows amd64 .exe
build darwin amd64 ""
build darwin arm64 ""

ls -la dist/
