package notion

import (
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/arcships/open-doc-cli/internal/adapter"
)

// Extraction pulls the asset references and internal document links out of a
// Notion markdown body. Notion enhanced markdown mixes standard markdown with a
// small set of HTML-ish tags; the two opendoc cares about here are inline images
// (S3 signed URLs) and <page>/<database> references (internal links).

var (
	// mdImageRe matches a markdown image ![alt](url) and captures the URL.
	mdImageRe = regexp.MustCompile(`!\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	// pageTagRe matches a <page url="..."> reference and captures the URL.
	pageTagRe = regexp.MustCompile(`(?s)<page\b[^>]*\burl="([^"]*)"[^>]*>`)
	// dbTagRe matches a whole <database ...> opening tag.
	dbTagRe = regexp.MustCompile(`(?s)<database\b[^>]*>`)
	// attrRe extracts key="value" attributes from a tag.
	attrRe = regexp.MustCompile(`([a-zA-Z_:-]+)="([^"]*)"`)
	// notionIDRe captures the trailing 32-hex ID from a notion URL.
	notionIDRe = regexp.MustCompile(`([0-9a-fA-F]{32})`)
	// dataSourceRe captures the id from a collection:// data-source URL.
	dataSourceRe = regexp.MustCompile(`collection://([0-9a-fA-F-]{32,36})`)
)

// isAssetURL reports whether a markdown image URL points at a downloadable
// Notion-hosted file (an S3 signed URL or the notion file host) rather than an
// external image opendoc should leave alone.
func isAssetURL(u *url.URL) bool {
	host := strings.ToLower(u.Host)
	switch {
	case strings.Contains(host, "amazonaws.com"):
		return true
	case strings.Contains(host, "secure.notion-static.com"):
		return true
	case strings.Contains(host, "prod-files-secure"):
		return true
	case strings.HasSuffix(host, "file.notion.so"):
		return true
	}
	return false
}

// remoteKey derives the stable dedupe key for a Notion asset: the URL path
// without the (short-lived, signed) query string — workspace_id/file_uuid/name
// (P0-verified stable). The leading slash is trimmed for a clean key.
func remoteKey(u *url.URL) string {
	return strings.TrimPrefix(u.Path, "/")
}

// extractAssets returns one AssetRef per inline Notion image, deduped by remote
// key. RemoteKey is the query-stripped path (download + dedupe key), URL and
// BodyURL are the full signed URL (fetched immediately; BodyURL is the exact
// body substring the engine rewrites to a local path), and Filename is the last
// path segment.
func extractAssets(md string) []adapter.AssetRef {
	var refs []adapter.AssetRef
	seen := map[string]bool{}
	for _, m := range mdImageRe.FindAllStringSubmatch(md, -1) {
		raw := m[1]
		u, err := url.Parse(raw)
		if err != nil || !isAssetURL(u) {
			continue
		}
		key := remoteKey(u)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, adapter.AssetRef{
			RemoteKey: key,
			URL:       raw,
			BodyURL:   raw,
			Filename:  path.Base(u.Path),
		})
	}
	return refs
}

// extractLinks returns the internal document references in the body: <page>
// children (target = the page ID) and <database> references (target = the data
// source ID, which is the database node's manifest id). Targets are canonical
// hyphenated IDs so they match enumeration; duplicates are collapsed. External
// or unresolved URLs are simply not captured.
func extractLinks(md string) []adapter.DocRef {
	var links []adapter.DocRef
	seen := map[string]bool{}
	add := func(target, raw string) {
		if target == "" || seen[target] {
			return
		}
		seen[target] = true
		links = append(links, adapter.DocRef{TargetID: target, RawURL: raw})
	}

	for _, m := range pageTagRe.FindAllStringSubmatch(md, -1) {
		raw := m[1]
		if id := notionIDRe.FindString(raw); id != "" {
			add(canonicalID(id), raw)
		}
	}
	for _, tag := range dbTagRe.FindAllString(md, -1) {
		attrs := parseAttrs(tag)
		raw := attrs["url"]
		// Prefer the data-source id (the database node's manifest id); it is the
		// stable link target the row pages and enumeration agree on.
		if ds := dataSourceRe.FindStringSubmatch(attrs["data-source-url"]); ds != nil {
			add(canonicalID(ds[1]), raw)
		}
	}
	return links
}

// parseAttrs extracts key="value" attributes from a single tag.
func parseAttrs(tag string) map[string]string {
	m := map[string]string{}
	for _, kv := range attrRe.FindAllStringSubmatch(tag, -1) {
		m[kv[1]] = kv[2]
	}
	return m
}
