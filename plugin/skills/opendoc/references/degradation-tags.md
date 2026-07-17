# Degradation tags: reading and drilling down

Conversion is inherently lossy (whiteboards, embedded tables, oversized documents,
unsupported property types), but opendoc's red line is that **loss is never silent**:
every degradation leaves a readable placeholder + a drillable ID + an online link (at
least two of the three) in the body/frontmatter, and adds one to the sync report's
**degradation counts**. This document describes the marker shapes the engine
**actually emits**, what was lost, and how to drill down.

General drill-down tool: `opendoc resolve <token|url|path>` looks up any of the three
forms — a token from a tag, an online URL, a local path — against the other two
(`--json` for machine output). For exact data, follow the file's frontmatter `url` or
the token in the tag to the online original.

Note: some engine-emitted strings quoted below are Chinese — that is verbatim engine
output, exactly what you will see in mirrored files.

---

## Feishu

### Whiteboard → mermaid (`<!-- opendoc:whiteboard token="..." -->`)

The embedded lark engine emits whiteboards directly as mermaid source. The sync
engine inserts a comment
line before each ` ```mermaid ` fence:

```
<!-- opendoc:whiteboard token="doxcnXXXX" -->
```mermaid
graph TD; ...
```
```

- **What was lost**: almost nothing — the mermaid source IS the content, free fidelity.
  The token is a bonus drill-down lead.
- **Drill down**: usually unnecessary; to see the original whiteboard, `opendoc resolve
  <token>` or open the parent document's url.
- Comments pair with fences **positionally** in document order (the i-th whiteboard
  gets the i-th token); if there are fewer tokens than fences, the extra mermaid blocks
  simply lack the token comment — the source is still there.

### Bitable → `<!-- opendoc:base_refer ... -->` + table/summary + link

Feishu has two bitable forms — created inline (source tag `<bitable>`) and referencing
an existing app (source tag `<base_refer>`) — and the engine normalizes **both** into
the same `opendoc:base_refer` comment block. One of three shapes:

1. **Rendered inline (row count ≤ threshold, default 200)**:
   ```
   <!-- opendoc:base_refer table-id="tblXXXX" token="bascnXXXX" view-id="vewXXXX" -->

   | Column A | Column B |
   | --- | --- |
   | ... | ... |

   [Open the bitable in Feishu](https://<tenant>.feishu.cn/base/bascnXXXX?table=tblXXXX&view=vewXXXX)
   ```
2. **Over threshold**: no full table — schema plus the true row count:
   `Bitable (<N> rows, over the inline threshold; showing structure only):` followed by
   `- Columns: <column> (<type>), ...` (the column list), ending with the same
   `[Open the bitable in Feishu]` link. Counted as `bitables_oversize` in the report.
3. **Fetch failed** (API error, or token/table-id missing so no drill-down is
   possible): only the comment + the line
   `> Failed to fetch the bitable; open it online to view.` + the link (which may be
   absent when the token is missing). Counted as `bitables_failed`.

- **What was lost**: shape 2 loses the row data (schema + row count remain); shape 3
  loses the entire table content. Shape 1 is lossless.
- **Drill down**: the comment's `token` (= the bitable app token) and `table-id` are
  exact coordinates. Open the link with them, or call
  `opendoc lark-engine api POST /open-apis/bitable/v1/apps/<token>/tables/<table-id>/records/search
  --params '{"page_size":200}' --data '{}'` for exact data. The threshold is config
  `bitable_inline_max_rows` (`opendoc init --bitable-inline-max-rows`).

### Embedded spreadsheet → `<sheet>` (preserved verbatim, data not fetched)

```
<sheet sheet-id="M721og" token="Z71estwEkhNN2yta3uuc3C1tnqh"></sheet>
```

- **Actual behavior**: the engine **preserves verbatim** the `<sheet>` tag the lark
  engine emits (it is a "recognized" tag, not counted as an unknown block) and **fetches
  no spreadsheet data, adds no automatic online link**. (Pulling sheet data needs the
  full sheets API; the value/cost ratio was judged too low — not implemented.)
- **What was lost**: the entire table content — the body holds only this empty tag with
  its `sheet-id` + `token`.
- **Drill down**: `token` is the sheet's obj token. `opendoc resolve <token>`, or follow
  the **parent document's** frontmatter `url` online and view the embedded sheet in the
  original.

### Standalone resource nodes → placeholder `.md`

A wiki-tree node whose obj_type is sheet/bitable/slides/mindnote/file (the **whole
node**, not an embed) becomes a placeholder file with normal frontmatter
(`id`/`type`/`url` all present) and this body (verbatim engine output):

```
> Standalone resource node (<type>): this content is not expanded in the mirror; drill down or open the original online to view it.
>
> Drill-down token: `<obj_token>`
>
> View online: <url>
```

- **What was lost**: the resource's content is not expanded (the placeholder exists so
  the tree has no hole).
- **Drill down**: open the frontmatter `url` directly, or take the token in the body to
  `opendoc resolve` / the type-specific API.

---

## Notion

### Oversized page truncation → `<!-- opendoc:truncated -->`

The markdown endpoint reported `truncated:true` (too many blocks; the body was cut).
The engine prepends this comment at the **top of the file** (after the frontmatter).
Counted as `truncated_pages` in the report.

- **What was lost**: trailing blocks of the page.
- **Drill down**: follow the frontmatter `url` to the full page online.

### Unrenderable blocks → `<!-- opendoc:unknown-blocks n="N" ids="id1,id2" -->`

The endpoint returned `unknown_block_ids` (blocks it could not render itself). The
engine prepends at the top of the file:

```
<!-- opendoc:unknown-blocks n="2" ids="block-uuid-1,block-uuid-2" -->
```

Counted as `unknown_block_ids` in the report.

- **What was lost**: those blocks' content (the endpoint gave no markdown for them).
- **Drill down**: the `ids` are Notion block ids; follow the frontmatter `url` online
  and locate the blocks.

### Sub-page/database references → relative links (or the original URL)

`<page url="...">Title</page>` and `<database url="..." ...>Name</database>` are
internal links. In the finalize phase the engine rewrites references whose **target is
mirrored** into local relative paths (following links = graph traversal); references to
unmirrored targets (unauthorized/external) keep the original Notion URL untouched. This
is link rewriting, not loss — it is not counted as degradation.

### synced_block → `<synced_block>` (preserved verbatim)

The synced block's **content itself is expanded** into the body; the `<synced_block>`
tag is only a metadata marker — preserved verbatim, not counted as an unknown block.

### Property degradation (database-row frontmatter `properties:`)

Notion database-row properties are flattened into the frontmatter; most types render as
readable strings. Lossy/unsupported types leave drill-down leads or placeholders:

- **relation**: rendered as the **related pages' titles** (when the targets are
  mirrored), otherwise degraded to a list of **page canonical ids** — the id itself is
  the drill-down lead; `opendoc resolve <id>` locates it. Shaped like
  `properties: { 关联: "[Title A, 3762d1de-...]" }`.
- **rollup**: scalars render directly; arrays render as a `[...]` list; irreducible
  rollups leave `<rollup: <type>>`.
- **formula**: rendered by result type; unsupported result types leave
  `<unsupported: formula/<type>>`.
- **Other unsupported types**: `<unsupported: <type>>` (or
  `<unsupported: unparseable>` when the value cannot be parsed).

The complete type mapping (the relation/rollup/formula degradation decisions) is in the
source repo: `docs/notion-properties-mapping.md`.

---

## Unknown blocks (both platforms — the red line's last link)

Any opening tag the engine does not recognize (not in its recognized-tag set) = an
**unknown block**: **preserved verbatim in the body**, never silently dropped, and
counted as `unknown_blocks` in the report. Tags inside code fences, inline code, and
HTML comments are not counted (so examples are never miscounted).

- **When you see an unknown tag**: treat it as a lead — "a block opendoc does not
  support yet lived here"; if you need the content, follow the frontmatter `url` online.
  If some block type shows up often enough to be worth supporting, add it to
  `recognizedTags` in the source repo's `internal/feishu/degrade.go` /
  `internal/notion/degrade.go` (a checkout of `github.com/arcships/open-doc-cli`) and
  implement the rendering.

## Degradation counts in reports

Both the end of `opendoc sync` output and `opendoc status` print degradation counts; the
fields map to this document: `unknown_blocks`, `bitables_rendered`, `bitables_oversize`,
`bitables_failed`, `truncated_pages`, `unknown_block_ids`, and their sum
`degradations`. A non-zero count is not an error — it is the ledger of where loss
happened, so you can decide whether drilling down is worth it.
