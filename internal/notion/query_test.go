package notion_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/notion"
)

// pagingTransport serves a fixed sequence of query-endpoint pages, one per call,
// so QueryDatabaseRows' cursor pagination can be exercised with no network.
type pagingTransport struct {
	pages []string
	calls int
}

func (p *pagingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	i := p.calls
	if i >= len(p.pages) {
		i = len(p.pages) - 1
	}
	p.calls++
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(p.pages[i]))}, nil
}

// TestQueryDatabaseRowsPagination checks the data_sources query follows
// next_cursor to exhaustion and returns every row keyed by canonical id, with
// properties rendered.
func TestQueryDatabaseRowsPagination(t *testing.T) {
	rt := &pagingTransport{pages: []string{
		`{"object":"list","results":[{"object":"page","id":"11111111-1111-1111-1111-111111111111","properties":{"N":{"type":"number","number":1}}}],"has_more":true,"next_cursor":"c1"}`,
		`{"object":"list","results":[{"object":"page","id":"22222222222222222222222222222222","properties":{"N":{"type":"number","number":2}}}],"has_more":false,"next_cursor":null}`,
	}}
	a := notion.NewAdapter("tok", rt)

	rows, err := a.QueryDatabaseRows(context.Background(), adapter.RemoteDoc{ID: "ds"}, nil)
	if err != nil {
		t.Fatalf("QueryDatabaseRows: %v", err)
	}
	if rt.calls != 2 {
		t.Errorf("query made %d calls, want 2 (paginated)", rt.calls)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	// Both ids are keyed canonically (hyphenated), including the dashless input.
	for _, id := range []string{"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"} {
		rp, ok := rows[id]
		if !ok {
			t.Errorf("missing row %s", id)
			continue
		}
		if len(rp.Entries) != 1 || rp.Entries[0].Key != "N" {
			t.Errorf("row %s properties wrong: %+v", id, rp.Entries)
		}
	}
}
