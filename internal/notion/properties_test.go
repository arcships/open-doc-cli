package notion

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRenderPropertyMapping exercises the full properties type-mapping table
// (docs/notion-properties-mapping.md) with fixture JSON, including the types the
// real workspace does not exercise (relation/rollup/formula/people/files/...), so
// every branch round-trips to its documented rendering.
func TestRenderPropertyMapping(t *testing.T) {
	titles := map[string]string{
		"11111111-1111-1111-1111-111111111111": "Linked Page A",
		// The second relation id is intentionally absent → falls back to the id.
	}

	cases := []struct {
		name string
		json string
		want string
	}{
		{"rich_text", `{"type":"rich_text","rich_text":[{"plain_text":"hello "},{"plain_text":"world"}]}`, "hello world"},
		{"number_int", `{"type":"number","number":42}`, "42"},
		{"number_float", `{"type":"number","number":3.5}`, "3.5"},
		{"number_null", `{"type":"number","number":null}`, ""},
		{"select", `{"type":"select","select":{"name":"Term 1"}}`, "Term 1"},
		{"select_null", `{"type":"select","select":null}`, ""},
		{"status", `{"type":"status","status":{"name":"已完成"}}`, "已完成"},
		{"multi_select", `{"type":"multi_select","multi_select":[{"name":"a"},{"name":"b"}]}`, "[a, b]"},
		{"multi_select_empty", `{"type":"multi_select","multi_select":[]}`, "[]"},
		{"date_single", `{"type":"date","date":{"start":"2024-01-01"}}`, "2024-01-01"},
		{"date_range", `{"type":"date","date":{"start":"2024-01-01","end":"2024-02-01"}}`, "2024-01-01/2024-02-01"},
		{"date_null", `{"type":"date","date":null}`, ""},
		{"checkbox_true", `{"type":"checkbox","checkbox":true}`, "true"},
		{"checkbox_false", `{"type":"checkbox","checkbox":false}`, "false"},
		{"url", `{"type":"url","url":"https://example.com"}`, "https://example.com"},
		{"email", `{"type":"email","email":"x@y.com"}`, "x@y.com"},
		{"phone", `{"type":"phone_number","phone_number":"+61 400 000 000"}`, "+61 400 000 000"},
		{"people", `{"type":"people","people":[{"name":"Tao"},{"name":"Ben"}]}`, "[Tao, Ben]"},
		{"people_noname", `{"type":"people","people":[{"id":"user-123"}]}`, "[user-123]"},
		{"files", `{"type":"files","files":[{"name":"a.pdf"},{"name":"b.png"}]}`, "[a.pdf, b.png]"},
		{"relation_resolved", `{"type":"relation","relation":[{"id":"11111111-1111-1111-1111-111111111111"}]}`, "[Linked Page A]"},
		{"relation_unresolved", `{"type":"relation","relation":[{"id":"22222222222222222222222222222222"}]}`, "[22222222-2222-2222-2222-222222222222]"},
		{"rollup_number", `{"type":"rollup","rollup":{"type":"number","number":7}}`, "7"},
		{"rollup_date", `{"type":"rollup","rollup":{"type":"date","date":{"start":"2024-03-01"}}}`, "2024-03-01"},
		{"rollup_array", `{"type":"rollup","rollup":{"type":"array","array":[{"type":"select","select":{"name":"x"}},{"type":"number","number":2}]}}`, "[x, 2]"},
		{"rollup_incomplete", `{"type":"rollup","rollup":{"type":"incomplete"}}`, "<rollup: incomplete>"},
		{"formula_string", `{"type":"formula","formula":{"type":"string","string":"computed"}}`, "computed"},
		{"formula_number", `{"type":"formula","formula":{"type":"number","number":9}}`, "9"},
		{"formula_boolean", `{"type":"formula","formula":{"type":"boolean","boolean":true}}`, "true"},
		{"formula_date", `{"type":"formula","formula":{"type":"date","date":{"start":"2024-04-01"}}}`, "2024-04-01"},
		{"created_time", `{"type":"created_time","created_time":"2024-01-01T00:00:00.000Z"}`, "2024-01-01T00:00:00.000Z"},
		{"last_edited_time", `{"type":"last_edited_time","last_edited_time":"2024-05-01T00:00:00.000Z"}`, "2024-05-01T00:00:00.000Z"},
		{"created_by", `{"type":"created_by","created_by":{"name":"Tao"}}`, "Tao"},
		{"last_edited_by", `{"type":"last_edited_by","last_edited_by":{"name":"Ben"}}`, "Ben"},
		{"unique_id_prefix", `{"type":"unique_id","unique_id":{"prefix":"TASK","number":12}}`, "TASK-12"},
		{"unique_id_noprefix", `{"type":"unique_id","unique_id":{"number":5}}`, "5"},
		{"unknown_type", `{"type":"verification","verification":{"state":"verified"}}`, "<unsupported: verification>"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := map[string]json.RawMessage{"P": json.RawMessage(tc.json)}
			got := renderRowProperties(raw, titles)
			if len(got.Entries) != 1 {
				t.Fatalf("want 1 entry, got %d", len(got.Entries))
			}
			if got.Entries[0].Value != tc.want {
				t.Errorf("render(%s) = %q, want %q", tc.name, got.Entries[0].Value, tc.want)
			}
		})
	}
}

// TestRenderRowPropertiesOrderAndTitleOmission checks deterministic name-order
// and that the title property is dropped (it is the frontmatter title).
func TestRenderRowPropertiesOrderAndTitleOmission(t *testing.T) {
	raw := map[string]json.RawMessage{
		"Zeta":  json.RawMessage(`{"type":"number","number":1}`),
		"Alpha": json.RawMessage(`{"type":"number","number":2}`),
		"Name":  json.RawMessage(`{"type":"title","title":[{"plain_text":"Row"}]}`),
	}
	got := renderRowProperties(raw, nil)
	if len(got.Entries) != 2 {
		t.Fatalf("want 2 entries (title omitted), got %d: %+v", len(got.Entries), got.Entries)
	}
	if got.Entries[0].Key != "Alpha" || got.Entries[1].Key != "Zeta" {
		t.Errorf("entries not name-sorted: %+v", got.Entries)
	}
}

// TestCanonicalPropsChangeDetection proves the canonical serialization changes
// when a value changes and is stable otherwise — the basis for property-only
// dirty detection.
func TestCanonicalPropsChangeDetection(t *testing.T) {
	base := map[string]json.RawMessage{
		"状态": json.RawMessage(`{"type":"status","status":{"name":"进行中"}}`),
	}
	changed := map[string]json.RawMessage{
		"状态": json.RawMessage(`{"type":"status","status":{"name":"已完成"}}`),
	}
	c1 := renderRowProperties(base, nil).Canonical
	c1again := renderRowProperties(base, nil).Canonical
	c2 := renderRowProperties(changed, nil).Canonical
	if c1 != c1again {
		t.Errorf("canonical not stable: %q vs %q", c1, c1again)
	}
	if c1 == c2 {
		t.Errorf("canonical should differ after a value change: %q", c1)
	}
	if !strings.Contains(c2, "已完成") {
		t.Errorf("canonical should reflect the new value: %q", c2)
	}
}
