package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/layout"
	"github.com/arcships/open-doc-cli/internal/manifest"
)

// fakeAdapter is an in-memory Adapter for exercising the pipeline without a
// platform. It records how many times each document body was fetched.
type fakeAdapter struct {
	docs      []adapter.RemoteDoc
	bodies    map[string]adapter.FetchResult
	fetchHits map[string]int
}

func (f *fakeAdapter) Platform() string { return "feishu" }

func (f *fakeAdapter) Enumerate(ctx context.Context) (<-chan adapter.RemoteDoc, <-chan error) {
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

func (f *fakeAdapter) FetchMarkdown(ctx context.Context, doc adapter.RemoteDoc) (adapter.FetchResult, error) {
	if f.fetchHits == nil {
		f.fetchHits = map[string]int{}
	}
	f.fetchHits[doc.ID]++
	return f.bodies[doc.ID], nil
}

func (f *fakeAdapter) DownloadAsset(ctx context.Context, ref adapter.AssetRef, dest string) error {
	// Write deterministic bytes so the engine can content-address the asset.
	return os.WriteFile(dest, []byte("asset:"+ref.RemoteKey), 0o644)
}

func newFakeAdapter() *fakeAdapter {
	return &fakeAdapter{
		docs: []adapter.RemoteDoc{
			{ID: "root", Type: adapter.TypeFolder, Title: "wiki-空间"},
			{ID: "docA", Type: adapter.TypeDocx, ParentID: "root", Title: "欢迎", URL: "https://t/docx/docA", EditedAt: "2026-07-14T09:17:21Z"},
			{ID: "dirB", Type: adapter.TypeDocx, ParentID: "root", Title: "手册", URL: "https://t/docx/dirB", EditedAt: "2026-07-14T10:00:00Z"},
			{ID: "childC", Type: adapter.TypeDocx, ParentID: "dirB", Title: "权限", URL: "https://t/docx/childC", EditedAt: "2026-07-14T11:00:00Z"},
			{ID: "sheetD", Type: adapter.TypeSheet, ParentID: "dirB", Title: "排期", URL: "https://t/sheets/sheetD", EditedAt: "2026-07-14T12:00:00Z"},
		},
		bodies: map[string]adapter.FetchResult{
			"docA":   {Markdown: "# 欢迎\n正文A", Assets: []adapter.AssetRef{{RemoteKey: "tok1", URL: "u"}}, Links: []adapter.DocRef{{TargetID: "dirB", RawURL: "https://t/docx/dirB"}}},
			"dirB":   {Markdown: "# 手册\n正文B"},
			"childC": {Markdown: "# 权限\n正文C"},
		},
	}
}

func runOnce(t *testing.T, l layout.Layout, a adapter.Adapter) Result {
	t.Helper()
	eng, err := New(l, Options{Adapters: []adapter.Adapter{a}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Close()
	res, err := eng.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return res
}

func TestPipelineWritesTreeAndManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)
	fa := newFakeAdapter()

	res := runOnce(t, l, fa)
	if res.Stats.Added != 4 { // docA, dirB, childC, sheetD (placeholder)
		t.Fatalf("Added = %d, want 4; stats=%+v", res.Stats.Added, res.Stats)
	}

	// Files exist at the expected paths.
	mustFile(t, root, "feishu/wiki-空间/欢迎.md")
	mustFile(t, root, "feishu/wiki-空间/手册/README.md")
	mustFile(t, root, "feishu/wiki-空间/手册/权限.md")
	mustFile(t, root, "feishu/wiki-空间/手册/排期.md")

	// docA has frontmatter + body, red line, and correct type.
	body := readFile(t, root, "feishu/wiki-空间/欢迎.md")
	for _, want := range []string{"# Read-only mirror", `id: "docA"`, `source: "feishu"`, `type: "docx"`, "正文A"} {
		if !strings.Contains(body, want) {
			t.Errorf("docA missing %q", want)
		}
	}

	// Placeholder sheet body.
	sheet := readFile(t, root, "feishu/wiki-空间/手册/排期.md")
	if !strings.Contains(sheet, "Standalone resource node") || !strings.Contains(sheet, "sheetD") {
		t.Errorf("placeholder body wrong:\n%s", sheet)
	}

	// Manifest: documents, one asset, one link recorded.
	db, err := manifest.Open(l.ManifestPath())
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer db.Close()
	n, _ := db.CountDocuments()
	if n != 5 { // 4 bodies + 1 folder root
		t.Errorf("documents count = %d, want 5", n)
	}
	if doc, found, _ := db.GetDocument("docA"); !found || doc.ContentHash == "" || doc.LocalPath != "feishu/wiki-空间/欢迎.md" {
		t.Errorf("docA manifest row wrong: %+v found=%v", doc, found)
	}
}

func TestPipelineSkipsUnchangedOnRerun(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)
	fa := newFakeAdapter()

	runOnce(t, l, fa)
	baseHits := map[string]int{}
	for k, v := range fa.fetchHits {
		baseHits[k] = v
	}

	// Second run: remote_edited unchanged -> pre-fetch skip, no new fetches.
	res := runOnce(t, l, fa)
	if res.Stats.Skipped == 0 || res.Stats.Added != 0 || res.Stats.Updated != 0 {
		t.Fatalf("rerun should skip all, got %+v", res.Stats)
	}
	for id, before := range baseHits {
		if fa.fetchHits[id] != before {
			t.Errorf("doc %s was re-fetched on rerun (%d -> %d); pre-fetch skip failed", id, before, fa.fetchHits[id])
		}
	}
}

