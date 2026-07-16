package notion

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/arcships/open-doc-cli/internal/adapter"
)

// pageMarkdown is the shape of a GET /v1/pages/{id}/markdown response
// (P0-verified): {object, id, markdown, truncated, unknown_block_ids[]}.
type pageMarkdown struct {
	Object          string   `json:"object"`
	ID              string   `json:"id"`
	Markdown        string   `json:"markdown"`
	Truncated       bool     `json:"truncated"`
	UnknownBlockIDs []string `json:"unknown_block_ids"`
}

// FetchMarkdown fetches one page's body from the markdown endpoint. An empty
// markdown ("") is trusted as a genuinely empty page (P0-verified). The raw
// markdown is returned verbatim in Markdown (content_hash is taken over it, so it
// stays stable); Body carries the same content with the degradation contract
// applied — truncated / unknown-block markers prepended. Assets
// and links are extracted from the body for the engine's asset pipeline and
// two-phase link rewrite.
func (a *Adapter) FetchMarkdown(ctx context.Context, doc adapter.RemoteDoc) (adapter.FetchResult, error) {
	raw, err := a.client.doAPI(ctx, "GET", "/v1/pages/"+doc.ID+"/markdown", nil)
	if err != nil {
		return adapter.FetchResult{}, fmt.Errorf("fetch markdown %s: %w", doc.ID, err)
	}
	var pm pageMarkdown
	if err := json.Unmarshal(raw, &pm); err != nil {
		return adapter.FetchResult{}, fmt.Errorf("decode markdown %s: %w", doc.ID, err)
	}
	body, deg := degrade(pm.Markdown, pm.Truncated, pm.UnknownBlockIDs)
	return adapter.FetchResult{
		Markdown:    pm.Markdown,
		Body:        body,
		Assets:      extractAssets(pm.Markdown),
		Links:       extractLinks(pm.Markdown),
		Degradation: deg,
	}, nil
}
