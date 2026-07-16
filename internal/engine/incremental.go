package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/manifest"
	"github.com/arcships/open-doc-cli/internal/naming"
)

// runIncremental is the incremental round: it enumerates only the
// documents changed since the checkpoint, resolves their on-disk placement from
// the manifest (unenumerated ancestors), fetches and writes them, and finalizes
// links — but performs NO delete/move reconciliation, because an incremental
// round's inventory is not the full set (absence carries no signal). It returns
// the advanced checkpoint (max last_edited_time this round, from the adapter).
//
// Deliberate scope limits, all caught by the next reconciliation round:
//   - Directory subtrees (containers, and pages that already own a README/children)
//     are never reshaped here: a rename/reparent of a directory is deferred so an
//     unenumerated descendant is never orphaned by a half-applied move.
//   - A database's `_index.md` row index is not regenerated (it needs the full row
//     set); only changed row FILES are refreshed, with their properties.
//   - A page whose parent moved WITHOUT an edit is missed (it is not in the result
//     set) — exactly the case the reconciliation round exists to correct.
func (e *Engine) runIncremental(ctx context.Context, a adapter.Adapter, db *manifest.DB, prevCheckpoint string, stats Stats) (Stats, string, error) {
	inc, ok := a.(adapter.IncrementalEnumerator)
	if !ok {
		// Defensive: the mode decision only picks incremental for capable adapters.
		return e.runFull(ctx, a, db, stats)
	}
	docs, newCheckpoint, err := inc.EnumerateIncremental(ctx, prevCheckpoint)
	if err != nil {
		return stats, prevCheckpoint, fmt.Errorf("%s: incremental enumerate: %w", a.Platform(), err)
	}

	now := time.Now().UTC()
	rc := &runContext{
		rowProps:    map[string]adapter.RowProperties{},
		titles:      make(map[string]string, len(docs)),
		queried:     map[string]bool{},
		skipDBIndex: true, // an incremental round leaves _index.md for the reconcile round
	}
	for _, d := range docs {
		rc.titles[d.ID] = d.Title
	}
	if ex, ok := a.(adapter.DatabaseExpander); ok {
		rc.expander = ex
	}

	nodes, err := e.resolveIncrementalNodes(db, a.Platform(), docs)
	if err != nil {
		return stats, prevCheckpoint, fmt.Errorf("%s: resolve incremental placement: %w", a.Platform(), err)
	}

	// Pre-query only the databases whose container or rows appear dirty this round
	// (never all N), so a changed row keeps its properties regardless of node
	// order and even when its database node is not enumerated.
	e.prequeryDirtyDatabases(ctx, db, rc, docs, &stats)

	var moves []moveRecord
	for _, n := range nodes {
		if err := ctx.Err(); err != nil {
			return stats, prevCheckpoint, err
		}
		if err := e.processNode(ctx, a, db, n, rc, now, &stats, &moves); err != nil {
			stats.Failures = append(stats.Failures, fmt.Sprintf("%s: %v", n.doc.ID, err))
		}
	}

	if err := e.finalizeLinks(db, a.Platform(), moves, &stats); err != nil {
		return stats, prevCheckpoint, err
	}
	return stats, newCheckpoint, nil
}

