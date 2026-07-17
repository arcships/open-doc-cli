# opendoc

[![CI](https://github.com/arcships/open-doc-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/arcships/open-doc-cli/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.25-00ADD8.svg)](go.mod)

English | [简体中文](README.zh-CN.md)

![opendoc — mirror your cloud docs into a local library](docs/assets/cloud-archive-banner.png)

**One-way mirror your authorized Notion and Feishu/Lark docs into a local, read-only Markdown tree that agents read directly — no API, no credentials, no network at query time.**

> [!NOTE]
> **This repository is not a plugin marketplace** — you can't install opendoc by pointing your agent at `arcships/open-doc-cli`. To install, follow the [Getting started](#getting-started) guide below, which installs from the [arcships/plugins](https://github.com/arcships/plugins) marketplace.

## Why opendoc

Your knowledge lives online — Notion for personal notes, Feishu/Lark for work docs. Your agent lives on your machine. Every time it needs something you wrote, it has to cross that gap through an API: manage credentials, page through results, respect rate limits, and lean on platform search endpoints whose recall was never designed for how an agent actually retrieves.

Meanwhile, the retrieval interface agents are *best* at already exists: the filesystem. Grep, glob, read a file, follow a link, walk a directory — coding agents do this natively, fast, and offline.

opendoc closes the gap from the other direction: **instead of teaching the agent to operate the platforms, it brings the documents to the agent.** One command mirrors everything you've authorized into a local Markdown tree; from then on, "search my notes" is just grep.

What lands on disk is more than a cache — it's your **local knowledge base**. Years of meeting notes, design docs, decisions, and half-remembered conclusions, scattered across platforms until now, become one coherent, greppable library on your own disk. Your agent stops answering from general knowledge and starts answering from *your* knowledge. And because it's plain Markdown in an open format, it's yours in the strongest sense: readable by every tool you'll ever use, available offline, and built to outlive any platform.

opendoc is a **porter and a librarian**, not an operator. It faithfully carries your online content down to disk, organizes it, and keeps it fresh — all writes still happen online. The deliverable is a directory tree on disk, not a service.

## What makes it work

- **Zero-integration consumption.** No SDK, no MCP round-trip, no daemon, no index to keep warm. The mirror is plain Markdown that Grep/Glob/Read already understand — installing nothing on the consumer side is the feature.
- **Query time costs nothing.** After a sync, retrieval needs no network, no credentials, and hits no rate limit. It works on a plane.
- **The tree itself is retrieval signal.** Paths are built from real titles (`notion/Japan Trip 2025/Itinerary.md`), and every file carries frontmatter — stable ID, online URL, breadcrumb, timestamps — so both the path and the header are grep-able context, and any hit jumps back to its online source in one step.
- **Fidelity first, loss made visible.** Conversion is inevitably lossy (whiteboards, embedded tables), but content is never dropped silently: every degradation leaves a readable placeholder, a drillable ID, and/or the online link, and is counted in the sync report.
- **Incremental and unattended.** Day-to-day syncs pull only what changed; a thousand-doc library finishes in minutes. `opendoc schedule` hangs it off launchd so the mirror is never more than a few hours stale.
- **Safe by construction.** The mirror is read-only (local edits are overwritten next sync; to change content, follow the frontmatter URL and edit online), opendoc never writes back to the platforms, and delete detection has a permission-flap guard so a temporarily expired token can't wipe your mirror into the trash.
- **Built to be called by agents.** Deterministic exit codes, structured `--json` output, never prompts (except `init`). Doctor's failure codes (`F2-NOAUTH`, `N3-EMPTY`, …) are stable routing keys an agent can branch on to repair its own setup.

## Architecture

```
┌────────────────────────────────────────────────┐
│ Consumers: agent (Grep/Read) · human (editor, 2nd)│
│   Guides: SKILL.md · INDEX.md · frontmatter      │
├────────────────────────────────────────────────┤
│ Knowledge base: markdown tree + assets pool      │
├────────────────────────────────────────────────┤
│ Sync engine: enumerate→diff→fetch→assets→write→link→index │
│ State: manifest.sqlite                           │
├────────────────────────────────────────────────┤
│ Platform adapters                                │
│  notion: official markdown endpoint + flat search enumerate │
│  feishu: embedded lark engine fetch + wiki/drive enumerate  │
└────────────────────────────────────────────────┘
```

The core invariant: **the engine knows nothing about any platform** — it only knows the `adapter.Adapter` interface. Adding a platform (Yuque, Confluence…) means writing one package that implements the interface and registering it in the CLI; the engine doesn't change. See [docs/dev/architecture.md](docs/dev/architecture.md) for the full deep dive.

## Mirror layout

Default root `~/.opendoc/` (override with `--root` or `OPENDOC_ROOT`):

```
~/.opendoc/
├── INDEX.md                   # auto-generated directory tree of the whole library
├── assets/                    # global asset pool, bucketed by first 2 hex of sha256
├── notion/                    # has children = directory + README.md; leaf = single file
│   └── …                      # database → directory + _index.md + one subdir per row
├── feishu/
│   ├── wiki-<space>/          # one subtree per wiki space
│   └── drive-<space>/         # drive folder tree
└── .internal/                 # internal state (manifest.sqlite, config.toml, trash, logs)
                               # search and browse tools should ignore this
```

Every `.md` carries YAML frontmatter: `id` (platform-stable ID), `source`, `type`, `url` (jump back online), `title`, `breadcrumb` (online ancestor path), `updated`, `synced`; database rows additionally carry `properties`.

## Getting started

opendoc is distributed as an agent plugin — one package, dual-manifested for both agents — via the [arcships/plugins](https://github.com/arcships/plugins) marketplace catalog. Installing sparse-fetches only the plugin package (`plugin/`), never this repo's source tree. Then just start using it (e.g. ask the agent to search your notes; it will walk you through first-time setup). The engine binary isn't committed; on first use the skill notices it's missing and, with your OK, downloads the platform build (`opendoc-<os>-<arch>`) from GitHub releases, verified by sha256.

**Claude Code** — inside a `claude` session:

```
/plugin marketplace add arcships/plugins
/plugin install opendoc@arcships
```

**Codex** — in your terminal:

```bash
codex plugin marketplace add arcships/plugins
codex plugin add opendoc@arcships
```

**For development** — build the engine from source and install the plugin from your working tree (this repo carries a dev-only catalog named `arcships-dev` so it can't collide with the real marketplace):

```bash
./scripts/build-skill.sh                      # builds plugin/bin/opendoc-dev
claude plugin marketplace add "$(pwd)"        # or: codex plugin marketplace add "$(pwd)"
# then install opendoc@arcships-dev
```

Once installed, the agent drives everything — but the CLI also works standalone:

```bash
opendoc init                # interactive setup, writes .internal/config.toml
opendoc sync                # first run = full mirror, afterwards incremental
```

## Commands

| Command | What it does |
|---|---|
| `opendoc init` | Interactive setup; writes `.internal/config.toml`. |
| `opendoc sync` | Full / incremental sync (first run full, then incremental + reconcile rounds). |
| `opendoc status` | Mirror overview: last sync, doc count, pending assets, etc. |
| `opendoc doctor` | Environment check: config, credentials, platform reachability; emits structured failure codes (`--json`). |
| `opendoc resolve <id\|url\|path>` | Cross-lookup between stable ID, online URL, and local path. |
| `opendoc schedule` | Manage the unattended launchd job (`com.arcships.opendoc.sync`) that runs `opendoc sync` on a schedule. |

Exit codes are deterministic, output is structured, and the commands never prompt — they are built to be called by agents. When not yet initialized, `sync`/`status`/`resolve` exit with code 3 (`ExitNotInitialized`) and point at the onboarding docs.

## Non-goals

- **No two-way sync.** opendoc only pulls down; it never writes back to Notion or Feishu.
- **No realtime.** It polls on demand or on a schedule; there is no live subscription.
- **No permission mirroring.** Comments, version history, and access controls are not reproduced.
- **No vector index.** Up to a few thousand documents, filesystem retrieval plus the guidance files is the whole retrieval layer; the directory and frontmatter conventions leave the door open for one later.
- **Plaintext on disk.** Anyone who can read this machine can read all mirrored content. Run opendoc only on a trusted personal machine.

## Documentation

- [docs/dev/architecture.md](docs/dev/architecture.md) — the spec of record: layering, what a single sync does end to end, the adapter contract, what each package owns.
- [docs/dev/README.md](docs/dev/README.md) — contributor onboarding: build, test, and where to start.
- [docs/dev/testing.md](docs/dev/testing.md) — how tests are organized, mock patterns, fixture red lines.
- [docs/notion-properties-mapping.md](docs/notion-properties-mapping.md) — Notion properties → frontmatter mapping.
- [plugin/skills/opendoc/SKILL.md](plugin/skills/opendoc/SKILL.md) — the Agent Skill guide that ships in the plugin.

## License

Apache-2.0. Copyright arcships. See [LICENSE](LICENSE).
