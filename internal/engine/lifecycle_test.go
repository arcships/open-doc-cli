package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/layout"
	"github.com/arcships/open-doc-cli/internal/manifest"
)

// mutAdapter is a per-run adapter whose enumerated docs and bodies are supplied
// by the test, counting how many times each body is fetched so incremental and
// --full behaviour can be asserted.
type mutAdapter struct {
	docs    []adapter.RemoteDoc
	bodies  map[string]adapter.FetchResult
	fetches map[string]int
}

func (m *mutAdapter) Platform() string { return "feishu" }

func (m *mutAdapter) Enumerate(ctx context.Context) (<-chan adapter.RemoteDoc, <-chan error) {
	out := make(chan adapter.RemoteDoc)
	errc := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errc)
		for _, d := range m.docs {
			out <- d
		}
	}()
	return out, errc
}

func (m *mutAdapter) FetchMarkdown(ctx context.Context, doc adapter.RemoteDoc) (adapter.FetchResult, error) {
	if m.fetches == nil {
		m.fetches = map[string]int{}
	}
	m.fetches[doc.ID]++
	return m.bodies[doc.ID], nil
}

func (m *mutAdapter) DownloadAsset(ctx context.Context, ref adapter.AssetRef, dest string) error {
	return os.WriteFile(dest, []byte("asset:"+ref.RemoteKey), 0o644)
}

// runWith runs one sync against a, optionally forcing --full, returning the
// Result.
func runWith(t *testing.T, l layout.Layout, a adapter.Adapter, full bool) Result {
	t.Helper()
	eng, err := New(l, Options{Adapters: []adapter.Adapter{a}, Full: full})
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

func openManifest(t *testing.T, l layout.Layout) *manifest.DB {
	t.Helper()
	db, err := manifest.Open(l.ManifestPath())
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	return db
}

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

// ---- rename / move ------------------------------------------------------

func TestRenameFollowsLocalFileAndManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)

	run1 := &mutAdapter{
		docs: []adapter.RemoteDoc{
			{ID: "root", Type: adapter.TypeFolder, Title: "wiki-空间"},
			{ID: "docA", Type: adapter.TypeDocx, ParentID: "root", Title: "欢迎", URL: "https://x.feishu.cn/docx/docA", EditedAt: "2026-07-14T09:00:00Z"},
		},
		bodies: map[string]adapter.FetchResult{"docA": {Markdown: "# 欢迎\n正文\n"}},
	}
	runWith(t, l, run1, false)
	if !exists(root, "feishu/wiki-空间/欢迎.md") {
		t.Fatalf("run1 file missing")
	}

	// Rename online: same id + edit time, new title. The local file must follow
	// with no refetch, and the old path must be gone.
	run2 := &mutAdapter{
		docs: []adapter.RemoteDoc{
			{ID: "root", Type: adapter.TypeFolder, Title: "wiki-空间"},
			{ID: "docA", Type: adapter.TypeDocx, ParentID: "root", Title: "指南", URL: "https://x.feishu.cn/docx/docA", EditedAt: "2026-07-14T09:00:00Z"},
		},
		bodies: map[string]adapter.FetchResult{"docA": {Markdown: "# 欢迎\n正文\n"}},
	}
	res := runWith(t, l, run2, false)
	if res.Stats.Moved != 1 {
		t.Fatalf("Moved = %d, want 1; stats=%+v", res.Stats.Moved, res.Stats)
	}
	if run2.fetches["docA"] != 0 {
		t.Fatalf("renamed-unchanged doc was refetched %d times, want 0", run2.fetches["docA"])
	}
	if exists(root, "feishu/wiki-空间/欢迎.md") {
		t.Fatalf("old path should be gone after rename")
	}
	if !exists(root, "feishu/wiki-空间/指南.md") {
		t.Fatalf("new path missing after rename")
	}

	db := openManifest(t, l)
	defer db.Close()
	if doc, found, _ := db.GetDocument("docA"); !found || doc.LocalPath != "feishu/wiki-空间/指南.md" {
		t.Fatalf("manifest local_path not updated: %+v", doc)
	}
	// INDEX reflects the new title/path.
	idx := readFile(t, root, "INDEX.md")
	if !strings.Contains(idx, "指南") || strings.Contains(idx, "欢迎") {
		t.Fatalf("INDEX not updated after rename:\n%s", idx)
	}
}

