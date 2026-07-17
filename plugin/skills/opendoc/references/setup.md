# opendoc onboarding — the agent-executed setup script

You (the agent) drive first-time setup and repair by following this document. You are
the executor: you run the probes, explain failure states, walk the user through the
browser steps, retry on their behalf, and finally write the config by driving
`opendoc init --no-input` with flags. The opendoc process does not host this state
machine.

**Four iron rules**

1. **Probe-driven, not question-driven.** Each step first probes reality (run the real
   command), and whatever is missing becomes "the next concrete action + an exact link
   or command"; the user does it and you retry. Never ask "did you set it up?" — verify
   it yourself.
2. **The user only does the three browser things**: (1) click through creating the
   app/integration on the open platform / my-integrations page; (2) click Approve on
   the OAuth consent page; (3) copy the Notion token. Everything else (probing,
   validation, explaining errors, parsing URLs, writing config) is yours.
3. **Relay actions strictly from this document's failure-code mapping — do not
   improvise.** Do not invent scope names or links from memory; where this document
   pins a value, use it, and where it says "confirm on the permission page", have the
   user confirm rather than guessing an exact string.
4. **Probe IDs and failure codes are routing keys, not user vocabulary.** `F1`,
   `F2-NOCONFIG`, `N3-EMPTY` etc. exist so doctor's JSON can index into this document —
   they mean nothing to the user. Say what the state *is* in plain words ("Feishu
   access isn't authorized yet", "your token works but isn't connected to any page"),
   never the code. The user should be able to finish onboarding without ever learning
   that the code table exists.

**Retry limit**: after the same probe fails **3 times in a row**, stop looping — output
that item's complete troubleshooting checklist (the whole mapping-table row including
links, all at once) and hand control back to the user.

---

## First question: which platforms?

Ask the user first: **Feishu, Notion, or both?** Skip an unselected line entirely (its
config section stays empty, matching the "unconfigured = disabled" semantics of
`Notion.Enabled()` / Feishu `Configured()`). It can always be added later via the
"unconfigured platform" report item in SKILL.md.

Then run `opendoc doctor --json` once as the state oracle and enter the branch below that
matches. When uninitialized (`initialized:false` / `G0 NOT_INITIALIZED`), run the full
flow; when a single check fails, do only that item's repair subset.

**Probe before config exists with `--platform`.** On a fresh machine the config does not
name any platform yet, so the Feishu/Notion probes would all skip. Force them:
`opendoc doctor --platform feishu,notion --json` runs F1–F4 and N1–N3 against reality
even before `opendoc init` (F3 forces the wiki+drive category probes; F5 and the docs
probe still need real config/a mirrored doc). Use this instead of hand-rolling
`curl`/`lark-cli` calls wherever a probe exists — only drop to raw `lark-cli` where
doctor has no equivalent (e.g. enumerating wiki spaces for scope selection). The command
still exits 3 (`NOT_INITIALIZED`) until `opendoc init` runs; that is expected — read the
platform rows, not the exit code, during onboarding.

## G0 / G1 / G2 · Mirror root and quota

- `G1-UNWRITABLE`: the mirror root is not writable. The default root is `~/.opendoc`
  (changeable via `--root` or `OPENDOC_ROOT`). Suggest a writable path or fixing
  directory permissions; afterwards pass the same `--root` to every `opendoc` command.
- G0 `NOT_INITIALIZED` is the normal starting point, not an error — it only means
  `opendoc init` has not run yet; the final `opendoc init` at the end of this flow
  clears it.
- `G2-QUOTA-LOW` is a **warning, never a blocker** — relay it and carry on: "today's
  Feishu asset-download quota is running low; an image-heavy library may not finish its
  first full mirror today — resumable sync picks up the rest tomorrow." Most likely to
  fire during the first minimal sync at the end of this flow; do not abort onboarding
  over it.

---

## Feishu line (skip if not selected)

Feishu content/enumeration all goes through the **embedded lark engine** — the
official Feishu CLI compiled into the opendoc binary itself, invoked as
`opendoc lark-engine <args>`. **Nothing to install**: no Node, no npm, no external
lark-cli. opendoc itself never touches Feishu secrets — credentials are fully managed
by the engine (config + keychain under `~/.lark-cli`).

### F1 · engine present (`F1-MISSING` / `F1-VERSION`)

The default embedded engine always passes F1 (it reports the go.mod-pinned
version). `F1-MISSING` / `F1-VERSION` can only fire when the `OPENDOC_LARK_CLI`
escape hatch points at an external lark-cli binary — tell the user to unset it
(or fix the path / upgrade that binary to ≥ 1.0.69) and retry.