func TestPipelineContentHashSkipWhenEditTimeUnknown(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)
	fa := newFakeAdapter()
	// No EditedAt anywhere: pre-fetch skip can never fire, so the content_hash
	// skip must carry resume instead.
	for i := range fa.docs {
		fa.docs[i].EditedAt = ""
	}

	runOnce(t, l, fa)
	res := runOnce(t, l, fa)
	// Bodies get re-fetched (no edit-time short-circuit) but not rewritten.
	if res.Stats.Added != 0 || res.Stats.Updated != 0 {
		t.Fatalf("content-hash skip failed, got %+v", res.Stats)
	}
	if res.Stats.Skipped == 0 {
		t.Fatalf("expected content-hash skips, got %+v", res.Stats)
	}
}

// flakyAdapter serves one docx with one inline image and lets the test toggle
// download failure, to exercise the pending-asset degrade + retry path.
type flakyAdapter struct {
	fail      bool
	downloads int
	bodyURL   string
}

func (f *flakyAdapter) Platform() string { return "feishu" }

func (f *flakyAdapter) Enumerate(ctx context.Context) (<-chan adapter.RemoteDoc, <-chan error) {
	out := make(chan adapter.RemoteDoc)
	errc := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errc)
		out <- adapter.RemoteDoc{ID: "root", Type: adapter.TypeFolder, Title: "wiki-空间"}
		out <- adapter.RemoteDoc{ID: "docP", Type: adapter.TypeDocx, ParentID: "root", Title: "图文", URL: "https://t/docx/docP", EditedAt: "2026-07-14T09:00:00Z"}
	}()
	return out, errc
}

func (f *flakyAdapter) FetchMarkdown(ctx context.Context, doc adapter.RemoteDoc) (adapter.FetchResult, error) {
	body := "# 图文\n\n![](" + f.bodyURL + ")\n"
	return adapter.FetchResult{
		Markdown: body,
		Body:     body,
		Assets:   []adapter.AssetRef{{RemoteKey: "tokP", BodyURL: f.bodyURL}},
	}, nil
}

func (f *flakyAdapter) DownloadAsset(ctx context.Context, ref adapter.AssetRef, dest string) error {
	if f.fail {
		return errors.New("simulated download failure")
	}
	f.downloads++
	return os.WriteFile(dest, []byte("PNGDATA:"+ref.RemoteKey), 0o644)
}

func TestPendingAssetDegradeThenRetry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)
	fa := &flakyAdapter{fail: true, bodyURL: "https://x.feishu.cn/stream/authcode/?code=ABC"}

	// Run 1: download fails -> body keeps URL + pending marker, asset pending,
	// doc marked pending_assets.
	res := runOnce(t, l, fa)
	if res.Stats.AssetsPending != 1 || res.Stats.AssetsDownloaded != 0 {
		t.Fatalf("run1 asset stats = %+v", res.Stats)
	}
	body := readFile(t, root, "feishu/wiki-空间/图文.md")
	if !strings.Contains(body, "![]("+fa.bodyURL+")<!-- opendoc:asset-pending -->") {
		t.Fatalf("run1 body missing pending marker:\n%s", body)
	}
	db, err := manifest.Open(l.ManifestPath())
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	if a, found, _ := db.GetAsset("tokP"); !found || a.Status != "pending" {
		t.Fatalf("run1 asset row = %+v found=%v, want pending", a, found)
	}
	if doc, _, _ := db.GetDocument("docP"); doc.Status != "pending_assets" {
		t.Fatalf("run1 doc status = %q, want pending_assets", doc.Status)
	}
	db.Close()

	// Run 2: download succeeds this time. Even though remote_edited is unchanged,
	// the pending status forces reprocessing; the link is fixed up.
	fa.fail = false
	res = runOnce(t, l, fa)
	if res.Stats.AssetsDownloaded != 1 || res.Stats.AssetsPending != 0 {
		t.Fatalf("run2 asset stats = %+v", res.Stats)
	}
	body = readFile(t, root, "feishu/wiki-空间/图文.md")
	if strings.Contains(body, "opendoc:asset-pending") {
		t.Fatalf("run2 body still marked pending:\n%s", body)
	}
	if !strings.Contains(body, "![](../../assets/") {
		t.Fatalf("run2 body image not rewritten to local path:\n%s", body)
	}
	db, err = manifest.Open(l.ManifestPath())
	if err != nil {
		t.Fatalf("reopen manifest: %v", err)
	}
	defer db.Close()
	if a, _, _ := db.GetAsset("tokP"); a.Status != "done" || a.LocalPath == "" {
		t.Fatalf("run2 asset row = %+v, want done with local_path", a)
	}
	if doc, _, _ := db.GetDocument("docP"); doc.Status != "active" {
		t.Fatalf("run2 doc status = %q, want active", doc.Status)
	}

	// Run 3: nothing pending, unchanged -> pre-fetch skip, no re-download.
	before := fa.downloads
	res = runOnce(t, l, fa)
	if res.Stats.Skipped == 0 || res.Stats.AssetsDownloaded != 0 || fa.downloads != before {
		t.Fatalf("run3 should skip without re-download, stats=%+v downloads=%d->%d", res.Stats, before, fa.downloads)
	}
}

func mustFile(t *testing.T, root, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
		t.Errorf("expected file %s: %v", rel, err)
	}
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}
