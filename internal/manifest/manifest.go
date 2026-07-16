// Package manifest owns manifest.sqlite, the sync engine's bookkeeping ledger.
// It is a single-file SQLite database, opened via the pure-Go
// modernc.org/sqlite driver (no cgo).
//
// Opening is idempotent: the four tables are created with CREATE TABLE IF NOT
// EXISTS, so deleting the file and re-opening rebuilds the schema from zero.
// A lost or corrupt manifest is recoverable — `opendoc sync --full` rebuilds
// content from scratch.
package manifest

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// schema is the manifest's table set: the four core tables (documents, assets,
// links, sync_runs) plus the doc_aliases auxiliary index. Every statement is
// idempotent so Open can run it unconditionally.
const schema = `
-- Document master table: keyed by the platform-native ID; everything
-- reconciles against it.
CREATE TABLE IF NOT EXISTS documents (
  id            TEXT PRIMARY KEY,   -- page_id / obj_token
  platform      TEXT NOT NULL,      -- notion | feishu
  type          TEXT NOT NULL,      -- page | db | db_row | docx | folder ...
  parent_id     TEXT,
  title         TEXT,
  local_path    TEXT UNIQUE,        -- path relative to the mirror root
  remote_edited TEXT,               -- remote last_edited_time / obj_edit_time
  content_hash  TEXT,               -- sha256 of the fetch product (pre link-rewrite)
  synced_at     TEXT,
  status        TEXT NOT NULL       -- active | trashed | error | pending_assets
);

-- Asset table: keyed by a platform-stable key to prevent re-downloads.
CREATE TABLE IF NOT EXISTS assets (
  remote_key    TEXT PRIMARY KEY,   -- Notion: S3 path; Feishu: asset token
  sha256        TEXT,
  local_path    TEXT,
  status        TEXT NOT NULL,      -- done | pending
  ref_count     INTEGER DEFAULT 1   -- reference only; P1 does not reclaim assets
);

-- Backlink table: reverse-lookup "who references me" on rename/move.
CREATE TABLE IF NOT EXISTS links (
  from_id TEXT, to_id TEXT,
  PRIMARY KEY (from_id, to_id)
);

-- Alias table: maps a document's secondary identifier to its primary id, so an
-- internal link that uses the alias can be resolved to the target document
-- (Feishu /wiki/<node_token> URLs reference node_token, but the
-- document is keyed by obj_token). Not one of the four core tables; it is an
-- auxiliary index the engine populates from enumeration and reads during
-- link rewriting. Rebuilt from zero on --full like everything else.
CREATE TABLE IF NOT EXISTS doc_aliases (
  alias  TEXT PRIMARY KEY,   -- e.g. wiki node_token
  doc_id TEXT NOT NULL       -- documents.id (obj_token / page_id)
);

-- Sync history: resumable checkpoints and audit.
CREATE TABLE IF NOT EXISTS sync_runs (
  id INTEGER PRIMARY KEY,
  platform TEXT, started_at TEXT, finished_at TEXT,
  checkpoint TEXT,                  -- notion: max last_edited_time this run
  stats TEXT                        -- JSON: added/updated/deleted/degradation/failures
);
`

// DB wraps the SQLite handle for a manifest.
type DB struct {
	sql *sql.DB
}

