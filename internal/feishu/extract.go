package feishu

import (
	"html"
	"regexp"

	"github.com/arcships/open-doc-cli/internal/adapter"
)

// Asset and link extraction operates on both forms of a fetched document. The
// XML form (lark-cli --doc-format xml) carries the stable asset token in the
// <img src="TOKEN"> attribute (the href is a short-lived authcode URL) and the
// whiteboard token in <whiteboard token="..."> ; the markdown form carries the
// body-facing image URL that must be rewritten to a local path. The two forms
// are separate fetches, so their per-image authcode URLs differ — the markdown
// image URL cannot be string-matched to an XML href. They are instead
// correlated positionally: the Nth markdown image corresponds to the Nth XML
// <img> (lark-cli renders the same block tree in the same order into both
// formats). Extraction is regex-based: the XML is a flat, well-formed fragment
// and a full parser buys nothing here while costing robustness against Feishu's
// ad-hoc tag shapes.

var (
	// imgTagRe matches a self-contained <img .../> or <img ...> tag.
	imgTagRe = regexp.MustCompile(`(?s)<img\b[^>]*>`)
	// attrRe extracts key="value" attributes from a tag.
	attrRe = regexp.MustCompile(`([a-zA-Z_-]+)="([^"]*)"`)
	// hrefRe matches href="value" occurrences (for <a> links).
	hrefRe = regexp.MustCompile(`href="([^"]*)"`)
	// feishuDocRe matches a Feishu/Lark document URL and captures its object
	// token. Covers the common doc surfaces; unknown surfaces are ignored.
	feishuDocRe = regexp.MustCompile(`https?://[^/"\s]+\.(?:feishu\.cn|larksuite\.com|feishu\.net)/(?:docx|docs|wiki|sheets|base|file|minutes|slides|mindnotes)/([A-Za-z0-9]+)`)
	// mdImageRe matches a markdown image ![alt](url) and captures the URL.
	mdImageRe = regexp.MustCompile(`!\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	// whiteboardRe matches a <whiteboard ...> opening tag and captures its token.
	whiteboardRe = regexp.MustCompile(`<whiteboard\b[^>]*\btoken="([^"]*)"[^>]*>`)
)

// imgTag holds the attributes of one <img> in fetch order.
type imgTag struct {
	src  string // stable asset token (RemoteKey)
	href string // short-lived authcode URL
	name string // suggested filename
}

// parseImages returns every <img> in the XML body, in document order.
func parseImages(xml string) []imgTag {
	var imgs []imgTag
	for _, tag := range imgTagRe.FindAllString(xml, -1) {
		var t imgTag
		for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
			switch m[1] {
			case "src":
				t.src = m[2]
			case "href":
				t.href = html.UnescapeString(m[2])
			case "name":
				t.name = m[2]
			}
		}
		imgs = append(imgs, t)
	}
	return imgs
}

// extractAssets returns the deduped asset references embedded in the XML body.
// The RemoteKey is the stable <img src> token; the URL is the short-lived href.
// It carries no BodyURL (no markdown context); buildAssetRefs is used when the
// body-facing URL is needed for rewriting.
func extractAssets(xml string) []adapter.AssetRef {
	var assets []adapter.AssetRef
	seen := map[string]bool{}
	for _, t := range parseImages(xml) {
		if t.src == "" || seen[t.src] {
			continue
		}
		seen[t.src] = true
		assets = append(assets, adapter.AssetRef{RemoteKey: t.src, URL: t.href, Filename: t.name})
	}
	return assets
}

// buildAssetRefs correlates the markdown body's image URLs with the XML's asset
// tokens positionally, returning one AssetRef per markdown image whose token is
// known. RemoteKey is the stable token (the download + dedupe key), BodyURL is
// the exact markdown URL to rewrite, URL is the (unused) authcode href, and
// Filename is the suggested name. Markdown images beyond the XML image count
// (no token to download with) are skipped; the extra body URL is simply left
// untouched by the engine.
func buildAssetRefs(md, xml string) []adapter.AssetRef {
	imgs := parseImages(xml)
	var refs []adapter.AssetRef
	i := 0
	for _, m := range mdImageRe.FindAllStringSubmatch(md, -1) {
		bodyURL := html.UnescapeString(m[1])
		if i >= len(imgs) {
			break
		}
		t := imgs[i]
		i++
		if t.src == "" {
			continue
		}
		refs = append(refs, adapter.AssetRef{
			RemoteKey: t.src,
			URL:       t.href,
			BodyURL:   bodyURL,
			Filename:  t.name,
		})
	}
	return refs
}

// extractWhiteboardTokens returns the whiteboard tokens in the XML body, in
// document order. They are correlated positionally with the ```mermaid blocks
// lark-cli emits into the markdown body.
func extractWhiteboardTokens(xml string) []string {
	var toks []string
	for _, m := range whiteboardRe.FindAllStringSubmatch(xml, -1) {
		toks = append(toks, m[1])
	}
	return toks
}

// extractLinks returns the document-to-document references in the XML body,
// keyed by the target's platform token. Only Feishu/Lark doc URLs are captured;
// external links are left alone. Duplicates (same target) are collapsed.
func extractLinks(xml string) []adapter.DocRef {
	var links []adapter.DocRef
	seen := map[string]bool{}
	for _, m := range hrefRe.FindAllStringSubmatch(xml, -1) {
		raw := html.UnescapeString(m[1])
		sub := feishuDocRe.FindStringSubmatch(raw)
		if sub == nil {
			continue
		}
		target := sub[1]
		if seen[target] {
			continue
		}
		seen[target] = true
		links = append(links, adapter.DocRef{TargetID: target, RawURL: raw})
	}
	return links
}
