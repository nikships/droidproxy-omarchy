#!/usr/bin/env bash
# Download the pinned CLIProxyAPI release for one architecture, verify it
# against upstream's checksums.txt, and place the binary at DEST.
#
# Usage: scripts/fetch-cliproxyapi.sh <amd64|arm64> <dest-path> [version]
set -euo pipefail

arch="${1:?arch (amd64|arm64) required}"
dest="${2:?destination path required}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="${3:-$(tr -d '[:space:]' < "$root/CLIPROXYAPI_VERSION")}"

case "$arch" in
  amd64) upstream_arch="amd64" ;;
  arm64) upstream_arch="aarch64" ;;
  *) echo "unsupported arch: $arch" >&2; exit 1 ;;
esac

file="CLIProxyAPI_${version}_linux_${upstream_arch}.tar.gz"
base="https://github.com/router-for-me/CLIProxyAPI/releases/download/v${version}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL --retry 3 --retry-delay 5 -o "$tmp/$file" "$base/$file"
curl -fsSL --retry 3 --retry-delay 5 -o "$tmp/checksums.txt" "$base/checksums.txt"

expected="$(awk -v f="$file" '$2 == f { print $1 }' "$tmp/checksums.txt")"
if [ -z "$expected" ]; then
  echo "no checksum for $file in upstream checksums.txt" >&2
  exit 1
fi
actual="$(sha256sum "$tmp/$file" | awk '{print $1}')"
if [ "$expected" != "$actual" ]; then
  echo "checksum mismatch for $file: expected $expected, got $actual" >&2
  exit 1
fi

tar -xzf "$tmp/$file" -C "$tmp" cli-proxy-api
mkdir -p "$(dirname "$dest")"
install -m 0755 "$tmp/cli-proxy-api" "$dest"
echo "CLIProxyAPI ${version} (linux/${arch}) -> ${dest}"
