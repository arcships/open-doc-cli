package feishu

import (
	"context"
	"strings"
	"testing"

	"github.com/arcships/open-doc-cli/internal/config"
)

func TestCountUnknownBlocks(t *testing.T) {
	md := "# Title\n\n" +
		"<callout emoji=\"💡\">recognized</callout>\n\n" +
		"<mention id=\"u1\"></mention> and <equation>x^2</equation>\n\n" +
		"```python\n<not-a-tag-in-code>\n```\n\n" +
		"inline `<also-not-a-tag>` code\n\n" +
		"<!-- opendoc:whiteboard token=\"T\" --> <ghost>\n"
	// Unknown: <mention>, <equation>, <ghost> = 3. The code-fenced and inline-code
	// pseudo-tags and the commented-out one must not count; callout is recognized.
	if got := countUnknownBlocks(md); got != 3 {
		t.Fatalf("countUnknownBlocks = %d, want 3", got)
	}
}

func TestAnnotateWhiteboards(t *testing.T) {
	md := "# 画板\n\n```mermaid\nflowchart LR\nA-->B\n```\n\nmid\n\n```mermaid\ngraph TD\n```\n"
	out := annotateWhiteboards(md, []string{"TOK1", "TOK2"})
	if strings.Count(out, "```mermaid") != 2 {
		t.Fatalf("mermaid fences lost:\n%s", out)
	}
	if !strings.Contains(out, "<!-- opendoc:whiteboard token=\"TOK1\" -->\n```mermaid") {
		t.Fatalf("first whiteboard token comment missing:\n%s", out)
	}
	if !strings.Contains(out, "<!-- opendoc:whiteboard token=\"TOK2\" -->\n```mermaid") {
		t.Fatalf("second whiteboard token comment missing:\n%s", out)
	}
}

func TestDegradePreservesUnknownVerbatim(t *testing.T) {
	a := NewAdapter(config.Feishu{}, &fakeRunner{}, 200)
	md := "# Doc\n\n<addon addon-id=\"x\">payload</addon>\n\nnormal **text**\n"
	body, deg := a.degrade(context.Background(), md, "", "https://acme.feishu.cn/docx/D1")
	if deg.UnknownBlocks != 1 {
		t.Fatalf("UnknownBlocks = %d, want 1", deg.UnknownBlocks)
	}
	// The unknown tag must survive verbatim (red line: never silently drop).
	if !strings.Contains(body, `<addon addon-id="x">payload</addon>`) {
		t.Fatalf("unknown block not preserved verbatim:\n%s", body)
	}
}

// bitableFakeRunner serves fields + records/search responses for a single
// bitable.
func bitableFakeRunner(fields, records string) *fakeRunner {
	return &fakeRunner{responses: []fakeResponse{
		{"/fields", ok(fields)},
		{"/records/search", ok(records)},
	}}
}

const smallFields = `{"has_more":false,"total":3,"items":[
	{"field_name":"文本","ui_type":"Text","is_primary":true},
	{"field_name":"文本 2","ui_type":"Text"},
	{"field_name":"单选","ui_type":"SingleSelect"}
]}`

// smallRecords mirrors the records/search response shape: text cells are
// segment arrays ([{text,type}]), not plain strings (verified live).
const smallRecords = `{"has_more":false,"total":3,"items":[
	{"fields":{"文本":[{"text":"23232 232 ","type":"text"}]}},
	{"fields":{}},
	{"fields":{"单选":"甲","文本 2":"b"}}
]}`

func TestDegradeBaseReferRendersTable(t *testing.T) {
	f := bitableFakeRunner(smallFields, smallRecords)
	a := NewAdapter(config.Feishu{}, f, 200)
	md := "# 表\n\n<base_refer table-id=\"tbl1\" token=\"app1\" view-id=\"vew1\"></base_refer>\n"
	xml := "<base_refer table-id=\"tbl1\" token=\"app1\" view-id=\"vew1\"></base_refer>"
	body, deg := a.degrade(context.Background(), md, xml, "https://acme.feishu.cn/docx/D1")

	if deg.BitablesRendered != 1 || deg.Total() != 1 {
		t.Fatalf("degradation = %+v, want 1 rendered", deg)
	}
	// Original tag preserved in a comment.
	if !strings.Contains(body, "<!-- opendoc:base_refer table-id=\"tbl1\" token=\"app1\" view-id=\"vew1\" -->") {
		t.Fatalf("base_refer tag not preserved in comment:\n%s", body)
	}
	// Rendered markdown table with headers and a cell value.
	if !strings.Contains(body, "| 文本 | 文本 2 | 单选 |") || !strings.Contains(body, "23232 232") {
		t.Fatalf("bitable table not rendered:\n%s", body)
	}
	// Online link present.
	if !strings.Contains(body, "https://acme.feishu.cn/base/app1?") {
		t.Fatalf("bitable online link missing:\n%s", body)
	}
}

func TestDegradeBaseReferOversize(t *testing.T) {
	// total=3 but threshold=1 -> schema-only degrade.
	f := bitableFakeRunner(smallFields, smallRecords)
	a := NewAdapter(config.Feishu{}, f, 1)
	md := "<base_refer table-id=\"tbl1\" token=\"app1\" view-id=\"vew1\"></base_refer>"
	body, deg := a.degrade(context.Background(), md, md, "https://acme.feishu.cn/docx/D1")
	if deg.BitablesOversize != 1 {
		t.Fatalf("degradation = %+v, want 1 oversize", deg)
	}
	if strings.Contains(body, "| 文本 |") {
		t.Fatalf("oversize bitable should not render full table:\n%s", body)
	}
	if !strings.Contains(body, "3 rows") || !strings.Contains(body, "单选 (SingleSelect)") {
		t.Fatalf("schema summary missing row count/columns:\n%s", body)
	}
}

func TestDegradeBitableFetchFailureDegrades(t *testing.T) {
	// Runner returns no matching response -> fields fetch errors.
	f := &fakeRunner{}
	a := NewAdapter(config.Feishu{}, f, 200)
	md := "<base_refer table-id=\"tbl1\" token=\"app1\" view-id=\"vew1\"></base_refer>"
	body, deg := a.degrade(context.Background(), md, md, "https://acme.feishu.cn/docx/D1")
	if deg.BitablesFailed != 1 {
		t.Fatalf("degradation = %+v, want 1 failed", deg)
	}
	// Tag preserved + online link, no crash, document not failed.
	if !strings.Contains(body, "<!-- opendoc:base_refer") || !strings.Contains(body, "https://acme.feishu.cn/base/app1") {
		t.Fatalf("failed bitable did not degrade to comment+link:\n%s", body)
	}
}

func TestRenderBitableTableEscaping(t *testing.T) {
	bd := bitableData{
		fields:  []bitableField{{FieldName: "a|b"}, {FieldName: "c"}},
		records: []bitableRecord{{Fields: mustRawMap(`{"a|b":"x|y","c":"line1\nline2"}`)}},
		total:   1,
	}
	out := renderBitableTable(bd)
	if !strings.Contains(out, `a\|b`) || !strings.Contains(out, `x\|y`) {
		t.Fatalf("pipe not escaped:\n%s", out)
	}
	if strings.Contains(out, "line1\nline2") {
		t.Fatalf("newline not flattened in cell:\n%s", out)
	}
}
