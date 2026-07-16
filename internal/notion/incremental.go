package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/arcships/open-doc-cli/internal/adapter"
)

// SafetyWindow is the look-back applied to the checkpoint during an incremental
// search. Notion's search timestamp is effectively minute-precision and a
// checkpoint persisted mid-edit could be slightly stale, so pages edited within
// this window of the checkpoint are re-enumerated rather than skipped; the
// engine's content-hash skip absorbs the harmless repeats.
const SafetyWindow = 5 * time.Minute

// EnumerateIncremental implements adapter.IncrementalEnumerator. It pages
// POST /v1/search ordered by last_edited_time descending and stops paginating
// once it reaches an entry edited before checkpoint-SafetyWindow: in descending
// order every later entry is older still, so nothing after it can be dirty.
// It returns the changed docs and the new checkpoint to persist — the max
// last_edited_time observed this round, never below the passed checkpoint (so an
// empty round never regresses the high-water mark).
func (a *Adapter) EnumerateIncremental(ctx context.Context, checkpoint string) ([]adapter.RemoteDoc, string, error) {
	ck, err := time.Parse(time.RFC3339, checkpoint)
	if err != nil {
		return nil, checkpoint, fmt.Errorf("parse checkpoint %q: %w", checkpoint, err)
	}
	cutoff := ck.Add(-SafetyWindow)

	var docs []adapter.RemoteDoc
	maxEdited := ck
	cursor := ""
	for {
		body := map[string]any{
			"page_size": 100,
			// Descending by last_edited_time lets pagination stop at the first
			// entry older than the safety cutoff.
			"sort": map[string]any{"timestamp": "last_edited_time", "direction": "descending"},
		}
		if cursor != "" {
			body["start_cursor"] = cursor
		}
		reqBody, _ := json.Marshal(body)
		raw, err := a.client.doAPI(ctx, "POST", "/v1/search", reqBody)
		if err != nil {
			return nil, checkpoint, fmt.Errorf("incremental search: %w", err)
		}
		var page searchResponse
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, checkpoint, fmt.Errorf("decode incremental search page: %w", err)
		}

		stop := false
		for _, rawObj := range page.Results {
			var obj object
			if err := json.Unmarshal(rawObj, &obj); err != nil {
				return nil, checkpoint, fmt.Errorf("decode search object: %w", err)
			}
			if obj.InTrash || obj.IsArchived || obj.Archived {
				continue // never mirror trashed/archived objects
			}
			edited, ok := parseEditedTime(obj.LastEditedTime)
			// An entry strictly before the cutoff (and every entry after it, in
			// descending order) is outside the dirty window: stop after this page.
			// Entries with an unparseable/absent time are treated as "recent" so a
			// timestamp-less object never truncates the scan prematurely.
			if ok && edited.Before(cutoff) {
				stop = true
				continue
			}
			if d, ok2 := toRemoteDoc(obj); ok2 {
				docs = append(docs, d)
				if ok && edited.After(maxEdited) {
					maxEdited = edited
				}
			}
		}
		if stop || !page.HasMore || page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return docs, maxEdited.UTC().Format(time.RFC3339), nil
}

// parseEditedTime parses a Notion last_edited_time (RFC3339, milliseconds) into a
// UTC time. ok is false for an empty or unparseable value.
func parseEditedTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
