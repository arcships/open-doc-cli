package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/manifest"
)

// moveRecord captures one document's on-disk relocation this run (rename,
// reparent, or leaf<->dir conversion). oldPath/newPath are mirror-root-relative,
// forward-slashed. isDir marks a directory-shaped node (a README-bearing
// document or a folder) versus a single-file leaf.
type moveRecord struct {
	id      string
	oldPath string
	newPath string
	isDir   bool
}

// jitterKeepFraction is the permission-jitter guard threshold: when a
// platform's enumeration yields fewer than this fraction of its manifest-active
// count, the delete step is aborted so a transient loss of access can never
// sweep the whole mirror into trash.
const jitterKeepFraction = 0.8

// belowJitterThreshold reports whether an enumeration of enumCount documents,
// against activeCount manifest-active documents, has lost more than 20% of the
// inventory — the trigger for aborting the delete step. An empty manifest
// (activeCount == 0, i.e. a first run) never trips the guard.
func belowJitterThreshold(enumCount, activeCount int) bool {
	if activeCount <= 0 {
		return false
	}
	return float64(enumCount) < jitterKeepFraction*float64(activeCount)
}

// reconcileDeletes trashes documents that are active in the manifest but absent
// from this round's full enumeration. The permission-jitter guard
// runs first: if enumeration lost more than 20% of the platform's active
// inventory, no deletes are performed and a loud warning is recorded. Trashed
// bodies are moved under .internal/trash/<date>/<original relative path>, the
// manifest row is kept and flipped to status=trashed (so a later run neither
// resurrects nor re-counts it), and the freed directory is queued for the
// empty-dir sweep.
func (e *Engine) reconcileDeletes(db *manifest.DB, platform string, enumIDs map[string]bool, enumCount int, now time.Time, stats *Stats, moves *[]moveRecord) error {
	active, err := db.ListDocumentsByPlatform(platform, statusActive)
	if err != nil {
		return err
	}
	if belowJitterThreshold(enumCount, len(active)) {
		stats.Warnings = append(stats.Warnings, fmt.Sprintf(
			"permission-jitter guard: %s enumeration returned %d of %d active documents (<%.0f%%); delete step ABORTED, no documents trashed",
			platform, enumCount, len(active), jitterKeepFraction*100))
		return nil
	}

	// Trash deepest paths first so a parent directory is already empty by the time
	// it (or the sweep) reaches it, and a child is never orphaned inside a parent
	// that moved out from under it.
	var gone []manifest.Document
	for _, d := range active {
		if !enumIDs[d.ID] {
			gone = append(gone, d)
		}
	}
	sort.Slice(gone, func(i, j int) bool { return len(gone[i].LocalPath) > len(gone[j].LocalPath) })

	date := now.Format("2006-01-02")
	for _, d := range gone {
		if d.LocalPath == "" {
			// No body on disk (synthetic container); just tombstone it.
			if err := db.SetDocumentTrashed(d.ID, "", now.Format(time.RFC3339)); err != nil {
				return err
			}
			stats.Deleted++
			continue
		}
		trashRel := filepath.ToSlash(filepath.Join(e.layout.TrashRelDir(), date, filepath.FromSlash(d.LocalPath)))
		srcAbs := filepath.Join(e.layout.Root, filepath.FromSlash(d.LocalPath))
		dstAbs := filepath.Join(e.layout.Root, filepath.FromSlash(trashRel))
		if fileExists(srcAbs) || dirExists(srcAbs) {
			if err := os.MkdirAll(filepath.Dir(dstAbs), 0o755); err != nil {
				return fmt.Errorf("mkdir trash dir for %s: %w", d.ID, err)
			}
			if err := os.Rename(srcAbs, dstAbs); err != nil {
				return fmt.Errorf("trash %s: %w", d.LocalPath, err)
			}
		}
		if err := db.SetDocumentTrashed(d.ID, trashRel, now.Format(time.RFC3339)); err != nil {
			return err
		}
		stats.Deleted++
		// Queue the vacated directory for the empty-dir sweep.
		*moves = append(*moves, moveRecord{id: d.ID, oldPath: d.LocalPath, newPath: trashRel, isDir: d.Type == string(adapter.TypeFolder)})
	}
	return nil
}