### F2 · Authenticated (`F2-NOCONFIG` / `F2-NOAUTH`)

Probe = `opendoc doctor --platform feishu --json` (or the raw `opendoc lark-engine api
GET /open-apis/authen/v1/user_info`). An unconfigured engine returns a *structured*
envelope `{"ok":false,"error":{"type":"config","subtype":"not_configured",...}}`, so
doctor distinguishes the two states for you: a `config` error ⇒ `F2-NOCONFIG`; any other
`ok:false` ⇒ `F2-NOAUTH`. Two action cards:

- `F2-NOCONFIG` (no app configured yet). **Preferred: one-click app creation** — no
  developer-console visit, no secret pasted anywhere:
  ```
  opendoc lark-engine config init --new
  ```
  This runs a device flow: relay the printed verification link/QR to the user, they
  approve in Feishu, and the app (App ID + Secret) is created and stored
  automatically. Alternative, when the user already has a custom app: the user runs
  `opendoc lark-engine config init` interactively in their own terminal (enter App ID /
  Secret at the prompts); if they paste the App ID / Secret into this chat instead,
  use the stdin form (`printf '%s' '<app-secret>' | opendoc lark-engine config init
  --app-id <app-id> --app-secret-stdin`) and ⚠ tell them to **rotate the App Secret**
  afterwards — a secret pasted into a chat is exposed. Then do the `auth login`
  device flow below. Retry F2 when done.
- `F2-NOAUTH` (app configured but not logged in, or token expired): just redo the
  `auth login` device flow below. Retry when done.

`opendoc init` (interactive) drives this whole flow itself — the cards above are for
repair and for agent-driven setup.

#### `opendoc lark-engine auth login` — the device flow (scopes are mandatory)

`auth login` **with no args fails**: scopes are required
(`{"error":{"subtype":"invalid_argument","param":"--scope"}}`). Drive it as an agent
device flow:

1. **Initiate** (relay, do not block):
   ```
   opendoc lark-engine auth login --no-wait --json --scope "<scope string below>"
   ```
   Returns `{device_code, verification_url, user_code, expires_in:600}`.
2. **Relay** the `verification_url` (and `user_code`) to the user; they open it in the
   browser and click **Approve**. This is one of the three browser things the user does.
3. **Complete** once they confirm:
   ```
   opendoc lark-engine auth login --device-code <device_code>
   ```
4. **10-minute expiry**; **never reuse a device code** across attempts — on any failure
   or timeout, re-initiate from step 1 for a fresh code.

The read-only scope string (space-separated; the same set `opendoc init` uses — see F3
for the per-category breakdown). **Grant all of these at app-creation time and pass
them here**:

```
offline_access docx:document:readonly docs:document.content:read docs:document.media:download wiki:space:read wiki:node:retrieve wiki:node:read space:document:retrieve drive:drive.metadata:readonly bitable:app:readonly board:whiteboard:node:read
```

**`offline_access` is REQUIRED** for unattended (launchd) runs: it grants the refresh
token, without which the access token expires and scheduled syncs die mid-day. Include
it every time. Use `opendoc lark-engine auth scopes` to list the scopes the app
currently has enabled if a grant looks short.

### F3 · Sufficient scopes (`F3-SCOPE-wiki` / `F3-SCOPE-docs` / `F3-SCOPE-drive`)

**Authenticated ≠ authorized.** The open-platform permission page is the highest-churn
step. doctor fires one read-only probe per category and reports exactly which category
is missing. Each category's probe endpoint and the **exact scopes** to grant:

| Failure code | Probe endpoint | Scopes to grant |
|---|---|---|
| `F3-SCOPE-wiki` | `GET /open-apis/wiki/v2/spaces` | `wiki:space:read` `wiki:node:read` `wiki:node:retrieve` |
| `F3-SCOPE-docs` | `GET /open-apis/docx/v1/documents/<id>` | `docs:document.content:read` `docx:document:readonly` `docs:document.media:download` |
| `F3-SCOPE-drive` | `GET /open-apis/drive/v1/files` | `space:document:retrieve` `drive:drive.metadata:readonly` |

Plus, for bitable mirroring/drill-down: `bitable:app:readonly`; for whiteboards:
`board:whiteboard:node:read`. And **always** `offline_access` (refresh token for
unattended runs — see F2). This is the same set as the F2 scope string.

