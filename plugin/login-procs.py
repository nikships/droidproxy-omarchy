#!/usr/bin/env python3
"""List or stop DroidProxy OAuth login helpers.

The released daemon leaves `cli-proxy-api -claude-login` (and the Codex,
Antigravity, and Kimi equivalents) running after Add account returns. That
helper holds the callback port. This script only matches those argv flags on
a cli-proxy-api binary. It does not signal the proxy server.
"""

import os
import signal
import sys

FLAGS = {
    "claude": b"-claude-login",
    "codex": b"-codex-login",
    "antigravity": b"-antigravity-login",
    "kimi": b"-kimi-login",
}


def cmdline(pid):
    try:
        raw = open("/proc/%s/cmdline" % pid, "rb").read()
    except OSError:
        return None
    if not raw:
        return None
    parts = raw.split(b"\0")
    if parts and parts[-1] == b"":
        parts.pop()
    return parts


def is_helper(parts, flag):
    if not parts or flag not in parts:
        return False
    base = os.path.basename(parts[0])
    return base == b"cli-proxy-api" or base == b"cli-proxy-api-plus"


def find():
    found = {name: [] for name in FLAGS}
    try:
        names = os.listdir("/proc")
    except OSError:
        return found
    for name in names:
        if not name.isdigit():
            continue
        parts = cmdline(name)
        if not parts:
            continue
        for provider, flag in FLAGS.items():
            if is_helper(parts, flag):
                found[provider].append(int(name))
    return found


def scan():
    found = find()
    bits = []
    for provider in FLAGS:
        pids = ",".join(str(pid) for pid in found[provider])
        bits.append("%s=%s" % (provider, pids))
    print(" ".join(bits))


def cancel(provider):
    flag = FLAGS.get(provider)
    if flag is None:
        return 2
    killed = 0
    for pid in find().get(provider, []):
        parts = cmdline(str(pid))
        if not parts or not is_helper(parts, flag):
            continue
        try:
            os.kill(pid, signal.SIGTERM)
            killed += 1
        except OSError:
            pass
    print(killed)
    return 0


def main():
    if len(sys.argv) < 2:
        return 2
    cmd = sys.argv[1]
    if cmd == "scan":
        scan()
        return 0
    if cmd == "cancel" and len(sys.argv) == 3:
        return cancel(sys.argv[2])
    return 2


if __name__ == "__main__":
    sys.exit(main())