// prequeryDirtyDatabases issues one data-source query per database that is dirty
// this round (an enumerated database container, or the parent of an enumerated
// row) and folds the results into rc.rowProps / rc.queried, so processNode never
// re-queries and every changed row renders its properties.
func (e *Engine) prequeryDirtyDatabases(ctx context.Context, db *manifest.DB, rc *runContext, docs []adapter.RemoteDoc, stats *Stats) {
	if rc.expander == nil {
		return
	}
	enumeratedDB := map[string]bool{}
	for _, d := range docs {
		if d.Type == adapter.TypeDB {
			enumeratedDB[d.ID] = true
		}
	}
	dsIDs := map[string]bool{}
	for _, d := range docs {
		switch d.Type {
		case adapter.TypeDB:
			dsIDs[d.ID] = true
		case adapter.TypeDBRow:
			if d.ParentID == "" || enumeratedDB[d.ParentID] {
				if d.ParentID != "" {
					dsIDs[d.ParentID] = true
				}
				continue
			}
			// Only pre-query a row's parent when it is a known, mirrored database. An
			// orphan row's parent data source is not shared, so querying it
			// would 404 every incremental round; skip it and let the row keep its
			// prior properties (the reconcile round re-derives placement).
			pd, found, gerr := db.GetDocument(d.ParentID)
			if gerr == nil && found && pd.Type == string(adapter.TypeDB) && pd.Status == statusActive {
				dsIDs[d.ParentID] = true
			}
		}
	}
	for id := range dsIDs {
		if rc.queried[id] {
			continue
		}
		props, err := rc.expander.QueryDatabaseRows(ctx, adapter.RemoteDoc{ID: id, Type: adapter.TypeDB}, rc.titles)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			stats.Failures = append(stats.Failures, fmt.Sprintf("%s: pre-query database rows: %v", id, err))
			continue
		}
		rc.queried[id] = true
		for rid, p := range props {
			rc.rowProps[rid] = p
		}
	}
}

// resolveIncrementalNodes computes on-disk placement for an incremental round's
// changed docs using manifest state for unenumerated ancestors — never the
// in-round tree. New siblings dedupe against the parent's
// existing manifest children; a parent that gains its first child this round is
// converted from a leaf page into a directory (README); existing directory
// subtrees are reused verbatim (no reshape) so descendants are never orphaned.
func (e *Engine) resolveIncrementalNodes(db *manifest.DB, platform string, docs []adapter.RemoteDoc) ([]*treeNode, error) {
	// Placement chains and sibling dedup consider every live document — both
	// active and pending_assets — so a parent that is merely awaiting an asset
	// retry still resolves as an ancestor (its child is not misrouted to _orphans)
	// and its on-disk slot still blocks a duplicate name.
	active, err := db.ListDocumentsByPlatform(platform, statusActive)
	if err != nil {
		return nil, err
	}
	pending, err := db.ListDocumentsByPlatform(platform, statusPendingAssets)
	if err != nil {
		return nil, err
	}
	active = append(active, pending...)
	activeByID := make(map[string]manifest.Document, len(active))
	for _, d := range active {
		activeByID[d.ID] = d
	}

	enumSet := make(map[string]bool, len(docs))
	gainsChild := map[string]bool{}
	for _, d := range docs {
		enumSet[d.ID] = true
		if d.ParentID != "" {
			gainsChild[d.ParentID] = true
		}
	}

	// claimed tracks names taken in each directory as this round's docs are placed,
	// so two new siblings never collide on the clean name.
	claimed := map[string]map[string]bool{}
	convertsByID := map[string]*treeNode{}

	var nodes []*treeNode
	for _, d := range docs {
		childDir, crumb, convert, ok := e.childContext(activeByID, platform, d.ParentID, enumSet)
		if !ok {
			// Parent chain broken (unmirrored/trashed ancestor): route to _orphans,
			// matching buildTree's fallback.
			childDir = filepath.ToSlash(filepath.Join(platform, orphansFolderName))
			crumb = ""
			convert = nil
		}
		if convert != nil {
			if _, seen := convertsByID[convert.doc.ID]; !seen {
				convertsByID[convert.doc.ID] = convert
			}
		}
		node, err := e.placeIncrementalNode(db, active, activeByID, platform, d, childDir, crumb, gainsChild, claimed)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}

	// Append conversion nodes for unenumerated leaf parents (an enumerated parent
	// converts through its own node).
	for id, cn := range convertsByID {
		if !enumSet[id] {
			nodes = append(nodes, cn)
		}
	}

	// Shallow paths first, so a parent directory or conversion is materialised
	// before its deeper descendants (defensive; processNode also mkdirs on demand).
	sort.SliceStable(nodes, func(i, j int) bool {
		return nodePathDepth(nodes[i]) < nodePathDepth(nodes[j])
	})
	return nodes, nil
}

