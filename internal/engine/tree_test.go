package engine

import (
	"testing"

	"github.com/arcships/open-doc-cli/internal/adapter"
)

func TestBuildTreeLeafAndDirPlacement(t *testing.T) {
	docs := []adapter.RemoteDoc{
		{ID: "root", Type: adapter.TypeFolder, ParentID: "", Title: "wiki-空间"},
		{ID: "leaf", Type: adapter.TypeDocx, ParentID: "root", Title: "欢迎"},
		{ID: "dir", Type: adapter.TypeDocx, ParentID: "root", Title: "手册"},
		{ID: "child", Type: adapter.TypeDocx, ParentID: "dir", Title: "权限"},
		{ID: "sheet", Type: adapter.TypeSheet, ParentID: "dir", Title: "排期"},
	}
	roots := buildTree("feishu", docs)
	nodes := map[string]*treeNode{}
	for _, n := range walk(roots) {
		nodes[n.doc.ID] = n
	}

	// A node with children becomes a directory + README.md.
	if got := nodes["dir"].bodyPath; got != "feishu/wiki-空间/手册/README.md" {
		t.Errorf("dir bodyPath = %q", got)
	}
	if !nodes["dir"].isDir {
		t.Error("dir should be isDir")
	}
	// A childless content node is a single <title>.md leaf.
	if got := nodes["leaf"].bodyPath; got != "feishu/wiki-空间/欢迎.md" {
		t.Errorf("leaf bodyPath = %q", got)
	}
	if nodes["leaf"].isDir {
		t.Error("leaf should not be isDir")
	}
	// Nested child sits under its parent's directory.
	if got := nodes["child"].bodyPath; got != "feishu/wiki-空间/手册/权限.md" {
		t.Errorf("child bodyPath = %q", got)
	}
	// A placeholder resource leaf still gets a body file.
	if got := nodes["sheet"].bodyPath; got != "feishu/wiki-空间/手册/排期.md" {
		t.Errorf("sheet bodyPath = %q", got)
	}
	// Synthetic folder root is a directory with no body.
	if nodes["root"].hasBody {
		t.Error("folder root should have no body")
	}
	if got := nodes["root"].dirPath; got != "feishu/wiki-空间" {
		t.Errorf("root dirPath = %q", got)
	}
}

func TestBuildTreeBreadcrumb(t *testing.T) {
	docs := []adapter.RemoteDoc{
		{ID: "root", Type: adapter.TypeFolder, Title: "wiki-空间"},
		{ID: "dir", Type: adapter.TypeDocx, ParentID: "root", Title: "手册"},
		{ID: "child", Type: adapter.TypeDocx, ParentID: "dir", Title: "权限"},
	}
	roots := buildTree("feishu", docs)
	nodes := map[string]*treeNode{}
	for _, n := range walk(roots) {
		nodes[n.doc.ID] = n
	}
	if got := nodes["child"].breadcrumb; got != "wiki-空间 / 手册" {
		t.Errorf("child breadcrumb = %q, want 'wiki-空间 / 手册'", got)
	}
	if got := nodes["dir"].breadcrumb; got != "wiki-空间" {
		t.Errorf("dir breadcrumb = %q, want 'wiki-空间'", got)
	}
}

func TestBuildTreeDuplicateSiblingTitles(t *testing.T) {
	docs := []adapter.RemoteDoc{
		{ID: "root", Type: adapter.TypeFolder, Title: "wiki-空间"},
		{ID: "aaaa1111xxxx", Type: adapter.TypeDocx, ParentID: "root", Title: "设计"},
		{ID: "bbbb2222yyyy", Type: adapter.TypeDocx, ParentID: "root", Title: "设计"},
	}
	roots := buildTree("feishu", docs)
	nodes := map[string]*treeNode{}
	for _, n := range walk(roots) {
		nodes[n.doc.ID] = n
	}
	if got := nodes["aaaa1111xxxx"].bodyPath; got != "feishu/wiki-空间/设计.md" {
		t.Errorf("first duplicate = %q, want clean name", got)
	}
	if got := nodes["bbbb2222yyyy"].bodyPath; got != "feishu/wiki-空间/设计-bbbb2222.md" {
		t.Errorf("second duplicate = %q, want ID-suffixed name", got)
	}
}

func TestBuildTreeOrphanRoutedToOrphansFolder(t *testing.T) {
	// A node whose parent is non-empty but absent from the set is an orphan: it is
	// collected under a synthetic platform-level "_orphans" folder rather than
	// dropped or scattered at the platform root.
	docs := []adapter.RemoteDoc{
		{ID: "lonely", Type: adapter.TypeDocx, ParentID: "missing", Title: "孤儿"},
	}
	roots := buildTree("feishu", docs)
	if len(roots) != 1 {
		t.Fatalf("want a single _orphans root, got %d: %+v", len(roots), roots)
	}
	orphanRoot := roots[0]
	if orphanRoot.doc.ID != "feishu:_orphans" || orphanRoot.dirPath != "feishu/_orphans" {
		t.Fatalf("orphan root wrong: id=%q dirPath=%q", orphanRoot.doc.ID, orphanRoot.dirPath)
	}
	if len(orphanRoot.children) != 1 || orphanRoot.children[0].doc.ID != "lonely" {
		t.Fatalf("orphan not under _orphans: %+v", orphanRoot.children)
	}
	if got := orphanRoot.children[0].bodyPath; got != "feishu/_orphans/孤儿.md" {
		t.Errorf("orphan bodyPath = %q, want feishu/_orphans/孤儿.md", got)
	}
}

func TestBuildTreeEmptyParentStaysRoot(t *testing.T) {
	// A node with an empty ParentID is a genuine root and must NOT be treated as
	// an orphan.
	docs := []adapter.RemoteDoc{
		{ID: "top", Type: adapter.TypeDocx, ParentID: "", Title: "顶层"},
	}
	roots := buildTree("notion", docs)
	if len(roots) != 1 || roots[0].doc.ID != "top" || roots[0].bodyPath != "notion/顶层.md" {
		t.Fatalf("root placement wrong: %+v", roots)
	}
}
