package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/config"
	"github.com/arcships/open-doc-cli/internal/ratelimit"
)

// FetchQPS is the fetch rate cap (5 QPS) for lark-cli docs +fetch.
// It also paces the bitable drill-down API calls made during degradation.
const FetchQPS = 5.0

// AssetQPS is the separate rate cap for asset downloads: assets get their own
// 5 QPS token bucket, independent of the body fetch bucket.
const AssetQPS = 5.0

// Adapter implements adapter.Adapter for Feishu. It enumerates wiki spaces, the
// personal library, and drive folders via lark-cli, and fetches document bodies
// by shelling out to `lark-cli docs +fetch`.
type Adapter struct {
	cfg           config.Feishu
	run           Runner
	limiter       *ratelimit.Bucket // body fetch + bitable API
	assetLimiter  *ratelimit.Bucket // asset downloads
	backoff       ratelimit.Backoff
	bitableMaxRow int
}

// NewAdapter builds a Feishu adapter driving the supplied Runner (production:
// ExecRunner). Limiters and backoff are fixed to the platform's documented
// limits. bitableMaxRow is the inline-render row threshold (config
// bitable_inline_max_rows); values <= 0 fall back to the config default.
func NewAdapter(cfg config.Feishu, run Runner, bitableMaxRow int) *Adapter {
	if bitableMaxRow <= 0 {
		bitableMaxRow = config.DefaultBitableInlineMaxRows
	}
	return &Adapter{
		cfg:           cfg,
		run:           run,
		limiter:       ratelimit.NewPerSecond(FetchQPS),
		assetLimiter:  ratelimit.NewPerSecond(AssetQPS),
		backoff:       ratelimit.DefaultBackoff,
		bitableMaxRow: bitableMaxRow,
	}
}

// Configured reports whether the config names any source to mirror. When false
// the engine can skip registering the adapter.
func (a *Adapter) Configured() bool {
	return len(a.cfg.WikiSpaces) > 0 || len(a.cfg.DriveFolders) > 0 || a.cfg.IncludeMyLibrary
}

// Platform returns the manifest platform tag.
func (a *Adapter) Platform() string { return "feishu" }

// Enumerate streams the authorized inventory. Enumeration runs eagerly (it is
// necessarily batched for the up-front metadata enrichment pass), then the
// result is replayed onto the channel for the engine to consume as a stream.
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

// mediaDownloadData is the data payload of a docs +media-download response.
type mediaDownloadData struct {
	ContentType string `json:"content_type"`
	SavedPath   string `json:"saved_path"`
	SizeBytes   int64  `json:"size_bytes"`
}

// DownloadAsset downloads the asset identified by ref.RemoteKey (a Feishu asset
// token) into destPath by shelling out to `lark-cli docs +media-download`.
//
// media-download requires --output to be a relative path inside the working
// directory, so the download runs with the process directory set to destPath's
// parent. It also auto-appends a detected file extension to --output, so the
// file lands at a different path (data.saved_path); this method moves it onto
// the exact destPath requested. lark-cli resolves a fresh download URL
// internally, so an expired href in ref.URL is irrelevant. Paced by the
// dedicated asset limiter and retried with backoff on rate-limit errors.
func (a *Adapter) DownloadAsset(ctx context.Context, ref adapter.AssetRef, destPath string) error {
	dir := filepath.Dir(destPath)
	base := filepath.Base(destPath)
	return a.backoff.Retry(ctx, isRateLimited, func() (time.Duration, error) {
		if err := a.assetLimiter.Wait(ctx); err != nil {
			return 0, err
		}
		out, err := a.run.RunInDir(ctx, dir,
			"docs", "+media-download",
			"--token", ref.RemoteKey,
			"--output", base,
			"--overwrite",
			"--format", "json")
		if err != nil {
			return retryAfter(err), err
		}
		// media-download reports success in an {ok:true,...} envelope; surface an
		// API-level failure (e.g. permission) as an error so it degrades to
		// pending rather than leaving a truncated/empty file behind.
		data, err := unwrap(out)
		if err != nil {
			return retryAfter(err), fmt.Errorf("media-download %s: %w", ref.RemoteKey, err)
		}
		var md mediaDownloadData
		if err := json.Unmarshal(data, &md); err != nil {
			return 0, fmt.Errorf("decode media-download %s: %w", ref.RemoteKey, err)
		}
		if md.SavedPath == "" {
			return 0, fmt.Errorf("media-download %s: no saved_path in response", ref.RemoteKey)
		}
		// Move the extension-appended file onto destPath (the engine owns
		// content addressing).
		if md.SavedPath != destPath {
			if err := os.Rename(md.SavedPath, destPath); err != nil {
				return 0, fmt.Errorf("move downloaded asset %s: %w", ref.RemoteKey, err)
			}
		}
		return 0, nil
	})
}
