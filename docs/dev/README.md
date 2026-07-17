# Contributor Onboarding Guide

> What opendoc is and why it's designed this way → read the root [README](../../README.md) first (5 minutes). This directory answers "I want to change the code, where do I start?"

## Environment Setup

All you need is **Go ≥ 1.25**. No cgo (sqlite uses the pure-Go modernc driver), no C toolchain requirement — building and testing **do not require** network access or credentials (the Feishu engine lark-cli is embedded into the opendoc binary as a Go dependency; `go build` fetches it automatically):

```bash
git clone <repo> && cd open-doc-cli
go build ./...
go test ./...        # all green means the environment is ready, ~20 seconds
```

Runtime credentials are only needed if you want to actually run a sync (not required): Feishu authorization (`opendoc init` walks you through it — the embedded engine handles QR-code app creation + login, zero install) and a Notion integration token. See [plugin/skills/opendoc/references/setup.md](../../plugin/skills/opendoc/references/setup.md) for the configuration flow — that's a script written for an agent to guide a user through, but a human can follow it too.

## Running Locally (Without Polluting Your Real Mirror)

```bash
go run ./cmd/opendoc --root /tmp/opendoc-dev init --no-input --notion-token-env NOTION_TOKEN
go run ./cmd/opendoc --root /tmp/opendoc-dev doctor        # see skip/fail behavior when credentials aren't configured
go run ./cmd/opendoc --root /tmp/opendoc-dev sync
```

`--root` (or `OPENDOC_ROOT`) isolates all state (config, manifest, mirror tree) to the given directory — delete it and start fresh. The default root is `~/.opendoc` — always pass `--root` explicitly during development to avoid accidentally writing real data.

## Reading Path

1. Root [README](../../README.md) — why the project exists, the product shape, design principles.
2. [architecture.md](architecture.md) — the layering, the Adapter contract, the full flow of one sync, what each package is responsible for. **Required reading before changing code — this is the spec of record.**
3. [testing.md](testing.md) — how tests are organized, mock patterns, fixture red lines. **Required reading before writing tests.**
4. Reference material (consult as needed): [notion-properties-mapping.md](../notion-properties-mapping.md) — how Notion database properties land in frontmatter; [plugin/skills/opendoc/references/degradation-tags.md](../../plugin/skills/opendoc/references/degradation-tags.md) — the exact degradation markers the engine emits.

## Repository Map

```
cmd/opendoc/       Entry point (3 lines, all logic lives in internal/cli)
internal/
├── cli/           Subcommands + exit code contract + doctor probes
├── engine/        Sync pipeline (platform-agnostic core)
├── adapter/       Adapter interface — the only boundary between engine and platforms, comments are the spec
├── notion/        Notion adapter (pure net/http)
├── feishu/        Feishu adapter (self-exec embedded lark engine, via the Runner interface)
├── manifest/      manifest.sqlite ledger
├── layout/        Path resolution (--root > OPENDOC_ROOT > ~/.opendoc)
├── naming/        File name normalization rule chain
├── frontmatter/   Hand-rolled frontmatter rendering (deterministic key order)
├── config/        config.toml + .internal/env fallback
└── ratelimit/     Token bucket + backoff
plugin/            plugin package for Claude Code + Codex (SKILL.md + bin/ shim + references/)
scripts/           build-skill.sh (build + install the plugin, see the comment header in the file)
docs/              Reference material + this directory (evergreen docs only)
```

## Contribution Conventions

- **Commit messages are in English** (docs and discussion may be in Chinese), conventional-commit style: `feat: ...` / `fix: ...` / `docs(skill): ...`.
- **Tests must be hermetic**: no network calls, no reading real credentials, no dependency on `~/.opendoc`. Fixtures must never use real production data (see the red-lines section of [testing.md](testing.md) for details).
- **Degradation red line**: any conversion loss must leave a trace (placeholder tag + count + report); changes that silently drop content will not be accepted (see the degradation contract in [architecture.md](architecture.md)). New degradation tags must be kept in sync with [degradation-tags.md](../../skill/skills/opendoc/references/degradation-tags.md).
- **Keep the docs true**: [architecture.md](architecture.md) is the spec of record. When a change alters behavior it describes, update the document in the same change — a stale spec is worse than none.
- **Agent contract stability**: the CLI's exit codes, the `doctor --json` schema, and probe failure codes (`F2-NOAUTH`, etc.) are consumed by [SKILL.md](../../skill/skills/opendoc/SKILL.md) and setup.md — they're part of the external API. Changes must be accompanied by updates to the skill docs.
- Before submitting: `go build ./... && go test ./...`; if you touched anything related to the skill package, run `./scripts/build-skill.sh --build-only` once.