func TestMoveReparentRewritesReferrerLink(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)

	urlB := "https://x.feishu.cn/docx/objB"
	bodyA := "# 首页\n\n见 [手册](" + urlB + ")\n"
	// A links to B (both leaves under the space root); a folder "归档" exists as a
	// reparent destination.
	base := func() *mutAdapter {
		return &mutAdapter{
			docs: []adapter.RemoteDoc{
				{ID: "root", Type: adapter.TypeFolder, Title: "wiki-空间"},
				{ID: "arch", Type: adapter.TypeFolder, ParentID: "root", Title: "归档"},
				{ID: "objA", Type: adapter.TypeDocx, ParentID: "root", Title: "首页", URL: "https://x.feishu.cn/docx/objA", EditedAt: "2026-07-14T09:00:00Z"},
				{ID: "objB", Type: adapter.TypeDocx, ParentID: "root", Title: "手册", URL: urlB, EditedAt: "2026-07-14T10:00:00Z"},
			},
			bodies: map[string]adapter.FetchResult{
				"objA": {Markdown: bodyA, Body: bodyA, Links: []adapter.DocRef{{TargetID: "objB", RawURL: urlB}}},
				"objB": {Markdown: "# 手册\n正文B\n"},
			},
		}
	}

	res1 := runWith(t, l, base(), false)
	if res1.Stats.LinksRewritten != 1 {
		t.Fatalf("run1 links_rewritten = %d, want 1", res1.Stats.LinksRewritten)
	}
	bodyA1 := readFile(t, root, "feishu/wiki-空间/首页.md")
	if !strings.Contains(bodyA1, "[手册](手册.md)") {
		t.Fatalf("run1 A link not rewritten to sibling:\n%s", bodyA1)
	}

	// Reparent B under 归档 (same id + edit time). B's file must move and A's
	// already-relative link must be rewritten to the new location.
	run2 := base()
	for i := range run2.docs {
		if run2.docs[i].ID == "objB" {
			run2.docs[i].ParentID = "arch"
		}
	}
	res2 := runWith(t, l, run2, false)
	if res2.Stats.Moved != 1 {
		t.Fatalf("run2 Moved = %d, want 1; stats=%+v", res2.Stats.Moved, res2.Stats)
	}
	if !exists(root, "feishu/wiki-空间/归档/手册.md") || exists(root, "feishu/wiki-空间/手册.md") {
		t.Fatalf("B not moved into 归档")
	}
	bodyA2 := readFile(t, root, "feishu/wiki-空间/首页.md")
	if !strings.Contains(bodyA2, "[手册](归档/手册.md)") {
		t.Fatalf("referrer link not rewritten to new relative path:\n%s", bodyA2)
	}
	// The rewritten link resolves on disk.
	target := filepath.Join(root, "feishu/wiki-空间", "归档/手册.md")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("rewritten link target does not resolve on disk: %v", err)
	}
}

// ---- leaf <-> dir conversion -------------------------------------------

