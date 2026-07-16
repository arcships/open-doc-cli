package manifest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var wantTables = []string{"assets", "doc_aliases", "documents", "links", "sync_runs"}

func TestOpenCreatesSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.sqlite")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tables, err := db.TableNames()
	if err != nil {
		t.Fatalf("TableNames: %v", err)
	}
	if !reflect.DeepEqual(tables, wantTables) {
		t.Fatalf("tables = %v, want %v", tables, wantTables)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.sqlite")
	db1, err := Open(path)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	db1.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open 2 (idempotent): %v", err)
	}
	defer db2.Close()

	tables, err := db2.TableNames()
	if err != nil {
		t.Fatalf("TableNames: %v", err)
	}
	if !reflect.DeepEqual(tables, wantTables) {
		t.Fatalf("tables = %v, want %v", tables, wantTables)
	}
}

func TestRebuildAfterDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.sqlite")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.InsertSyncRun(SyncRun{StartedAt: time.Now(), FinishedAt: time.Now()}); err != nil {
		t.Fatalf("InsertSyncRun: %v", err)
	}
	db.Close()

	// Delete the manifest file (and any WAL/SHM sidecars) and re-open.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("manifest still present after delete: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open after delete: %v", err)
	}
	defer db2.Close()

	tables, err := db2.TableNames()
	if err != nil {
		t.Fatalf("TableNames: %v", err)
	}
	if !reflect.DeepEqual(tables, wantTables) {
		t.Fatalf("rebuilt tables = %v, want %v", tables, wantTables)
	}

	// Fresh DB starts from zero.
	n, err := db2.CountSyncRuns()
	if err != nil {
		t.Fatalf("CountSyncRuns: %v", err)
	}
	if n != 0 {
		t.Fatalf("sync_runs count after rebuild = %d, want 0", n)
	}
}

func TestInsertSyncRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.sqlite")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	start := time.Now()
	id, err := db.InsertSyncRun(SyncRun{
		Platform:   "feishu",
		StartedAt:  start,
		FinishedAt: start.Add(time.Second),
		Stats:      `{"added":0}`,
	})
	if err != nil {
		t.Fatalf("InsertSyncRun: %v", err)
	}
	if id <= 0 {
		t.Fatalf("InsertSyncRun id = %d, want > 0", id)
	}

	n, err := db.CountSyncRuns()
	if err != nil {
		t.Fatalf("CountSyncRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("CountSyncRuns = %d, want 1", n)
	}
}

func TestUpsertAndGetDocument(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "m.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	doc := Document{
		ID: "objA", Platform: "feishu", Type: "docx", ParentID: "root",
		Title: "欢迎", LocalPath: "feishu/wiki-x/欢迎.md",
		RemoteEdited: "2026-07-14T09:17:21Z", ContentHash: "abc123",
		SyncedAt: "2026-07-15T00:00:00Z", Status: "active",
	}
	if err := db.UpsertDocument(doc); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	got, found, err := db.GetDocument("objA")
	if err != nil || !found {
		t.Fatalf("GetDocument: found=%v err=%v", found, err)
	}
	if got != doc {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, doc)
	}

	// Upsert again with a new hash -> replace, not duplicate.
	doc.ContentHash = "def456"
	doc.Title = "欢迎v2"
	if err := db.UpsertDocument(doc); err != nil {
		t.Fatalf("UpsertDocument (update): %v", err)
	}
	got, _, _ = db.GetDocument("objA")
	if got.ContentHash != "def456" || got.Title != "欢迎v2" {
		t.Fatalf("update not applied: %+v", got)
	}
	if n, _ := db.CountDocuments(); n != 1 {
		t.Fatalf("documents count = %d, want 1", n)
	}
}

func TestGetDocumentMissing(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "m.sqlite"))
	defer db.Close()
	if _, found, err := db.GetDocument("nope"); found || err != nil {
		t.Fatalf("missing doc: found=%v err=%v", found, err)
	}
}

