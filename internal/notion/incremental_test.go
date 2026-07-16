package notion

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// editedPage is a search page object carrying an explicit last_edited_time so the
// incremental scan's cutoff/stop logic can be exercised.
func editedPage(id, title, edited string) string {
	return wsPage(id, title, edited)
}

// TestIncrementalStopsAtSafetyWindow proves the descending scan (a) re-enumerates
// a doc just inside the safety window (checkpoint-3min) so the content-hash skip
// can absorb it, and (b) stops paginating at a doc past the window
// (checkpoint-10min), never requesting the next page.
func TestIncrementalStopsAtSafetyWindow(t *testing.T) {
	checkpoint := "2026-07-14T12:00:00Z"
	// Page 1 (descending): a fresh edit, then one 3min inside the window.
	page1 := searchPage(strings.Join([]string{
		editedPage("11111111-1111-1111-1111-111111111111", "Fresh", "2026-07-14T12:30:00.000Z"),
		editedPage("22222222-2222-2222-2222-222222222222", "InsideWindow", "2026-07-14T11:57:00.000Z"),
	}, ","), true, "CURSOR2")
	// Page 2: a doc 10min before the checkpoint → outside the window → stop, and it
	// must NOT be emitted.
	page2 := searchPage(
		editedPage("33333333-3333-3333-3333-333333333333", "TooOld", "2026-07-14T11:50:00.000Z"),
		true, "CURSOR3")

	var call int
	ft := &fakeTransport{handler: func(req *http.Request) (*http.Response, error) {
		call++
		switch call {
		case 1:
			return jsonResp(http.StatusOK, page1, nil), nil
		case 2:
			return jsonResp(http.StatusOK, page2, nil), nil
		default:
			t.Errorf("unexpected extra search request #%d (should have stopped)", call)
			return jsonResp(http.StatusOK, searchPage("", false, ""), nil), nil
		}
	}}

	a := NewAdapter("tok", ft)
	docs, newCk, err := a.EnumerateIncremental(context.Background(), checkpoint)
	if err != nil {
		t.Fatalf("EnumerateIncremental: %v", err)
	}
	// Only the fresh and inside-window docs are emitted; the too-old doc is not.
	ids := map[string]bool{}
	for _, d := range docs {
		ids[d.ID] = true
	}
	if len(docs) != 2 || !ids["11111111-1111-1111-1111-111111111111"] || !ids["22222222-2222-2222-2222-222222222222"] {
		t.Fatalf("want the fresh + inside-window docs only, got %d: %v", len(docs), ids)
	}
	if ids["33333333-3333-3333-3333-333333333333"] {
		t.Errorf("the too-old doc (checkpoint-10min) must not be emitted")
	}
	// Pagination stopped after page 2 (the page containing the too-old doc); page 3
	// was never requested.
	if call != 2 {
		t.Fatalf("expected pagination to stop after 2 pages, made %d requests", call)
	}
	// The new checkpoint advanced to the max edit seen (the fresh doc).
	want := mustParse(t, "2026-07-14T12:30:00Z")
	if got := mustParse(t, newCk); !got.Equal(want) {
		t.Errorf("new checkpoint = %q, want 2026-07-14T12:30:00Z", newCk)
	}
}

// TestIncrementalEmptyRoundKeepsCheckpoint proves an incremental round that finds
// nothing new (first entry already outside the window) emits no docs and never
// regresses the checkpoint below its input.
func TestIncrementalEmptyRoundKeepsCheckpoint(t *testing.T) {
	checkpoint := "2026-07-14T12:00:00Z"
	page1 := searchPage(
		editedPage("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "Old", "2026-07-14T11:00:00.000Z"),
		false, "")
	var call int
	ft := &fakeTransport{handler: func(req *http.Request) (*http.Response, error) {
		call++
		return jsonResp(http.StatusOK, page1, nil), nil
	}}
	a := NewAdapter("tok", ft)
	docs, newCk, err := a.EnumerateIncremental(context.Background(), checkpoint)
	if err != nil {
		t.Fatalf("EnumerateIncremental: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("want no docs, got %d", len(docs))
	}
	if got, want := mustParse(t, newCk), mustParse(t, checkpoint); !got.Equal(want) {
		t.Errorf("checkpoint regressed to %q, want it to stay %q", newCk, checkpoint)
	}
	if call != 1 {
		t.Fatalf("expected a single page request, made %d", call)
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tt.UTC()
}
