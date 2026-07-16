package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/arcships/open-doc-cli/internal/layout"
	"github.com/arcships/open-doc-cli/internal/manifest"
)

func TestSyncEmptyRunRecordsSyncRun(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)

	eng, err := New(l, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Close()

	res, err := eng.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// A sync_runs row must exist and be the one reported.
	if res.SyncRunID <= 0 {
		t.Fatalf("SyncRunID = %d, want > 0", res.SyncRunID)
	}
	if res.Root != root {
		t.Fatalf("Result.Root = %q, want %q", res.Root, root)
	}
	if res.FinishedAt.Before(res.StartedAt) {
		t.Fatalf("FinishedAt before StartedAt")
	}

	// The internal structure must be materialised.
	for _, dir := range []string{l.Internal, l.LogsDir(), l.TrashDir(), l.AssetsDir()} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("expected directory %s: err=%v", dir, err)
		}
	}

	// Verify the row is really in the manifest.
	db, err := manifest.Open(l.ManifestPath())
	if err != nil {
		t.Fatalf("Open manifest: %v", err)
	}
	defer db.Close()
	n, err := db.CountSyncRuns()
	if err != nil {
		t.Fatalf("CountSyncRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("sync_runs count = %d, want 1", n)
	}

	// Empty run: no adapters, all counters zero.
	if len(res.Stats.Adapters) != 0 {
		t.Fatalf("Adapters = %v, want empty", res.Stats.Adapters)
	}
	if res.Stats.Added != 0 || res.Stats.Updated != 0 || res.Stats.Deleted != 0 {
		t.Fatalf("expected zero counters, got %+v", res.Stats)
	}
}

func TestSyncTwiceRecordsTwoRuns(t *testing.T) {
	l := layout.For(filepath.Join(t.TempDir(), "mirror"))
	eng, err := New(l, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Close()

	if _, err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("Sync 1: %v", err)
	}
	if _, err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("Sync 2: %v", err)
	}

	db, err := manifest.Open(l.ManifestPath())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	n, err := db.CountSyncRuns()
	if err != nil {
		t.Fatalf("CountSyncRuns: %v", err)
	}
	if n != 2 {
		t.Fatalf("sync_runs count = %d, want 2", n)
	}
}
