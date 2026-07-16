package notion

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/arcships/open-doc-cli/internal/adapter"
)

// Properties type mapping. Every property type renders to a plain, greppable
// string; lossy types leave a drill-down trail (page ids, source notes) rather
// than being silently dropped.
// The full table lives in docs/notion-properties-mapping.md — keep it in sync.
//
// The row's own title property (type "title") is intentionally NOT emitted here:
// it already lives in the frontmatter `title:` field, so repeating it would be
// noise. Every other property is emitted, in property-name order, even when its
// value is empty (the key still conveys the schema and stays greppable).

// selectOption is a select / status option (also a multi_select element).
type selectOption struct {
	Name string `json:"name"`
}

// person is a Notion user reference (people / created_by / last_edited_by).
type person struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// fileItem is a files-property element; only its display name is rendered.
type fileItem struct {
	Name string `json:"name"`
}

// relationRef is one related page reference.
type relationRef struct {
	ID string `json:"id"`
}

// dateValue is a date / formula-date / rollup-date value.
type dateValue struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// uniqueID is the auto-incrementing unique_id property.
type uniqueID struct {
	Prefix string `json:"prefix"`
	Number *int64 `json:"number"`
}

// formulaValue is a computed formula result (one of string/number/boolean/date).
type formulaValue struct {
	Type    string     `json:"type"`
	String  *string    `json:"string"`
	Number  *float64   `json:"number"`
	Boolean *bool      `json:"boolean"`
	Date    *dateValue `json:"date"`
}

// rollupValue is a rollup result: a scalar (number/date) or an array of nested
// property values.
type rollupValue struct {
	Type   string            `json:"type"`
	Number *float64          `json:"number"`
	Date   *dateValue        `json:"date"`
	Array  []json.RawMessage `json:"array"`
}

// propValue is the union of the value shapes a Notion row property can take. Only
// the field matching Type is populated. title/rich_text are kept raw because a
// data_source's property schema encodes them as config objects rather than
// arrays (P0 caveat); reading a row value they are arrays, but the raw form
// tolerates either without panicking.
type propValue struct {
	Type           string          `json:"type"`
	Title          json.RawMessage `json:"title"`
	RichText       json.RawMessage `json:"rich_text"`
	Number         *float64        `json:"number"`
	Select         *selectOption   `json:"select"`
	Status         *selectOption   `json:"status"`
	MultiSelect    []selectOption  `json:"multi_select"`
	Date           *dateValue      `json:"date"`
	Checkbox       *bool           `json:"checkbox"`
	URL            *string         `json:"url"`
	Email          *string         `json:"email"`
	PhoneNumber    *string         `json:"phone_number"`
	People         []person        `json:"people"`
	Files          []fileItem      `json:"files"`
	Relation       []relationRef   `json:"relation"`
	Formula        *formulaValue   `json:"formula"`
	Rollup         *rollupValue    `json:"rollup"`
	CreatedTime    *string         `json:"created_time"`
	LastEditedTime *string         `json:"last_edited_time"`
	CreatedBy      *person         `json:"created_by"`
	LastEditedBy   *person         `json:"last_edited_by"`
	UniqueID       *uniqueID       `json:"unique_id"`
}

// renderRowProperties turns a row's raw properties map into ordered, rendered
// entries plus a canonical serialization for change detection. Properties are
// emitted in property-name order for determinism; the title property is skipped
// (it is the frontmatter title). titles resolves relation target ids to titles.
func renderRowProperties(raw map[string]json.RawMessage, titles map[string]string) adapter.RowProperties {
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)

	var entries []adapter.PropertyKV
	for _, name := range names {
		var pv propValue
		if err := json.Unmarshal(raw[name], &pv); err != nil {
			// Never drop silently: surface an unrenderable value as a placeholder.
			entries = append(entries, adapter.PropertyKV{Key: name, Value: `<unsupported: unparseable>`})
			continue
		}
		if pv.Type == "title" {
			continue // already the frontmatter title
		}
		entries = append(entries, adapter.PropertyKV{Key: name, Value: renderProperty(pv, titles)})
	}

	return adapter.RowProperties{Entries: entries, Canonical: canonicalProps(entries)}
}

// canonicalProps builds a stable serialization of rendered entries, folded into
// the row's content_hash. Entries are already name-sorted; the form is
// unambiguous (length-prefixed keys) so distinct property sets never collide.
func canonicalProps(entries []adapter.PropertyKV) string {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%d:%s=%s\n", len(e.Key), e.Key, e.Value)
	}
	return b.String()
}

