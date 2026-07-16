package notion_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/engine"
	"github.com/arcships/open-doc-cli/internal/layout"
	"github.com/arcships/open-doc-cli/internal/manifest"
	"github.com/arcships/open-doc-cli/internal/notion"
)

// editedObj is obj() with an explicit last_edited_time, so a round can move a
// single page's timestamp across the checkpoint.
func editedObj(kind, id, title, parentType, parentID, edited string) string {
	dl := strings.ReplaceAll(id, "-", "")
	var parent string
	switch parentType {
	case "workspace":
		parent = `{"type":"workspace","workspace":true}`
	case "page_id":
		parent = `{"type":"page_id","page_id":"` + parentID + `"}`
	}
	return `{"object":"` + kind + `","id":"` + id + `","url":"https://app.notion.com/p/` + dl +
		`","last_edited_time":"` + edited + `","parent":` + parent +
		`,"properties":{"title":{"type":"title","title":[{"plain_text":"` + title + `"}]}}}`
}

func syncNotion(t *testing.T, l layout.Layout, m *mockNotion, full bool) engine.Result {
	t.Helper()
	eng, err := engine.New(l, engine.Options{Adapters: []adapter.Adapter{notion.NewAdapter("tok", m)}, Full: full})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Close()
	res, err := eng.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return res
}

func notionMode(t *testing.T, res engine.Result) string {
	t.Helper()
	for _, p := range res.Platforms {
		if p.Platform == "notion" {
			return p.Stats.Mode
		}
	}
	t.Fatalf("no notion platform run in result")
	return ""
}

func latestCheckpoint(t *testing.T, l layout.Layout) string {
	t.Helper()
	db, err := manifest.Open(l.ManifestPath())
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer db.Close()
	ck, err := db.LatestCheckpoint("notion")
	if err != nil {
		t.Fatalf("latest checkpoint: %v", err)
	}
	return ck
}

