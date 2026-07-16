package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/arcships/open-doc-cli/internal/adapter"
)

// searchResponse is one page of a POST /v1/search result.
type searchResponse struct {
	Results    []json.RawMessage `json:"results"`
	HasMore    bool              `json:"has_more"`
	NextCursor string            `json:"next_cursor"`
}

// object captures the fields opendoc reads from a search result (a page or a
// data_source). Unused fields are ignored.
type object struct {
	Object         string              `json:"object"`
	ID             string              `json:"id"`
	URL            string              `json:"url"`
	InTrash        bool                `json:"in_trash"`
	IsArchived     bool                `json:"is_archived"`
	Archived       bool                `json:"archived"`
	LastEditedTime string              `json:"last_edited_time"`
	Parent         parentRef           `json:"parent"`
	DatabaseParent parentRef           `json:"database_parent"`
	Title          []richText          `json:"title"`      // data_source display title
	Properties     map[string]property `json:"properties"` // page title lives here
}

// parentRef is a Notion parent pointer. Only the type-relevant id field is set.
type parentRef struct {
	Type         string `json:"type"`
	PageID       string `json:"page_id"`
	DataSourceID string `json:"data_source_id"`
	DatabaseID   string `json:"database_id"`
	Workspace    bool   `json:"workspace"`
}

// richText is the minimal slice of a Notion rich-text run opendoc needs.
type richText struct {
	PlainText string `json:"plain_text"`
}

// property is a page property; only the title property carries the page title.
// The title field is kept raw because its shape differs by object kind: on a page
// it is a rich-text array, but on a data_source's property schema it is a config
// object. It is decoded to rich text only when reading a page title.
type property struct {
	Type  string          `json:"type"`
	Title json.RawMessage `json:"title"`
}

// enumerate paginates POST /v1/search to exhaustion and converts every visible
// page and data_source into a RemoteDoc with a resolved parent pointer. It is the
// shared implementation behind the streaming Adapter.Enumerate. Trees are rebuilt
// downstream by the engine from these parent pointers; unresolved parents become
// orphans there (mirrored under _orphans/).
func (a *Adapter) enumerate(ctx context.Context) ([]adapter.RemoteDoc, error) {
	var docs []adapter.RemoteDoc
	cursor := ""
	for {
		body := map[string]any{"page_size": 100}
		if cursor != "" {
			body["start_cursor"] = cursor
		}
		reqBody, _ := json.Marshal(body)
		raw, err := a.client.doAPI(ctx, "POST", "/v1/search", reqBody)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		var page searchResponse
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("decode search page: %w", err)
		}
		for _, rawObj := range page.Results {
			var obj object
			if err := json.Unmarshal(rawObj, &obj); err != nil {
				return nil, fmt.Errorf("decode search object: %w", err)
			}
			if obj.InTrash || obj.IsArchived || obj.Archived {
				continue // never mirror trashed/archived objects
			}
			if d, ok := toRemoteDoc(obj); ok {
				docs = append(docs, d)
			}
		}
		if !page.HasMore || page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return docs, nil
}

// toRemoteDoc converts a search object into a RemoteDoc. It returns ok=false for
// object kinds opendoc does not mirror. Pages become page / db_row nodes; data_sources
// become the database container node (a placeholder).
func toRemoteDoc(obj object) (adapter.RemoteDoc, bool) {
	switch obj.Object {
	case "page":
		return pageDoc(obj), true
	case "data_source":
		return dataSourceDoc(obj), true
	default:
		return adapter.RemoteDoc{}, false
	}
}

// pageDoc builds the RemoteDoc for a page. A page parented by a data source is a
// database row (TypeDBRow); otherwise it is an ordinary page under the workspace
// or another page.
func pageDoc(obj object) adapter.RemoteDoc {
	d := adapter.RemoteDoc{
		ID:       canonicalID(obj.ID),
		AltID:    dashless(obj.ID),
		Type:     adapter.TypePage,
		Title:    pageTitle(obj),
		URL:      obj.URL,
		EditedAt: normalizeTime(obj.LastEditedTime),
	}
	switch obj.Parent.Type {
	case "workspace":
		d.ParentID = ""
	case "page_id":
		d.ParentID = canonicalID(obj.Parent.PageID)
	case "data_source_id":
		d.ParentID = canonicalID(obj.Parent.DataSourceID)
		d.Type = adapter.TypeDBRow
	default:
		// Unknown/blank parent → surface as an orphan (non-empty, unresolvable id).
		d.ParentID = unresolvedParent(obj.Parent)
	}
	return d
}

// dataSourceDoc builds the RemoteDoc for a data_source, which opendoc models as the
// database container node. Its manifest id is the data source id (the target row
// pages and <database data-source-url> agree on), while its alias is the dashless
// database id that appears in <database url> tags so those links resolve. It is
// placed by its database_parent (where the database itself lives).
func dataSourceDoc(obj object) adapter.RemoteDoc {
	title := titleText(obj.Title)
	if title == "" {
		title = "Untitled Database"
	}
	d := adapter.RemoteDoc{
		ID:       canonicalID(obj.ID),
		AltID:    dashless(obj.Parent.DatabaseID),
		Type:     adapter.TypeDB,
		Title:    title,
		URL:      obj.URL,
		EditedAt: normalizeTime(obj.LastEditedTime),
	}
	switch obj.DatabaseParent.Type {
	case "workspace":
		d.ParentID = ""
	case "page_id":
		d.ParentID = canonicalID(obj.DatabaseParent.PageID)
	case "data_source_id":
		d.ParentID = canonicalID(obj.DatabaseParent.DataSourceID)
	default:
		d.ParentID = unresolvedParent(obj.DatabaseParent)
	}
	return d
}

// unresolvedParent returns a non-empty, deliberately unmatchable parent id for a
// parent pointer opendoc cannot place, so the engine routes the node to _orphans
// rather than promoting it to a platform root.
func unresolvedParent(p parentRef) string {
	for _, id := range []string{p.PageID, p.DataSourceID, p.DatabaseID} {
		if id != "" {
			return canonicalID(id)
		}
	}
	return "notion:unresolved"
}

// pageTitle extracts a page's title from its title-typed property, or "" when the
// page has no title (untitled pages are legal; naming supplies a fallback).
func pageTitle(obj object) string {
	for _, p := range obj.Properties {
		if p.Type == "title" {
			var rt []richText
			if err := json.Unmarshal(p.Title, &rt); err == nil {
				return titleText(rt)
			}
		}
	}
	return ""
}

// titleText concatenates the plain_text of a rich-text run slice.
func titleText(rt []richText) string {
	var s string
	for _, r := range rt {
		s += r.PlainText
	}
	return s
}

// normalizeTime parses a Notion timestamp (RFC3339 with milliseconds) and
// re-emits it as plain RFC3339 UTC, matching the frontmatter/manifest convention.
// An unparseable/empty value yields "".
func normalizeTime(s string) string {
	if s == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
