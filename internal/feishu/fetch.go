package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/arcships/open-doc-cli/internal/adapter"
)

// fetchContent is the payload shape of a docs +fetch response
// (data.document.content).
type fetchContent struct {
	Document struct {
		Content    string `json:"content"`
		DocumentID string `json:"document_id"`
		RevisionID int    `json:"revision_id"`
	} `json:"document"`
}

// FetchMarkdown fetches one document body by obj_token via two rate-limited
// lark-cli calls: markdown for the body, XML for the stable asset tokens and
// doc-link references. Markdown holds the raw body verbatim (content_hash is
// taken over it, so it stays stable); Body carries the same content with the
// degradation contract applied. Assets carry the body-facing image
// URLs (correlated with the XML tokens) for the engine to rewrite.
func (a *Adapter) FetchMarkdown(ctx context.Context, doc adapter.RemoteDoc) (adapter.FetchResult, error) {
	id := doc.ID
	markdown, err := a.fetchOne(ctx, id, "markdown")
	if err != nil {
		return adapter.FetchResult{}, fmt.Errorf("fetch markdown %s: %w", id, err)
	}
	xml, err := a.fetchOne(ctx, id, "xml")
	if err != nil {
		return adapter.FetchResult{}, fmt.Errorf("fetch xml %s: %w", id, err)
	}
	body, deg := a.degrade(ctx, markdown, xml, doc.URL)
	return adapter.FetchResult{
		Markdown:    markdown,
		Body:        body,
		Assets:      buildAssetRefs(markdown, xml),
		Links:       extractLinks(xml),
		Degradation: deg,
	}, nil
}

// fetchOne runs a single docs +fetch in the given format, honouring the rate
// limiter and retrying on rate-limit errors with exponential backoff.
func (a *Adapter) fetchOne(ctx context.Context, id, format string) (string, error) {
	var content string
	err := a.backoff.Retry(ctx, isRateLimited, func() (time.Duration, error) {
		if err := a.limiter.Wait(ctx); err != nil {
			return 0, err
		}
		out, err := a.run.Run(ctx, "docs", "+fetch", "--doc", id, "--doc-format", format, "--format", "json")
		if err != nil {
			return retryAfter(err), err
		}
		data, err := unwrap(out)
		if err != nil {
			return retryAfter(err), err
		}
		var fc fetchContent
		if err := json.Unmarshal(data, &fc); err != nil {
			return 0, fmt.Errorf("decode fetch content: %w", err)
		}
		content = fc.Document.Content
		return 0, nil
	})
	return content, err
}

// isRateLimited reports whether err looks like a Feishu rate-limit rejection,
// which is worth retrying with backoff. Other errors fail fast.
func isRateLimited(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "429") ||
		strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "too many request") ||
		strings.Contains(msg, "frequency limit")
}

// retryAfter extracts a Retry-After hint from an error message when present.
// lark-cli does not currently surface one in a structured form, so this returns
// 0 (fall back to exponential backoff) unless a plain "retry-after: <secs>"
// substring is found.
func retryAfter(err error) time.Duration {
	msg := strings.ToLower(err.Error())
	i := strings.Index(msg, "retry-after:")
	if i < 0 {
		return 0
	}
	rest := strings.TrimSpace(msg[i+len("retry-after:"):])
	var secs int
	for _, r := range rest {
		if r < '0' || r > '9' {
			break
		}
		secs = secs*10 + int(r-'0')
	}
	if secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}
