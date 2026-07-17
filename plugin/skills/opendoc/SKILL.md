---
name: opendoc
description: >-
  Local knowledge-base retrieval and sync. Use when the user wants to recall or
  find documents, conclusions, or notes they wrote in Notion or Feishu
  — e.g. "上次关于 X 的结论是什么" / "what did we conclude about X",
  "搜一下我飞书/Notion 里关于 Y 的笔记" / "search my Feishu/Notion notes about Y" —
  or wants to sync/refresh the local mirror, or wants to connect or repair
  platform access ("帮我把 Notion 连上" / "connect my Notion", "飞书授权过期了" /
  "my Feishu auth expired"). opendoc one-way mirrors the documents each platform
  has authorized into a local read-only Markdown tree that the agent consumes
  directly with Grep/Glob/Read — no API, no credentials, no network.
---

# opendoc — mirrored-docs knowledge base

opendoc **one-way mirrors** the user's authorized Notion / Feishu documents into a local
read-only Markdown tree (default `~/.opendoc/`). You (the agent) answer questions by
reading it directly with Grep/Glob/Read; writes always happen online, never locally.

Three different things share the "opendoc" name — do not confuse them:

- **Source repo** — a checkout of `github.com/arcships/open-doc-cli`: the Go source.
  Readable, editable, rebuilds in seconds.
