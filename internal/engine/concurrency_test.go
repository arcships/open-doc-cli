package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/layout"
	"github.com/arcships/open-doc-cli/internal/manifest"
)

// baseAdapter is a minimal in-memory adapter for the dual-platform concurrency
// test. Its id space is prefixed with the platform so two of them never collide.
type baseAdapter struct {
	platform string
	docs     []adapter.RemoteDoc
	bodies   map[string]adapter.FetchResult
}

func (b *baseAdapter) Platform() string { return b.platform }

func (b *baseAdapter) Enumerate(ctx context.Context) (<-chan adapter.RemoteDoc, <-chan error) {
	out := make(chan adapter.RemoteDoc)
	errc := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errc)
		for _, d := range b.docs {
			out <- d
		}
	}()
	return out, errc
}

func (b *baseAdapter) FetchMarkdown(ctx context.Context, doc adapter.RemoteDoc) (adapter.FetchResult, error) {
	return b.bodies[doc.ID], nil
}

func (b *baseAdapter) DownloadAsset(ctx context.Context, ref adapter.AssetRef, dest string) error {
	return os.WriteFile(dest, []byte("asset:"+ref.RemoteKey), 0o644)
}

// incAdapter is a Notion-shaped adapter that also supports incremental
// enumeration; EnumerateIncremental replays the full doc set (the engine's
// content-hash skip absorbs the unchanged repeats) and reports a fixed checkpoint.
type incAdapter struct {
	baseAdapter
}

func (a *incAdapter) EnumerateIncremental(ctx context.Context, checkpoint string) ([]adapter.RemoteDoc, string, error) {
	return a.docs, "2026-07-14T09:00:00Z", nil
}

func mkDocs(platform string) ([]adapter.RemoteDoc, map[string]adapter.FetchResult) {
	root := platform + ":root"
	child := platform + ":child"
	docs := []adapter.RemoteDoc{
		{ID: root, Type: adapter.TypePage, Title: platform + " Home", URL: "https://x/" + root, EditedAt: "2026-07-14T09:00:00Z"},
		{ID: child, Type: adapter.TypePage, ParentID: root, Title: platform + " Child", URL: "https://x/" + child, EditedAt: "2026-07-14T09:00:00Z"},
	}
	bodies := map[string]adapter.FetchResult{
		root:  {Markdown: "# " + platform + " home"},
		child: {Markdown: "# " + platform + " child"},
	}
	return docs, bodies
}

// TestDualPlatformConcurrentSync runs a Feishu-shaped adapter and a Notion-shaped
// (incremental-capable) adapter through a single Sync, concurrently. It asserts
// independent per-platform sync_runs rows, a combined INDEX spanning both, and a
// clean idempotent second run. Run under -race, it also guards the
// shared manifest handle.
func TestDualPlatformConcurrentSync(t *testing.T) {
	l := layout.For(filepath.Join(t.TempDir(), "mirror"))

	fdocs, fbodies := mkDocs("feishu")
	ndocs, nbodies := mkDocs("notion")
	feishu := &baseAdapter{platform: "feishu", docs: fdocs, bodies: fbodies}
	notion := &incAdapter{baseAdapter{platform: "notion", docs: ndocs, bodies: nbodies}}

	eng, err := New(l, Options{Adapters: []adapter.Adapter{feishu, notion}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Close()

	res, err := eng.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Two platforms ran, each with its own sync_runs row and distinct id.
	if len(res.Platforms) != 2 {
		t.Fatalf("want 2 platform runs, got %d", len(res.Platforms))
	}
	seen := map[string]int64{}
	for _, p := range res.Platforms {
		seen[p.Platform] = p.SyncRunID
	}
	if seen["feishu"] == 0 || seen["notion"] == 0 || seen["feishu"] == seen["notion"] {
		t.Fatalf("platform sync_runs ids not independent: %v", seen)
	}
	// Feishu is always full; Notion's first run is full (first ever).
	for _, p := range res.Platforms {
		if p.Platform == "feishu" && p.Stats.Mode != "full" {
			t.Errorf("feishu mode = %q, want full", p.Stats.Mode)
		}
		if p.Platform == "notion" && p.Stats.Mode != "full" {
			t.Errorf("notion first run mode = %q, want full", p.Stats.Mode)
		}
	}

	// Both platforms' documents landed on disk.
	for _, rel := range []string{
		"feishu/feishu Home/README.md",
		"feishu/feishu Home/feishu Child.md",
		"notion/notion Home/README.md",
		"notion/notion Home/notion Child.md",
	} {
		if _, err := os.Stat(filepath.Join(l.Root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}

	// The combined INDEX spans both platforms (finalized once, after both).
	index := readEngineFile(t, l.Root, "INDEX.md")
	if !strings.Contains(index, "feishu Home") || !strings.Contains(index, "notion Home") {
		t.Errorf("INDEX.md missing a platform:\n%s", index)
	}

	// Two sync_runs rows exist in the manifest.
	db, err := manifest.Open(l.ManifestPath())
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer db.Close()
	n, err := db.CountSyncRuns()
	if err != nil {
		t.Fatalf("CountSyncRuns: %v", err)
	}
	if n != 2 {
		t.Fatalf("sync_runs = %d, want 2", n)
	}

	// Second run: clean and idempotent. Notion goes incremental (same day, checkpoint
	// present); Feishu stays full. Neither adds nor updates anything.
	eng2, err := New(l, Options{Adapters: []adapter.Adapter{feishu, notion}})
	if err != nil {
		t.Fatalf("New 2: %v", err)
	}
	defer eng2.Close()
	res2, err := eng2.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync 2: %v", err)
	}
	if res2.Stats.Added != 0 || res2.Stats.Updated != 0 {
		t.Errorf("second run not idempotent: added=%d updated=%d", res2.Stats.Added, res2.Stats.Updated)
	}
	for _, p := range res2.Platforms {
		if p.Platform == "notion" && p.Stats.Mode != "incremental" {
			t.Errorf("notion second run mode = %q, want incremental", p.Stats.Mode)
		}
	}
}

func readEngineFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}
