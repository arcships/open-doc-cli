package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/frontmatter"
	"github.com/arcships/open-doc-cli/internal/manifest"
)

// isBodyFetchType reports whether a node's body is fetched from the platform (a
// real document) versus a container/placeholder. Notion database rows (TypeDBRow)
// are real pages whose body comes from the markdown endpoint, so they fetch too;
// the database node itself (TypeDB) is a directory container with a generated
// row index, not a fetched body.
func isBodyFetchType(t adapter.DocType) bool {
	return t == adapter.TypeDocx || t == adapter.TypePage || t == adapter.TypeDBRow
}

// runContext carries per-run, per-platform state that node processing needs
// beyond the manifest: the queried database-row properties (populated when a db
// node is processed, consumed by its rows and its generated row index) and the
// id->title map used to resolve relation property values.
type runContext struct {
	// rowProps maps a db_row's manifest id to its rendered properties.
	rowProps map[string]adapter.RowProperties
	// titles maps every enumerated document id to its title (relation resolution).
	titles map[string]string
	// expander is the adapter's database-expansion capability, or nil when it has
	// no databases (e.g. Feishu).
	expander adapter.DatabaseExpander
	// queried records which data-source ids have already had their rows queried
	// this run, so a database node processed after an incremental pre-query does not
	// re-issue the same query. Nil is treated as empty.
	queried map[string]bool
	// skipDBIndex suppresses `_index.md` regeneration. An incremental round sets it
	// because it lacks a database's full row set (only changed rows are enumerated);
	// the reconciliation round rebuilds the index from the complete inventory.
	skipDBIndex bool
}

// runAdapter executes the pipeline for one platform adapter against db in the
// given mode, returning the per-platform Stats and the checkpoint to persist. A
// per-document failure is recorded and does not abort the run; only a
// fatal enumeration error aborts. The checkpoint is the platform's incremental
// high-water mark (max last_edited_time), or "" for a platform without
// incremental enumeration.
func (e *Engine) runAdapter(ctx context.Context, a adapter.Adapter, db *manifest.DB, mode, prevCheckpoint string) (Stats, string, error) {
	stats := Stats{Failures: []string{}, Warnings: []string{}, Adapters: []string{a.Platform()}, Mode: mode}
	if mode == modeIncremental {
		return e.runIncremental(ctx, a, db, prevCheckpoint, stats)
	}
	return e.runFull(ctx, a, db, stats)
}

// runFull is the reconciliation round: a complete enumeration, full tree rebuild,
// delete/move detection, and link finalize. For an
// incremental-capable platform it also returns the round's checkpoint (max
// last_edited_time of the inventory) so the next round can go incremental.
func (e *Engine) runFull(ctx context.Context, a adapter.Adapter, db *manifest.DB, stats Stats) (Stats, string, error) {
	docs, err := collectEnumeration(ctx, a)
	if err != nil {
		return stats, "", fmt.Errorf("%s: enumerate: %w", a.Platform(), err)
	}

	roots := buildTree(a.Platform(), docs)
	now := time.Now().UTC()
	allNodes := walk(roots)

	// Per-run context: the id->title map (for relation property rendering) and a
	// bucket the db nodes fill with their rows' queried properties, consumed by
	// the rows and their generated `_index.md`.
	rc := &runContext{
		rowProps: map[string]adapter.RowProperties{},
		titles:   make(map[string]string, len(docs)),
		queried:  map[string]bool{},
	}
	for _, d := range docs {
		rc.titles[d.ID] = d.Title
	}
	if ex, ok := a.(adapter.DatabaseExpander); ok {
		rc.expander = ex
	}

	// moves records every document whose on-disk location changed this run
	// (rename, reparent, or leaf<->dir conversion). The referrer link fixup
	// consumes it after the walk.
	var moves []moveRecord
	for _, n := range allNodes {
		if err := ctx.Err(); err != nil {
			return stats, "", err
		}
		if err := e.processNode(ctx, a, db, n, rc, now, &stats, &moves); err != nil {
			stats.Failures = append(stats.Failures, fmt.Sprintf("%s: %v", n.doc.ID, err))
		}
	}

	// Delete detection: a full round enumerates the complete inventory, so a
	// manifest-active document absent from this round is genuinely gone — subject
	// to the permission-jitter guard. The present set is taken from
	// the built tree (not the raw enumeration) so engine-synthesized folders such as
	// "_orphans" are never mistaken for deletions. This runs ONLY in a full round:
	// an incremental round's absence carries no delete signal.
	enumIDs := make(map[string]bool, len(allNodes))
	for _, n := range allNodes {
		enumIDs[n.doc.ID] = true
	}
	if err := e.reconcileDeletes(db, a.Platform(), enumIDs, len(enumIDs), now, &stats, &moves); err != nil {
		return stats, "", err
	}

	if err := e.finalizeLinks(db, a.Platform(), moves, &stats); err != nil {
		return stats, "", err
	}
	return stats, e.checkpointFor(a, docs, ""), nil
}

