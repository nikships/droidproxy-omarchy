#!/usr/bin/env bash
# Build one release tarball.
#
# Usage: scripts/build-release.sh <version> <amd64|arm64> [outdir]
#
# Produces <outdir>/droidproxy-<version>-linux-<arch>.tar.gz with the layout
# the installer and updater expect:
#   bin/droidproxy  libexec/cli-proxy-api  share/{config.yaml,plugin,icons,systemd,applications}
#   LICENSE  README.md  VERSION
set -euo pipefail

version="${1:?version required}"
arch="${2:?arch (amd64|arm64) required}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
outdir="${3:-$root/dist}"

name="droidproxy-${version}-linux-${arch}"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
tree="$stage/$name"

"$root/scripts/stage-tree.sh" "$tree" "$version" "$arch"

mkdir -p "$outdir"
tar -C "$stage" --owner=0 --group=0 --numeric-owner --sort=name \
  -czf "$outdir/$name.tar.gz" "$name"
echo "built $outdir/$name.tar.gz"
