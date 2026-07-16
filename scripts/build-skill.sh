#!/usr/bin/env bash
#
# build-skill.sh — build the opendoc sync-engine binary into the plugin package and
# install/refresh a symlink so `~/.claude/skills/opendoc` (or a --target dir) points
# at the repo's skill/ directory.
#
# The repo's skill/ dir IS a Claude Code plugin root (the delivery unit):
# .claude-plugin/plugin.json (manifest) + SKILL.md at the root (single-skill
# layout) + references/ + a static Go binary at bin/opendoc. With the plugin enabled,
# bin/ is added to the Bash tool's PATH, so the agent can invoke `opendoc` bare.
# Installing = symlinking into ~/.claude/skills/ (a skills-directory plugin,
# auto-loaded as opendoc@skills-dir on the next session). This script only builds and
# symlinks; it never touches mirror data (~/.opendoc), credentials, or launchctl.
#
# Usage:
#   scripts/build-skill.sh                 # build + symlink ~/.claude/skills/opendoc -> repo skill/
#   scripts/build-skill.sh --target LINK   # create/refresh symlink LINK -> repo skill/ instead
#                                          # (LINK is the link path itself, e.g. /tmp/x/opendoc)
#   scripts/build-skill.sh --build-only    # build the binary, skip the symlink
#
# Idempotent: re-running rebuilds the binary and leaves an already-correct
# symlink in place. It refuses to clobber an existing NON-symlink target.

set -euo pipefail

# Resolve the repo root from this script's own location (scripts/ lives at repo root).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
SKILL_DIR="${REPO_ROOT}/skill"
BIN_DIR="${SKILL_DIR}/bin"
BIN_PATH="${BIN_DIR}/opendoc"

TARGET="${HOME}/.claude/skills/opendoc"
DO_LINK=1

while [ $# -gt 0 ]; do
  case "$1" in
    --target)
      shift
      [ $# -gt 0 ] || { echo "build-skill.sh: --target needs a directory argument" >&2; exit 2; }
      TARGET="$1"
      ;;
    --target=*)
      TARGET="${1#--target=}"
      ;;
    --build-only)
      DO_LINK=0
      ;;
    -h|--help)
      sed -n '2,23p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "build-skill.sh: unknown argument: $1" >&2
      exit 2
      ;;
  esac
  shift
done

# --- build ---------------------------------------------------------------
mkdir -p "${BIN_DIR}"
echo "building ${BIN_PATH} (static, CGO disabled) ..."
# CGO_ENABLED=0: the sqlite driver is pure Go (modernc.org/sqlite), so the binary
# stays static and single-file. Trim paths for a reproducible build;
# strip symbols (-s -w) — the embedded lark engine makes the unstripped binary
# ~15M larger.
( cd "${REPO_ROOT}" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "${BIN_PATH}" ./cmd/opendoc )
echo "built $(cd "${REPO_ROOT}" && du -h "${BIN_PATH}" | cut -f1) binary: ${BIN_PATH}"

if [ "${DO_LINK}" -eq 0 ]; then
  echo "skipping symlink (--build-only)"
  exit 0
fi

# --- install symlink -----------------------------------------------------
# Refuse to clobber a real file/dir; only ever replace a symlink of our own.
if [ -e "${TARGET}" ] && [ ! -L "${TARGET}" ]; then
  echo "build-skill.sh: refusing to overwrite ${TARGET} — it exists and is not a symlink." >&2
  echo "  Move or remove it first, then re-run." >&2
  exit 1
fi

mkdir -p "$(dirname "${TARGET}")"

if [ -L "${TARGET}" ]; then
  CURRENT="$(readlink "${TARGET}")"
  if [ "${CURRENT}" = "${SKILL_DIR}" ]; then
    echo "symlink already current: ${TARGET} -> ${SKILL_DIR}"
    exit 0
  fi
  echo "refreshing symlink: ${TARGET} (was -> ${CURRENT})"
  rm -f "${TARGET}"
fi

ln -s "${SKILL_DIR}" "${TARGET}"
echo "linked ${TARGET} -> ${SKILL_DIR}"