// childContext returns the directory that children of parentID live in, the
// breadcrumb those children carry (the titles of parentID and all its ancestors),
// and — when the immediate parent is an unenumerated leaf page gaining its first
// child — a conversion node that moves that parent's body into its new directory
// (README). ok is false when the chain hits an unmirrored/trashed ancestor, so
// the caller routes the doc to _orphans.
func (e *Engine) childContext(activeByID map[string]manifest.Document, platform, parentID string, enumSet map[string]bool) (childDir, childCrumb string, convert *treeNode, ok bool) {
	if parentID == "" {
		return platform, "", nil, true
	}
	p, found := activeByID[parentID]
	if !found {
		return "", "", nil, false
	}
	_, pCrumb, _, ok := e.childContext(activeByID, platform, p.ParentID, enumSet)
	if !ok {
		return "", "", nil, false
	}
	childCrumb = joinBreadcrumb(pCrumb, p.Title)

	lp := filepath.ToSlash(p.LocalPath)
	switch {
	case isContainerType(adapter.DocType(p.Type)):
		childDir = lp // folder / database directory
	case strings.HasSuffix(lp, "/README.md"):
		childDir = strings.TrimSuffix(lp, "/README.md")
	case lp == "README.md":
		childDir = ""
	case strings.HasSuffix(lp, ".md"):
		// Leaf page gaining its first child: children live in its implied directory.
		childDir = strings.TrimSuffix(lp, ".md")
		if !enumSet[p.ID] {
			convert = leafToDirNode(p, childDir, pCrumb)
		}
	default:
		childDir = lp
	}
	return childDir, childCrumb, convert, true
}

// placeIncrementalNode computes the treeNode (with dirPath/bodyPath/breadcrumb)
// for one changed doc. An existing directory subtree is reused verbatim (no
// reshape). Otherwise a fresh path is computed: the name is deduped against the
// target directory's existing manifest children, excluding the doc's own subtree
// so an unchanged title reproduces the same name and a rename frees the old one.
func (e *Engine) placeIncrementalNode(db *manifest.DB, active []manifest.Document, activeByID map[string]manifest.Document, platform string, d adapter.RemoteDoc, childDir, crumb string, gainsChild map[string]bool, claimed map[string]map[string]bool) (*treeNode, error) {
	existing, found, err := db.GetDocument(d.ID)
	if err != nil {
		return nil, err
	}

	isDir := isContainerType(d.Type)
	hasBody := !isContainerType(d.Type)

	existingIsDir := found && existing.LocalPath != "" &&
		(isContainerType(adapter.DocType(existing.Type)) || strings.HasSuffix(filepath.ToSlash(existing.LocalPath), "/README.md"))

	if !isDir {
		// A page renders as a directory (README) when it owns a subtree: an existing
		// directory form, existing manifest children, or a child arriving this round.
		switch {
		case existingIsDir, gainsChild[d.ID]:
			isDir = true
		default:
			cnt, err := db.ChildrenCount(platform, d.ID)
			if err != nil {
				return nil, err
			}
			if cnt > 0 {
				isDir = true
			}
		}
	}

	node := &treeNode{doc: d, isDir: isDir, hasBody: hasBody, breadcrumb: crumb}

	// Reuse an existing directory subtree verbatim: incremental never reshapes a
	// directory (rename/reparent of a subtree is deferred to the reconcile round).
	if existingIsDir {
		dir := containerDirOf(existing)
		node.dirPath = dir
		if hasBody {
			node.bodyPath = filepath.Join(dir, "README.md")
		}
		return node, nil
	}

	// Compute a fresh name, deduping against the target directory's existing
	// occupants (excluding this doc's own subtree) plus this round's claims.
	excludeDir := ""
	if found && existing.LocalPath != "" {
		elp := filepath.ToSlash(existing.LocalPath)
		if isContainerType(adapter.DocType(existing.Type)) {
			excludeDir = elp
		} else if strings.HasSuffix(elp, "/README.md") {
			excludeDir = strings.TrimSuffix(elp, "/README.md")
		}
	}
	used := siblingUsed(active, childDir, d.ID, excludeDir)
	for k := range claimed[childDir] {
		used[k] = true
	}
	name := naming.Component(d.Title, d.ID, used)
	if claimed[childDir] == nil {
		claimed[childDir] = map[string]bool{}
	}
	claimed[childDir][strings.ToLower(name)] = true

	osDir := filepath.FromSlash(childDir)
	if isDir {
		node.dirPath = filepath.Join(osDir, name)
		if hasBody {
			node.bodyPath = filepath.Join(node.dirPath, "README.md")
		}
	} else {
		node.bodyPath = filepath.Join(osDir, name+".md")
	}
	return node, nil
}