func TestLeafDirConversionBothDirectionsWithReferrer(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)

	urlP := "https://x.feishu.cn/docx/objP"
	bodyR := "# 引用\n\n见 [项目](" + urlP + ")\n"

	bodies := map[string]adapter.FetchResult{
		"objR": {Markdown: bodyR, Body: bodyR, Links: []adapter.DocRef{{TargetID: "objP", RawURL: urlP}}},
		"objP": {Markdown: "# 项目\n正文P\n"},
		"objC": {Markdown: "# 子\n正文C\n"},
	}
	docs := func(withChild bool) []adapter.RemoteDoc {
		ds := []adapter.RemoteDoc{
			{ID: "root", Type: adapter.TypeFolder, Title: "wiki-空间"},
			{ID: "objR", Type: adapter.TypeDocx, ParentID: "root", Title: "引用", URL: "https://x.feishu.cn/docx/objR", EditedAt: "2026-07-14T09:00:00Z"},
			{ID: "objP", Type: adapter.TypeDocx, ParentID: "root", Title: "项目", URL: urlP, EditedAt: "2026-07-14T10:00:00Z"},
		}
		// Filler docs keep the inventory large enough that removing the single
		// child is well under the 20% permission-jitter threshold.
		for i := 0; i < 6; i++ {
			id := "f" + string(rune('0'+i))
			ds = append(ds, adapter.RemoteDoc{ID: id, Type: adapter.TypeDocx, ParentID: "root", Title: "填充" + id, EditedAt: "2026-07-14T09:00:00Z"})
			bodies[id] = adapter.FetchResult{Markdown: "# 填充\n"}
		}
		if withChild {
			ds = append(ds, adapter.RemoteDoc{ID: "objC", Type: adapter.TypeDocx, ParentID: "objP", Title: "子", URL: "https://x.feishu.cn/docx/objC", EditedAt: "2026-07-14T11:00:00Z"})
		}
		return ds
	}

	// Run 1: P is a leaf. R's link -> 项目.md.
	runWith(t, l, &mutAdapter{docs: docs(false), bodies: bodies}, false)
	if !exists(root, "feishu/wiki-空间/项目.md") {
		t.Fatalf("run1 P should be a leaf file")
	}
	r1 := readFile(t, root, "feishu/wiki-空间/引用.md")
	if !strings.Contains(r1, "[项目](项目.md)") {
		t.Fatalf("run1 referrer link wrong:\n%s", r1)
	}

	// Run 2: P gains a child -> becomes dir + README; referrer link follows.
	res2 := runWith(t, l, &mutAdapter{docs: docs(true), bodies: bodies}, false)
	if res2.Stats.Moved != 1 {
		t.Fatalf("run2 Moved = %d, want 1 (leaf->dir); stats=%+v", res2.Stats.Moved, res2.Stats)
	}
	if exists(root, "feishu/wiki-空间/项目.md") || !exists(root, "feishu/wiki-空间/项目/README.md") {
		t.Fatalf("run2 P should be dir+README")
	}
	if !exists(root, "feishu/wiki-空间/项目/子.md") {
		t.Fatalf("run2 child missing")
	}
	r2 := readFile(t, root, "feishu/wiki-空间/引用.md")
	if !strings.Contains(r2, "[项目](项目/README.md)") {
		t.Fatalf("run2 referrer link not converted to README path:\n%s", r2)
	}

	// Run 3: child removed -> P collapses back to a leaf; referrer link follows.
	res3 := runWith(t, l, &mutAdapter{docs: docs(false), bodies: bodies}, false)
	if res3.Stats.Moved != 1 {
		t.Fatalf("run3 Moved = %d, want 1 (dir->leaf); stats=%+v", res3.Stats.Moved, res3.Stats)
	}
	if res3.Stats.Deleted != 1 {
		t.Fatalf("run3 Deleted = %d, want 1 (child trashed); stats=%+v", res3.Stats.Deleted, res3.Stats)
	}
	if !exists(root, "feishu/wiki-空间/项目.md") || exists(root, "feishu/wiki-空间/项目/README.md") {
		t.Fatalf("run3 P should collapse back to a leaf file")
	}
	// The vacated directory is swept.
	if exists(root, "feishu/wiki-空间/项目") {
		t.Fatalf("run3 empty 项目 directory should be pruned")
	}
	r3 := readFile(t, root, "feishu/wiki-空间/引用.md")
	if !strings.Contains(r3, "[项目](项目.md)") {
		t.Fatalf("run3 referrer link not converted back to leaf path:\n%s", r3)
	}
}

// ---- delete -> trash ----------------------------------------------------

