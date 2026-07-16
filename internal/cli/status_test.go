package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arcships/open-doc-cli/internal/layout"
	"github.com/arcships/open-doc-cli/internal/manifest"
)

// newManifest creates <root>/.internal, writes a default config (a manifest
// implies an initialized mirror), and opens a fresh manifest for seeding. The
// caller must Close the returned DB.
func newManifest(t *testing.T, root string) *manifest.DB {
	t.Helper()
	writeDefaultConfig(t, root)
	db, err := manifest.Open(layout.For(root).ManifestPath())
	if err != nil {
		t.Fatalf("manifest.Open: %v", err)
	}
	return db
}

// seedStatusManifest populates a manifest with a small mixed population.
func seedStatusManifest(t *testing.T, db *manifest.DB) {
	t.Helper()
	docs := []manifest.Document{
		{ID: "n1", Platform: "notion", Type: "page", Title: "Notion One", LocalPath: "notion/one.md", Status: "active"},
		{ID: "n2", Platform: "notion", Type: "page", Title: "Notion Two", LocalPath: "notion/two.md", Status: "active"},
		{ID: "n3", Platform: "notion", Type: "page", Title: "Notion Trashed", LocalPath: ".internal/trash/x.md", Status: "trashed"},
		{ID: "f1", Platform: "feishu", Type: "docx", Title: "Feishu One", LocalPath: "feishu/one.md", Status: "active"},
	}
	for _, d := range docs {
		if err := db.UpsertDocument(d); err != nil {
			t.Fatalf("UpsertDocument %s: %v", d.ID, err)
		}
	}
	if err := db.UpsertAsset("k1", "done"); err != nil {
		t.Fatalf("UpsertAsset: %v", err)
	}
	if err := db.MarkAssetPending("k2"); err != nil {
		t.Fatalf("MarkAssetPending: %v", err)
	}
	now := time.Now()
	_, err := db.InsertSyncRun(manifest.SyncRun{
		Platform: "notion", StartedAt: now, FinishedAt: now,
		Stats: `{"mode":"full","degradations":3,"unknown_blocks":1,"truncated_pages":2}`,
	})
	if err != nil {
		t.Fatalf("InsertSyncRun: %v", err)
	}
	_, err = db.InsertSyncRun(manifest.SyncRun{
		Platform: "feishu", StartedAt: now, FinishedAt: now,
		Stats: `{"mode":"full","degradations":5,"bitables_oversize":5}`,
	})
	if err != nil {
		t.Fatalf("InsertSyncRun: %v", err)
	}
}

func TestStatusJSON(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	db := newManifest(t, root)
	seedStatusManifest(t, db)
	db.Close()

	env, out, errb := newEnv("status", "--root", root, "--json")
	if code := Run(env); code != ExitOK {
		t.Fatalf("status = %d, want %d; stderr=%s", code, ExitOK, errb.String())
	}

	var r statusReport
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("status JSON invalid: %v; got %q", err, out.String())
	}
	if r.Documents.Total != 4 {
		t.Errorf("Documents.Total = %d, want 4", r.Documents.Total)
	}
	if r.Documents.ByPlatform["notion"] != 3 || r.Documents.ByPlatform["feishu"] != 1 {
		t.Errorf("ByPlatform = %v, want notion=3 feishu=1", r.Documents.ByPlatform)
	}
	if r.Documents.ByStatus["active"] != 3 || r.Documents.ByStatus["trashed"] != 1 {
		t.Errorf("ByStatus = %v, want active=3 trashed=1", r.Documents.ByStatus)
	}
	if r.Assets.Total != 2 || r.Assets.Pending != 1 {
		t.Errorf("Assets = %+v, want total=2 pending=1", r.Assets)
	}
	if r.SyncRuns != 2 {
		t.Errorf("SyncRuns = %d, want 2", r.SyncRuns)
	}
	// Degradation counts come from each platform's latest run.
	var notion, feishu *platformStatus
	for i := range r.Platforms {
		switch r.Platforms[i].Platform {
		case "notion":
			notion = &r.Platforms[i]
		case "feishu":
			feishu = &r.Platforms[i]
		}
	}
	if notion == nil || notion.Degradations.Total != 3 || notion.Degradations.TruncatedPages != 2 {
		t.Errorf("notion degradations = %+v, want total=3 truncated=2", notion)
	}
	if feishu == nil || feishu.Degradations.Total != 5 || feishu.Degradations.BitablesOversize != 5 {
		t.Errorf("feishu degradations = %+v, want total=5 oversize=5", feishu)
	}
}

func TestStatusNoManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	writeDefaultConfig(t, root)
	env, out, errb := newEnv("status", "--root", root, "--json")
	if code := Run(env); code != ExitOK {
		t.Fatalf("status (no manifest) = %d, want %d; stderr=%s", code, ExitOK, errb.String())
	}
	var r statusReport
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("status JSON invalid: %v; got %q", err, out.String())
	}
	if r.Manifest {
		t.Errorf("Manifest = true, want false for a fresh root")
	}
	if r.Documents.Total != 0 {
		t.Errorf("Documents.Total = %d, want 0", r.Documents.Total)
	}
	// Status must never create the manifest (read-only guarantee).
	if fileExists(layout.For(root).ManifestPath()) {
		t.Errorf("status created a manifest; it must be read-only")
	}
}

func TestStatusHumanOutput(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	db := newManifest(t, root)
	seedStatusManifest(t, db)
	db.Close()

	env, out, errb := newEnv("status", "--root", root)
	if code := Run(env); code != ExitOK {
		t.Fatalf("status = %d; stderr=%s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"documents: 4 total", "notion=3", "feishu=1", "assets: 1 pending"} {
		if !strings.Contains(s, want) {
			t.Errorf("status output missing %q; got:\n%s", want, s)
		}
	}
}
