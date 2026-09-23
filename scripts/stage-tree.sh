#!/usr/bin/env bash
# Assemble an unpacked DroidProxy tree (same layout as a release tarball).
#
# Usage: scripts/stage-tree.sh <dest-dir> <version> <amd64|arm64>
# Set SKIP_CLIPROXYAPI=1 to reuse an existing libexec/cli-proxy-api in dest.
set -euo pipefail

tree="${1:?destination dir required}"
version="${2:?version required}"
arch="${3:?arch required}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

commit="$(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo unknown)"
date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
pkg="github.com/nikships/droidproxy-omarchy/internal/buildinfo"

mkdir -p "$tree/bin" "$tree/libexec" "$tree/share"

CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -C "$root" -trimpath \
  -ldflags "-s -w -X $pkg.Version=$version -X $pkg.Commit=$commit -X $pkg.BuildDate=$date" \
  -o "$tree/bin/droidproxy" ./cmd/droidproxy

if [ "${SKIP_CLIPROXYAPI:-0}" != "1" ] || [ ! -x "$tree/libexec/cli-proxy-api" ]; then
  "$root/scripts/fetch-cliproxyapi.sh" "$arch" "$tree/libexec/cli-proxy-api"
fi

install -m 0644 "$root/packaging/config.yaml" "$tree/share/config.yaml"
rm -rf "$tree/share/plugin" "$tree/share/icons" "$tree/share/systemd" "$tree/share/applications"
cp -R "$root/plugin" "$tree/share/plugin"
rm -rf "$tree/share/plugin/dev"
cp -R "$root/packaging/icons" "$tree/share/icons"
mkdir -p "$tree/share/systemd" "$tree/share/applications"
install -m 0644 "$root/packaging/systemd/droidproxy.service" "$tree/share/systemd/droidproxy.service"
install -m 0644 "$root/packaging/applications/droidproxy.desktop" "$tree/share/applications/droidproxy.desktop"
install -m 0644 "$root/LICENSE" "$tree/LICENSE"
install -m 0644 "$root/README.md" "$tree/README.md"
tr -d '[:space:]' < "$root/CLIPROXYAPI_VERSION" > "$tree/share/cliproxyapi-version"
printf '%s\n' "$version" > "$tree/VERSION"