// TestNotionIncrementalLifecycle drives the full incremental lifecycle with a fake
// transport (no network): a full first round persisting a checkpoint; an
// immediate incremental re-run that is clean; an online add picked up
// incrementally; an incremental rename; an archive missed by incremental; and a
// forced reconciliation round that finally trashes the archived page.
func TestNotionIncrementalLifecycle(t *testing.T) {
	root := "decafbad-0000-4000-8000-00000000000b"
	s1 := "facefeed-0000-4000-8000-00000000000a"
	newID := "aaaaaaaa-0000-0000-0000-000000000001"

	// Base inventory: School (root) with several children, all edited at 09:00 →
	// the first full round's checkpoint is 09:00:00Z. The extra filler pages keep
	// the active count high enough that trashing one page later stays under the 20%
	// permission-jitter guard threshold.
	base := []string{
		editedObj("page", root, "School", "workspace", "", "2026-07-14T09:00:00.000Z"),
		editedObj("page", s1, "Term 1", "page_id", root, "2026-07-14T09:00:00.000Z"),
	}
	fillerIDs := []string{
		"11110000-0000-0000-0000-000000000001",
		"11110000-0000-0000-0000-000000000002",
		"11110000-0000-0000-0000-000000000003",
		"11110000-0000-0000-0000-000000000004",
		"11110000-0000-0000-0000-000000000005",
	}
	markdown := map[string]string{
		root: mdResp("Welcome to School.", false, ""),
		s1:   mdResp("Semester overview.", false, ""),
	}
	for i, id := range fillerIDs {
		base = append(base, editedObj("page", id, "Note "+string(rune('A'+i)), "page_id", root, "2026-07-14T09:00:00.000Z"))
		markdown[id] = mdResp("Filler note.", false, "")
	}

	m := &mockNotion{
		search:   []string{notionSearch(base)},
		asset:    tinyPNG,
		markdown: markdown,
	}

	dir := t.TempDir()
	l := layout.For(dir)

	// Round 1: first ever → FULL. Checkpoint persisted.
	res1 := syncNotion(t, l, m, false)
	if mode := notionMode(t, res1); mode != "full" {
		t.Fatalf("round 1 mode = %q, want full (first ever)", mode)
	}
	if ck := latestCheckpoint(t, l); ck == "" {
		t.Fatalf("round 1 did not persist a checkpoint")
	}

	// Round 2: immediate re-run, nothing changed → INCREMENTAL, 0 added/updated.
	res2 := syncNotion(t, l, m, false)
	if mode := notionMode(t, res2); mode != "incremental" {
		t.Fatalf("round 2 mode = %q, want incremental", mode)
	}
	if res2.Stats.Added != 0 || res2.Stats.Updated != 0 {
		t.Fatalf("round 2 should be clean: added=%d updated=%d", res2.Stats.Added, res2.Stats.Updated)
	}

	// Round 3: a new page is created online under School, edited AFTER the
	// checkpoint. Incremental must pick it up (added=1) with no full enumeration.
	withNew := append(append([]string{}, base...),
		editedObj("page", newID, "Fresh Note", "page_id", root, "2026-07-14T10:00:00.000Z"))
	m.search[0] = notionSearch(withNew)
	m.markdown[newID] = mdResp("A brand new note.", false, "")

	res3 := syncNotion(t, l, m, false)
	if mode := notionMode(t, res3); mode != "incremental" {
		t.Fatalf("round 3 mode = %q, want incremental", mode)
	}
	if res3.Stats.Added != 1 {
		t.Fatalf("round 3 added = %d, want 1 (new page picked up incrementally)", res3.Stats.Added)
	}
	newPath := filepath.Join(dir, "notion/School/Fresh Note.md")
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("new page not written at %s: %v", newPath, err)
	}

	// Round 4: the new page is renamed online (title change bumps last_edited).
	// Incremental rename must follow: old path gone, new path present.
	renamed := append(append([]string{}, base...),
		editedObj("page", newID, "Renamed Note", "page_id", root, "2026-07-14T10:30:00.000Z"))
	m.search[0] = notionSearch(renamed)
	res4 := syncNotion(t, l, m, false)
	if mode := notionMode(t, res4); mode != "incremental" {
		t.Fatalf("round 4 mode = %q, want incremental", mode)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Errorf("old title file should be gone after rename, stat err = %v", err)
	}
	renamedPath := filepath.Join(dir, "notion/School/Renamed Note.md")
	if _, err := os.Stat(renamedPath); err != nil {
		t.Errorf("renamed file missing at %s: %v", renamedPath, err)
	}

	// Round 5: the page is archived online (drops out of the inventory). An
	// incremental round MUST NOT detect the deletion — the file survives.
	m.search[0] = notionSearch(base)
	res5 := syncNotion(t, l, m, false)
	if mode := notionMode(t, res5); mode != "incremental" {
		t.Fatalf("round 5 mode = %q, want incremental", mode)
	}
	if res5.Stats.Deleted != 0 {
		t.Errorf("incremental round must not delete, got deleted=%d", res5.Stats.Deleted)
	}
	if _, err := os.Stat(renamedPath); err != nil {
		t.Errorf("archived page must survive an incremental round, stat err = %v", err)
	}

	// Round 6: force a reconciliation round (--full). It sees the full inventory
	// without the archived page and finally trashes it.
	res6 := syncNotion(t, l, m, true)
	if mode := notionMode(t, res6); mode != "full" {
		t.Fatalf("round 6 mode = %q, want full (--full)", mode)
	}
	if res6.Stats.Deleted != 1 {
		t.Errorf("reconcile round should trash the archived page, deleted=%d", res6.Stats.Deleted)
	}
	if _, err := os.Stat(renamedPath); !os.IsNotExist(err) {
		t.Errorf("archived page should be gone from the tree after reconcile, stat err = %v", err)
	}
}