// leafToDirNode synthesizes the conversion node that moves an unenumerated leaf
// parent's body from <dir>.md into <dir>/README.md when it gains its first child
// this round. It carries the parent's manifest metadata (unchanged remote_edited)
// so processNode moves the file and skips the refetch.
func leafToDirNode(p manifest.Document, dir, crumb string) *treeNode {
	osDir := filepath.FromSlash(dir)
	return &treeNode{
		doc: adapter.RemoteDoc{
			ID:       p.ID,
			Type:     adapter.DocType(p.Type),
			ParentID: p.ParentID,
			Title:    p.Title,
			EditedAt: p.RemoteEdited,
		},
		isDir:      true,
		hasBody:    true,
		dirPath:    osDir,
		bodyPath:   filepath.Join(osDir, "README.md"),
		breadcrumb: crumb,
	}
}

// containerDirOf returns the directory a container/README document occupies.
func containerDirOf(d manifest.Document) string {
	lp := filepath.ToSlash(d.LocalPath)
	if strings.HasSuffix(lp, "/README.md") {
		return filepath.FromSlash(strings.TrimSuffix(lp, "/README.md"))
	}
	return filepath.FromSlash(lp)
}

// siblingUsed builds the casefolded set of base names (extension-stripped) already
// occupying childDir among active documents, excluding the doc itself (selfID) and
// its own subtree (excludeDir). Each direct child contributes its first path
// segment under childDir — the slot the naming dedup competes for.
func siblingUsed(active []manifest.Document, childDir, selfID, excludeDir string) map[string]bool {
	used := map[string]bool{}
	prefix := childDir + "/"
	for _, d := range active {
		if d.ID == selfID || d.LocalPath == "" {
			continue
		}
		lp := filepath.ToSlash(d.LocalPath)
		if !strings.HasPrefix(lp, prefix) {
			continue
		}
		if excludeDir != "" && (lp == excludeDir || strings.HasPrefix(lp, excludeDir+"/")) {
			continue
		}
		rest := lp[len(prefix):]
		seg := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			seg = rest[:i]
		}
		seg = strings.TrimSuffix(seg, ".md")
		if seg != "" {
			used[strings.ToLower(seg)] = true
		}
	}
	return used
}

// nodePathDepth is the slash depth of a node's on-disk path, for shallow-first
// ordering of the incremental node list.
func nodePathDepth(n *treeNode) int {
	p := n.bodyPath
	if p == "" {
		p = n.dirPath
	}
	return strings.Count(filepath.ToSlash(p), "/")
}

// maxCheckpoint returns the max last_edited_time (RFC3339 UTC) across docs, never
// below floor. It is the checkpoint a round persists so the next incremental round
// resumes from it. An empty result (no dated docs, no floor)
// yields floor unchanged.
func maxCheckpoint(docs []adapter.RemoteDoc, floor string) string {
	var max time.Time
	set := false
	if floor != "" {
		if t, err := time.Parse(time.RFC3339, floor); err == nil {
			max = t.UTC()
			set = true
		}
	}
	for _, d := range docs {
		if d.EditedAt == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, d.EditedAt)
		if err != nil {
			continue
		}
		if t = t.UTC(); !set || t.After(max) {
			max = t
			set = true
		}
	}
	if !set {
		return floor
	}
	return max.Format(time.RFC3339)
}
