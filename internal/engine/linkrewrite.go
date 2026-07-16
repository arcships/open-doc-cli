package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/arcships/open-doc-cli/internal/manifest"
)

// docURLRe matches a Feishu/Lark document URL as it appears in a mirrored
// Markdown body and captures the target token. It mirrors the URL surfaces the
// Feishu adapter extracts (see internal/feishu/extract.go feishuDocRe): /docx/
// and legacy /docs/ carry the obj_token directly, /wiki/ carries the node_token
// (resolved via the alias table), and the resource surfaces (sheets/base/...)
// name independent resource nodes that may also be mirrored. The trailing class
// consumes any query string/anchor so the whole URL — not just the token — is
// replaced.
var docURLRe = regexp.MustCompile(
	`https?://[A-Za-z0-9.\-]+\.(?:feishu\.cn|larksuite\.com|feishu\.net)/(?:docx|docs|wiki|sheets|base|bitable|file|minutes|slides|mindnotes)/([A-Za-z0-9]+)[^)\s"'<>\]]*`)

// notionURLRe matches a Notion page/database URL as it appears in a mirrored
// body (the <page url> and <database url> tags emitted by the markdown endpoint,
// plus plain notion.so links) and captures the trailing 32-hex ID. Notion URLs
// carry the dashless ID, whereas manifest documents.id is the canonical
// hyphenated UUID, so the captured token is resolved through the alias table
// (each Notion node records its dashless ID as an alias). A leading slug segment
// ("Title-<32hex>") is consumed by the optional non-capturing prefix.
var notionURLRe = regexp.MustCompile(
	`https?://(?:www\.notion\.so|notion\.so|app\.notion\.com)/(?:p/)?(?:[^/\s"'<>)\]]*-)?([0-9a-fA-F]{32})[^)\s"'<>\]]*`)

// rewriteLinks is the phase-2 internal-link finalize step, run after
// every document of the platform has been written. It scans the whole links
// table — not just this run's documents — so a link whose target arrived in a
// later run is still rewritten even though its source was skipped this run. For
// each source document it replaces every platform doc URL whose target resolves
// to a mirrored active document with the correct relative path, rewriting the
// file atomically. The content_hash is deliberately not touched (it hashes the
// raw fetch product), so a rewrite never makes the document look dirty next run.
func (e *Engine) rewriteLinks(db *manifest.DB, platform string, stats *Stats) error {
	fromIDs, err := db.DistinctLinkFromIDs()
	if err != nil {
		return err
	}
	resolve := func(token string) (string, bool) {
		lp, ok, rerr := db.ResolveLinkTarget(token)
		if rerr != nil {
			return "", false
		}
		return lp, ok
	}

	for _, fromID := range fromIDs {
		doc, found, err := db.GetDocument(fromID)
		if err != nil {
			return err
		}
		// Only rewrite active, this-platform documents that own a body file.
		if !found || doc.Status != statusActive || doc.Platform != platform || doc.LocalPath == "" {
			continue
		}
		abs := filepath.Join(e.layout.Root, filepath.FromSlash(doc.LocalPath))
		if !fileExists(abs) {
			continue
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			stats.Failures = append(stats.Failures, fmt.Sprintf("%s: read for link rewrite: %v", fromID, err))
			continue
		}
		head, body := splitAfterFrontmatter(string(raw))
		newBody, n := rewriteBodyLinks(body, doc.LocalPath, resolve)
		if n == 0 || newBody == body {
			continue
		}
		if err := atomicWrite(abs, []byte(head+newBody)); err != nil {
			stats.Failures = append(stats.Failures, fmt.Sprintf("%s: write link rewrite: %v", fromID, err))
			continue
		}
		stats.LinksRewritten += n
	}
	return nil
}

// rewriteBodyLinks replaces every Feishu/Notion doc URL in body whose target
// resolves (via resolve) to a mirrored document with the relative path from the
// source document at fromPath to that target. Unresolved URLs
// (external/unauthorized) are left untouched. It returns the rewritten body and
// the number of URLs replaced.
func rewriteBodyLinks(body, fromPath string, resolve func(token string) (string, bool)) (string, int) {
	count := 0
	repl := func(re *regexp.Regexp, s string) string {
		return re.ReplaceAllStringFunc(s, func(match string) string {
			sub := re.FindStringSubmatch(match)
			if sub == nil {
				return match
			}
			target, ok := resolve(sub[1])
			if !ok {
				return match
			}
			count++
			// relAsset computes the path relative to the source document's directory,
			// which is exactly what an internal link needs.
			return relAsset(fromPath, target)
		})
	}
	out := repl(docURLRe, body)
	out = repl(notionURLRe, out)
	return out, count
}

// splitAfterFrontmatter splits content into its leading YAML frontmatter block
// (including the closing fence and its trailing newline) and the remaining body.
// Only the body is scanned for link rewriting, so the frontmatter `url:` field —
// itself a platform doc URL — is never rewritten. When content has no
// frontmatter the whole string is treated as body.
func splitAfterFrontmatter(content string) (head, body string) {
	const fence = "---\n"
	if !strings.HasPrefix(content, fence) {
		return "", content
	}
	rest := content[len(fence):]
	idx := strings.Index(rest, "\n"+fence)
	if idx < 0 {
		return "", content
	}
	end := len(fence) + idx + len("\n"+fence)
	return content[:end], content[end:]
}
