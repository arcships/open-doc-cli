package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/arcships/open-doc-cli/internal/adapter"
)

// objTypeToDocType maps a Feishu obj_type/file-type string to the engine's
// DocType. Unknown content types fall back to a placeholder resource type so the
// tree never has a hole: the node still appears, degraded but drillable.
func objTypeToDocType(objType string) adapter.DocType {
	switch objType {
	case "docx", "doc":
		return adapter.TypeDocx
	case "folder":
		return adapter.TypeFolder
	case "sheet":
		return adapter.TypeSheet
	case "bitable":
		return adapter.TypeBitable
	case "mindnote":
		return adapter.TypeMindnote
	case "slides":
		return adapter.TypeSlides
	default:
		return adapter.TypeFile
	}
}

// wikiNode mirrors one entry of a wiki +node-list response.
type wikiNode struct {
	HasChild        bool   `json:"has_child"`
	NodeToken       string `json:"node_token"`
	ObjToken        string `json:"obj_token"`
	ObjType         string `json:"obj_type"`
	ParentNodeToken string `json:"parent_node_token"`
	SpaceID         string `json:"space_id"`
	Title           string `json:"title"`
}

// wikiNodeList is the data payload of wiki +node-list.
type wikiNodeList struct {
	Nodes []wikiNode `json:"nodes"`
}

