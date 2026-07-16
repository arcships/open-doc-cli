# Architecture Deep Dive

> For contributors: how the code is layered, what happens from start to end of one sync, what each package owns. For the product-level "what/why" see the [README](../../README.md). **This document is the spec of record** — when behavior and documentation disagree, fix whichever is wrong, in the same change.

## Layering Overview

```
cmd/opendoc ──→ internal/cli ──→ internal/engine ──→ internal/adapter (interface)
                │                 │                    ↑ implementations
                │                 ├─ internal/manifest │
                │                 ├─ internal/frontmatter
                │                 ├─ internal/naming   ├─ internal/notion
                └─ internal/config│                    └─ internal/feishu
                   internal/layout└─ internal/ratelimit (shared by both adapters)
```

Core invariant: **the engine knows nothing about platforms** — it only knows the [adapter.Adapter](../../internal/adapter/adapter.go) interface. Adding a new platform (Yuque, Confluence, …) = write a new package implementing that interface + register it at the CLI wiring point, zero changes to the engine.

## The Adapter Contract (`internal/adapter/`)

The entire contract lives in one file: [adapter.go](../../internal/adapter/adapter.go) — the comments are the spec. Three required methods:

| Method | Responsibility |
|---|---|
| `Enumerate(ctx)` | Streams out metadata for every document within the authorized scope (`RemoteDoc`: ID/AltID/Type/ParentID/Title/EditedAt/URL) — metadata only, no body |
| `FetchMarkdown(ctx, doc)` | Fetches one document's body, returns `FetchResult`: the raw `Markdown` (the object hashed for content_hash) + the degraded `Body` + asset references + inter-document links + degradation count |
| `DownloadAsset(ctx, ref, destPath)` | Downloads an asset; URLs are short-lived, must be fetched immediately |

Two **optional capabilities**, which the engine probes for via type assertion:

- `DatabaseExpander` — fetches all rows' properties for a database in a single query (Notion implementation; queried once per db node per run).
- `IncrementalEnumerator` — enumerates only changed documents based on a checkpoint (Notion implementation; Feishu doesn't have one — its enumeration is inherently a full listing, so every run there is a reconciliation run). Implementing it changes the engine's contract: in an incremental run, "not enumerated" ≠ "deleted," so the engine turns off delete/move detection.

## The Full Flow of One Sync (`internal/engine/`)

The orchestration entry point is `Engine.Sync` ([engine.go](../../internal/engine/engine.go)):

1. **Preparation**: ensure the directory structure exists, open the manifest.
2. **Mode decision** ([mode.go](../../internal/engine/mode.go)): decide per-platform whether this run does a full or incremental pass. Any one of the full-run conditions triggers a full run: `--full`, no usable checkpoint, first run of the day (the first run each day forces reconciliation), or the `notion_reconcile_every_runs` cadence coming due. Feishu is always full.
3. **Platforms run in parallel**: each platform runs its own pipeline in one goroutine (id spaces don't overlap, each writes its own `sync_runs` row, no cross-platform contention; a fatal failure on one platform doesn't affect the other).
4. **Whole-repo finalization** (once, after all platforms finish): expired trash cleanup → regenerate `INDEX.md`.

### Full Run (Reconciliation Run): `runFull` ([pipeline.go](../../internal/engine/pipeline.go))

```
Enumerate the full listing → buildTree (build tree from parent pointers, resolve paths/breadcrumbs)
→ processNode per node (sequential loop)
→ reconcileDeletes (delete/move detection)
→ finalizeLinks (whole-repo internal link rewriting + empty directory sweep)
→ persist checkpoint
```

The order inside `processNode` (the order matters — read the comments before changing it):

```
Alias registration (before any skip — links must still resolve even if the target gets skipped)
→ mkdir → container node short-circuit (folder/db has no body)
→ move/rename follow (manifest local_path ≠ this run's path → move the file)
→ pre-fetch skip (remote_edited unchanged → skip the whole fetch)
→ FetchMarkdown → compute content_hash (on the raw Markdown; for db rows, properties are folded into the Canonical serialization)
→ register assets/links → download assets and replace in-body image URLs with relative paths
→ post-fetch skip (content_hash unchanged → don't write to disk)
→ frontmatter + atomic write (temp file + rename) → manifest upsert
```

### Incremental Run: `runIncremental` ([incremental.go](../../internal/engine/incremental.go))

Only enumerates changed documents; position resolution is done against the manifest (for ancestors not enumerated in this run), not against this run's tree; only dirty databases are pre-queried; no delete/move detection is performed. Three classes of scenarios are intentionally deferred (directory shape changes, `_index.md` regeneration, a parent moving while the node itself wasn't edited) — all are backstopped by the next reconciliation run. The file header comment lists them in full.