// Open opens (creating if necessary) the manifest at path and ensures the
// schema exists. The caller owns the returned *DB and must Close it.
func Open(path string) (*DB, error) {
	// Busy timeout guards against transient locks during concurrent runs.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open manifest %s: %w", path, err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping manifest %s: %w", path, err)
	}
	// Serialize all access onto a single connection: `opendoc sync` runs both platform
	// pipelines concurrently against this one handle, so pinning the pool to
	// one connection makes database/sql queue every statement and the sqlite driver
	// never sees two concurrent writers (no SQLITE_BUSY). The workload is I/O-bound
	// on network fetches, so the lost read parallelism costs nothing.
	sqlDB.SetMaxOpenConns(1)
	db := &DB{sql: sqlDB}
	if err := db.ensureSchema(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// ensureSchema runs the idempotent CREATE TABLE statements.
func (db *DB) ensureSchema() error {
	if _, err := db.sql.Exec(schema); err != nil {
		return fmt.Errorf("create manifest schema: %w", err)
	}
	return nil
}

// Close closes the underlying database handle.
func (db *DB) Close() error {
	if db == nil || db.sql == nil {
		return nil
	}
	return db.sql.Close()
}

// SQL exposes the underlying *sql.DB for packages that need direct access
// (e.g. the engine). It is intentionally minimal.
func (db *DB) SQL() *sql.DB { return db.sql }

// TableNames returns the sorted names of the user tables present in the
// manifest. It is used by tests and diagnostics to assert the schema.
func (db *DB) TableNames() ([]string, error) {
	rows, err := db.sql.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// Document is a row of the documents table. Timestamps are stored as
// RFC3339 strings (or the platform-native string for RemoteEdited).
type Document struct {
	ID           string
	Platform     string
	Type         string
	ParentID     string
	Title        string
	LocalPath    string
	RemoteEdited string
	ContentHash  string
	SyncedAt     string
	Status       string
}

// GetDocument returns the document row for id. found is false when no row
// exists.
func (db *DB) GetDocument(id string) (doc Document, found bool, err error) {
	row := db.sql.QueryRow(
		`SELECT id, platform, type, parent_id, title, local_path, remote_edited, content_hash, synced_at, status
		 FROM documents WHERE id = ?`, id)
	var parentID, title, localPath, remoteEdited, contentHash, syncedAt sql.NullString
	err = row.Scan(&doc.ID, &doc.Platform, &doc.Type, &parentID, &title, &localPath,
		&remoteEdited, &contentHash, &syncedAt, &doc.Status)
	if err == sql.ErrNoRows {
		return Document{}, false, nil
	}
	if err != nil {
		return Document{}, false, fmt.Errorf("get document %s: %w", id, err)
	}
	doc.ParentID = parentID.String
	doc.Title = title.String
	doc.LocalPath = localPath.String
	doc.RemoteEdited = remoteEdited.String
	doc.ContentHash = contentHash.String
	doc.SyncedAt = syncedAt.String
	return doc, true, nil
}

// UpsertDocument inserts or replaces a document row, keyed by ID.
func (db *DB) UpsertDocument(doc Document) error {
	_, err := db.sql.Exec(
		`INSERT INTO documents (id, platform, type, parent_id, title, local_path, remote_edited, content_hash, synced_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   platform=excluded.platform, type=excluded.type, parent_id=excluded.parent_id,
		   title=excluded.title, local_path=excluded.local_path, remote_edited=excluded.remote_edited,
		   content_hash=excluded.content_hash, synced_at=excluded.synced_at, status=excluded.status`,
		doc.ID, doc.Platform, doc.Type, nullable(doc.ParentID), nullable(doc.Title),
		nullable(doc.LocalPath), nullable(doc.RemoteEdited), nullable(doc.ContentHash),
		nullable(doc.SyncedAt), doc.Status)
	if err != nil {
		return fmt.Errorf("upsert document %s: %w", doc.ID, err)
	}
	return nil
}

// CountDocuments returns the number of rows in documents.
func (db *DB) CountDocuments() (int, error) {
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM documents`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count documents: %w", err)
	}
	return n, nil
}

// ListDocumentsByPlatform returns every document row for a platform whose status
// matches (id, type, parent_id, title, local_path, status). It backs the
// lifecycle reconciliation (delete detection and the permission-jitter
// guard).
func (db *DB) ListDocumentsByPlatform(platform, status string) ([]Document, error) {
	rows, err := db.sql.Query(
		`SELECT id, type, parent_id, title, local_path, status
		 FROM documents WHERE platform=? AND status=?`, platform, status)
	if err != nil {
		return nil, fmt.Errorf("list documents by platform %s/%s: %w", platform, status, err)
	}
	defer rows.Close()
	var docs []Document
	for rows.Next() {
		var d Document
		var parentID, title, localPath sql.NullString
		if err := rows.Scan(&d.ID, &d.Type, &parentID, &title, &localPath, &d.Status); err != nil {
			return nil, err
		}
		d.Platform = platform
		d.ParentID = parentID.String
		d.Title = title.String
		d.LocalPath = localPath.String
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// SetDocumentTrashed marks a document as trashed: it flips the status, points
// local_path at the doc's on-disk resting place in the trash, and stamps
// synced_at (the trash time, later consumed by trash aging). The row is kept so
// a later run neither resurrects nor re-counts it.
func (db *DB) SetDocumentTrashed(id, trashPath, syncedAt string) error {
	_, err := db.sql.Exec(
		`UPDATE documents SET status='trashed', local_path=?, synced_at=? WHERE id=?`,
		nullable(trashPath), nullable(syncedAt), id)
	if err != nil {
		return fmt.Errorf("mark document trashed %s: %w", id, err)
	}
	return nil
}

// PurgeTrashedBefore deletes trashed document rows whose synced_at (trash time)
// is strictly older than cutoff (an RFC3339 UTC string). It returns the number
// of rows removed. On-disk trash directories are aged separately by the engine.
func (db *DB) PurgeTrashedBefore(cutoff string) (int, error) {
	res, err := db.sql.Exec(
		`DELETE FROM documents WHERE status='trashed' AND synced_at IS NOT NULL AND synced_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge trashed before %s: %w", cutoff, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// LinkPair is one edge of the links table.
type LinkPair struct {
	From string
	To   string
}

// AllLinks returns every recorded from->to edge. The move fixup scans them to
// find referrers of a moved document.
func (db *DB) AllLinks() ([]LinkPair, error) {
	rows, err := db.sql.Query(`SELECT from_id, to_id FROM links`)
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	defer rows.Close()
	var pairs []LinkPair
	for rows.Next() {
		var p LinkPair
		if err := rows.Scan(&p.From, &p.To); err != nil {
			return nil, err
		}
		pairs = append(pairs, p)
	}
	return pairs, rows.Err()
}

// ResolveTargetDocID maps a link target token (a documents.id or a doc_aliases
// alias such as a wiki node_token) to the primary documents.id of the active
// document it names. found is false for an unmirrored/external/trashed target.
// Unlike ResolveLinkTarget it returns the document id, not its local path, so the
// move fixup can look up both the pre- and post-move paths.
func (db *DB) ResolveTargetDocID(token string) (docID string, found bool, err error) {
	if token == "" {
		return "", false, nil
	}
	row := db.sql.QueryRow(`SELECT id FROM documents WHERE id=? AND status='active'`, token)
	switch err = row.Scan(&docID); {
	case err == nil:
		return docID, true, nil
	case err == sql.ErrNoRows:
		// fall through to alias lookup
	default:
		return "", false, fmt.Errorf("resolve target doc id %s: %w", token, err)
	}
	row = db.sql.QueryRow(
		`SELECT d.id FROM documents d JOIN doc_aliases a ON d.id=a.doc_id
		 WHERE a.alias=? AND d.status='active'`, token)
	err = row.Scan(&docID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve target alias %s: %w", token, err)
	}
	return docID, true, nil
}

// UpsertLink records a from->to document reference (idempotent).
func (db *DB) UpsertLink(fromID, toID string) error {
	_, err := db.sql.Exec(
		`INSERT INTO links (from_id, to_id) VALUES (?, ?) ON CONFLICT(from_id, to_id) DO NOTHING`,
		fromID, toID)
	if err != nil {
		return fmt.Errorf("upsert link %s->%s: %w", fromID, toID, err)
	}
	return nil
}

// UpsertAlias records that alias resolves to docID (idempotent). Used for
// Feishu wiki node_token -> obj_token resolution during link rewriting.
func (db *DB) UpsertAlias(alias, docID string) error {
	if alias == "" || docID == "" {
		return nil
	}
	_, err := db.sql.Exec(
		`INSERT INTO doc_aliases (alias, doc_id) VALUES (?, ?)
		 ON CONFLICT(alias) DO UPDATE SET doc_id=excluded.doc_id`,
		alias, docID)
	if err != nil {
		return fmt.Errorf("upsert alias %s->%s: %w", alias, docID, err)
	}
	return nil
}

// DistinctLinkFromIDs returns the sorted set of source document ids that have at
// least one recorded outgoing link. The link-rewrite finalize step scans only
// these documents' bodies rather than the whole tree.
func (db *DB) DistinctLinkFromIDs() ([]string, error) {
	rows, err := db.sql.Query(`SELECT DISTINCT from_id FROM links ORDER BY from_id`)
	if err != nil {
		return nil, fmt.Errorf("list link sources: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ResolveLinkTarget maps a token extracted from an internal-link URL to the
// local_path of the active document it names, resolving either directly by
// document id or via the alias table (wiki node_token). found is false when the
// token names no active mirrored document (external/unauthorized target), in
// which case the link is left untouched.
func (db *DB) ResolveLinkTarget(token string) (localPath string, found bool, err error) {
	if token == "" {
		return "", false, nil
	}
	var lp sql.NullString
	row := db.sql.QueryRow(
		`SELECT local_path FROM documents WHERE id=? AND status='active'`, token)
	switch err = row.Scan(&lp); {
	case err == nil:
		if lp.Valid && lp.String != "" {
			return lp.String, true, nil
		}
	case err == sql.ErrNoRows:
		// fall through to alias lookup
	default:
		return "", false, fmt.Errorf("resolve link target %s: %w", token, err)
	}

	row = db.sql.QueryRow(
		`SELECT d.local_path FROM documents d
		 JOIN doc_aliases a ON d.id = a.doc_id
		 WHERE a.alias=? AND d.status='active'`, token)
	err = row.Scan(&lp)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve link alias %s: %w", token, err)
	}
	if lp.Valid && lp.String != "" {
		return lp.String, true, nil
	}
	return "", false, nil
}

// ListActiveDocuments returns every active document row (id, platform, type,
// title, local_path, remote_edited) for INDEX.md generation. Ordering is left
// to the caller (INDEX renders a deterministic tree order).
func (db *DB) ListActiveDocuments() ([]Document, error) {
	rows, err := db.sql.Query(
		`SELECT id, platform, type, title, local_path, remote_edited
		 FROM documents WHERE status='active'`)
	if err != nil {
		return nil, fmt.Errorf("list active documents: %w", err)
	}
	defer rows.Close()
	var docs []Document
	for rows.Next() {
		var d Document
		var title, localPath, remoteEdited sql.NullString
		if err := rows.Scan(&d.ID, &d.Platform, &d.Type, &title, &localPath, &remoteEdited); err != nil {
			return nil, err
		}
		d.Title = title.String
		d.LocalPath = localPath.String
		d.RemoteEdited = remoteEdited.String
		d.Status = statusActiveConst
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// statusActiveConst mirrors the engine's "active" status literal for rows read
// back by ListActiveDocuments (which filters on status='active').
const statusActiveConst = "active"

// UpsertAsset records an asset by its platform-stable remote key. An existing
// row is left untouched so a completed download is never demoted back to
// pending by a later registration pass.
func (db *DB) UpsertAsset(remoteKey, status string) error {
	_, err := db.sql.Exec(
		`INSERT INTO assets (remote_key, status) VALUES (?, ?) ON CONFLICT(remote_key) DO NOTHING`,
		remoteKey, status)
	if err != nil {
		return fmt.Errorf("upsert asset %s: %w", remoteKey, err)
	}
	return nil
}

// Asset is a row of the assets table.
type Asset struct {
	RemoteKey string
	SHA256    string
	LocalPath string
	Status    string
	RefCount  int
}

// GetAsset returns the asset row for remoteKey. found is false when no row
// exists.
func (db *DB) GetAsset(remoteKey string) (a Asset, found bool, err error) {
	row := db.sql.QueryRow(
		`SELECT remote_key, sha256, local_path, status, ref_count FROM assets WHERE remote_key = ?`, remoteKey)
	var sha, local sql.NullString
	var refCount sql.NullInt64
	err = row.Scan(&a.RemoteKey, &sha, &local, &a.Status, &refCount)
	if err == sql.ErrNoRows {
		return Asset{}, false, nil
	}
	if err != nil {
		return Asset{}, false, fmt.Errorf("get asset %s: %w", remoteKey, err)
	}
	a.SHA256 = sha.String
	a.LocalPath = local.String
	a.RefCount = int(refCount.Int64)
	return a, true, nil
}

// MarkAssetDone records a successful download: it stores the content hash and
// local path and flips the status to "done". The row is created if absent.
func (db *DB) MarkAssetDone(remoteKey, sha256, localPath string) error {
	_, err := db.sql.Exec(
		`INSERT INTO assets (remote_key, sha256, local_path, status) VALUES (?, ?, ?, 'done')
		 ON CONFLICT(remote_key) DO UPDATE SET sha256=excluded.sha256, local_path=excluded.local_path, status='done'`,
		remoteKey, sha256, localPath)
	if err != nil {
		return fmt.Errorf("mark asset done %s: %w", remoteKey, err)
	}
	return nil
}

// MarkAssetPending records a failed/again-pending download, leaving any prior
// sha256/local_path in place but flipping the status back to "pending". The row
// is created if absent.
func (db *DB) MarkAssetPending(remoteKey string) error {
	_, err := db.sql.Exec(
		`INSERT INTO assets (remote_key, status) VALUES (?, 'pending')
		 ON CONFLICT(remote_key) DO UPDATE SET status='pending'`,
		remoteKey)
	if err != nil {
		return fmt.Errorf("mark asset pending %s: %w", remoteKey, err)
	}
	return nil
}

// CountAssetsByStatus returns the number of asset rows with the given status.
func (db *DB) CountAssetsByStatus(status string) (int, error) {
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM assets WHERE status = ?`, status).Scan(&n); err != nil {
		return 0, fmt.Errorf("count assets by status %s: %w", status, err)
	}
	return n, nil
}

// CountAssets returns the total number of asset rows.
func (db *DB) CountAssets() (int, error) {
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM assets`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count assets: %w", err)
	}
	return n, nil
}

// SyncRun captures the fields recorded for a single sync run.
type SyncRun struct {
	Platform   string
	StartedAt  time.Time
	FinishedAt time.Time
	Checkpoint string
	// Stats is an opaque JSON blob (the sync report).
	Stats string
}

// InsertSyncRun records one row in sync_runs and returns its generated id.
// Timestamps are stored as RFC3339 UTC strings.
func (db *DB) InsertSyncRun(run SyncRun) (int64, error) {
	res, err := db.sql.Exec(
		`INSERT INTO sync_runs (platform, started_at, finished_at, checkpoint, stats) VALUES (?, ?, ?, ?, ?)`,
		nullable(run.Platform),
		run.StartedAt.UTC().Format(time.RFC3339),
		run.FinishedAt.UTC().Format(time.RFC3339),
		nullable(run.Checkpoint),
		nullable(run.Stats),
	)
	if err != nil {
		return 0, fmt.Errorf("insert sync_run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("sync_run last insert id: %w", err)
	}
	return id, nil
}

// CountSyncRuns returns the number of rows in sync_runs.
func (db *DB) CountSyncRuns() (int, error) {
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sync_runs`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count sync_runs: %w", err)
	}
	return n, nil
}

// SyncRunRecord is a sync_runs row read back for the incremental / reconciliation
// mode decision. StartedAtRaw is the stored RFC3339 string (parsed by the
// engine into local time for the daily-first-run check); Checkpoint and Stats
// carry the persisted checkpoint and the stats JSON (which holds the run's mode).
type SyncRunRecord struct {
	ID           int64
	StartedAtRaw string
	Checkpoint   string
	Stats        string
}

// RecentSyncRuns returns up to limit of a platform's most recent sync_runs rows,
// newest first (by id). The engine reads them to decide a Notion run's mode:
// their started_at drives the daily-first-run trigger and their stats.mode drives
// the every-N-runs cadence.
func (db *DB) RecentSyncRuns(platform string, limit int) ([]SyncRunRecord, error) {
	rows, err := db.sql.Query(
		`SELECT id, started_at, checkpoint, stats FROM sync_runs
		 WHERE platform=? ORDER BY id DESC LIMIT ?`, platform, limit)
	if err != nil {
		return nil, fmt.Errorf("recent sync_runs for %s: %w", platform, err)
	}
	defer rows.Close()
	var out []SyncRunRecord
	for rows.Next() {
		var r SyncRunRecord
		var started, checkpoint, stats sql.NullString
		if err := rows.Scan(&r.ID, &started, &checkpoint, &stats); err != nil {
			return nil, err
		}
		r.StartedAtRaw = started.String
		r.Checkpoint = checkpoint.String
		r.Stats = stats.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestCheckpoint returns the most recent non-empty checkpoint recorded for a
// platform (the high-water last_edited_time an incremental round resumes from),
// or "" when none exists yet (first-ever run → full enumeration).
func (db *DB) LatestCheckpoint(platform string) (string, error) {
	rows, err := db.sql.Query(
		`SELECT checkpoint FROM sync_runs
		 WHERE platform=? AND checkpoint IS NOT NULL AND checkpoint<>''
		 ORDER BY id DESC LIMIT 1`, platform)
	if err != nil {
		return "", fmt.Errorf("latest checkpoint for %s: %w", platform, err)
	}
	defer rows.Close()
	if rows.Next() {
		var ck string
		if err := rows.Scan(&ck); err != nil {
			return "", err
		}
		return ck, rows.Err()
	}
	return "", rows.Err()
}

// ChildrenCount returns the number of active documents whose parent_id is
// parentID within a platform. The incremental placement resolver uses it to tell
// whether an enumerated page already owns a subtree (so it renders as a directory
// with a README) even when none of its children are enumerated this round.
func (db *DB) ChildrenCount(platform, parentID string) (int, error) {
	var n int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM documents WHERE platform=? AND parent_id=? AND status='active'`,
		platform, parentID).Scan(&n); err != nil {
		return 0, fmt.Errorf("children count %s/%s: %w", platform, parentID, err)
	}
	return n, nil
}

// Auxiliary-command query helpers (opendoc status / doctor / resolve).

// DocCount is one (platform, status) population bucket of the documents table.
type DocCount struct {
	Platform string
	Status   string
	Count    int
}

// GroupedDocumentCounts returns the document population grouped by platform and
// status. It backs `opendoc status`'s document overview (from it the caller derives
// both the by-platform and the by-status totals). Ordering is deterministic.
func (db *DB) GroupedDocumentCounts() ([]DocCount, error) {
	rows, err := db.sql.Query(
		`SELECT platform, status, COUNT(*) FROM documents GROUP BY platform, status ORDER BY platform, status`)
	if err != nil {
		return nil, fmt.Errorf("grouped document counts: %w", err)
	}
	defer rows.Close()
	var out []DocCount
	for rows.Next() {
		var c DocCount
		if err := rows.Scan(&c.Platform, &c.Status, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PlatformSync is the most recent sync_runs row recorded for one platform.
// StartedAt/FinishedAt are the stored RFC3339 strings; Stats is the opaque
// stats JSON blob (an engine.Stats document).
type PlatformSync struct {
	Platform   string
	StartedAt  string
	FinishedAt string
	Stats      string
}

// LatestSyncRunPerPlatform returns the newest sync_runs row (by id) for each
// platform. It backs the "last sync" and degradation sections of `opendoc status`.
func (db *DB) LatestSyncRunPerPlatform() ([]PlatformSync, error) {
	rows, err := db.sql.Query(
		`SELECT platform, started_at, finished_at, stats FROM sync_runs
		 WHERE platform IS NOT NULL AND id IN (
		   SELECT MAX(id) FROM sync_runs WHERE platform IS NOT NULL GROUP BY platform)
		 ORDER BY platform`)
	if err != nil {
		return nil, fmt.Errorf("latest sync run per platform: %w", err)
	}
	defer rows.Close()
	var out []PlatformSync
	for rows.Next() {
		var p PlatformSync
		var platform, started, finished, stats sql.NullString
		if err := rows.Scan(&platform, &started, &finished, &stats); err != nil {
			return nil, err
		}
		p.Platform = platform.String
		p.StartedAt = started.String
		p.FinishedAt = finished.String
		p.Stats = stats.String
		out = append(out, p)
	}
	return out, rows.Err()
}

// StatsSince returns the stats JSON of every sync_runs row for platform whose
// started_at is at or after since (an RFC3339 string). It backs `opendoc doctor`'s
// daily-quota estimate (summing assets_downloaded over the current day's runs).
func (db *DB) StatsSince(platform, since string) ([]string, error) {
	rows, err := db.sql.Query(
		`SELECT stats FROM sync_runs
		 WHERE platform=? AND started_at IS NOT NULL AND started_at >= ? ORDER BY id`,
		platform, since)
	if err != nil {
		return nil, fmt.Errorf("stats since %s for %s: %w", since, platform, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s.String)
	}
	return out, rows.Err()
}

// GetDocumentByLocalPath returns the document whose local_path matches (a path
// relative to the mirror root, forward-slashed). found is false when no row
// matches. It backs `opendoc resolve`'s local-path → online-identity direction.
func (db *DB) GetDocumentByLocalPath(localPath string) (doc Document, found bool, err error) {
	if localPath == "" {
		return Document{}, false, nil
	}
	row := db.sql.QueryRow(
		`SELECT id, platform, type, parent_id, title, local_path, remote_edited, content_hash, synced_at, status
		 FROM documents WHERE local_path = ?`, localPath)
	var parentID, title, lp, remoteEdited, contentHash, syncedAt sql.NullString
	err = row.Scan(&doc.ID, &doc.Platform, &doc.Type, &parentID, &title, &lp,
		&remoteEdited, &contentHash, &syncedAt, &doc.Status)
	if err == sql.ErrNoRows {
		return Document{}, false, nil
	}
	if err != nil {
		return Document{}, false, fmt.Errorf("get document by local_path %s: %w", localPath, err)
	}
	doc.ParentID = parentID.String
	doc.Title = title.String
	doc.LocalPath = lp.String
	doc.RemoteEdited = remoteEdited.String
	doc.ContentHash = contentHash.String
	doc.SyncedAt = syncedAt.String
	return doc, true, nil
}

// DocIDForAlias resolves a doc_aliases alias (e.g. a Feishu wiki node_token or a
// Notion dashless id) to its documents.id, regardless of document status. Unlike
// ResolveTargetDocID it does not filter to active documents, so `opendoc resolve` can
// report a trashed or errored target rather than claim it is unknown. found is
// false when the alias is not recorded.
func (db *DB) DocIDForAlias(alias string) (docID string, found bool, err error) {
	if alias == "" {
		return "", false, nil
	}
	row := db.sql.QueryRow(`SELECT doc_id FROM doc_aliases WHERE alias = ?`, alias)
	err = row.Scan(&docID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("doc id for alias %s: %w", alias, err)
	}
	return docID, true, nil
}

// nullable maps empty strings to SQL NULL so optional columns stay NULL rather
// than storing "".
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