// fixupMovedLinks rewrites the already-relative internal links affected by this
// run's moves. The phase-2 URL rewrite (rewriteLinks) only touches
// bodies that still carry a platform URL; once a link has been rewritten to a
// relative path, a later move of either endpoint must be followed here. For every
// links-table edge with a moved endpoint it recomputes the old and new relative
// destinations and swaps the former for the latter inside the referrer's body.
func (e *Engine) fixupMovedLinks(db *manifest.DB, platform string, moves []moveRecord, stats *Stats) error {
	if len(moves) == 0 {
		return nil
	}
	oldOf := map[string]string{}
	newOf := map[string]string{}
	movedSet := map[string]bool{}
	for _, m := range moves {
		// A document could appear twice (e.g. moved then trashed); the first move
		// carries the true pre-run path, so keep it.
		if !movedSet[m.id] {
			oldOf[m.id] = m.oldPath
		}
		newOf[m.id] = m.newPath
		movedSet[m.id] = true
	}

	// pathOf returns the pre- and post-run body paths for a document id, and
	// whether it is an active, this-platform document with a body on disk.
	pathOf := func(id string) (oldP, newP string, ok bool) {
		doc, found, err := db.GetDocument(id)
		if err != nil || !found || doc.Status != statusActive || doc.Platform != platform || doc.LocalPath == "" {
			return "", "", false
		}
		if movedSet[id] {
			return oldOf[id], doc.LocalPath, true
		}
		return doc.LocalPath, doc.LocalPath, true
	}

	links, err := db.AllLinks()
	if err != nil {
		return err
	}

	// Group the (oldRel -> newRel) swaps per referrer file so each is read and
	// rewritten once even when it points at several moved targets.
	type swap struct{ oldRel, newRel string }
	perFrom := map[string][]swap{}
	fromNewPath := map[string]string{}

	for _, ln := range links {
		toID, found, err := db.ResolveTargetDocID(ln.To)
		if err != nil {
			return err
		}
		fromMoved := movedSet[ln.From]
		toMoved := found && movedSet[toID]
		if !fromMoved && !toMoved {
			continue
		}
		fOld, fNew, ok := pathOf(ln.From)
		if !ok || !found {
			continue
		}
		tOld, tNew, ok := pathOf(toID)
		if !ok {
			continue
		}
		oldRel := relAsset(fOld, tOld)
		newRel := relAsset(fNew, tNew)
		if oldRel == newRel {
			continue
		}
		perFrom[ln.From] = append(perFrom[ln.From], swap{oldRel, newRel})
		fromNewPath[ln.From] = fNew
	}

	for fromID, swaps := range perFrom {
		abs := filepath.Join(e.layout.Root, filepath.FromSlash(fromNewPath[fromID]))
		if !fileExists(abs) {
			continue
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			stats.Failures = append(stats.Failures, fmt.Sprintf("%s: read for move fixup: %v", fromID, err))
			continue
		}
		head, body := splitAfterFrontmatter(string(raw))
		newBody := body
		applied := 0
		for _, s := range swaps {
			replaced, n := replaceLinkDest(newBody, s.oldRel, s.newRel)
			newBody = replaced
			applied += n
		}
		if applied == 0 || newBody == body {
			continue
		}
		if err := atomicWrite(abs, []byte(head+newBody)); err != nil {
			stats.Failures = append(stats.Failures, fmt.Sprintf("%s: write move fixup: %v", fromID, err))
			continue
		}
		stats.LinksRewritten += applied
	}
	return nil
}

// replaceLinkDest swaps a Markdown link destination oldRel for newRel, matching
// both the bare `](oldRel)` form and the angle-bracket `](<oldRel>)` form used
// for destinations containing spaces or parentheses. It returns the new body and
// the number of destinations replaced.
func replaceLinkDest(body, oldRel, newRel string) (string, int) {
	count := 0
	for _, form := range []struct{ from, to string }{
		{"](" + oldRel + ")", "](" + newRel + ")"},
		{"](<" + oldRel + ">)", "](<" + newRel + ">)"},
	} {
		if c := strings.Count(body, form.from); c > 0 {
			body = strings.ReplaceAll(body, form.from, form.to)
			count += c
		}
	}
	return body, count
}

// pruneEmptyDirs removes directories left empty by moves and trashing, walking up
// from each vacated location and stopping at the first non-empty ancestor (never
// removing the mirror root, the internal state dir, or the asset pool).
func (e *Engine) pruneEmptyDirs(moves []moveRecord) {
	seen := map[string]bool{}
	for _, m := range moves {
		dir := filepath.Dir(filepath.FromSlash(m.oldPath))
		if dir == "." || dir == "" {
			continue
		}
		e.removeEmptyAncestors(filepath.Join(e.layout.Root, dir), seen)
	}
}

// removeEmptyAncestors removes absDir and its ancestors while each is an empty
// directory, halting at a protected or non-empty directory.
func (e *Engine) removeEmptyAncestors(absDir string, seen map[string]bool) {
	protected := map[string]bool{
		e.layout.Root:        true,
		e.layout.Internal:    true,
		e.layout.AssetsDir(): true,
	}
	for {
		if absDir == "" || seen[absDir] || protected[absDir] {
			return
		}
		if !strings.HasPrefix(absDir, e.layout.Root+string(os.PathSeparator)) {
			return
		}
		entries, err := os.ReadDir(absDir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(absDir); err != nil {
			return
		}
		seen[absDir] = true
		absDir = filepath.Dir(absDir)
	}
}

// purgeAgedTrash reclaims trash entries older than trash_keep_days:
// it removes on-disk .internal/trash/<date> directories whose date is older than
// the retention window and purges the matching manifest tombstones. It returns
// the number of dated trash directories removed.
func (e *Engine) purgeAgedTrash(db *manifest.DB, now time.Time) (int, error) {
	keepDays := e.config.Sync.TrashKeepDays
	if keepDays <= 0 {
		keepDays = 30
	}
	cutoff := now.AddDate(0, 0, -keepDays)

	// Purge manifest tombstones trashed before the cutoff.
	if _, err := db.PurgeTrashedBefore(cutoff.UTC().Format(time.RFC3339)); err != nil {
		return 0, err
	}

	trashDir := e.layout.TrashDir()
	entries, err := os.ReadDir(trashDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read trash dir: %w", err)
	}
	removed := 0
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		day, perr := time.Parse("2006-01-02", ent.Name())
		if perr != nil {
			continue // not a dated bucket; leave it alone
		}
		// A bucket dated strictly before the cutoff day is past the window.
		if day.Before(cutoff.Truncate(24 * time.Hour)) {
			if err := os.RemoveAll(filepath.Join(trashDir, ent.Name())); err != nil {
				return removed, fmt.Errorf("purge trash %s: %w", ent.Name(), err)
			}
			removed++
		}
	}
	return removed, nil
}

// dirExists reports whether path is an existing directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
