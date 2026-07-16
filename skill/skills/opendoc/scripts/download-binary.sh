#!/usr/bin/env bash
#
# download-binary.sh — fetch the platform-correct prebuilt opendoc engine binary
# from GitHub Releases into the plugin's bin/opendoc, verified by sha256.
#
# This is how an end user (who has no Go toolchain and no source checkout) gets
# the engine: when the binary is missing, SKILL.md's bootstrap rule has the agent
# run this script (with the user's OK); it detects the
# platform, downloads the matching release asset, checks it against the release's
# checksums.txt, and installs it at <plugin-root>/bin/opendoc. Developers don't
# need this — they build from source with scripts/build-skill.sh instead.
#
# Resolution is self-contained (locates itself via BASH_SOURCE and walks up to the
# plugin root — the nearest ancestor holding a .claude-plugin/ or .codex-plugin/
# manifest), so it works the same under a Claude Code or Codex marketplace install
# and in a repo checkout. The release tag is read from the plugin manifest's
# version, so that manifest is the single source of truth — no version is
# hard-coded here.
#
# Overrides (env):
#   OPENDOC_REPO   owner/repo to download from  (default: arcships/open-doc-cli)
#
# Usage:
#   scripts/download-binary.sh            # install/refresh <root>/bin/opendoc

set -euo pipefail

REPO="${OPENDOC_REPO:-arcships/open-doc-cli}"

die() { echo "download-binary.sh: $*" >&2; exit 1; }

# --- locate the plugin root (this script lives at <root>/skills/opendoc/scripts/) ---
# Walk up from the script to the nearest directory holding a plugin manifest, so
# the script keeps working if the skill nesting depth ever changes.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT=""
probe="${SCRIPT_DIR}"
for _ in 1 2 3 4 5; do
  probe="$(cd "${probe}/.." && pwd)"
  if [ -f "${probe}/.claude-plugin/plugin.json" ] || [ -f "${probe}/.codex-plugin/plugin.json" ]; then
    ROOT="${probe}"
    break
  fi
done
[ -n "${ROOT}" ] || die "could not locate the plugin root: no .claude-plugin/ or .codex-plugin/ manifest in any ancestor of ${SCRIPT_DIR}"
BIN_DIR="${ROOT}/bin"
BIN_PATH="${BIN_DIR}/opendoc"
MANIFEST="${ROOT}/.claude-plugin/plugin.json"
[ -f "${MANIFEST}" ] || MANIFEST="${ROOT}/.codex-plugin/plugin.json"

# --- read the version from the plugin manifest → release tag -----------------
if command -v jq >/dev/null 2>&1; then
  VERSION="$(jq -r '.version' "${MANIFEST}")"
else
  # jq-less fallback: pull the first "version": "..." string.
  VERSION="$(grep -o '"version"[[:space:]]*:[[:space:]]*"[^"]*"' "${MANIFEST}" | head -1 | sed 's/.*"\([^"]*\)"$/\1/')"
fi
[ -n "${VERSION}" ] && [ "${VERSION}" != "null" ] || die "could not read version from ${MANIFEST}"
TAG="v${VERSION}"

# --- detect platform → asset name --------------------------------------------
case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  *) die "unsupported OS '$(uname -s)'. Build from source instead: scripts/build-skill.sh" ;;
esac
case "$(uname -m)" in
  arm64|aarch64) ARCH=arm64 ;;
  x86_64|amd64)  ARCH=amd64 ;;
  *) die "unsupported arch '$(uname -m)'. Build from source instead: scripts/build-skill.sh" ;;
esac
ASSET="opendoc-${OS}-${ARCH}"

echo "opendoc ${TAG} · ${ASSET} · repo ${REPO}" >&2

# --- sha256 helper (Linux: sha256sum, macOS: shasum -a 256) ------------------
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# --- fetch <asset-name> <out-path> -------------------------------------------
fetch_asset() {
  local name="$1" out="$2"
  curl -fsSL "https://github.com/${REPO}/releases/download/${TAG}/${name}" -o "${out}"
}

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

# --- checksums first, so we know the expected digest before downloading ------
echo "fetching checksums.txt ..." >&2
fetch_asset "checksums.txt" "${TMP}/checksums.txt" \
  || die "could not fetch release ${TAG} from ${REPO} — the release may not exist yet."
EXPECTED="$(grep " ${ASSET}\$" "${TMP}/checksums.txt" | awk '{print $1}' | head -1)"
[ -n "${EXPECTED}" ] || die "no checksum for ${ASSET} in release ${TAG} (is this platform published?)"

# --- idempotent: skip if the installed binary already matches ----------------
if [ -f "${BIN_PATH}" ] && [ "$(sha256_of "${BIN_PATH}")" = "${EXPECTED}" ]; then
  echo "already up to date: ${BIN_PATH} (${ASSET} ${TAG})" >&2
  exit 0
fi

# --- download, verify, then install atomically -------------------------------
echo "downloading ${ASSET} ..." >&2
fetch_asset "${ASSET}" "${TMP}/opendoc" || die "failed to download ${ASSET} from release ${TAG}"
ACTUAL="$(sha256_of "${TMP}/opendoc")"
[ "${ACTUAL}" = "${EXPECTED}" ] || die "checksum mismatch for ${ASSET}: expected ${EXPECTED}, got ${ACTUAL} — refusing to install"

chmod +x "${TMP}/opendoc"
mkdir -p "${BIN_DIR}"
mv -f "${TMP}/opendoc" "${BIN_PATH}"
echo "installed ${BIN_PATH} ($(sha256_of "${BIN_PATH}" | cut -c1-12)… ${ASSET} ${TAG})" >&2