// TestNotionUnrefreshedMoveReconciled proves the unrefreshed-move defence: a page whose
// parent changed WITHOUT refreshing last_edited_time is invisible to an
// incremental round (it stays below the checkpoint) but is corrected by the next
// reconciliation round — as a MOVE (not delete+add), with its referrer link kept
// intact.
func TestNotionUnrefreshedMoveReconciled(t *testing.T) {
	home := "decafbad-0000-4000-8000-00000000000b"
	a := "facefeed-0000-4000-8000-00000000000a" // folder-ish page A (has the moving child)
	b := "aaaaaaaa-0000-0000-0000-00000000000b" // page B (has the child after move)
	child := "cccccccc-0000-0000-0000-00000000000c"
	referrer := "dddddddd-0000-0000-0000-00000000000d"
	// recent is edited later than everything else so the persisted checkpoint sits
	// well past the child's (unrefreshed) 09:00 timestamp — the child then falls
	// outside the incremental safety window and is genuinely missed.
	recent := "eeeeeeee-0000-0000-0000-00000000000e"

	dashless := func(s string) string { return strings.ReplaceAll(s, "-", "") }

	// Round 1 inventory: Home > {A, B, Referrer, Recent}; child under A. The
	// referrer links to the child via a <page> tag, so the internal link rewrite
	// records the edge and rewrites it to a relative path.
	inv1 := []string{
		editedObj("page", home, "Home", "workspace", "", "2026-07-14T09:00:00.000Z"),
		editedObj("page", a, "Section A", "page_id", home, "2026-07-14T09:00:00.000Z"),
		editedObj("page", b, "Section B", "page_id", home, "2026-07-14T09:00:00.000Z"),
		editedObj("page", referrer, "Referrer", "page_id", home, "2026-07-14T09:00:00.000Z"),
		editedObj("page", recent, "Recent", "page_id", home, "2026-07-14T12:00:00.000Z"),
		editedObj("page", child, "Moving Child", "page_id", a, "2026-07-14T09:00:00.000Z"),
	}
	referrerMD := `See <page url="https://app.notion.com/p/` + dashless(child) + `">Moving Child</page>.`

	m := &mockNotion{
		search: []string{notionSearch(inv1)},
		asset:  tinyPNG,
		markdown: map[string]string{
			home:     mdResp("Home page.", false, ""),
			a:        mdResp("Section A.", false, ""),
			b:        mdResp("Section B.", false, ""),
			referrer: mdResp(referrerMD, false, ""),
			recent:   mdResp("Recent page.", false, ""),
			child:    mdResp("The child body.", false, ""),
		},
	}

	dir := t.TempDir()
	l := layout.For(dir)

	// Round 1 (full): everything mirrored; the referrer's link is rewritten to the
	// child's relative path under Section A.
	syncNotion(t, l, m, false)
	childUnderA := filepath.Join(dir, "notion/Home/Section A/Moving Child.md")
	if _, err := os.Stat(childUnderA); err != nil {
		t.Fatalf("child not under Section A after round 1: %v", err)
	}
	referrerPath := filepath.Join(dir, "notion/Home/Referrer.md")
	if got := readFile(t, referrerPath); !strings.Contains(got, "Section A/Moving Child.md") {
		t.Fatalf("referrer link not rewritten to child under A:\n%s", got)
	}

	// The child moves online from A to B, but its last_edited_time is NOT refreshed
	// (stays at 09:00, at/below the checkpoint) — the unrefreshed-move case.
	inv2 := []string{
		editedObj("page", home, "Home", "workspace", "", "2026-07-14T09:00:00.000Z"),
		editedObj("page", a, "Section A", "page_id", home, "2026-07-14T09:00:00.000Z"),
		editedObj("page", b, "Section B", "page_id", home, "2026-07-14T09:00:00.000Z"),
		editedObj("page", referrer, "Referrer", "page_id", home, "2026-07-14T09:00:00.000Z"),
		editedObj("page", recent, "Recent", "page_id", home, "2026-07-14T12:00:00.000Z"),
		editedObj("page", child, "Moving Child", "page_id", b, "2026-07-14T09:00:00.000Z"),
	}
	m.search[0] = notionSearch(inv2)

	// Round 2 (incremental): the moved child is below the checkpoint, so it is not
	// enumerated → the move is MISSED. The child still sits under Section A.
	res2 := syncNotion(t, l, m, false)
	if mode := notionMode(t, res2); mode != "incremental" {
		t.Fatalf("round 2 mode = %q, want incremental", mode)
	}
	if _, err := os.Stat(childUnderA); err != nil {
		t.Errorf("incremental round should have missed the unrefreshed move (child stays under A): %v", err)
	}

	// Round 3 (reconcile, --full): the full tree rebuild sees the new parent and
	// MOVES the child under Section B — not delete+add — keeping the referrer link
	// intact and repointed.
	syncNotion(t, l, m, true)
	childUnderB := filepath.Join(dir, "notion/Home/Section B/Moving Child.md")
	if _, err := os.Stat(childUnderB); err != nil {
		t.Fatalf("child not moved under Section B after reconcile: %v", err)
	}
	if _, err := os.Stat(childUnderA); !os.IsNotExist(err) {
		t.Errorf("child should no longer be under Section A after reconcile: stat err = %v", err)
	}
	// The move is not a delete: the child keeps its id/body, and the referrer link
	// now points at the new location.
	if got := readFile(t, childUnderB); !strings.Contains(got, "The child body.") {
		t.Errorf("moved child lost its body (looks like delete+add):\n%s", got)
	}
	if got := readFile(t, referrerPath); !strings.Contains(got, "Section B/Moving Child.md") {
		t.Errorf("referrer link not repointed to the moved child:\n%s", got)
	}
}
