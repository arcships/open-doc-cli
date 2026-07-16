package feishu

import (
	"context"
	"encoding/json"
	"fmt"
)

// metasBatchSize is the per-request cap for drive/v1/metas/batch_query
// (the endpoint accepts at most 200 tokens per call).
const metasBatchSize = 200

// metaEntry is one result of a metas/batch_query response.
type metaEntry struct {
	DocToken         string `json:"doc_token"`
	DocType          string `json:"doc_type"`
	LatestModifyTime string `json:"latest_modify_time"`
	Title            string `json:"title"`
	URL              string `json:"url"`
}

// metasResult is the data payload of metas/batch_query.
type metasResult struct {
	Metas []metaEntry `json:"metas"`
}

// enrichMetadata fills URL and EditedAt for every content-bearing node via
// drive/v1/metas/batch_query (with_url=true). This is the single endpoint that
// yields both the canonical online URL (even for wiki-origin docs, as a /docx/
// link) and the latest modify time, which wiki node listings omit. Failures for
// individual tokens are tolerated: nodes simply keep whatever URL/time they
// already had.
func (a *Adapter) enrichMetadata(ctx context.Context, c *collector) error {
	type req struct {
		DocToken string `json:"doc_token"`
		DocType  string `json:"doc_type"`
	}
	var reqs []req
	for token, docType := range c.enrich {
		reqs = append(reqs, req{DocToken: token, DocType: docType})
	}
	if len(reqs) == 0 {
		return nil
	}

	// token -> resolved metadata.
	meta := make(map[string]metaEntry, len(reqs))
	for start := 0; start < len(reqs); start += metasBatchSize {
		end := start + metasBatchSize
		if end > len(reqs) {
			end = len(reqs)
		}
		batch := reqs[start:end]
		payload := map[string]any{"request_docs": batch, "with_url": true}
		body, _ := json.Marshal(payload)
		out, err := a.run.Run(ctx, "api", "POST", "/open-apis/drive/v1/metas/batch_query", "--data", string(body))
		if err != nil {
			return err
		}
		data, err := unwrap(out)
		if err != nil {
			return err
		}
		var res metasResult
		if err := json.Unmarshal(data, &res); err != nil {
			return fmt.Errorf("decode metas: %w", err)
		}
		for _, m := range res.Metas {
			meta[m.DocToken] = m
		}
	}

	for i := range c.docs {
		m, ok := meta[c.docs[i].ID]
		if !ok {
			continue
		}
		if m.URL != "" {
			c.docs[i].URL = m.URL
		}
		if t := unixToRFC3339(m.LatestModifyTime); t != "" {
			c.docs[i].EditedAt = t
		}
	}
	return nil
}

// folderName resolves a drive folder token to its display name via metas.
func (a *Adapter) folderName(ctx context.Context, folderToken string) (string, error) {
	payload := map[string]any{
		"request_docs": []map[string]string{{"doc_token": folderToken, "doc_type": "folder"}},
		"with_url":     true,
	}
	body, _ := json.Marshal(payload)
	out, err := a.run.Run(ctx, "api", "POST", "/open-apis/drive/v1/metas/batch_query", "--data", string(body))
	if err != nil {
		return "", err
	}
	data, err := unwrap(out)
	if err != nil {
		return "", err
	}
	var res metasResult
	if err := json.Unmarshal(data, &res); err != nil {
		return "", fmt.Errorf("decode folder meta: %w", err)
	}
	if len(res.Metas) == 0 || res.Metas[0].Title == "" {
		return folderToken, nil
	}
	return res.Metas[0].Title, nil
}