func TestResolveLinkTargetByIDAndAlias(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "m.sqlite"))
	defer db.Close()

	// A docx keyed by obj_token, reachable both by id and by wiki node_token alias.
	if err := db.UpsertDocument(Document{
		ID: "objB", Platform: "feishu", Type: "docx",
		Title: "手册", LocalPath: "feishu/wiki-x/手册/README.md", Status: "active",
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	if err := db.UpsertAlias("nodeB", "objB"); err != nil {
		t.Fatalf("UpsertAlias: %v", err)
	}

	// Direct id resolution (docx URL token).
	if lp, ok, err := db.ResolveLinkTarget("objB"); err != nil || !ok || lp != "feishu/wiki-x/手册/README.md" {
		t.Fatalf("resolve by id: lp=%q ok=%v err=%v", lp, ok, err)
	}
	// Alias resolution (wiki node_token URL token).
	if lp, ok, err := db.ResolveLinkTarget("nodeB"); err != nil || !ok || lp != "feishu/wiki-x/手册/README.md" {
		t.Fatalf("resolve by alias: lp=%q ok=%v err=%v", lp, ok, err)
	}
	// Unknown token (external doc) -> not found, left untouched by rewrite.
	if _, ok, err := db.ResolveLinkTarget("外部"); ok || err != nil {
		t.Fatalf("resolve unknown: ok=%v err=%v", ok, err)
	}

	// Trashed documents are not link targets.
	if err := db.UpsertDocument(Document{ID: "objT", Platform: "feishu", Type: "docx",
		Title: "trashed", LocalPath: "feishu/wiki-x/t.md", Status: "trashed"}); err != nil {
		t.Fatalf("UpsertDocument trashed: %v", err)
	}
	if _, ok, _ := db.ResolveLinkTarget("objT"); ok {
		t.Fatalf("trashed doc should not resolve as a link target")
	}
}

func TestDistinctLinkFromIDsAndListActive(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "m.sqlite"))
	defer db.Close()

	db.UpsertDocument(Document{ID: "a", Platform: "feishu", Type: "docx", Title: "A", LocalPath: "feishu/a.md", Status: "active"})
	db.UpsertDocument(Document{ID: "b", Platform: "feishu", Type: "docx", Title: "B", LocalPath: "feishu/b.md", Status: "active"})
	db.UpsertDocument(Document{ID: "z", Platform: "feishu", Type: "docx", Title: "Z", LocalPath: "feishu/z.md", Status: "trashed"})
	db.UpsertLink("b", "a")
	db.UpsertLink("a", "b")
	db.UpsertLink("a", "b") // duplicate collapses

	ids, err := db.DistinctLinkFromIDs()
	if err != nil {
		t.Fatalf("DistinctLinkFromIDs: %v", err)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("distinct from ids = %v, want [a b] sorted", ids)
	}

	active, err := db.ListActiveDocuments()
	if err != nil {
		t.Fatalf("ListActiveDocuments: %v", err)
	}
	if len(active) != 2 { // z is trashed
		t.Fatalf("active docs = %d, want 2 (%+v)", len(active), active)
	}
}

func TestUpsertLinkAndAssetIdempotent(t *testing.T) {
	db, _ := Open(filepath.Join(t.TempDir(), "m.sqlite"))
	defer db.Close()

	for i := 0; i < 2; i++ {
		if err := db.UpsertLink("from", "to"); err != nil {
			t.Fatalf("UpsertLink: %v", err)
		}
		if err := db.UpsertAsset("tok1", "pending"); err != nil {
			t.Fatalf("UpsertAsset: %v", err)
		}
	}
	var links, assets int
	db.SQL().QueryRow(`SELECT COUNT(*) FROM links`).Scan(&links)
	db.SQL().QueryRow(`SELECT COUNT(*) FROM assets`).Scan(&assets)
	if links != 1 || assets != 1 {
		t.Fatalf("idempotency broken: links=%d assets=%d", links, assets)
	}
}