The leaf-to-directory conversion that happens the first time a leaf gains a child (`<x>.md` → `<x>/README.md`) is also handled in incremental runs (`leafToDirNode`).

### Four Cross-Cutting Invariants

- **content_hash is always computed on the "fetched original"** (before degradation, before rewriting), so degraded output and link rewriting never make a document look "dirty." Link rewriting also deliberately doesn't write back the hash.
- **All disk writes go through `atomicWrite`** (temp + fsync + rename); readers never see a half-written file.
- **Frontmatter is hand-rendered** (no YAML library), guaranteeing deterministic key order and byte-for-byte stability across runs ([frontmatter.go](../../internal/frontmatter/frontmatter.go)).
- **The degradation contract: loss is never silent.** Conversion is inevitably lossy (whiteboards, embedded tables, oversized pages, unsupported blocks), but every degraded resource block must land at least two of three things: **readable degraded content, a drillable ID, and a jumpable online link** — and every degradation increments a counter in the sync report. An unknown block is preserved as its verbatim tag rather than dropped. The exact marker shapes the engine emits, per platform, are catalogued in [skill/skills/opendoc/references/degradation-tags.md](../../skill/skills/opendoc/references/degradation-tags.md) (that file ships in the plugin and must stay in sync with the emitters in `internal/feishu/degrade.go` / `internal/notion/degrade.go`).

## manifest.sqlite (`internal/manifest/`)

A pure-Go driver, `modernc.org/sqlite` (no cgo), single connection (`SetMaxOpenConns(1)`) so that `database/sql` serializes all statements — the load bottleneck is network I/O, not the ledger. The schema is idempotent `CREATE TABLE IF NOT EXISTS` (constants at the top of [manifest.go](../../internal/manifest/manifest.go)); deleting the database and rerunning rebuilds it from scratch. Five tables:

