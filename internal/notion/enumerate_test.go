package notion

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/arcships/open-doc-cli/internal/adapter"
)

// searchPage builds a POST /v1/search response body with the given results and
// pagination fields.
func searchPage(results string, hasMore bool, next string) string {
	m := map[string]any{
		"object":      "list",
		"results":     json.RawMessage("[" + results + "]"),
		"has_more":    hasMore,
		"next_cursor": next,
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// pageObj / dbRow / wsPage / dataSourceObj are fixture builders for search
// results, mirroring the real API shapes verified in P0.
func wsPage(id, title, edited string) string {
	return `{"object":"page","id":"` + id + `","url":"https://app.notion.com/p/` + strings.ReplaceAll(id, "-", "") + `","last_edited_time":"` + edited + `","parent":{"type":"workspace","workspace":true},"properties":{"title":{"type":"title","title":[{"plain_text":"` + title + `"}]}}}`
}
func childPage(id, parentPage, title string) string {
	return `{"object":"page","id":"` + id + `","url":"https://app.notion.com/p/x","parent":{"type":"page_id","page_id":"` + parentPage + `"},"properties":{"title":{"type":"title","title":[{"plain_text":"` + title + `"}]}}}`
}
func dbRow(id, dsID, dbID, title string) string {
	return `{"object":"page","id":"` + id + `","url":"https://app.notion.com/p/x","parent":{"type":"data_source_id","data_source_id":"` + dsID + `","database_id":"` + dbID + `"},"properties":{"Name":{"type":"title","title":[{"plain_text":"` + title + `"}]}}}`
}
func dataSourceObj(id, dbID, parentPage, title string) string {
	return `{"object":"data_source","id":"` + id + `","url":"https://app.notion.com/p/` + strings.ReplaceAll(dbID, "-", "") + `","parent":{"type":"database_id","database_id":"` + dbID + `"},"database_parent":{"type":"page_id","page_id":"` + parentPage + `"},"title":[{"plain_text":"` + title + `"}]}`
}

func TestEnumeratePaginationAndParents(t *testing.T) {
	root := "decafbad-0000-4000-8000-00000000000b"
	dsID := "c0ffee00-0000-4000-8000-00000000000e"
	dbID := "dbdbdbdb-0000-4000-8000-00000000000f"
	page1 := searchPage(strings.Join([]string{
		wsPage(root, "School", "2026-07-14T09:12:00.000Z"),
		dataSourceObj(dsID, dbID, root, "Subjects"),
	}, ","), true, "CURSOR2")
	page2 := searchPage(strings.Join([]string{
		childPage("facefeed-0000-4000-8000-00000000000a", root, "Term 1"),
		dbRow("ab1e0000-0000-4000-8000-000000000010", dsID, dbID, "Advanced Topics"),
		// A row whose data source is NOT enumerated → its ParentID points at a
		// non-empty, unresolvable id (the engine routes it to _orphans).
		dbRow("beefcafe-0000-4000-8000-00000000000c", "faded0ff-0000-4000-8000-00000000000d", dbID, "Behind the Prompt"),
		// A trashed page must be skipped entirely.
		`{"object":"page","id":"dead0000-0000-0000-0000-000000000000","in_trash":true,"parent":{"type":"workspace","workspace":true},"properties":{}}`,
	}, ","), false, "")

	// Serve page1 then page2 by call sequence (pagination follows next_cursor).
	var call int
	ft := &fakeTransport{handler: func(req *http.Request) (*http.Response, error) {
		call++
		if call == 1 {
			return jsonResp(http.StatusOK, page1, nil), nil
		}
		return jsonResp(http.StatusOK, page2, nil), nil
	}}

	a := NewAdapter("tok", ft)
	docs, err := a.enumerate(context.Background())
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if call != 2 {
		t.Fatalf("expected 2 search requests (pagination), got %d", call)
	}

	byID := map[string]adapter.RemoteDoc{}
	for _, d := range docs {
		byID[d.ID] = d
	}
	if len(docs) != 5 {
		t.Fatalf("want 5 docs (trashed skipped), got %d: %v", len(docs), byID)
	}

	// Workspace root: empty parent, page type, alias = dashless id.
	if r := byID[root]; r.ParentID != "" || r.Type != adapter.TypePage || r.AltID != dashless(root) || r.Title != "School" {
		t.Errorf("workspace root wrong: %+v", r)
	}
	// Child page parents onto the (canonical) root id.
	if c := byID["facefeed-0000-4000-8000-00000000000a"]; c.ParentID != root || c.Type != adapter.TypePage {
		t.Errorf("child page wrong: %+v", c)
	}
	// data_source becomes a db node: id = data source id, parent = database_parent
	// page, alias = dashless(database_id) so <database url> resolves.
	if d := byID[dsID]; d.Type != adapter.TypeDB || d.ParentID != root || d.AltID != dashless(dbID) || d.Title != "Subjects" {
		t.Errorf("data_source node wrong: %+v", d)
	}
	// Row under an enumerated data source: parents onto the db node, TypeDBRow.
	if r := byID["ab1e0000-0000-4000-8000-000000000010"]; r.ParentID != dsID || r.Type != adapter.TypeDBRow {
		t.Errorf("db row wrong: %+v", r)
	}
	// Orphan row: parent is the (unenumerated) data source id — non-empty and
	// unresolvable, so buildTree will route it to _orphans.
	orphan := byID["beefcafe-0000-4000-8000-00000000000c"]
	if orphan.ParentID != "faded0ff-0000-4000-8000-00000000000d" {
		t.Errorf("orphan row parent = %q, want the unenumerated data source id", orphan.ParentID)
	}
	if _, resolvable := byID[orphan.ParentID]; resolvable {
		t.Errorf("orphan parent must be unresolvable within the doc set")
	}
}