func TestDeleteMovesToTrashAndDoesNotResurrect(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)

	full := []adapter.RemoteDoc{
		{ID: "root", Type: adapter.TypeFolder, Title: "wiki-空间"},
		{ID: "d1", Type: adapter.TypeDocx, ParentID: "root", Title: "甲", URL: "https://x.feishu.cn/docx/d1", EditedAt: "2026-07-14T09:00:00Z"},
		{ID: "d2", Type: adapter.TypeDocx, ParentID: "root", Title: "乙", URL: "https://x.feishu.cn/docx/d2", EditedAt: "2026-07-14T09:00:00Z"},
		{ID: "d3", Type: adapter.TypeDocx, ParentID: "root", Title: "丙", URL: "https://x.feishu.cn/docx/d3", EditedAt: "2026-07-14T09:00:00Z"},
	}
	bodies := map[string]adapter.FetchResult{
		"d1": {Markdown: "# 甲\n"}, "d2": {Markdown: "# 乙\n"}, "d3": {Markdown: "# 丙\n"},
	}
	runWith(t, l, &mutAdapter{docs: full, bodies: bodies}, false)

	// Drop d2 from the inventory (active=4, enum=3 -> 3 >= 0.8*4=3.2? no, 3<3.2).
	// Use active=4 with two survivors would trip the guard, so keep three docs by
	// removing only d3 instead (enum=3 of 4 -> 3 < 3.2 trips). Add a fourth doc so
	// the fraction stays safe: enum 4 of 5.
	full5 := append([]adapter.RemoteDoc{}, full...)
	full5 = append(full5, adapter.RemoteDoc{ID: "d4", Type: adapter.TypeDocx, ParentID: "root", Title: "丁", URL: "https://x.feishu.cn/docx/d4", EditedAt: "2026-07-14T09:00:00Z"})
	bodies["d4"] = adapter.FetchResult{Markdown: "# 丁\n"}
	runWith(t, l, &mutAdapter{docs: full5, bodies: bodies}, false) // active becomes 5

	// Now remove d2 only: enum = 4 of 5 -> 4 >= 0.8*5=4.0, proceeds; d2 trashed.
	minus := []adapter.RemoteDoc{}
	for _, d := range full5 {
		if d.ID != "d2" {
			minus = append(minus, d)
		}
	}
	res := runWith(t, l, &mutAdapter{docs: minus, bodies: bodies}, false)
	if res.Stats.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1; warnings=%v", res.Stats.Deleted, res.Stats.Warnings)
	}
	if len(res.Stats.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Stats.Warnings)
	}
	if exists(root, "feishu/wiki-空间/乙.md") {
		t.Fatalf("deleted doc still at original path")
	}
	date := time.Now().UTC().Format("2006-01-02")
	trashRel := ".internal/trash/" + date + "/feishu/wiki-空间/乙.md"
	if !exists(root, trashRel) {
		t.Fatalf("trashed file not at %s", trashRel)
	}
	db := openManifest(t, l)
	if doc, found, _ := db.GetDocument("d2"); !found || doc.Status != "trashed" {
		t.Fatalf("d2 status = %q found=%v, want trashed", doc.Status, found)
	}
	db.Close()
	// Trashed doc absent from INDEX.
	idx := readFile(t, root, "INDEX.md")
	if strings.Contains(idx, "乙") {
		t.Fatalf("trashed doc should not appear in INDEX:\n%s", idx)
	}

	// A subsequent sync with the same (d2-less) inventory must not resurrect or
	// re-count it.
	res2 := runWith(t, l, &mutAdapter{docs: minus, bodies: bodies}, false)
	if res2.Stats.Deleted != 0 {
		t.Fatalf("re-sync Deleted = %d, want 0 (no resurrection/re-trash)", res2.Stats.Deleted)
	}
	if exists(root, "feishu/wiki-空间/乙.md") {
		t.Fatalf("trashed doc resurrected on re-sync")
	}
}

// ---- permission-jitter guard -------------------------------------------

