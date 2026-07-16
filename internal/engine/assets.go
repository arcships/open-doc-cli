package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/manifest"
)

// assetPendingMarker is appended immediately after an image whose asset could
// not be downloaded, so the lost-but-traceable link is visible.
const assetPendingMarker = "<!-- opendoc:asset-pending -->"

// assetOutcome summarizes the asset work done for one document.
type assetOutcome struct {
	body       string // body with asset links rewritten / pending markers added
	downloaded int    // assets fetched this pass
	pending    int    // assets left pending (download failed) this pass
}

// processAssets downloads every asset the document references (deduping by
// remote_key so an asset is never fetched twice, even across documents),
// rewrites each body image URL to a local relative path, and appends a pending
// marker where a download failed. It never fails the document: a failed
// download leaves the original URL plus marker and records the asset pending
// for a later retry.
func (e *Engine) processAssets(ctx context.Context, a adapter.Adapter, db *manifest.DB, bodyPath, body string, assets []adapter.AssetRef) (assetOutcome, error) {
	out := assetOutcome{body: body}
	seen := map[string]bool{}
	for _, ref := range assets {
		if ref.RemoteKey == "" || seen[ref.RemoteKey] {
			continue
		}
		seen[ref.RemoteKey] = true

		// Dedup: a completed asset whose file is still on disk is reused as-is,
		// with no re-download (same asset in N docs → one file, one fetch).
		var localPath string
		if row, found, err := db.GetAsset(ref.RemoteKey); err != nil {
			return out, err
		} else if found && row.Status == statusAssetDone && row.LocalPath != "" &&
			fileExists(filepath.Join(e.layout.Root, row.LocalPath)) {
			localPath = row.LocalPath
		} else {
			rel, derr := e.downloadAsset(ctx, a, db, ref)
			if derr != nil {
				// A ctx cancellation aborts the run; any other failure degrades
				// to pending (keep the original URL + marker) for a later retry.
				if ctx.Err() != nil {
					return out, ctx.Err()
				}
				if err := db.MarkAssetPending(ref.RemoteKey); err != nil {
					return out, err
				}
				out.pending++
				if ref.BodyURL != "" {
					out.body = appendPendingMarker(out.body, ref.BodyURL)
				}
				continue
			}
			localPath = rel
			out.downloaded++
		}

		if ref.BodyURL != "" {
			out.body = strings.ReplaceAll(out.body, ref.BodyURL, relAsset(bodyPath, localPath))
		}
	}
	return out, nil
}

// downloadAsset downloads ref to a temp file, content-addresses it, moves it
// into the global pool at assets/<sha[:2]>/<sha><ext>, and records the manifest
// row. Two assets with identical bytes collapse onto one file (content dedup on
// top of the remote_key dedup).
func (e *Engine) downloadAsset(ctx context.Context, a adapter.Adapter, db *manifest.DB, ref adapter.AssetRef) (string, error) {
	tmpDir := filepath.Join(e.layout.Internal, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(tmpDir, "asset-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpName) }()

	if err := a.DownloadAsset(ctx, ref, tmpName); err != nil {
		return "", err
	}
	data, err := os.ReadFile(tmpName)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("asset %s downloaded empty", ref.RemoteKey)
	}

	sum := sha256.Sum256(data)
	hexsum := hex.EncodeToString(sum[:])
	ext := pickExt(ref.Filename, data)
	rel := contentPath(hexsum, ext)
	abs := filepath.Join(e.layout.Root, rel)

	if !fileExists(abs) {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return "", err
		}
		if err := os.Rename(tmpName, abs); err != nil {
			return "", err
		}
	}
	if err := db.MarkAssetDone(ref.RemoteKey, hexsum, rel); err != nil {
		return "", err
	}
	return rel, nil
}

// contentPath is the pool-relative path for a content hash: the first two hex
// characters bucket the file, the full hash (plus extension) names it.
func contentPath(hexsum, ext string) string {
	return filepath.ToSlash(filepath.Join("assets", hexsum[:2], hexsum+ext))
}

// relAsset computes the relative link from the document at bodyPath (both paths
// are relative to the mirror root, forward-slashed) to the asset at assetPath.
// The link is relative to the document's directory so it resolves from the
// file's own location.
func relAsset(bodyPath, assetPath string) string {
	dir := filepath.Dir(filepath.FromSlash(bodyPath))
	rel, err := filepath.Rel(dir, filepath.FromSlash(assetPath))
	if err != nil {
		// Fall back to a root-anchored path; never emit an absolute local path.
		return filepath.ToSlash(assetPath)
	}
	return filepath.ToSlash(rel)
}

// pickExt chooses a file extension for the pool filename. Feishu's name/mime
// attributes are unreliable, so a conclusive sniff of the actual bytes wins;
// the filename's own extension is only a fallback for content that cannot be
// sniffed (office documents, plain text, etc.).
func pickExt(filename string, data []byte) string {
	if ext := sniffExt(data); ext != "" {
		return ext
	}
	if ext := filepath.Ext(filename); ext != "" && len(ext) <= 6 {
		return strings.ToLower(ext)
	}
	return ""
}

// sniffExt maps the detected content type of data to a file extension, or "".
// It deliberately does not map application/zip: office formats (docx/xlsx/pptx)
// sniff as zip, so those are better served by the filename extension fallback.
func sniffExt(data []byte) string {
	ct := http.DetectContentType(data)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	switch strings.TrimSpace(ct) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/vnd.microsoft.icon", "image/x-icon":
		return ".ico"
	case "application/pdf":
		return ".pdf"
	case "image/svg+xml":
		return ".svg"
	default:
		return ""
	}
}

// appendPendingMarker appends the pending marker immediately after each markdown
// image that references bodyURL, so the failed download stays visible without
// corrupting the image syntax.
func appendPendingMarker(body, bodyURL string) string {
	re := regexp.MustCompile(`!\[[^\]]*\]\(` + regexp.QuoteMeta(bodyURL) + `[^)]*\)`)
	return re.ReplaceAllStringFunc(body, func(img string) string {
		if strings.Contains(body, img+assetPendingMarker) {
			return img // already marked
		}
		return img + assetPendingMarker
	})
}
