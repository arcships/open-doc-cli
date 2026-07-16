package notion

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/arcships/open-doc-cli/internal/adapter"
)

// queryResponse is one page of a POST /v1/data_sources/{id}/query result
// (P0-verified): a list of row page objects plus the pagination cursor.
type queryResponse struct {
	Results    []queryRow `json:"results"`
	HasMore    bool       `json:"has_more"`
	NextCursor string     `json:"next_cursor"`
}

// queryRow is one row page: its id and its raw properties map (each value is
// decoded lazily by the properties renderer).
type queryRow struct {
	Object     string                     `json:"object"`
	ID         string                     `json:"id"`
	Properties map[string]json.RawMessage `json:"properties"`
}

// QueryDatabaseRows implements adapter.DatabaseExpander. It paginates
// POST /v1/data_sources/{id}/query to exhaustion and renders every row's
// properties, keyed by the row's canonical page id (matching the row RemoteDoc's
// ID and manifest id). One query per database yields all rows' properties, so
// the engine never fetches properties per row. Read-only: the
// query endpoint returns rows without mutating anything.
func (a *Adapter) QueryDatabaseRows(ctx context.Context, dbDoc adapter.RemoteDoc, titles map[string]string) (map[string]adapter.RowProperties, error) {
	out := make(map[string]adapter.RowProperties)
	cursor := ""
	for {
		body := map[string]any{"page_size": 100}
		if cursor != "" {
			body["start_cursor"] = cursor
		}
		reqBody, _ := json.Marshal(body)
		raw, err := a.client.doAPI(ctx, "POST", "/v1/data_sources/"+dbDoc.ID+"/query", reqBody)
		if err != nil {
			return nil, fmt.Errorf("query data source %s: %w", dbDoc.ID, err)
		}
		var page queryResponse
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("decode data source query %s: %w", dbDoc.ID, err)
		}
		for _, r := range page.Results {
			if r.ID == "" {
				continue
			}
			out[canonicalID(r.ID)] = renderRowProperties(r.Properties, titles)
		}
		if !page.HasMore || page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}
