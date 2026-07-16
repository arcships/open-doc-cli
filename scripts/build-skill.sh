#!/usr/bin/env bash
#
# build-skill.sh — build the opendoc sync-engine binary into the plugin package.
#
# The repo's skill/ dir IS the plugin root (the delivery unit), dual-manifested
# for both supported agents:
#   .claude-plugin/plugin.json   Claude Code manifest
#   .codex-plugin/plugin.json    Codex manifest ("skills": "./skills/")
#   skills/opendoc/              the skill: SKILL.md + references/ + scripts/
#   bin/opendoc                  the engine binary (this script's output; never
#                                committed — end users get it via the skill's
#                                scripts/download-binary.sh instead)
#
# Under Claude Code an enabled plugin's bin/ is on the Bash tool's PATH; Codex
# has no such mechanism, so SKILL.md instructs agents to call the binary by
# path there. End-user distribution is the arcships/plugins marketplace repo
# (git-subdir entries pointing at this repo's skill/); the catalogs in THIS
# repo's root (.claude-plugin/marketplace.json, .agents/plugins/marketplace.json)
# are the dev-only "arcships-dev" marketplace that installs from the working
# tree. This script only builds — it never touches mirror data (~/.opendoc),
# credentials, or launchctl.
#
# Local development install (point the marketplace at your checkout):
#   Claude Code:  /plugin marketplace add /path/to/open-doc-cli
#                 /plugin install opendoc@arcships-dev
#   Codex:        codex plugin marketplace add /path/to/open-doc-cli
#                 codex plugin add opendoc@arcships-dev
#
# Usage:
#   scripts/build-skill.sh        # build skill/bin/opendoc
#
# Idempotent: re-running just rebuilds the binary.

set -euo pipefail

# Resolve the repo root from this script's own location (scripts/ lives at repo root).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
SKILL_DIR="${REPO_ROOT}/skill"
BIN_DIR="${SKILL_DIR}/bin"
BIN_PATH="${BIN_DIR}/opendoc"

case "${1:-}" in
  -h|--help)
    sed -n '2,32p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit 0
    ;;
  "")
    ;;
  *)
    echo "build-skill.sh: unknown argument: $1" >&2
    exit 2
    ;;
esac

# --- build ---------------------------------------------------------------
mkdir -p "${BIN_DIR}"
echo "building ${BIN_PATH} (static, CGO disabled) ..."
# CGO_ENABLED=0: the sqlite driver is pure Go (modernc.org/sqlite), so the binary
# stays static and single-file. Trim paths for a reproducible build;
# strip symbols (-s -w) — the embedded lark engine makes the unstripped binary
# ~15M larger.
( cd "${REPO_ROOT}" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "${BIN_PATH}" ./cmd/opendoc )
echo "built $(cd "${REPO_ROOT}" && du -h "${BIN_PATH}" | cut -f1) binary: ${BIN_PATH}"
echo
echo "plugin package ready at: ${SKILL_DIR}"
echo "local install: /plugin marketplace add ${REPO_ROOT}   (Claude Code)"
echo "               codex plugin marketplace add ${REPO_ROOT}   (Codex)"
echo "               then install plugin 'opendoc' from marketplace 'arcships-dev'"
