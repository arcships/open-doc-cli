package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/frontmatter"
)

// bodyHash is the content_hash of a fetch product: the sha256 of the raw body
// (pre link-rewrite, excluding frontmatter so the ever-changing synced timestamp
// never triggers a false dirty).
func bodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// assemble builds the full file contents: the frontmatter block followed by a
// blank line and the body.
func assemble(fm frontmatter.Doc, body string) string {
	return frontmatter.Render(fm) + "\n" + body + "\n"
}

// placeholderBody renders the body for an independent resource node (a wiki-tree
// node whose obj_type is sheet/bitable/slides/mindnote/file, not an embed).
// It is a lossy-but-traceable stub: a one-line explanation plus the drillable
// token and the online link, so the tree has no hole and the loss is never
// silent.
func placeholderBody(doc adapter.RemoteDoc) string {
	line := fmt.Sprintf("> Standalone resource node (%s): this content is not expanded in the mirror; drill down or open the original online to view it.", doc.Type)
	token := fmt.Sprintf("> Drill-down token: `%s`", doc.ID)
	body := line + "\n>\n" + token
	if doc.URL != "" {
		body += fmt.Sprintf("\n>\n> View online: %s", doc.URL)
	}
	return body
}

// parseEdited converts an RFC3339 EditedAt string to a time.Time, returning the
// zero time when empty or unparseable (frontmatter then renders `updated: null`).
func parseEdited(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// atomicWrite writes data to path via a temp file + rename, so a reader never
// observes a half-written document. Parent directories must exist.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".opendoc-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

// fileExists reports whether path is an existing regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
