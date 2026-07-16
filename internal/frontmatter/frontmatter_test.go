package frontmatter

import (
	"strings"
	"testing"
	"time"
)

func TestRenderContainsAllFields(t *testing.T) {
	d := Doc{
		ID:         "Fj5VdNtP",
		Source:     "feishu",
		Type:       "docx",
		URL:        "https://x.feishu.cn/docx/Fj5VdNtP",
		Title:      "P0 复杂块降级测试",
		Breadcrumb: "drive-22323",
		Updated:    time.Date(2026, 7, 14, 9, 17, 21, 0, time.UTC),
		Synced:     time.Date(2026, 7, 15, 2, 43, 52, 0, time.UTC),
	}
	got := Render(d)

	if !strings.HasPrefix(got, "---\n") || !strings.HasSuffix(got, "---\n") {
		t.Fatalf("frontmatter must be fenced by ---:\n%s", got)
	}
	for _, want := range []string{
		"# Read-only mirror",
		`id: "Fj5VdNtP"`,
		`source: "feishu"`,
		`type: "docx"`,
		`url: "https://x.feishu.cn/docx/Fj5VdNtP"`,
		`title: "P0 复杂块降级测试"`,
		`breadcrumb: "drive-22323"`,
		"updated: 2026-07-14T09:17:21Z",
		"synced: 2026-07-15T02:43:52Z",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("frontmatter missing %q:\n%s", want, got)
		}
	}
}

func TestRenderZeroUpdatedIsNull(t *testing.T) {
	got := Render(Doc{ID: "x", Source: "feishu", Type: "docx", Synced: time.Now()})
	if !strings.Contains(got, "updated: null") {
		t.Fatalf("zero Updated should render null:\n%s", got)
	}
}

func TestRenderEscapesSpecialChars(t *testing.T) {
	got := Render(Doc{
		ID:    "x",
		Title: `A "quoted": title \ with: colons`,
	})
	if !strings.Contains(got, `title: "A \"quoted\": title \\ with: colons"`) {
		t.Fatalf("special chars not escaped:\n%s", got)
	}
}

func TestRenderNoPropertiesForFeishu(t *testing.T) {
	got := Render(Doc{ID: "x", Source: "feishu", Type: "docx"})
	if strings.Contains(got, "properties:") {
		t.Fatalf("feishu frontmatter must not contain properties:\n%s", got)
	}
}

func TestRenderProperties(t *testing.T) {
	d := Doc{
		ID:     "row1",
		Source: "notion",
		Type:   "db_row",
		Title:  "Advanced Topics",
		Properties: []Property{
			{Key: "状态", Value: "已完成"},
			{Key: "学期", Value: "Term 1"},
			{Key: "标签", Value: "[a, b]"},
			// A key that needs quoting (contains a colon).
			{Key: "due:date", Value: "2024-01-01"},
		},
	}
	got := Render(d)
	for _, want := range []string{
		"properties:\n",
		"  状态: \"已完成\"",
		"  学期: \"Term 1\"",
		"  标签: \"[a, b]\"",
		"  \"due:date\": \"2024-01-01\"",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("properties block missing %q:\n%s", want, got)
		}
	}
}

func TestRenderNoPropertiesBlockWhenEmpty(t *testing.T) {
	got := Render(Doc{ID: "p", Source: "notion", Type: "page"})
	if strings.Contains(got, "properties:") {
		t.Errorf("no properties block expected when empty:\n%s", got)
	}
}