func TestBelowJitterThresholdBoundary(t *testing.T) {
	cases := []struct {
		enum, active int
		want         bool
	}{
		{4, 5, false},   // exactly 80% -> proceed
		{3, 5, true},    // 60% -> abort
		{8, 10, false},  // 80% -> proceed
		{7, 10, true},   // 70% -> abort
		{79, 100, true}, // just below -> abort
		{80, 100, false},
		{0, 0, false},   // first run, empty manifest
		{5, 0, false},   // no active docs
		{10, 10, false}, // full inventory
		{0, 1, true},    // total loss
	}
	for _, c := range cases {
		if got := belowJitterThreshold(c.enum, c.active); got != c.want {
			t.Errorf("belowJitterThreshold(%d,%d) = %v, want %v", c.enum, c.active, got, c.want)
		}
	}
}

func TestJitterGuardAbortsDeleteAndKeepsFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)

	full := []adapter.RemoteDoc{
		{ID: "root", Type: adapter.TypeFolder, Title: "wiki-空间"},
		{ID: "d1", Type: adapter.TypeDocx, ParentID: "root", Title: "甲", EditedAt: "2026-07-14T09:00:00Z"},
		{ID: "d2", Type: adapter.TypeDocx, ParentID: "root", Title: "乙", EditedAt: "2026-07-14T09:00:00Z"},
		{ID: "d3", Type: adapter.TypeDocx, ParentID: "root", Title: "丙", EditedAt: "2026-07-14T09:00:00Z"},
		{ID: "d4", Type: adapter.TypeDocx, ParentID: "root", Title: "丁", EditedAt: "2026-07-14T09:00:00Z"},
	}
	bodies := map[string]adapter.FetchResult{
		"d1": {Markdown: "1"}, "d2": {Markdown: "2"}, "d3": {Markdown: "3"}, "d4": {Markdown: "4"},
	}
	runWith(t, l, &mutAdapter{docs: full, bodies: bodies}, false) // active = 5

	// Simulate a permission jitter: only the root enumerates (enum=1 of 5 -> 20%).
	res := runWith(t, l, &mutAdapter{docs: full[:1], bodies: bodies}, false)
	if res.Stats.Deleted != 0 {
		t.Fatalf("guard should abort deletes, got Deleted=%d", res.Stats.Deleted)
	}
	if len(res.Stats.Warnings) == 0 || !strings.Contains(res.Stats.Warnings[0], "permission-jitter guard") {
		t.Fatalf("expected jitter warning, got %v", res.Stats.Warnings)
	}
	for _, rel := range []string{"甲", "乙", "丙", "丁"} {
		if !exists(root, "feishu/wiki-空间/"+rel+".md") {
			t.Fatalf("file %s.md should be intact after aborted delete", rel)
		}
	}
	db := openManifest(t, l)
	defer db.Close()
	if doc, _, _ := db.GetDocument("d2"); doc.Status != "active" {
		t.Fatalf("d2 status = %q, want active (not trashed)", doc.Status)
	}
}

// ---- --full -------------------------------------------------------------

func TestFullForcesRefetchAndStaysIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)

	mk := func() *mutAdapter {
		return &mutAdapter{
			docs: []adapter.RemoteDoc{
				{ID: "root", Type: adapter.TypeFolder, Title: "wiki-空间"},
				{ID: "a", Type: adapter.TypeDocx, ParentID: "root", Title: "甲", URL: "https://x.feishu.cn/docx/a", EditedAt: "2026-07-14T09:00:00Z"},
				{ID: "b", Type: adapter.TypeDocx, ParentID: "root", Title: "乙", URL: "https://x.feishu.cn/docx/b", EditedAt: "2026-07-14T09:00:00Z"},
			},
			bodies: map[string]adapter.FetchResult{"a": {Markdown: "# 甲\n正文\n"}, "b": {Markdown: "# 乙\n正文\n"}},
		}
	}
	runWith(t, l, mk(), false)
	before := snapshotTree(t, filepath.Join(root, "feishu"))

	// Incremental rerun: nothing refetched.
	inc := mk()
	res := runWith(t, l, inc, false)
	if res.Stats.Skipped == 0 || inc.fetches["a"] != 0 {
		t.Fatalf("incremental rerun should skip without refetch, stats=%+v fetches=%v", res.Stats, inc.fetches)
	}

	// --full: every body refetched.
	fu := mk()
	resFull := runWith(t, l, fu, true)
	if fu.fetches["a"] != 1 || fu.fetches["b"] != 1 {
		t.Fatalf("--full should refetch every body, fetches=%v", fu.fetches)
	}
	if resFull.Stats.Skipped != 0 {
		t.Fatalf("--full should skip nothing, got Skipped=%d", resFull.Stats.Skipped)
	}
	// Idempotent: the doc tree is content-identical to the pre-full snapshot. The
	// per-file `synced:` frontmatter line is the local fetch time and advances on
	// every rewrite by design, so it is excluded from the comparison.
	after := snapshotTree(t, filepath.Join(root, "feishu"))
	if len(before) != len(after) {
		t.Fatalf("file set changed after --full: %d -> %d", len(before), len(after))
	}
	for path, b := range before {
		if after[path] != b {
			t.Fatalf("--full changed %s (not idempotent)\nbefore:\n%s\nafter:\n%s", path, b, after[path])
		}
	}
}

// snapshotTree returns a map of relative path -> file contents for every file
// under dir, for content idempotency comparison. The `synced:` frontmatter line
// (the local fetch time, expected to advance) is stripped so the comparison is
// about mirrored content, not the moment of the write.
func snapshotTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(dir, p)
		var kept []string
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "synced:") {
				continue
			}
			kept = append(kept, line)
		}
		out[rel] = strings.Join(kept, "\n")
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return out
}

// ---- trash aging --------------------------------------------------------

func TestPurgeAgedTrash(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)
	if err := l.EnsureInternal(); err != nil {
		t.Fatalf("EnsureInternal: %v", err)
	}
	eng, err := New(l, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Close()

	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	oldDate := now.AddDate(0, 0, -40).Format("2006-01-02") // past 30-day window
	newDate := now.AddDate(0, 0, -5).Format("2006-01-02")  // within window
	for _, d := range []string{oldDate, newDate} {
		if err := os.MkdirAll(filepath.Join(l.TrashDir(), d, "feishu"), 0o755); err != nil {
			t.Fatalf("mk trash %s: %v", d, err)
		}
		if err := os.WriteFile(filepath.Join(l.TrashDir(), d, "feishu", "x.md"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write trash file: %v", err)
		}
	}

	db := openManifest(t, l)
	defer db.Close()
	// Two tombstones, one old one recent, to exercise the manifest purge too.
	if err := db.UpsertDocument(manifest.Document{ID: "old", Platform: "feishu", Type: "docx", Status: "trashed", SyncedAt: now.AddDate(0, 0, -40).Format(time.RFC3339)}); err != nil {
		t.Fatalf("insert old tombstone: %v", err)
	}
	if err := db.UpsertDocument(manifest.Document{ID: "new", Platform: "feishu", Type: "docx", Status: "trashed", SyncedAt: now.AddDate(0, 0, -5).Format(time.RFC3339)}); err != nil {
		t.Fatalf("insert new tombstone: %v", err)
	}

	removed, err := eng.purgeAgedTrash(db, now)
	if err != nil {
		t.Fatalf("purgeAgedTrash: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (only the 40-day-old bucket)", removed)
	}
	if exists(root, ".internal/trash/"+oldDate) {
		t.Fatalf("old trash bucket should be purged")
	}
	if !exists(root, ".internal/trash/"+newDate) {
		t.Fatalf("recent trash bucket should be kept")
	}
	// Manifest tombstones: old purged, recent kept.
	if _, found, _ := db.GetDocument("old"); found {
		t.Fatalf("old tombstone should be purged from manifest")
	}
	if _, found, _ := db.GetDocument("new"); !found {
		t.Fatalf("recent tombstone should be kept in manifest")
	}
}
