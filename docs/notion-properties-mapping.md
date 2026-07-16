# Notion properties → frontmatter type mapping

> Reference: how each Notion database property type lands in a row file's frontmatter `properties:` map, including how the lossy types (relation / rollup / formula) degrade. Implemented in `internal/notion/properties.go`, tested in `internal/notion/properties_test.go` (covers the full table).

## Principles

A database row's (`db_row`) properties are fetched in one shot via `POST /v1/data_sources/{id}/query` (one query per database, not one per row), and flattened into the row file's frontmatter `properties:` map. Three red lines:

1. **Every type renders to greppable plain text** — every value is a **string scalar** (even `multi_select` renders to a `"[a, b]"` string), so the frontmatter shape stays uniform, diffs stay stable, and grep works.
2. **Lossy types leave a drill-down trail** — `relation` falls back to the related page's ID/title, array-valued `rollup` renders item by item; nothing is ever dropped silently.
3. **Anything unrenderable gets a placeholder** — unknown types become `"<unsupported: type-name>"`, so they can never quietly vanish.

The row's own **title** property (type `title`) is **not** written into `properties:` — it's already the frontmatter `title:`, and duplicating it would just be noise. Every other property is always written, **sorted by property name** (deterministic); empty values still keep their key (the key itself is schema signal).

## Mapping table

| Notion type | Local rendering | Example |
|---|---|---|
| `title` | **Omitted** (already frontmatter `title:`) | — |
| `rich_text` | Plain text (plain_text segments concatenated) | `hello world` |
| `number` | Number (integers without `.0`) | `42` / `3.5` |
| `select` | Option name | `2024 S1` |
| `status` | Status name | `Done` |
| `multi_select` | `[a, b]` (bracketed, comma-joined string) | `[Required, Core]` |
| `date` | ISO string; ranges as `start/end` | `2024-01-01` / `2024-01-01/2024-02-01` |
| `checkbox` | `true` / `false` | `true` |
| `url` / `email` / `phone_number` | Raw string | `https://…` / `x@y.com` |
| `people` | List of names (falls back to user id if unnamed) | `[Tao, Ben]` |
| `files` | List of file names | `[a.pdf, b.png]` |
| `created_time` / `last_edited_time` | ISO string | `2024-01-01T00:00:00.000Z` |
| `created_by` / `last_edited_by` | Name (falls back to user id) | `Tao` |
| `unique_id` | `PREFIX-number` (number only if no prefix) | `TASK-12` |
| `formula` | Computed value, rendered by its result type (string/number/boolean/date) | `computed` / `9` / `true` / `2024-04-01` |
| **`relation`** | List of related pages' **titles** (when resolvable from the enumeration), otherwise normalized page IDs (drill-down trail) | `[Advanced Database Systems]` / `[3762d1de-…]` |
| **`rollup`** | Scalars (number/date) rendered directly; arrays rendered item by item as `[…]`; `incomplete`/`unsupported` left as a `<rollup: type>` note | `7` / `[x, 2]` / `<rollup: incomplete>` |
| Unknown type | `<unsupported: type-name>` (red line: never silently dropped) | `<unsupported: verification>` |

### Resolving relation titles

A `relation`'s target pages live in the same workspace as this database, and are usually also in the mirror. The engine passes the adapter an id→title table built from enumeration; when resolution succeeds it renders the **title** (human-readable, and greppable against the target file's name); when it doesn't (external page, or not connected) it falls back to the **normalized page ID** as a drill-down trail. The target row's own file carries `id`/`url` in its frontmatter, so either the title or the id is one hop away from the target.

### Rollup arrays

Each element of an array-valued rollup is itself a property value; it's rendered with the same renderer, item by item, and joined into `[…]`. Non-scalar, non-array results (`incomplete`/`unsupported`) are left as a `<rollup: type>` note, so the loss stays visible.

## content_hash semantics

A row's `content_hash` = `sha256(body markdown ⊕ normalized properties)` (`⊕` = fixed-length-delimited concatenation). The body comes from the markdown endpoint and the properties from the query; both feed the hash, so **a properties-only edit is still detected as dirty**. The normalized string is built from sorted `key=value` lines (length-prefixed to avoid ambiguity), so it's deterministic and byte-stable across runs. This hash still honors the pre-rewrite rule (content_hash is always computed on the artifact **before** internal-link rewriting, see [architecture.md](dev/architecture.md)) — the properties' normalized string is also a pre-rewrite rendering, unaffected by the final link-rewrite pass.

## `_index.md` row index

A database node = a directory + `_index.md` (machine-generated, not a mirrored page, has no frontmatter of its own, and is excluded from the manifest — so it doesn't interfere with delete reconciliation or INDEX.md). Regenerated every sync, deterministically ordered (rows sorted by **title, then id**; columns sorted by **property name**, capped at 4 columns). The table has a title column (linking to the row's local file) plus a handful of key property columns. Implemented in `internal/engine/dbindex.go`.
