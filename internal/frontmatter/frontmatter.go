// Package frontmatter renders the YAML frontmatter block that heads every
// mirrored Markdown file: the read-only red line, the stable platform
// ID, the online URL, and the breadcrumb/timestamps that retrieval and
// incremental sync depend on.
//
// Rendering is deterministic and hand-built (no YAML library) so the key order
// is fixed and the output diff stays stable across runs.
package frontmatter

import (
	"strings"
	"time"
)

// redLine is the read-only warning comment placed at the top of every
// frontmatter block. It is the only delivery channel for the
// red line that is guaranteed to travel with the content itself.
const redLine = "# Read-only mirror: local changes are overwritten on the next sync; to change content, follow the url and edit online"

// Property is one database-row column value flattened into the frontmatter
// `properties:` map. Both fields are plain strings so every property
// is greppable; the engine renders platform property types down to strings
// (docs/notion-properties-mapping.md) before handing them here.
type Property struct {
	Key   string
	Value string
}

// Doc is the set of fields rendered into a frontmatter block. Properties is
// populated only for Notion database rows (db_row); all other documents leave it
// nil and no `properties:` block is emitted.
type Doc struct {
	// ID is the platform-native stable identifier (obj_token / page_id).
	ID string
	// Source is the platform tag ("feishu" | "notion").
	Source string
	// Type is the document type (page | docx | db | db_row | folder | sheet ...).
	Type string
	// URL is the canonical online URL (one-step jump back to the original).
	URL string
	// Title is the human title.
	Title string
	// Breadcrumb is the online ancestor path, " / "-joined (may be empty).
	Breadcrumb string
	// Updated is the remote last-edited time (may be zero if unknown).
	Updated time.Time
	// Synced is the local fetch time.
	Synced time.Time
	// Properties are the flattened database-row column values (db_row only). The
	// caller supplies them in the order they should render (the engine sorts them
	// deterministically). Nil/empty means no `properties:` block is emitted.
	Properties []Property
}

// Render returns the complete frontmatter block, including the leading and
// trailing "---" fences and a trailing newline. String values are always
// double-quoted and escaped so titles containing colons, quotes, or leading
// special characters stay valid YAML.
func Render(d Doc) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString(redLine)
	b.WriteByte('\n')

	writeStr(&b, "id", d.ID)
	writeStr(&b, "source", d.Source)
	writeStr(&b, "type", d.Type)
	writeStr(&b, "url", d.URL)
	writeStr(&b, "title", d.Title)
	writeStr(&b, "breadcrumb", d.Breadcrumb)
	writeTime(&b, "updated", d.Updated)
	writeTime(&b, "synced", d.Synced)
	writeProperties(&b, d.Properties)

	b.WriteString("---\n")
	return b.String()
}

// writeProperties writes the `properties:` map when there is at least
// one entry. Each value is always double-quoted (like every other scalar);
// each key is quoted only when it is not a plain, unambiguous YAML key so common
// names like `状态` (a CJK key) stay readable while a key with a colon/leading
// marker stays valid.
func writeProperties(b *strings.Builder, props []Property) {
	if len(props) == 0 {
		return
	}
	b.WriteString("properties:\n")
	for _, p := range props {
		b.WriteString("  ")
		b.WriteString(propertyKey(p.Key))
		b.WriteString(": ")
		b.WriteString(quote(p.Value))
		b.WriteByte('\n')
	}
}

// propertyKey renders a mapping key: bare when it is a safe plain scalar,
// double-quoted (escaped) otherwise, so the YAML stays valid for any property
// name Notion allows.
func propertyKey(k string) string {
	if isPlainKey(k) {
		return k
	}
	return quote(k)
}

// isPlainKey reports whether k can be emitted as a bare YAML mapping key: no
// leading/trailing space, no character that would need quoting, and not an
// indicator that must start a quoted scalar.
func isPlainKey(k string) bool {
	if k == "" || k != strings.TrimSpace(k) {
		return false
	}
	switch k[0] {
	case '!', '&', '*', '-', '?', ':', ',', '[', ']', '{', '}', '#', '|', '>', '@', '`', '"', '\'', '%':
		return false
	}
	for _, r := range k {
		switch r {
		case ':', '#', '\n', '\r', '\t', '"', '\\':
			return false
		}
	}
	return true
}

// writeStr writes `key: "value"` with the value YAML-escaped.
func writeStr(b *strings.Builder, key, val string) {
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(quote(val))
	b.WriteByte('\n')
}

// writeTime writes `key: <RFC3339>` for a non-zero time, or `key: null` when the
// timestamp is unknown. Times are normalised to UTC.
func writeTime(b *strings.Builder, key string, t time.Time) {
	b.WriteString(key)
	b.WriteString(": ")
	if t.IsZero() {
		b.WriteString("null")
	} else {
		b.WriteString(t.UTC().Format(time.RFC3339))
	}
	b.WriteByte('\n')
}

// quote renders s as a double-quoted YAML scalar with the minimal escaping YAML
// requires inside double quotes: backslash, double-quote, and the common
// control characters.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