> Scope names verified against the **Feishu open platform as of 2026-07-16** (proven
> in a real OAuth grant). If the platform renames a scope, the app's **Development
> Configuration → Permission Management (开发配置 → 权限管理)** page
> (<https://open.feishu.cn/app> → select the app → 权限管理) is authoritative — have the
> user tick the read-only permissions the page actually shows.
>
> **After changing scopes the user MUST redo `opendoc lark-engine auth login`** (the
> device flow in F2) to refresh the grant, or the new scopes take no effect. This step
> is missed constantly.

(The docs-category probe piggybacks on a meta read of an already-mirrored Feishu doc;
when the library has no Feishu doc yet, that item skips — not a failure. Re-run doctor
after the first sync to verify the docs scope.)

### F4 · Output-format smoke check (`F4-DRIFT`)

The engine's output structure drifted from the shape opendoc parses. With the embedded
engine (version pinned by opendoc's own build) this indicates an opendoc bug or a
server-side change, not a local version problem — report it against the source repo
(a checkout of `github.com/arcships/open-doc-cli`) and rebuild from a known-good opendoc
release. If `OPENDOC_LARK_CLI` points at an external lark-cli, unset it or pin that
binary to a verified version. This is an environment anomaly beyond normal onboarding —
say so plainly and hand it back to the user.

### F5 · Mirror scope valid (`F5-STALE-<id>`)

A configured wiki space / drive folder is stale or access was lost. The detail lists
the specific `wiki:<id>` / `drive:<id>`. Either restore access on the Feishu side, or
remove the id from config (re-run `opendoc init --force` with the updated flags).

### Choosing the mirror scope

Once F1–F3 are green, settle with the user what to mirror, collecting values for the
`opendoc init` flags:

- **Wiki spaces**: enumerate the visible list yourself —
  `opendoc lark-engine api GET /open-apis/wiki/v2/spaces` — and present a numbered list
  of `name` + `space_id` for the user to pick from. **Never ask the user to paste a
  space ID by hand.** Join the chosen space_ids with commas → `--feishu-wiki-spaces`.
- **Drive folders**: cannot be enumerated exhaustively. Have the user open the target
  folder in Feishu and paste the browser URL. It looks like
  `https://<tenant>.feishu.cn/drive/folder/<TOKEN>` (possibly a folder link under
  `.../drive/home/`) — **the folder token is the segment after `/drive/folder/`**; just
  extract it. Join multiple with commas → `--feishu-drive-folders`. After parsing,
  immediately verify access via F5 (`opendoc doctor`, or directly
  `opendoc lark-engine api GET "/open-apis/drive/v1/files?folder_token=<TOKEN>"`) before
  it goes into config.
- **Personal library**: ask one question — "mirror your personal cloud-docs library
  too?" → `--include-my-library` (add the flag if yes).

## Notion line (skip if not selected)

Mirror scope = the set of pages the integration is connected to. **There is no scope
configuration in config** — pages are added/removed on the Notion side. Tell the user
this mental model once, so they later know where "add a page" lives.

### N1 · env var set (`N1-UNSET`)

The token is referenced indirectly through an environment variable and **never lands in
config** (config stores only the variable name, default `NOTION_TOKEN`). Two cards:

(1) **Create an internal integration** — <https://www.notion.so/my-integrations> →
New integration → type Internal, **capabilities: tick Read content only** (read-only is
enough) → copy the `ntn_...` / `secret_...` token.
(2) **Put the token where opendoc can resolve it.** opendoc resolves the token env var
(default `NOTION_TOKEN`) from **the process environment first, then a fallback file**
`<root>/.internal/env` (default `~/.opendoc/.internal/env`). Pick one:

- **Shell profile** (covers interactive terminals):
  `echo 'export NOTION_TOKEN="<paste token>"' >> ~/.zshrc && source ~/.zshrc` (zsh) or
  `~/.bashrc` (bash). **Note**: a shell-profile export does **not** reach launchd.
- **The env file** `<root>/.internal/env` (covers **both** interactive runs and
  launchd — no wrapper needed):
  ```sh
  printf 'export NOTION_TOKEN="%s"\n' '<paste token>' > ~/.opendoc/.internal/env
  chmod 600 ~/.opendoc/.internal/env
  ```
  opendoc reads this file natively (shell-style `export KEY="value"` / `KEY=value`
  lines; no shell evaluation). Recommended when the user wants unattended launchd syncs —
  it is the single place that covers everything. doctor N1 names which source it resolved
  from and warns if the file is not `chmod 600`.

### N2 · token valid (`N2-INVALID`)

`GET /v1/users/me` returned 401. The token was mispasted, expired, or revoked — check
or recreate it at my-integrations, reset the env var, retry.

### N3 · visible page count (`N3-EMPTY`)

Token valid but **0 visible pages**: the integration is connected to nothing, so sync
would produce nothing. This is the most common Notion onboarding trap, and at the API
level it is indistinguishable from "the workspace is genuinely empty" — **you must
explicitly stop the user here**; never let them finish setup with an empty scope
thinking it worked. Spell out the connect steps:

> Open the Notion page to mirror → the **⋯** menu at the top right →
> **Connections** → select the integration you just created. **Connecting a top-level
> page makes its entire subtree visible** — connecting the root page covers everything
> beneath it.

Retry after connecting; doctor should report "visible: N pages/databases" — confirm
that number matches the user's expectation.

---

## Writing config: `opendoc init --no-input`

With every line green and the scope settled, write the config atomically in one command
(this is implemented — do not hand-write TOML). Pass only the flags you actually
collected; omit the flags of unselected platforms:

```
opendoc init --no-input \
  --feishu-wiki-spaces "7384...,7511..." \
  --feishu-drive-folders "LudK..." \
  --include-my-library \
  --notion-token-env NOTION_TOKEN
```

The real flags (do not invent others): `--root`, `--force` (overwrite an existing
config), `--no-input`, `--feishu-wiki-spaces`, `--feishu-drive-folders`,
`--include-my-library`, `--notion-token-env`, `--bitable-inline-max-rows` (default
200), `--trash-keep-days` (default 30), `--notion-reconcile-every-runs`.
Feishu-only users omit `--notion-token-env` (empty disables Notion); Notion-only users
omit all feishu flags. Afterwards `opendoc doctor` should be all green.

## The success moment: the first mirrored file

**The finish line is the first mirrored document, not config.toml.** Immediately run a
minimal sync so the user sees real output:

- Feishu: start with the smallest space in the chosen scope, `opendoc sync feishu`;
  Notion: `opendoc sync notion`. (Run one platform first for speed; later `opendoc sync`
  with no argument runs everything.)
- Relay the sync report in plain words: added/updated counts, degradation counts, the
  failure list.
- Close with three things:
  1. **Mirror location + INDEX.md**: the mirror lives at `<root>/` (default
     `~/.opendoc/`); the library map is `<root>/INDEX.md`.
  2. **A grep example**: have them try
     `grep -ri "<a keyword you remember>" ~/.opendoc/` and see retrieval hit with their
     own eyes.
  3. **Maintenance pointers**: `opendoc doctor` for routine self-checks; unattended
     freshness via launchd, next section.

## Unattended (launchd)

The install path is the **`opendoc schedule`** command — it writes the LaunchAgent plist
(correct absolute paths, chosen times, `OPENDOC_ROOT` + log routing), so there is no
`sed` or hand-edited XML. Full details in **`references/launchd/README.md`**; the
`com.arcships.opendoc.sync.plist` template there is the by-hand fallback. Give the user
the gist:

- `opendoc schedule --at 08:00,20:00` installs the job; the user picks the times (any
  comma-separated `HH:MM` list). `opendoc schedule` shows the current schedule; `opendoc
  schedule --remove` (or `opendoc unschedule`) removes it. It runs `opendoc sync` at
  those times so the daytime mirror stays fresh; the plist invokes the binary
  **directly** (no wrapper).
- **`opendoc schedule` writes the plist but never runs `launchctl`** — it prints the
  `launchctl load` / `start` lines for the user to run (loading is a system-state change
  and the first run needs a human present, below). You never run `launchctl` for them.
- **Notion token injection**: launchd does **not** read shell profiles, so the `export
  NOTION_TOKEN` in `~/.zshrc` does not reach it. Put the token in the env file
  **`<root>/.internal/env`** (default `~/.opendoc/.internal/env`, `chmod 600`) — **opendoc
  reads it natively** before it constructs the Notion adapter, so the same file that
  covers interactive runs also covers launchd.
- **The Feishu engine needs no PATH handling**: it is embedded in the opendoc binary
  (`opendoc lark-engine ...`), so launchd's bare PATH cannot lose it.
- **`offline_access` must be in the Feishu scope grant** (see F2) or unattended syncs
  die when the Feishu access token expires — the refresh token is what keeps launchd
  runs alive.
- **The first run must happen with a human at the screen**: after `launchctl load`,
  immediately `launchctl start com.arcships.opendoc.sync` and watch the log. macOS
  surfaces its approvals on that first launchd-context run (Background/Login Items
  approval, Keychain — click **Always Allow** — or folder-access prompts); a dialog that
  first appears at an unattended 08:00 trigger fails into a void. Install is done only
  when this manual start produces a clean sync report.
- For the concrete steps (the `opendoc schedule` flags, `launchctl load`, log locations)
  follow `references/launchd/README.md` — do not improvise. **The user runs `launchctl
  load`; you never load it for them.**