// renderProperty maps one property value to a plain, greppable string per the
// mapping table. Unknown types and null values degrade to a visible marker or
// an empty string rather than being dropped.
func renderProperty(pv propValue, titles map[string]string) string {
	switch pv.Type {
	case "rich_text":
		return richTextPlain(pv.RichText)
	case "number":
		return numStr(pv.Number)
	case "select":
		return optName(pv.Select)
	case "status":
		return optName(pv.Status)
	case "multi_select":
		return renderList(optNames(pv.MultiSelect))
	case "date":
		return renderDate(pv.Date)
	case "checkbox":
		if pv.Checkbox != nil && *pv.Checkbox {
			return "true"
		}
		return "false"
	case "url":
		return strDeref(pv.URL)
	case "email":
		return strDeref(pv.Email)
	case "phone_number":
		return strDeref(pv.PhoneNumber)
	case "people":
		return renderList(personNames(pv.People))
	case "files":
		return renderList(fileNames(pv.Files))
	case "relation":
		return renderList(relationNames(pv.Relation, titles))
	case "formula":
		return renderFormula(pv.Formula)
	case "rollup":
		return renderRollup(pv.Rollup, titles)
	case "created_time":
		return strDeref(pv.CreatedTime)
	case "last_edited_time":
		return strDeref(pv.LastEditedTime)
	case "created_by":
		return personName(pv.CreatedBy)
	case "last_edited_by":
		return personName(pv.LastEditedBy)
	case "unique_id":
		return renderUniqueID(pv.UniqueID)
	default:
		return fmt.Sprintf("<unsupported: %s>", pv.Type)
	}
}

// richTextPlain concatenates the plain_text of a rich-text array, tolerating the
// schema-config shape ({}) by returning "" for anything that is not an array.
func richTextPlain(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var rt []richText
	if err := json.Unmarshal(raw, &rt); err != nil {
		return ""
	}
	return titleText(rt)
}

// numStr renders a number without a trailing ".0" for integers, "" for null.
func numStr(n *float64) string {
	if n == nil {
		return ""
	}
	return strconv.FormatFloat(*n, 'f', -1, 64)
}

func optName(o *selectOption) string {
	if o == nil {
		return ""
	}
	return o.Name
}

func optNames(os []selectOption) []string {
	names := make([]string, 0, len(os))
	for _, o := range os {
		names = append(names, o.Name)
	}
	return names
}

func personNames(ps []person) []string {
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		names = append(names, personName(&p))
	}
	return names
}

// personName prefers the display name, falling back to the user id (a drill-down
// trail) when the name is not exposed (e.g. a guest or a token without user
// read scope).
func personName(p *person) string {
	if p == nil {
		return ""
	}
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

func fileNames(fs []fileItem) []string {
	names := make([]string, 0, len(fs))
	for _, f := range fs {
		names = append(names, f.Name)
	}
	return names
}

// relationNames renders related pages as their titles when the target is a
// mirrored page, else the canonical id (drill-down trail).
func relationNames(rs []relationRef, titles map[string]string) []string {
	names := make([]string, 0, len(rs))
	for _, r := range rs {
		id := canonicalID(r.ID)
		if t, ok := titles[id]; ok && t != "" {
			names = append(names, t)
		} else {
			names = append(names, id)
		}
	}
	return names
}

// renderList renders a slice as a bracketed, comma-joined string ("[a, b]"),
// always greppable and stable. An empty slice renders "[]".
func renderList(items []string) string {
	return "[" + strings.Join(items, ", ") + "]"
}

// renderDate renders a date as its ISO start, or "start/end" for a range, "" for
// null.
func renderDate(d *dateValue) string {
	if d == nil || d.Start == "" {
		return ""
	}
	if d.End != "" {
		return d.Start + "/" + d.End
	}
	return d.Start
}

// renderFormula renders a computed formula result by its result type.
func renderFormula(f *formulaValue) string {
	if f == nil {
		return ""
	}
	switch f.Type {
	case "string":
		return strDeref(f.String)
	case "number":
		return numStr(f.Number)
	case "boolean":
		if f.Boolean != nil && *f.Boolean {
			return "true"
		}
		return "false"
	case "date":
		return renderDate(f.Date)
	default:
		return fmt.Sprintf("<unsupported: formula/%s>", f.Type)
	}
}

// renderRollup renders a rollup: a scalar value directly, or an array as a
// bracketed list of its nested rendered values. A non-scalar/non-array rollup
// leaves a note naming its type so the loss is traceable.
func renderRollup(r *rollupValue, titles map[string]string) string {
	if r == nil {
		return ""
	}
	switch r.Type {
	case "number":
		return numStr(r.Number)
	case "date":
		return renderDate(r.Date)
	case "array":
		items := make([]string, 0, len(r.Array))
		for _, raw := range r.Array {
			var pv propValue
			if err := json.Unmarshal(raw, &pv); err != nil {
				items = append(items, "<unsupported: unparseable>")
				continue
			}
			items = append(items, renderProperty(pv, titles))
		}
		return renderList(items)
	case "incomplete", "unsupported":
		return fmt.Sprintf("<rollup: %s>", r.Type)
	default:
		return fmt.Sprintf("<rollup: %s>", r.Type)
	}
}

func renderUniqueID(u *uniqueID) string {
	if u == nil || u.Number == nil {
		return ""
	}
	if u.Prefix != "" {
		return fmt.Sprintf("%s-%d", u.Prefix, *u.Number)
	}
	return strconv.FormatInt(*u.Number, 10)
}

func strDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