// finalizeLinks runs the per-platform link fixups shared by both modes: the moved-
// document referrer fixup, the phase-2 platform-URL → relative-path
// rewrite across the whole links table, and the empty-directory
// sweep. INDEX.md is regenerated once, later, across all platforms (Engine.Sync).
func (e *Engine) finalizeLinks(db *manifest.DB, platform string, moves []moveRecord, stats *Stats) error {
	if err := e.fixupMovedLinks(db, platform, moves, stats); err != nil {
		return err
	}
	if err := e.rewriteLinks(db, platform, stats); err != nil {
		return err
	}
	e.pruneEmptyDirs(moves)
	return nil
}

// checkpointFor returns the checkpoint to persist for a platform after a round:
// for an incremental-capable adapter it is the max last_edited_time across docs,
// never below floor; for any other platform it is "" (no checkpoint semantics).
func (e *Engine) checkpointFor(a adapter.Adapter, docs []adapter.RemoteDoc, floor string) string {
	if _, ok := a.(adapter.IncrementalEnumerator); !ok {
		return ""
	}
	return maxCheckpoint(docs, floor)
}

// collectEnumeration drains an adapter's streaming enumeration into a slice,
// surfacing any terminal error.
func collectEnumeration(ctx context.Context, a adapter.Adapter) ([]adapter.RemoteDoc, error) {
	docCh, errCh := a.Enumerate(ctx)
	var docs []adapter.RemoteDoc
	for docCh != nil || errCh != nil {
		select {
		case d, ok := <-docCh:
			if !ok {
				docCh = nil
				continue
			}
			docs = append(docs, d)
		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
			if err != nil {
				return nil, err
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return docs, nil
}

// processNode materialises a single tree node: it ensures directories, follows
// moves/renames, and for body-bearing nodes runs diff -> fetch -> atomic write
// -> manifest upsert.
func (e *Engine) processNode(ctx context.Context, a adapter.Adapter, db *manifest.DB, n *treeNode, rc *runContext, now time.Time, stats *Stats, moves *[]moveRecord) error {
	// Record the node's alias (e.g. wiki node_token -> obj_token) unconditionally,
	// before any skip, so link rewriting can resolve /wiki/ URLs even when the
	// target document itself is unchanged and skipped this run.
	if n.doc.AltID != "" {
		if err := db.UpsertAlias(n.doc.AltID, n.doc.ID); err != nil {
			return err
		}
	}

	// Ensure the node's directory (and, for leaves, the parent directory).
	if n.isDir {
		if err := os.MkdirAll(filepath.Join(e.layout.Root, n.dirPath), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", n.dirPath, err)
		}
	}

	if !n.hasBody {
		// Pure container (folder / synthetic root / database): record it so the
		// tree is fully reconciled, but there is no page body to fetch or write. A
		// container whose computed directory moved is recorded so its now-empty old
		// location is swept (children move themselves individually).
		existing, found, err := db.GetDocument(n.doc.ID)
		if err != nil {
			return err
		}
		if found && existing.LocalPath != "" && existing.LocalPath != n.dirPath && existing.Status != statusTrashed {
			// A page that became a database (leaf/README .md -> directory container)
			// leaves a stale body file at the old path; delete it so it does not
			// linger inside (or beside) the new directory.
			e.removeStaleBody(existing.LocalPath)
			*moves = append(*moves, moveRecord{id: n.doc.ID, oldPath: existing.LocalPath, newPath: n.dirPath, isDir: true})
			stats.Moved++
		}
		if n.doc.Type == adapter.TypeDB {
			if err := e.processDatabase(ctx, n, rc, stats); err != nil {
				return err
			}
		}
		return e.upsertFolder(db, a.Platform(), n, now)
	}

	if err := os.MkdirAll(filepath.Join(e.layout.Root, filepath.Dir(n.bodyPath)), 0o755); err != nil {
		return fmt.Errorf("mkdir parent of %s: %w", n.bodyPath, err)
	}

	existing, found, err := db.GetDocument(n.doc.ID)
	if err != nil {
		return err
	}
	absPath := filepath.Join(e.layout.Root, n.bodyPath)

	// Move/rename/leaf<->dir follow: the document is the same (same id) but its
	// computed on-disk path changed. Move the body file to the new location so an
	// unchanged document is not needlessly refetched, and record the move for the
	// referrer link fixup. Directory subtrees are not moved
	// wholesale — every descendant node relocates its own body file the same way,
	// so a plain rename of the single body file is always correct here.
	moved := false
	if found && existing.Status != statusTrashed && existing.LocalPath != "" && existing.LocalPath != n.bodyPath {
		oldAbs := filepath.Join(e.layout.Root, filepath.FromSlash(existing.LocalPath))
		if fileExists(oldAbs) {
			if err := os.Rename(oldAbs, absPath); err != nil {
				return fmt.Errorf("move %s -> %s: %w", existing.LocalPath, n.bodyPath, err)
			}
		}
		*moves = append(*moves, moveRecord{id: n.doc.ID, oldPath: existing.LocalPath, newPath: n.bodyPath, isDir: n.isDir})
		stats.Moved++
		moved = true
	}

	// A document carrying pending assets from a prior run must be reprocessed so
	// the download is retried and its links fixed up, even when its content did
	// not change (the pending-asset retry).
	hadPending := found && existing.Status == statusPendingAssets

	// Pre-fetch skip: unchanged remote edit time, file present (at its possibly
	// new location), and no pending assets to retry. Avoids the fetch entirely
	// (diff on remote_edited). --full bypasses it. When the document
	// only moved, the skip still refreshes the manifest row so local_path follows.
	if !e.full && found && existing.Status == statusActive && !hadPending && n.doc.EditedAt != "" &&
		existing.RemoteEdited == n.doc.EditedAt && fileExists(absPath) {
		stats.Skipped++
		if moved {
			return e.upsertDoc(db, a.Platform(), n, existing.ContentHash, now, statusActive)
		}
		return nil
	}

	res, err := e.fetchBody(ctx, a, n)
	if err != nil {
		return err
	}
	// Database rows carry properties fetched separately (via the db node's query,
	// already in rc.rowProps). They fold into both the frontmatter and the
	// content_hash so a property-only edit is detected as dirty even when the body
	// is unchanged. The canonical form is pre-rewrite, keeping the
	// hash stable across the internal-link finalize.
	var rowProps []frontmatter.Property
	hashInput := res.Markdown
	if n.doc.Type == adapter.TypeDBRow {
		if rp, ok := rc.rowProps[n.doc.ID]; ok {
			hashInput = res.Markdown + "\x00opendoc:properties\x00" + rp.Canonical
			rowProps = toFrontmatterProps(rp.Entries)
		}
	}
	// content_hash is taken over the raw fetch product (pre-degradation,
	// pre-rewrite) so incremental skip stays stable.
	hash := bodyHash(hashInput)
	body := res.Body
	if body == "" {
		body = res.Markdown
	}

	// Register extracted assets (pending until downloaded) and links.
	for _, as := range res.Assets {
		if as.RemoteKey != "" {
			if err := db.UpsertAsset(as.RemoteKey, statusAssetPending); err != nil {
				return err
			}
		}
	}
	for _, ln := range res.Links {
		if ln.TargetID != "" {
			if err := db.UpsertLink(n.doc.ID, ln.TargetID); err != nil {
				return err
			}
		}
	}
	stats.addDegradation(res.Degradation)

	// Download assets and rewrite body image links to local relative paths.
	outcome, err := e.processAssets(ctx, a, db, n.bodyPath, body, res.Assets)
	if err != nil {
		return err
	}
	stats.AssetsDownloaded += outcome.downloaded
	stats.AssetsPending += outcome.pending
	body = outcome.body

	docStatus := statusActive
	if outcome.pending > 0 {
		docStatus = statusPendingAssets
	}

	// Content-hash skip: raw body unchanged, file present (at its possibly new
	// location), and no asset activity this run (nothing downloaded, nothing
	// pending, none carried over) — leave the file as-is and refresh bookkeeping.
	// --full bypasses it so every body is rewritten.
	if !e.full && found && existing.ContentHash == hash && fileExists(absPath) &&
		outcome.downloaded == 0 && outcome.pending == 0 && !hadPending {
		stats.Skipped++
		return e.upsertDoc(db, a.Platform(), n, hash, now, docStatus)
	}

	fm := frontmatter.Doc{
		ID:         n.doc.ID,
		Source:     a.Platform(),
		Type:       string(n.doc.Type),
		URL:        n.doc.URL,
		Title:      n.doc.Title,
		Breadcrumb: n.breadcrumb,
		Updated:    parseEdited(n.doc.EditedAt),
		Synced:     now,
		Properties: rowProps,
	}
	if err := atomicWrite(absPath, []byte(assemble(fm, body))); err != nil {
		return err
	}
	if err := e.upsertDoc(db, a.Platform(), n, hash, now, docStatus); err != nil {
		return err
	}
	if found {
		stats.Updated++
	} else {
		stats.Added++
	}
	return nil
}

// fetchBody returns the fetch result for a node: the platform markdown (with its
// degraded body, assets, links, degradation counts) for a real document, or a
// synthesized placeholder result for an independent resource node.
func (e *Engine) fetchBody(ctx context.Context, a adapter.Adapter, n *treeNode) (adapter.FetchResult, error) {
	if isBodyFetchType(n.doc.Type) {
		return a.FetchMarkdown(ctx, n.doc)
	}
	pb := placeholderBody(n.doc)
	return adapter.FetchResult{Markdown: pb, Body: pb}, nil
}

// upsertDoc writes the manifest row for a body-bearing node with the given
// status ("active" or "pending_assets").
func (e *Engine) upsertDoc(db *manifest.DB, platform string, n *treeNode, hash string, now time.Time, status string) error {
	return db.UpsertDocument(manifest.Document{
		ID:           n.doc.ID,
		Platform:     platform,
		Type:         string(n.doc.Type),
		ParentID:     n.doc.ParentID,
		Title:        n.doc.Title,
		LocalPath:    n.bodyPath,
		RemoteEdited: n.doc.EditedAt,
		ContentHash:  hash,
		SyncedAt:     now.Format(time.RFC3339),
		Status:       status,
	})
}

// upsertFolder writes the manifest row for a container node (folder / synthetic
// root / database), whose local_path is its directory and which has no content
// hash. The stored type preserves the node's real DocType — "db" for a database
// container, "folder" otherwise — so INDEX/lifecycle can tell a database
// directory (which links to its generated `_index.md`) from a plain folder.
func (e *Engine) upsertFolder(db *manifest.DB, platform string, n *treeNode, now time.Time) error {
	docType := n.doc.Type
	if docType == "" {
		docType = adapter.TypeFolder
	}
	return db.UpsertDocument(manifest.Document{
		ID:           n.doc.ID,
		Platform:     platform,
		Type:         string(docType),
		ParentID:     n.doc.ParentID,
		Title:        n.doc.Title,
		LocalPath:    n.dirPath,
		RemoteEdited: n.doc.EditedAt,
		SyncedAt:     now.Format(time.RFC3339),
		Status:       statusActive,
	})
}

// processDatabase handles a database container node: it queries every row's
// properties once (feeding the rows' frontmatter/content_hash via rc.rowProps)
// and regenerates the machine-generated `_index.md` row index. A query failure is
// recorded per-document and does not abort the run; the row index is still
// regenerated (from whatever placement/properties are available) so the tree
// keeps its index file.
func (e *Engine) processDatabase(ctx context.Context, n *treeNode, rc *runContext, stats *Stats) error {
	// Skip the query when this data source's rows were already fetched this run
	// (an incremental pre-query); the row index is still regenerated below.
	if rc.expander != nil && !rc.queried[n.doc.ID] {
		props, err := rc.expander.QueryDatabaseRows(ctx, n.doc, rc.titles)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			stats.Failures = append(stats.Failures, fmt.Sprintf("%s: query database rows: %v", n.doc.ID, err))
		} else {
			if rc.queried == nil {
				rc.queried = map[string]bool{}
			}
			rc.queried[n.doc.ID] = true
			for id, p := range props {
				rc.rowProps[id] = p
			}
		}
	}
	if rc.skipDBIndex {
		return nil
	}
	return e.generateDBIndex(n, rc)
}

// removeStaleBody deletes the body file a node left behind when it changed shape
// from a page (leaf/README) into a directory container. localPath is the old
// mirror-root-relative path; a container owns no body file, so any regular file
// still sitting there is stale and safe to remove.
func (e *Engine) removeStaleBody(localPath string) {
	abs := filepath.Join(e.layout.Root, filepath.FromSlash(localPath))
	if fileExists(abs) {
		_ = os.Remove(abs)
	}
}

// toFrontmatterProps converts rendered adapter properties into frontmatter
// properties, preserving order (renderRowProperties already sorts them).
func toFrontmatterProps(entries []adapter.PropertyKV) []frontmatter.Property {
	if len(entries) == 0 {
		return nil
	}
	out := make([]frontmatter.Property, len(entries))
	for i, e := range entries {
		out[i] = frontmatter.Property{Key: e.Key, Value: e.Value}
	}
	return out
}