// driveFile mirrors one entry of a drive/v1/files listing.
type driveFile struct {
	Token        string `json:"token"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	URL          string `json:"url"`
	ParentToken  string `json:"parent_token"`
	ModifiedTime string `json:"modified_time"`
}

// driveFileList is the data payload of drive/v1/files.
type driveFileList struct {
	Files         []driveFile `json:"files"`
	HasMore       bool        `json:"has_more"`
	NextPageToken string      `json:"next_page_token"`
}

// spaceInfo is the data payload of wiki spaces get.
type spaceInfo struct {
	Space struct {
		Name    string `json:"name"`
		SpaceID string `json:"space_id"`
	} `json:"space"`
}

// collector accumulates enumerated RemoteDocs plus the obj_token/type pairs that
// need metadata enrichment (url + edit time) via metas/batch_query.
type collector struct {
	docs []adapter.RemoteDoc
	// enrich maps obj_token -> doc_type for the metas batch query. Folders and
	// synthetic roots are excluded.
	enrich map[string]string
}

func newCollector() *collector {
	return &collector{enrich: map[string]string{}}
}

// add appends a RemoteDoc and, when it is a content-bearing node, records it for
// metadata enrichment.
func (c *collector) add(d adapter.RemoteDoc, enrichType string) {
	c.docs = append(c.docs, d)
	if enrichType != "" {
		c.enrich[d.ID] = enrichType
	}
}

// enumerate walks every configured source and returns the full RemoteDoc slice.
// It is the shared implementation behind the streaming Adapter.Enumerate.
func (a *Adapter) enumerate(ctx context.Context) ([]adapter.RemoteDoc, error) {
	c := newCollector()

	// Wiki spaces.
	for _, spaceID := range a.cfg.WikiSpaces {
		if err := a.enumerateWikiSpace(ctx, c, spaceID, "wiki"); err != nil {
			return nil, fmt.Errorf("enumerate wiki space %s: %w", spaceID, err)
		}
	}

	// Personal library ("my space"): mirrored through the wiki node API under
	// the per-user my_library alias (verified against lark-cli).
	if a.cfg.IncludeMyLibrary {
		if err := a.enumerateWikiSpace(ctx, c, "my_library", "my-library"); err != nil {
			return nil, fmt.Errorf("enumerate my_library: %w", err)
		}
	}

	// Drive folders.
	for _, folderToken := range a.cfg.DriveFolders {
		if err := a.enumerateDriveFolder(ctx, c, folderToken); err != nil {
			return nil, fmt.Errorf("enumerate drive folder %s: %w", folderToken, err)
		}
	}

	if err := a.enrichMetadata(ctx, c); err != nil {
		return nil, fmt.Errorf("enrich metadata: %w", err)
	}
	return c.docs, nil
}

// enumerateWikiSpace emits a synthetic root for the space and recursively walks
// its nodes. rootKind distinguishes the directory prefix ("wiki" vs
// "my-library").
func (a *Adapter) enumerateWikiSpace(ctx context.Context, c *collector, spaceID, rootKind string) error {
	name, resolvedID, err := a.spaceName(ctx, spaceID)
	if err != nil {
		return err
	}
	rootID := "feishu:" + rootKind + ":" + resolvedID
	var rootTitle string
	switch rootKind {
	case "my-library":
		rootTitle = "my-library-" + name
	default:
		rootTitle = "wiki-" + name
	}
	c.add(adapter.RemoteDoc{
		ID:       rootID,
		Type:     adapter.TypeFolder,
		ParentID: "",
		Title:    rootTitle,
	}, "")

	return a.walkWikiNodes(ctx, c, spaceID, "", rootID)
}

// walkWikiNodes lists the children of parentNodeToken (root when empty) and
// recurses into nodes that have children. parentID is the engine-facing parent
// ID (the synthetic root, or the parent node's obj_token).
func (a *Adapter) walkWikiNodes(ctx context.Context, c *collector, spaceID, parentNodeToken, parentID string) error {
	args := []string{"wiki", "+node-list", "--space-id", spaceID, "--page-all", "--page-limit", "0", "--format", "json"}
	if parentNodeToken != "" {
		args = append(args, "--parent-node-token", parentNodeToken)
	}
	out, err := a.run.Run(ctx, args...)
	if err != nil {
		return err
	}
	data, err := unwrap(out)
	if err != nil {
		return err
	}
	var list wikiNodeList
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("decode wiki nodes: %w", err)
	}

	for _, n := range list.Nodes {
		dt := objTypeToDocType(n.ObjType)
		c.add(adapter.RemoteDoc{
			ID:       n.ObjToken,
			AltID:    n.NodeToken, // /wiki/<node_token> URLs resolve here via the alias table.
			Type:     dt,
			ParentID: parentID,
			Title:    n.Title,
			// URL and EditedAt are filled by metadata enrichment.
		}, n.ObjType)

		if n.HasChild {
			if err := a.walkWikiNodes(ctx, c, spaceID, n.NodeToken, n.ObjToken); err != nil {
				return err
			}
		}
	}
	return nil
}

// enumerateDriveFolder emits the configured folder as a "drive-<name>" root and
// walks its contents, recursing into subfolders.
func (a *Adapter) enumerateDriveFolder(ctx context.Context, c *collector, folderToken string) error {
	name, err := a.folderName(ctx, folderToken)
	if err != nil {
		return err
	}
	c.add(adapter.RemoteDoc{
		ID:       folderToken,
		Type:     adapter.TypeFolder,
		ParentID: "",
		Title:    "drive-" + name,
	}, "")
	return a.walkDriveFolder(ctx, c, folderToken)
}

// walkDriveFolder lists the files under folderToken (paginating) and recurses
// into subfolders. Files parent onto folderToken.
func (a *Adapter) walkDriveFolder(ctx context.Context, c *collector, folderToken string) error {
	pageToken := ""
	for {
		params := map[string]any{"folder_token": folderToken, "page_size": 50}
		if pageToken != "" {
			params["page_token"] = pageToken
		}
		paramJSON, _ := json.Marshal(params)
		out, err := a.run.Run(ctx, "api", "GET", "/open-apis/drive/v1/files", "--params", string(paramJSON))
		if err != nil {
			return err
		}
		data, err := unwrap(out)
		if err != nil {
			return err
		}
		var list driveFileList
		if err := json.Unmarshal(data, &list); err != nil {
			return fmt.Errorf("decode drive files: %w", err)
		}
		for _, f := range list.Files {
			dt := objTypeToDocType(f.Type)
			doc := adapter.RemoteDoc{
				ID:       f.Token,
				Type:     dt,
				ParentID: folderToken,
				Title:    f.Name,
				URL:      f.URL,
				EditedAt: unixToRFC3339(f.ModifiedTime),
			}
			if f.Type == "folder" {
				c.add(doc, "")
				if err := a.walkDriveFolder(ctx, c, f.Token); err != nil {
					return err
				}
			} else {
				// Drive listings already carry url + modified_time, but enrich
				// anyway so wiki/drive edit-time semantics stay uniform.
				c.add(doc, f.Type)
			}
		}
		if !list.HasMore || list.NextPageToken == "" {
			break
		}
		pageToken = list.NextPageToken
	}
	return nil
}

// spaceName resolves a wiki space's display name and canonical space ID. The
// my_library alias resolves to a real per-user space ID.
func (a *Adapter) spaceName(ctx context.Context, spaceID string) (name, resolvedID string, err error) {
	out, err := a.run.Run(ctx, "wiki", "spaces", "get", "--space-id", spaceID, "--format", "json")
	if err != nil {
		return "", "", err
	}
	data, err := unwrap(out)
	if err != nil {
		return "", "", err
	}
	var info spaceInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return "", "", fmt.Errorf("decode space info: %w", err)
	}
	resolvedID = info.Space.SpaceID
	if resolvedID == "" {
		resolvedID = spaceID
	}
	name = info.Space.Name
	if name == "" {
		name = resolvedID
	}
	return name, resolvedID, nil
}

// unixToRFC3339 converts a Unix-seconds string ("1784020641") to an RFC3339 UTC
// timestamp, or returns "" for empty/invalid input.
func unixToRFC3339(unix string) string {
	if unix == "" {
		return ""
	}
	secs, err := strconv.ParseInt(unix, 10, 64)
	if err != nil {
		return ""
	}
	return time.Unix(secs, 0).UTC().Format(time.RFC3339)
}
