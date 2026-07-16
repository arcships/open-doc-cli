package notion

import (
	"context"
	"net/http"

	"github.com/arcships/open-doc-cli/internal/adapter"
)

// Adapter implements adapter.Adapter for Notion. It enumerates the workspace via
// POST /v1/search, fetches bodies from GET /v1/pages/{id}/markdown, and downloads
// assets from their S3 signed URLs. It is strictly read-only.
type Adapter struct {
	client *Client
}

// NewAdapter builds a Notion adapter for the given integration token, using rt as
// the HTTP transport (nil ⇒ the default transport). The token must be non-empty;
// callers gate construction on config + environment (see cli.buildAdapters).
func NewAdapter(token string, rt http.RoundTripper) *Adapter {
	return &Adapter{client: NewClient(token, rt)}
}

// Platform returns the manifest platform tag.
func (a *Adapter) Platform() string { return "notion" }

// Enumerate streams the authorized inventory. It performs the paginated search
// eagerly, then replays the result onto the channel so the engine consumes it as
// a stream (matching the Feishu adapter's shape).
func (a *Adapter) Enumerate(ctx context.Context) (<-chan adapter.RemoteDoc, <-chan error) {
	out := make(chan adapter.RemoteDoc)
	errc := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errc)
		docs, err := a.enumerate(ctx)
		if err != nil {
			errc <- err
			return
		}
		for _, d := range docs {
			select {
			case out <- d:
			case <-ctx.Done():
				errc <- ctx.Err()
				return
			}
		}
	}()
	return out, errc
}

// DownloadAsset downloads the asset at ref.URL (a short-lived S3 signed URL) to
// destPath via a plain HTTP GET. The signed URL carries its own credentials, so
// no Authorization header is sent.
func (a *Adapter) DownloadAsset(ctx context.Context, ref adapter.AssetRef, destPath string) error {
	return a.client.downloadTo(ctx, ref.URL, destPath)
}
