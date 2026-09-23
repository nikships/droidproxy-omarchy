#!/usr/bin/env bash
# Development loop: build an unpacked tree into build/dev and run the daemon
# in the foreground against it. Stops the installed service first so ports
# 8317/8318/8319 and the control socket are free; restart it afterwards with
# `systemctl --user start droidproxy`.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
arch="$(go env GOARCH)"
tree="$root/build/dev"

SKIP_CLIPROXYAPI=1 "$root/scripts/stage-tree.sh" "$tree" "0.0.0-dev" "$arch"

if systemctl --user is-active --quiet droidproxy.service 2>/dev/null; then
  echo "stopping installed droidproxy.service for the dev session"
  systemctl --user stop droidproxy.service
fi

exec env DROIDPROXY_ROOT="$tree" "$tree/bin/droidproxy" serve "$@"
