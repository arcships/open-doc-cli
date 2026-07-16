package engine

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arcships/open-doc-cli/internal/layout"
	"github.com/arcships/open-doc-cli/internal/manifest"
)

func indexDocs() []manifest.Document {
	return []manifest.Document{
		{ID: "root", Platform: "feishu", Type: "folder", Title: "wiki-空间", LocalPath: "feishu/wiki-空间"},
		{ID: "objA", Platform: "feishu", Type: "docx", Title: "首页", LocalPath: "feishu/wiki-空间/首页.md", RemoteEdited: "2026-07-14T09:00:00Z"},
		{ID: "objB", Platform: "feishu", Type: "docx", Title: "手册", LocalPath: "feishu/wiki-空间/手册/README.md", RemoteEdited: "2026-07-14T10:00:00Z"},
		{ID: "objC", Platform: "feishu", Type: "docx", Title: "权限", LocalPath: "feishu/wiki-空间/手册/权限.md", RemoteEdited: "2026-07-14T11:00:00Z"},
	}
}

func TestRenderIndexDeterministicAndComplete(t *testing.T) {
	ts := time.Date(2026, 7, 14, 21, 30, 12, 0, time.UTC)

	out := renderIndex(indexDocs(), ts)

	// Header: what-this-is, red line, last sync, per-platform counts.
	for _, want := range []string{
		"# opendoc Knowledge Base Index",
		"Read-only mirror",
		"Last synced: 2026-07-14T21:30:12Z",
		"Total documents: 3", // 3 body docs (folder excluded)
		"feishu: 3",
		"## Contents",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("INDEX missing %q\n---\n%s", want, out)
		}
	}

	// Every body doc is listed as a link with its online updated time.
	for _, want := range []string{
		"[首页](feishu/wiki-空间/首页.md) · 2026-07-14T09:00:00Z",
		"[手册](feishu/wiki-空间/手册/README.md) · 2026-07-14T10:00:00Z",
		"[权限](feishu/wiki-空间/手册/权限.md) · 2026-07-14T11:00:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("INDEX missing entry %q\n---\n%s", want, out)
		}
	}
	// The folder root is a structural line (bold, no link).
	if !strings.Contains(out, "- **wiki-空间**") {
		t.Errorf("INDEX missing folder line\n---\n%s", out)
	}

	// Tree order and indentation: 手册 (dir/README) before its child 权限, which
	// is indented one level deeper.
	iBook := strings.Index(out, "[手册]")
	iPerm := strings.Index(out, "[权限]")
	if iBook < 0 || iPerm < 0 || iBook > iPerm {
		t.Fatalf("expected 手册 before 权限 in tree order")
	}
	if !strings.Contains(out, "  - [权限]") { // deeper indent than 手册
		t.Errorf("child 权限 not indented under 手册\n---\n%s", out)
	}

	// Determinism: reordering the input rows yields byte-identical output.
	shuffled := indexDocs()
	shuffled[0], shuffled[3] = shuffled[3], shuffled[0]
	shuffled[1], shuffled[2] = shuffled[2], shuffled[1]
	if out2 := renderIndex(shuffled, ts); out2 != out {
		t.Fatalf("renderIndex not deterministic under row reordering")
	}
}

func TestGenerateIndexAtRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)
	runOnce(t, l, newFakeAdapter())

	idx := readFile(t, root, "INDEX.md")
	if !strings.Contains(idx, "# opendoc Knowledge Base Index") || !strings.Contains(idx, "## Contents") {
		t.Fatalf("INDEX.md not generated at root:\n%s", idx)
	}
	// The fake tree's docs appear.
	if !strings.Contains(idx, "欢迎") || !strings.Contains(idx, "手册") {
		t.Fatalf("INDEX.md missing tree docs:\n%s", idx)
	}
}
