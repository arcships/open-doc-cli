package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/layout"
)

// dbFakeAdapter is a Notion-shaped fake: it enumerates a page holding a database
// (TypeDB) with two rows — one leaf, one with a child page — and implements
// adapter.DatabaseExpander so the engine can query row properties. It records the
// query call count so the "one query per database" contract can be asserted.
type dbFakeAdapter struct {
	docs     []adapter.RemoteDoc
	bodies   map[string]adapter.FetchResult
	rows     map[string]adapter.RowProperties
	queryHit int
}

func (f *dbFakeAdapter) Platform() string { return "notion" }

func (f *dbFakeAdapter) Enumerate(ctx context.Context) (<-chan adapter.RemoteDoc, <-chan error) {
	out := make(chan adapter.RemoteDoc)
	errc := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errc)
		for _, d := range f.docs {
			out <- d
		}
	}()
	return out, errc
}

func (f *dbFakeAdapter) FetchMarkdown(ctx context.Context, doc adapter.RemoteDoc) (adapter.FetchResult, error) {
	return f.bodies[doc.ID], nil
}

func (f *dbFakeAdapter) DownloadAsset(ctx context.Context, ref adapter.AssetRef, dest string) error {
	return os.WriteFile(dest, []byte("asset"), 0o644)
}

func (f *dbFakeAdapter) QueryDatabaseRows(ctx context.Context, dbDoc adapter.RemoteDoc, titles map[string]string) (map[string]adapter.RowProperties, error) {
	f.queryHit++
	return f.rows, nil
}

func newDBFakeAdapter() *dbFakeAdapter {
	return &dbFakeAdapter{
		docs: []adapter.RemoteDoc{
			{ID: "home", Type: adapter.TypePage, Title: "Home", URL: "https://n/p/home", EditedAt: "2026-07-14T09:00:00Z"},
			{ID: "ds", Type: adapter.TypeDB, ParentID: "home", Title: "Subjects", URL: "https://n/p/ds", EditedAt: "2026-07-14T09:00:00Z"},
			// zrow sorts after arow by title, but is enumerated first — the row index
			// must still order deterministically by title, not enumeration order.
			{ID: "zrow", Type: adapter.TypeDBRow, ParentID: "ds", Title: "Zoology", URL: "https://n/p/zrow", EditedAt: "2026-07-14T09:00:00Z"},
			{ID: "arow", Type: adapter.TypeDBRow, ParentID: "ds", Title: "Algorithms", URL: "https://n/p/arow", EditedAt: "2026-07-14T09:00:00Z"},
			// arow has a child page → arow becomes a directory + README.
			{ID: "note", Type: adapter.TypePage, ParentID: "arow", Title: "Week 1", URL: "https://n/p/note", EditedAt: "2026-07-14T09:00:00Z"},
		},
		bodies: map[string]adapter.FetchResult{
			"home": {Markdown: "Home body"},
			"zrow": {Markdown: ""}, // empty-body leaf row
			"arow": {Markdown: "Algorithms body"},
			"note": {Markdown: "Week 1 body"},
		},
		rows: map[string]adapter.RowProperties{
			"arow": {
				Entries:   []adapter.PropertyKV{{Key: "状态", Value: "已完成"}, {Key: "学期", Value: "Term 1"}},
				Canonical: "6:状态=已完成\n6:学期=Term 1\n",
			},
			"zrow": {
				Entries:   []adapter.PropertyKV{{Key: "状态", Value: "进行中"}, {Key: "学期", Value: "2024 S2"}},
				Canonical: "6:状态=进行中\n6:学期=2024 S2\n",
			},
		},
	}
}

// TestDatabaseExpansionLayout asserts the database-expansion layout: the database becomes a
// directory with a generated `_index.md` (not a README), a body-less row is a
// leaf .md carrying its properties, a row with a child becomes a directory +
// README, and the row index links to each row's actual local file. It also
// checks the row index is byte-stable on a rerun and that the database is queried
// exactly once per sync.
func TestDatabaseExpansionLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)
	fa := newDBFakeAdapter()

	eng, err := New(l, Options{Adapters: []adapter.Adapter{fa}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Close()
	if _, err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if fa.queryHit != 1 {
		t.Errorf("database queried %d times, want 1 (one query per db per sync)", fa.queryHit)
	}

	// Layout: db directory + _index.md, leaf row, dir row + README.
	mustFile(t, root, "notion/Home/Subjects/_index.md")
	mustFile(t, root, "notion/Home/Subjects/Zoology.md")           // empty-body leaf row
	mustFile(t, root, "notion/Home/Subjects/Algorithms/README.md") // row with child
	mustFile(t, root, "notion/Home/Subjects/Algorithms/Week 1.md") // the child page
	if _, err := os.Stat(filepath.Join(root, "notion/Home/Subjects/README.md")); err == nil {
		t.Errorf("database directory must not have a README placeholder")
	}

	// The empty-body leaf row is still a valid file carrying its properties.
	zrow := readFile(t, root, "notion/Home/Subjects/Zoology.md")
	for _, want := range []string{`type: "db_row"`, "properties:", "状态: \"进行中\"", "学期: \"2024 S2\""} {
		if !strings.Contains(zrow, want) {
			t.Errorf("Zoology row missing %q:\n%s", want, zrow)
		}
	}

	// The row index links to the rows' real local files (leaf .md and dir/README).
	index := readFile(t, root, "notion/Home/Subjects/_index.md")
	if !strings.Contains(index, "Zoology.md") {
		t.Errorf("index missing leaf-row link:\n%s", index)
	}
	if !strings.Contains(index, "Algorithms/README.md") {
		t.Errorf("index missing dir-row link:\n%s", index)
	}
	// Deterministic order: Algorithms (a) before Zoology (z) despite reverse
	// enumeration order.
	if strings.Index(index, "Algorithms") > strings.Index(index, "Zoology") {
		t.Errorf("row index not title-ordered:\n%s", index)
	}

	// Rerun → _index.md byte-stable.
	if _, err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	index2 := readFile(t, root, "notion/Home/Subjects/_index.md")
	if index != index2 {
		t.Errorf("_index.md not byte-stable across reruns:\n--- run1 ---\n%s\n--- run2 ---\n%s", index, index2)
	}
}
