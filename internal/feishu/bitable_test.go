package feishu

import (
	"encoding/json"
	"testing"
)

func mustRawMap(s string) map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(err)
	}
	return m
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestStringifyCell(t *testing.T) {
	cases := []struct {
		in   json.RawMessage
		want string
	}{
		{raw(`"plain text"`), "plain text"},
		{raw(`123`), "123"},
		{raw(`12.5`), "12.5"},
		{raw(`true`), "true"},
		{raw(`null`), ""},
		{raw(``), ""},
		// Rich-text segment array.
		{raw(`[{"text":"hello ","type":"text"},{"text":"world","type":"text"}]`), "hello  world"},
		// Object with a link.
		{raw(`{"link":"https://x","text":"X"}`), "X"},
		// User object.
		{raw(`[{"name":"Tao","id":"u1"}]`), "Tao"},
	}
	for _, c := range cases {
		if got := stringifyCell(c.in); got != c.want {
			t.Errorf("stringifyCell(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildAssetRefsPositional(t *testing.T) {
	// Two images in markdown; two <img> tokens in XML, in the same order but with
	// different (regenerated) authcode URLs. Correlation is positional.
	md := "![](https://s/authA)\ntext\n![alt](https://s/authB)"
	xml := `<img src="TOK_A" href="https://s/xA"/><img src="TOK_B" href="https://s/xB" name="b.png"/>`
	refs := buildAssetRefs(md, xml)
	if len(refs) != 2 {
		t.Fatalf("want 2 refs, got %d: %+v", len(refs), refs)
	}
	if refs[0].RemoteKey != "TOK_A" || refs[0].BodyURL != "https://s/authA" {
		t.Errorf("ref0 = %+v", refs[0])
	}
	if refs[1].RemoteKey != "TOK_B" || refs[1].BodyURL != "https://s/authB" || refs[1].Filename != "b.png" {
		t.Errorf("ref1 = %+v", refs[1])
	}
}

func TestBuildAssetRefsMoreMarkdownThanXML(t *testing.T) {
	// An extra markdown image with no XML token is skipped (left untouched later).
	md := "![](https://s/a)\n![](https://s/b)"
	xml := `<img src="TOK_A"/>`
	refs := buildAssetRefs(md, xml)
	if len(refs) != 1 || refs[0].RemoteKey != "TOK_A" || refs[0].BodyURL != "https://s/a" {
		t.Fatalf("refs = %+v", refs)
	}
}

func TestExtractWhiteboardTokens(t *testing.T) {
	xml := `<whiteboard token="W1" type="mermaid">src</whiteboard>x<whiteboard type="mermaid" token="W2">s</whiteboard>`
	toks := extractWhiteboardTokens(xml)
	if len(toks) != 2 || toks[0] != "W1" || toks[1] != "W2" {
		t.Fatalf("tokens = %+v", toks)
	}
}
