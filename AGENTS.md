# AGENTS.md

## What opendoc is

opendoc is a one-way sync engine that mirrors documents you've authorized in
Notion and Feishu into a local, read-only Markdown tree. It never writes
back to the source platform. The mirror is meant to be consumed directly by
coding agents with Grep/Glob/Read — no API calls, no credentials, no network
access needed at read time. A companion plugin (`plugin/`) wraps the `opendoc`
binary and exposes it to agents as a skill. The one package is dual-manifested —
`.claude-plugin/plugin.json` for Claude Code, `.codex-plugin/plugin.json` for
Codex — and is distributed only through plugin marketplaces: end users install
from the separate catalog repo `arcships/plugins` (git-subdir entries that
sparse-fetch just `plugin/`); the catalogs at THIS repo's root are the dev-only
`arcships-dev` marketplace, installing from the local working tree.

## Repo layout

- `cmd/opendoc` — CLI entrypoint (`main.go`), wires subcommands to `internal/cli`.
- `internal/adapter` — platform-agnostic contract between the engine and a
  concrete source (Notion, Feishu, ...); the engine only depends on this interface.
- `internal/cli` — subcommand dispatch and flag parsing (init, sync, status,
  doctor, resolve, schedule).
- `internal/config` — on-disk mirror configuration (`.internal/config.toml`)
  and its readers/writers.
- `internal/engine` — the sync pipeline: fetch, diff, write, index, manifest bookkeeping.
- `internal/feishu` — Feishu/Lark adapter (bitable, doc fetch, metadata).
- `internal/frontmatter` — deterministic YAML frontmatter rendering for mirrored files.
- `internal/layout` — mirror-root resolution and on-disk directory layout.
- `internal/manifest` — `manifest.sqlite`, the sync engine's bookkeeping ledger.
- `internal/naming` — filesystem-safe path/file naming and collision rules.
- `internal/notion` — Notion adapter (query, fetch, properties→frontmatter mapping).
- `internal/ratelimit` — token bucket + backoff helpers for platform QPS limits.

## Build & test

```
go build ./...
go test ./...
./scripts/build-skill.sh   # builds the dev engine binary (plugin/bin/opendoc-dev)
```

## Key invariants

- **Fidelity-first**: never silently drop content. Anything that can't be
  rendered faithfully gets a placeholder tag, and the loss is counted in the
  sync report — not swallowed.
- **Mirror is read-only**: nothing in the mirrored tree is ever written back
  to Notion or Feishu.
- **Manifest keys are platform-native IDs**: the sync ledger is keyed by the
  source platform's own document/page/row IDs, not derived or local ones.
- **content_hash is computed before link rewriting**: the hash that drives
  change detection reflects the fetched content, not the post-processed
  (link-rewritten) output.
- **Doctor codes are stable identifiers**: probe/failure codes (e.g. `F1`,
  `F2-NOAUTH`, `N2-INVALID`, `N3-EMPTY`, `G2-QUOTA-LOW`) and exit codes (e.g.
  `3` = `ExitNotInitialized`) are part of the tool's contract. Don't rename
  or renumber them — scripts and docs key off the literal strings.

## `plugin/skills/opendoc/SKILL.md`

This file is runtime instructions read by coding agents, not just
documentation. Its semantics (what it tells an agent to do, and when) must
not drift casually — treat behavioral changes there with the same care as a
CLI flag change.
