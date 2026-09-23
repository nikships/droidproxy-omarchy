#!/usr/bin/env bash
# DroidProxy installer for Omarchy (Arch Linux).
#
#   curl -fsSL https://raw.githubusercontent.com/nikships/droidproxy-omarchy/main/install.sh | bash
#
# Downloads the latest release, verifies it (SHA-256 + ed25519 signature),
# and runs `droidproxy install`, which sets up the binary, systemd user
# service, Omarchy shell plugin, and icons.
#
# Environment:
#   DROIDPROXY_VERSION   pin a version (e.g. 1.2.3) instead of latest
#   DROIDPROXY_FEED_URL  override the latest.json feed URL
set -euo pipefail

REPO="nikships/droidproxy-omarchy"
FEED_URL="${DROIDPROXY_FEED_URL:-https://github.com/${REPO}/releases/latest/download/latest.json}"

info()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn()  { printf '\033[1;33mWARNING:\033[0m %s\n' "$*" >&2; }
fail()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

cleanup() { [[ -n "${WORK_DIR:-}" && -d "${WORK_DIR:-}" ]] && rm -rf "$WORK_DIR"; }
trap cleanup EXIT

# --- dependency checks -------------------------------------------------------

for tool in curl tar; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required but not installed."
done
if ! command -v jq >/dev/null 2>&1; then
  warn "jq not found; SHA-256 comes from SHA256SUMS instead of latest.json."
fi

# --- architecture ------------------------------------------------------------

case "$(uname -m)" in
  x86_64)  ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) fail "Unsupported architecture: $(uname -m) (need x86_64 or aarch64)" ;;
esac
PLATFORM="linux-${ARCH}"
info "Architecture: ${PLATFORM}"

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/droidproxy-install.XXXXXX")"

# --- fetch feed + tarball ----------------------------------------------------

if [[ -n "${DROIDPROXY_VERSION:-}" ]]; then
  VERSION="$DROIDPROXY_VERSION"
  info "Fetching version ${VERSION} from GitHub releases…"
  FEED_URL="https://github.com/${REPO}/releases/download/v${VERSION}/latest.json"
else
  info "Fetching latest release info…"
fi

FEED="${WORK_DIR}/latest.json"
curl -fsSL "$FEED_URL" -o "$FEED" \
  || fail "Could not download ${FEED_URL}. Check your connection or pin DROIDPROXY_VERSION."

if [[ -z "${DROIDPROXY_VERSION:-}" ]]; then
  VERSION="$(jq -r '.version // empty' "$FEED" 2>/dev/null || true)"
  [[ -n "$VERSION" ]] || fail "Feed is missing a version."
fi

TARBALL="droidproxy-${VERSION}-${PLATFORM}.tar.gz"
URL="$(jq -r --arg p "$PLATFORM" '.assets[$p].url // empty' "$FEED" 2>/dev/null || true)"
if [[ -z "$URL" ]]; then
  URL="https://github.com/${REPO}/releases/download/v${VERSION}/${TARBALL}"
  warn "Feed has no URL for ${PLATFORM}; falling back to ${URL}"
fi

info "Downloading DroidProxy ${VERSION}…"
curl -fL --progress-bar "$URL" -o "${WORK_DIR}/${TARBALL}" \
  || fail "Could not download ${URL}."

# --- verify SHA-256 ----------------------------------------------------------

EXPECTED_SHA="$(jq -r --arg p "$PLATFORM" '.assets[$p].sha256 // empty' "$FEED" 2>/dev/null || true)"
if [[ -z "$EXPECTED_SHA" ]]; then
  # Fall back to the release's SHA256SUMS file.
  if curl -fsSL "${URL%/*}/SHA256SUMS" -o "${WORK_DIR}/SHA256SUMS" 2>/dev/null; then
    EXPECTED_SHA="$(awk -v f="$TARBALL" '$2 == f { print $1 }' "${WORK_DIR}/SHA256SUMS" 2>/dev/null || true)"
  fi
fi

ACTUAL_SHA="$(sha256sum "${WORK_DIR}/${TARBALL}" | awk '{print $1}')"
if [[ -n "$EXPECTED_SHA" ]]; then
  [[ "$ACTUAL_SHA" == "$EXPECTED_SHA" ]] \
    || fail "SHA-256 mismatch for ${TARBALL}:
  expected: ${EXPECTED_SHA}
  actual:   ${ACTUAL_SHA}
The download may be corrupted or tampered with. Aborting."
  info "SHA-256 verified."
else
  warn "No SHA-256 available (feed had none and no SHA256SUMS found); continuing with signature check only."
fi

# --- verify ed25519 signature ------------------------------------------------

SIG_URL="${URL}.sig"
SIG_FILE="${WORK_DIR}/${TARBALL}.sig"
SIGNATURE_OK=0
if curl -fsSL "$SIG_URL" -o "$SIG_FILE" 2>/dev/null; then
  if command -v openssl >/dev/null 2>&1; then
    # The public key is a raw 32-byte ed25519 key stored base64; openssl needs
    # DER (302a300506032b6570032100 + key) wrapped in PEM.
    PUB_B64="CZQkgDPfujZE1bt3q5HxTyWjvWwSWEQ7iRlbkJ4Ehpk="
    printf '\x30\x2a\x30\x05\x06\x03\x2b\x65\x70\x03\x21\x00' > "${WORK_DIR}/pubkey.der"
    printf '%s' "$PUB_B64" | base64 -d >> "${WORK_DIR}/pubkey.der"
    {
      echo "-----BEGIN PUBLIC KEY-----"
      base64 -w0 < "${WORK_DIR}/pubkey.der"
      echo
      echo "-----END PUBLIC KEY-----"
    } > "${WORK_DIR}/pubkey.pem"
    openssl base64 -d -A -in "$SIG_FILE" -out "${SIG_FILE}.raw"
    if openssl pkeyutl -verify -pubin -inkey "${WORK_DIR}/pubkey.pem" -rawin \
         -in "${WORK_DIR}/${TARBALL}" -sigfile "${SIG_FILE}.raw" >/dev/null 2>&1; then
      SIGNATURE_OK=1
      info "ed25519 signature verified."
    else
      fail "ed25519 signature verification FAILED for ${TARBALL}. The download may be tampered with. Aborting."
    fi
  else
    warn "openssl not found; skipping ed25519 signature check (SHA-256 was verified)."
    SIGNATURE_OK=1
  fi
else
  fail "No signature file at ${SIG_URL}; refusing to install an unsigned release."
fi
[[ "$SIGNATURE_OK" == "1" ]] || fail "Signature could not be verified."

# --- extract + install -------------------------------------------------------

info "Extracting…"
tar -xzf "${WORK_DIR}/${TARBALL}" -C "$WORK_DIR"
SRC_DIR="${WORK_DIR}/droidproxy-${VERSION}-${PLATFORM}"
[[ -x "${SRC_DIR}/bin/droidproxy" ]] || fail "Release tree is missing bin/droidproxy."

info "Installing…"
"${SRC_DIR}/bin/droidproxy" install --from "$SRC_DIR"

# --- next steps --------------------------------------------------------------

cat <<EOF

DroidProxy ${VERSION} is installed.

Next steps:
  • The menu bar widget is enabled in the Omarchy shell; if it does not
    appear, run: omarchy plugin enable nikships.droidproxy
  • Start it now (usually automatic):  systemctl --user --now enable droidproxy.service
  • Open the settings panel:           droidproxy open
  • Follow the logs:                   droidproxy logs

To uninstall later: droidproxy uninstall        (add --purge to delete data)
EOF