- **Installed plugin** — the delivery unit (the repo's `plugin/`), installed via each
  agent's plugin marketplace. The plugin root holds the manifests
  (`.claude-plugin/plugin.json`, `.codex-plugin/plugin.json`); this file and its
  `references/` + `scripts/` live in the plugin's `skills/opendoc/` — so **the plugin
  root is two directories above this SKILL.md**. The plugin package never contains
  the engine binary.
- **Engine binary** — `~/.opendoc/bin/opendoc`, installed once by the bundled
  download script (bootstrap below). It lives deliberately OUTSIDE the plugin
  directory: plugin dirs are not a stable home — the Claude desktop app
  re-provisions the plugin per session, claude.ai cloud mounts it read-only, and
  Codex / the Claude Code CLI cache put every version in a fresh version-stamped
  directory. `~/.opendoc/bin` survives sessions and plugin updates and is shared by
  every agent host on the machine. What the plugin's `bin/` DOES ship is a thin
  shim (`bin/opendoc`) that execs the engine — see "Invoking the binary" below.
- **Mirror data directory** — default `~/.opendoc` (changeable via `--root` /
  `OPENDOC_ROOT`): the Markdown tree + `assets/` + `.internal/` (config, manifest —
  ignore when retrieving). Changing the root does **not** move the engine binary.

**Invoking the binary**: resolve once per session with this no-fail one-liner and
reuse the result (never probe with a bare `command -v opendoc` — its miss exits
non-zero and renders as a failed command):

```bash
OPENDOC=$(command -v opendoc || echo "$HOME/.opendoc/bin/opendoc")
```

Under Claude Code the enabled plugin's `bin/` is on PATH, so this finds the bundled
shim, which execs the engine (a dev build `bin/opendoc-dev` when present, else
`~/.opendoc/bin/opendoc`; `OPENDOC_ENGINE=<path>` overrides both). Codex puts no
plugin directory on PATH, so there the fallback IS the normal path, not an edge
case. This document writes bare `opendoc` throughout — substitute `"$OPENDOC"`
(shell state does not persist between Bash calls: re-derive it inline per command).
To change engine behavior: clone/checkout the repo `github.com/arcships/open-doc-cli` →
edit → `./scripts/build-skill.sh`.

## Session discipline: doctor first

**The first time you use opendoc in a session, run `opendoc doctor --json` first**, then
branch on `initialized` and the failure codes. doctor is mostly local probing and cheap;
reuse the result within the session — do not re-run it before every command.

**Bootstrap — the engine binary is missing** (`~/.opendoc/bin/opendoc` does not
exist; invoking through the shim then fails fast with exit 127 and a message
pointing here): missing, not broken. The engine is platform-specific and never
ships inside the plugin package (the shim is not the engine). Tell the user a
one-time engine download is needed (~40MB, from this repo's GitHub Releases,
sha256-verified, installed to `~/.opendoc/bin/opendoc`) and, once they agree, run
the bundled installer, then re-run doctor:

```bash
"${CLAUDE_SKILL_DIR}/scripts/download-binary.sh"
```

`${CLAUDE_SKILL_DIR}` is set by the Claude Code CLI; other hosts (Claude desktop
app, Codex) may leave it unset — then run `scripts/download-binary.sh` from the
directory holding this SKILL.md. The script self-locates (it walks up to the plugin
root only to read the release version from the manifest), installs into
`~/.opendoc/bin` (override: `OPENDOC_BIN_DIR`), is checksum-idempotent (re-running
when up to date downloads nothing), and reuses a checksum-matching local copy
instead of downloading. `OPENDOC_REPO=owner/repo` overrides the download source
(default `arcships/open-doc-cli`). On a checksum mismatch the script refuses to
install — report that verbatim rather than working around it.

**Update — plugin moved, engine didn't**: a plugin update replaces this SKILL.md
but leaves `~/.opendoc/bin/opendoc` untouched, so the engine can go stale. The
installer records what it installed in `~/.opendoc/bin/.version`; whenever the
engine in use is `~/.opendoc/bin/opendoc` — a PATH hit on the plugin shim counts,
since the shim execs that same path; only a dev build (`bin/opendoc-dev` beside
the shim, or `OPENDOC_ENGINE`) shadows this check, on purpose — compare that file
against the `version` in
`<plugin-root>/.claude-plugin/plugin.json` (plugin root: two directories above
this SKILL.md). Same → proceed, no network touched. Different or missing → tell
the user the plugin was updated and the engine needs to follow, then (with their
OK, as above) re-run the same installer — it converges the binary to the manifest
version and rewrites the stamp.

| doctor output | your action |
|---|---|
| `initialized: false` / a check with `code: NOT_INITIALIZED` | Not initialized → load `references/setup.md` and run the full onboarding |
| A platform probe fails (`F2-NOAUTH`, `N2-INVALID`, `F3-SCOPE-*`, `N3-EMPTY`, ...) | Load `references/setup.md` and apply that failure code's action mapping (an onboarding subset) |
| A platform's checks all skip ("not configured") — **a report item, not an error** | Remember the platform is unconfigured; offer to set it up when the user brings it up. Do not do it unprompted |
| Everything pass/warn/skip (`ok: true`) | Proceed with the actual task |

`warn` (e.g. `G2-QUOTA-LOW`) never blocks — relay it as a one-line heads-up. If you
skipped doctor and ran `opendoc sync`/`status`/`resolve` on an uninitialized mirror, the
command exits with code **3** (`ExitNotInitialized`) and points at
`references/setup.md` on stderr — reading that is itself the onboarding trigger.

Probe IDs and failure codes (`F1`, `F2-NOCONFIG`, `N3-EMPTY`, ...) are for YOUR
branching only — never say them to the user. Translate every state into plain
words ("Feishu access isn't authorized yet"); the codes stay in the JSON and this table.

**Config/repair intent goes straight to setup**: when the user says things like
"帮我把 Notion 连上" / "connect my Notion" or "飞书授权过期了" / "my Feishu auth
expired", skip retrieval and load `references/setup.md` directly.

## Retrieval: INDEX → Glob → Grep

1. **Read `<root>/INDEX.md` first** (default `~/.opendoc/INDEX.md`). It is the full
   library map rebuilt on every sync: a header with library metadata (last sync time,
   per-platform document counts — the generated file itself is in Chinese), then a
   path-sorted indented tree of `- [title](relative/path) · online-updated-time` lines.
   Use it to spot candidate documents at a glance.
2. **Glob** to narrow down: the path itself is a semantic signal, e.g.
   `~/.opendoc/feishu/wiki-工程/**/权限*.md`, `~/.opendoc/notion/**/*.md`.
   Layout conventions: a page with children = a directory + `README.md` (the body lives
   in the README); a leaf page = a single `.md`; a Notion database = a directory +
   `_index.md` (machine-generated row-index table) + one `README.md` per row.
3. **Grep** hits both body and frontmatter: Chinese titles, breadcrumbs, and properties
   are all plain text — grep them directly.

**Frontmatter is signal.** Every `.md` starts with a YAML block: `id` (stable platform
ID), `source` (feishu|notion), `type`, `url` (one-step jump back online), `title`,
`breadcrumb` (online ancestor path — grep context), `updated` (online edit time),
`synced` (local fetch time); database rows also carry `properties:`. When answering,
**cite the frontmatter `url`** for traceability.

## Red line: the mirror is read-only

Local files are mirror copies of the online originals. **Any local edit will be
overwritten by the next sync.** To change content, follow that file's frontmatter `url`
and edit the online original — do not edit local files, and do not advise the user to.
The red-line comment at the top of every file and the `INDEX.md` header both repeat
this, because the warning must travel with the content.

## Degradation tags: how to read them, how to drill down

Conversion is inherently lossy (whiteboards, embedded tables, oversized documents), but
every loss leaves a trace — readable fallback content + a drillable ID + an online
link, at least two of the three. You will encounter these markers in document bodies:

- `<!-- opendoc:whiteboard token="..." -->` immediately before a ` ```mermaid ` block —
  a Feishu whiteboard; the mermaid source IS the content.
- `<!-- opendoc:base_refer table-id="..." token="..." -->` followed by a markdown table,
  or a schema-only summary, plus an `[Open the bitable in Feishu](url)` link — a Feishu
  bitable. Over the row threshold only schema + row count remain; follow the link for
  exact data.
- `<!-- opendoc:truncated -->` / `<!-- opendoc:unknown-blocks ... -->` (at the top of a
  file) — an oversized Notion page / blocks the endpoint could not render.
- `<sheet sheet-id="..." token="...">` — an embedded Feishu spreadsheet: the tag is
  preserved, its data is not fetched; open the parent document's url to view it.
- Any XML/HTML-ish tag you do not recognize = an unknown block, preserved verbatim
  (the red line: better an opaque tag than a silent drop).

Full tag semantics, what was lost, and exact drill-down recipes:
**`references/degradation-tags.md`** (load on demand).

## Sync and commands

All commands are machine-friendly: structured output, deterministic exit codes
(0 success / 1 any fail / 2 usage error / 3 not initialized), non-interactive (except
`opendoc init`). Add `--json` for machine-readable output. `--root <path>` overrides the
mirror root.

- `opendoc sync [feishu|notion] [--full]` — one sync pass (default: all configured
  platforms). Incremental by default; `--full` ignores incremental skips and forces a
  complete re-scan. The report prints added/updated/moved/deleted counts, asset
  downloads/pending, **degradation counts**, and the failure/warning lists. **When to
  sync**: when the user explicitly asks for a refresh, or when retrieval shows the
  mirror is clearly stale (compare the `INDEX.md` header's last-sync line, labeled
  `Last synced:`). Day-to-day freshness belongs to launchd (see setup.md) — do not sync on
  every retrieval.
- `opendoc status [--json]` — manifest overview: document counts (by platform/status),
  pending assets, each platform's last sync time and degradation counts. Use it to
  answer "how much is in the library / how fresh is it".
- `opendoc doctor [--json] [--platform feishu,notion]` — credential/tooling/quota
  self-check (see the discipline above). `--platform` forces the named platforms' probes
  to run even before `opendoc init` / when config does not enable them — used during
  onboarding to probe reality before config exists.
- `opendoc resolve <id|url|path> [--json]` — three-way lookup between platform ID, online
  URL, and local path. Use it to map a token from a degradation tag, or an online link
  the user pasted, to the local file (or back to the online url).
- `opendoc schedule [--at HH:MM,HH:MM | --remove]` (macOS) — install/inspect/remove the
  unattended launchd sync at times the user picks (`opendoc unschedule` = `--remove`). It
  writes the LaunchAgent plist but **never runs `launchctl`**: it prints the
  `launchctl load`/`start` lines for the user to run (first run needs a human present
  for macOS approvals). Point the user at it when they ask to set up or change automatic
  syncing; details in `references/launchd/README.md`. **Path caveat**: the plist
  embeds the invoked binary's absolute path — always run `schedule` via the stable
  engine path (`~/.opendoc/bin/opendoc schedule ...`), never via a plugin-directory
  copy: plugin paths are version-stamped or per-session, so they go stale on the
  next update and silently kill the job.

When reading a sync report, focus on `warnings` (e.g. the permission-jitter guard: when
one platform's listing shrinks more than 20% versus the manifest, the delete step
aborts and warns — that is protection, not a fault) and `failures` (per-document
failures that did not block the run; retried next round).