| Table | Key | What it records |
|---|---|---|
| `documents` | `id` (platform-native ID) | The main ledger: type/parent/title/**local_path**(UNIQUE)/remote_edited/**content_hash**/status(`active\|trashed\|error\|pending_assets`) |
| `assets` | `remote_key` (the platform's stable key, not a temporary URL) | sha256, on-disk path, `done\|pending` |
| `links` | (from_id, to_id) | Backlinks: who references me — used to fix up referrers on rename/move |
| `doc_aliases` | `alias` | Secondary ID → primary ID (Feishu wiki node_token, Notion's hyphen-stripped form) |
| `sync_runs` | auto-increment id | Per-run audit: checkpoint high-water mark + stats JSON (including mode, feeding the next run's cadence decision) |

## Directory Layout and Naming (`internal/layout/`, `internal/naming/`, engine/tree.go)

- Root resolution priority: `--root` > `OPENDOC_ROOT` > `~/.opendoc`; the internal directory is called `.internal` (not `.opendoc`, to avoid `~/.opendoc/.opendoc`).
- **README/leaf duality**: a node with children or a container type → directory + `README.md`; a leaf → `<name>.md`. A database → directory + `_index.md` (row-property index) + one file per row.
- **Naming rule chain** ([naming.go](../../internal/naming/naming.go) `Component`, in order): slug (strip only illegal characters, keep CJK and spaces) → empty title → `untitled-<id prefix>` → reserved-name suffix (`readme`/`_index`/`claude`/`agents`…) → 200-byte truncation → same-directory casefold collision gets `-<first 8 chars of id>` appended → numeric fallback. First one there gets the clean name.
- **Orphan pages**: a node whose parent isn't in the enumeration results → filed under `_orphans/` at the platform root.

## Asset Pipeline (engine/assets.go)

Content-addressed storage: sha256 the downloaded bytes → `assets/<first 2 chars of sha>/<sha><ext>` (extension is sniffed from the bytes first, falling back to the filename suffix). Two levels of dedup: at the `remote_key` level (already done and the file is still there → don't re-download) and at the content level (multiple references to the same bytes share one pool file). **A failed download never loses the body**: the asset is marked `pending`, the body keeps the original URL and appends `<!-- opendoc:asset-pending -->`, the document is marked `pending_assets`, and the next run forces reprocessing.

## Internal Link Rewriting (engine/linkrewrite.go)

Two phases:

1. **At write time** (per document): asset URLs in the body are replaced in place with relative paths.
2. **At platform finalization** (whole repo): scan the **entire** links table (not limited to this run — old links whose target arrives later also get fixed), rewriting platform document URLs in the body to relative paths. Frontmatter is split off first; the `url:` field is never rewritten. The alias table is consulted here to resolve Feishu `/wiki/<node_token>` and Notion's hyphen-stripped IDs.

When a link that has already been rewritten to a relative path later has its endpoint move, `fixupMovedLinks` (lifecycle.go) recomputes the old/new relative paths from the links table and replaces it.

## Lifecycle: Deletion and Trash (engine/lifecycle.go)

Delete detection only happens on reconciliation runs: a document that's `active` in the manifest but wasn't enumerated this run → moved into `.internal/trash/<date>/<original relative path>`, the manifest row is kept and flipped to `trashed`. **Permission-flap guard**: if the enumeration result is less than 80% of the active count, deletion is aborted with a loud warning (to prevent a temporary loss of permissions from wiping out the mirror). Trash is purged past `trash_keep_days` (default 30 days).

## Rate Limiting (`internal/ratelimit/`)

Two zero-dependency primitives: `Bucket` (a steady-rate token bucket, burst 1, claims the slot before sleeping under a mutex, safe across goroutines) and `Backoff` (capped exponential backoff, HTTP `Retry-After` hints take priority over the computed value, retryable-ness is injectable). Notion API/assets are each 3 QPS, Feishu fetch/assets are each 5 QPS.

## Notion Adapter (`internal/notion/`)

- **Pure `net/http`, no SDK**, strictly read-only (only GET and the read-only POST `/v1/search`). The token is an integration token, sourced from the environment variable named by config's `token_env` (resolved at the CLI layer) — the adapter only receives a string and never logs it.
- **Enumeration**: `POST /v1/search` paginates flat to the end; the tree is rebuilt by the engine from parent pointers. Only `page` and `data_source` are mirrored. A page whose parent is a data_source → `db_row`.
- **Body**: `GET /v1/pages/{id}/markdown` (enhanced markdown). Two degradation signals, `truncated` / `unknown_block_ids` → marked with an HTML comment in the body + counted.
- **Dual ID forms**: the API uses hyphenated UUIDs, URLs/body use 32-char unhyphenated hex; the unhyphenated form is stored in `AltID` via the alias table.
- **Incremental**: search paginates in descending `last_edited_time` order, stopping once it reaches earlier than `checkpoint − a 5-minute safety window` (the minute-precision timestamp is absorbed by the double safeguard of the safety window plus content_hash).
- **Database expansion**: `POST /v1/data_sources/{id}/query` gets all rows' properties in one call; each property is rendered as a grep-able plain string (see the mapping table in [notion-properties-mapping.md](../notion-properties-mapping.md)), sorted by name for determinism; lossy types leave a drill-down clue rather than being dropped.

## Feishu Adapter (`internal/feishu/`)

- **Credentials are fully delegated to the embedded lark engine** (OAuth, token refresh, keychain) — opendoc never touches a Feishu secret. The engine = `github.com/larksuite/cli` compiled into the opendoc binary (version-pinned in go.mod), invoked busybox-style via self-exec through the hidden subcommand `opendoc lark-engine` (`cmd/opendoc/main.go` dispatches on `argv[1]`; self-exec rather than an in-process call preserves stdout capture, stderr error classification, cancellation, and crash isolation). All calls funnel through the `Runner` interface in [larkcli.go](../../internal/feishu/larkcli.go) — this is both the test seam (`fakeRunner`) and the escape-hatch attachment point (when `OPENDOC_LARK_CLI` overrides to an external lark-cli binary, doctor's F1 check requires ≥ 1.0.69).
- **Why go through the lark engine instead of the official markdown endpoint**: the official endpoint silently drops images, whiteboards, and tables — a direct violation of the degradation contract; `docs +fetch` makes two calls per document — markdown gets the body (as a bonus, whiteboards come with mermaid inlined for free), XML gets stable asset tokens and inter-document link references.
- **Enumeration**: wiki recurses per space via `+node-list` (`obj_token` is the primary ID, `node_token` goes into `AltID`); drive recurses via `GET /drive/v1/files`; then `metas/batch_query` (200/batch) backfills URL and edit time uniformly. Unknown obj types → placeholder `TypeFile`, leaving no holes in the tree.
- **Envelope discipline**: engine output is always an `{ok, data, error}` envelope; `envelope.go` explicitly asserts the shape (the source of F4 drift alerts), and error code ranges map to F2-NOAUTH / F3-SCOPE.
- **bitable degradation has three branches**: small tables are inlined as a GFM table; beyond `bitable_inline_max_rows` (default 200) → schema + row count + link; fetch failure → a comment-preserving tag + link. The list of degradation tags must stay in sync with [degradation-tags.md](../../skill/skills/opendoc/references/degradation-tags.md).

## CLI Layer (`internal/cli/`)

Manual dispatch with stdlib `flag` (no command framework, to control dependencies). The `Env` struct carries all I/O to make testing easy. **Exit code contract**:

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Runtime failure; doctor has any `fail` probe |
| 2 | Usage error |
| 3 | `ExitNotInitialized` — not initialized, stderr points to setup.md (all commands except `init`; doctor is special: it prints the full report first, then exits 3) |

`resolve` uses a narrower set: 0 found / 1 not found / 2 usage.

The **doctor probes** are the foundation of onboarding: the structured failure codes (`G0–G2` / `F1–F5` / `N1–N3`) are the agent's routing keys (see [setup.md](../../skill/skills/opendoc/references/setup.md)); only `fail` affects the exit code, an unconfigured platform produces a `skip` line rather than a failure — `--json` consumers always see a stable schema. The probes are entirely dependency-injected (fake Runner, fake Notion probe, fixed clock) and unit-testable without network.

## Configuration and Environment (`internal/config/`)

- `<root>/.internal/config.toml`: `[feishu]` wiki_spaces/drive_folders/include_my_library; `[notion]` `token_env` (stores the **variable name**, not the token); `[sync]` bitable_inline_max_rows / trash_keep_days / notion_reconcile_every_runs.
- Environment variables: `OPENDOC_ROOT` (mirror root), `OPENDOC_LARK_CLI` (override the embedded engine with an external lark-cli, for debugging), `$<token_env>` (defaults to `NOTION_TOKEN`).
- **Env file fallback**: `<root>/.internal/env` (0600, shell-style `export KEY="value"` but deliberately **not shell-evaluated** — no variable expansion, no command substitution). The process environment takes priority; the file is only read when it's empty; overly permissive permissions trigger a doctor warning. Interactive use and launchd share this one file.

## Build and Distribution

```bash
./scripts/build-skill.sh    # CGO_ENABLED=0 static build → skill/bin/opendoc
```

`skill/` is the plugin root, dual-manifested for both supported agents: `.claude-plugin/plugin.json` (Claude Code) + `.codex-plugin/plugin.json` (Codex) + `bin/opendoc` + the skill itself under `skills/opendoc/` (`SKILL.md` + `references/` + `scripts/`). Distribution is marketplace-only, via the separate catalog repo `arcships/plugins`, whose entries use `git-subdir` sources pointing at this repo's `skill/` — installs sparse-fetch only that directory, never the Go source. The catalogs at this repo's root (`.claude-plugin/marketplace.json`, `.agents/plugins/marketplace.json`) are the dev-only `arcships-dev` marketplace that installs from the local working tree. Under Claude Code an enabled plugin's `bin/` is on the agent's PATH; Codex has no PATH mechanism, so SKILL.md instructs the agent to invoke `<plugin-root>/bin/opendoc` by path. Unattended launchd invokes the binary via an absolute path (see the plist under [skill/skills/opendoc/references/launchd/](../../skill/skills/opendoc/references/launchd/)).
