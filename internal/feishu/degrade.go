package feishu

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/arcships/open-doc-cli/internal/adapter"
)

// The degradation contract turns lossy resource blocks in the fetched
// markdown into a form that keeps content, drill-down IDs, and online links
// visible, and counts every loss for the sync report. It operates on the
// lark-cli markdown body, which is a hybrid of standard markdown plus HTML-ish
// tags for blocks that have no markdown equivalent (callout, table, sheet,
// base_refer, bitable) and ```mermaid fences for whiteboards.
//
// recognizedTags is the explicit set of tag names opendoc understands — standard
// markdown/HTML structure plus the Feishu block tags it either renders or
// knowingly passes through. Any opening tag whose name is not in this set is an
// "unknown block": it is preserved verbatim and counted (the red line — never
// silently drop). Keep this list in sync with references/degradation-tags.md.
var recognizedTags = map[string]bool{
	// Feishu block tags opendoc handles or passes through intentionally.
	"callout": true, "sheet": true, "base_refer": true, "bitable": true,
	"whiteboard": true, "img": true,
	// Standard HTML structure lark-cli emits (tables, links, inline styling).
	"a": true, "title": true, "table": true, "thead": true, "tbody": true,
	"tr": true, "td": true, "th": true, "colgroup": true, "col": true,
	"p": true, "div": true, "span": true, "br": true, "hr": true,
	"b": true, "i": true, "u": true, "s": true, "strong": true, "em": true,
	"code": true, "pre": true, "blockquote": true, "mark": true, "del": true,
	"ins": true, "sub": true, "sup": true, "ul": true, "ol": true, "li": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"iframe": true,
}

var (
	// openTagRe matches an opening or self-closing tag (not a closing </tag>) and
	// captures its name, so each block is counted once. It is applied only to
	// text outside code spans/fences and outside HTML comments.
	openTagRe = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9_-]*)\b[^>]*>`)
	// baseReferRe / bitableRe match a whole base_refer/bitable element (opening
	// tag plus its optional immediate closing tag).
	baseReferRe = regexp.MustCompile(`(?s)<base_refer\b[^>]*>(?:\s*</base_refer>)?`)
	bitableRe   = regexp.MustCompile(`(?s)<bitable\b[^>]*>(?:\s*</bitable>)?`)
	// mermaidFenceRe matches the opening fence of a mermaid code block at the
	// start of a line.
	mermaidFenceRe = regexp.MustCompile("(?m)^```mermaid[ \t]*$")
)

// degrade applies the degradation contract to the markdown body and returns the
// transformed body plus the degradation counts. It never fails the document:
// bitable API errors degrade to a tag-in-comment + link and are counted.
func (a *Adapter) degrade(ctx context.Context, md, xml, docURL string) (string, adapter.Degradation) {
	var deg adapter.Degradation

	// 1. Count unknown blocks on the original body (before we add our own
	//    comments/renders), skipping code and existing comments.
	deg.UnknownBlocks = countUnknownBlocks(md)

	// 2. Annotate each ```mermaid whiteboard with its token comment.
	body := annotateWhiteboards(md, extractWhiteboardTokens(xml))

	// 3. Render bitables (base_refer references an existing app; bitable is one
	//    created inline). Both drill down through the same API.
	body = a.transformBitables(ctx, body, docURL, &deg)

	return body, deg
}

// countUnknownBlocks counts opening tags whose name is not recognized, ignoring
// anything inside fenced/inline code or HTML comments so code samples and our
// own annotations are never miscounted.
func countUnknownBlocks(md string) int {
	scan := blankOutNoScanRegions(md)
	count := 0
	for _, m := range openTagRe.FindAllStringSubmatch(scan, -1) {
		if !recognizedTags[strings.ToLower(m[1])] {
			count++
		}
	}
	return count
}

// blankOutNoScanRegions replaces the contents of fenced code blocks, inline code
// spans, and HTML comments with spaces (preserving length and newlines) so the
// tag scanner only sees prose. The returned string is used for scanning only,
// never emitted.
func blankOutNoScanRegions(s string) string {
	b := []byte(s)
	blank := func(start, end int) {
		for i := start; i < end && i < len(b); i++ {
			if b[i] != '\n' {
				b[i] = ' '
			}
		}
	}
	// Fenced code blocks: ``` ... ``` (line-based).
	fenceOpen := -1
	lineStart := 0
	for i := 0; i <= len(b); i++ {
		if i == len(b) || b[i] == '\n' {
			line := string(b[lineStart:i])
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				if fenceOpen < 0 {
					fenceOpen = lineStart
				} else {
					blank(fenceOpen, i)
					fenceOpen = -1
				}
			}
			lineStart = i + 1
		}
	}
	// Inline code spans: `...` (single line).
	inTick := -1
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '`':
			if inTick < 0 {
				inTick = i
			} else {
				blank(inTick+1, i)
				inTick = -1
			}
		case '\n':
			inTick = -1
		}
	}
	// HTML comments: <!-- ... -->
	from := 0
	for {
		rel := strings.Index(string(b[from:]), "<!--")
		if rel < 0 {
			break
		}
		start := from + rel
		endRel := strings.Index(string(b[start:]), "-->")
		if endRel < 0 {
			break
		}
		end := start + endRel + len("-->")
		blank(start, end)
		from = end
	}
	return string(b)
}

// annotateWhiteboards inserts an opendoc:whiteboard token comment on the line before
// each ```mermaid fence, pairing the i-th fence with the i-th token. Fences
// beyond the available tokens are left unannotated (the mermaid source is still
// preserved — the token is a nice-to-have for drill-down).
func annotateWhiteboards(md string, tokens []string) string {
	if len(tokens) == 0 {
		return md
	}
	idx := 0
	return mermaidFenceRe.ReplaceAllStringFunc(md, func(fence string) string {
		if idx >= len(tokens) {
			return fence
		}
		tok := tokens[idx]
		idx++
		return fmt.Sprintf("<!-- opendoc:whiteboard token=%q -->\n%s", tok, fence)
	})
}

// transformBitables replaces each base_refer/bitable element with its original
// tag preserved in an HTML comment followed by the rendered table (small) or
// schema + row count (oversize) or just the online link (fetch failed),
// updating the degradation counts.
func (a *Adapter) transformBitables(ctx context.Context, body, docURL string, deg *adapter.Degradation) string {
	repl := func(tag string) string {
		attrs := parseTagAttrs(tag)
		appToken := attrs["token"]
		tableID := attrs["table-id"]
		comment := "<!-- opendoc:base_refer " + strings.TrimSpace(tagAttrString(attrs)) + " -->"
		link := bitableURL(docURL, appToken, tableID, attrs["view-id"])

		if appToken == "" || tableID == "" {
			// Not enough to drill down; keep the tag comment + link.
			deg.BitablesFailed++
			return joinBlock(comment, link)
		}
		bd, err := a.fetchBitable(ctx, appToken, tableID, a.bitableMaxRow)
		if err != nil {
			deg.BitablesFailed++
			return joinBlock(comment, fmt.Sprintf("> Failed to fetch the bitable; open it online to view. %s", link))
		}
		if bd.total > a.bitableMaxRow {
			deg.BitablesOversize++
			return joinBlock(comment, renderBitableSchema(bd), link)
		}
		deg.BitablesRendered++
		return joinBlock(comment, renderBitableTable(bd), link)
	}
	body = baseReferRe.ReplaceAllStringFunc(body, repl)
	body = bitableRe.ReplaceAllStringFunc(body, repl)
	return body
}

// joinBlock joins non-empty parts with blank lines between them.
func joinBlock(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

// parseTagAttrs extracts key="value" attributes from a single tag.
func parseTagAttrs(tag string) map[string]string {
	m := map[string]string{}
	for _, kv := range attrRe.FindAllStringSubmatch(tag, -1) {
		m[kv[1]] = kv[2]
	}
	return m
}

// tagAttrString renders the drill-down attributes in a stable order for the
// preserved comment.
func tagAttrString(attrs map[string]string) string {
	var parts []string
	for _, k := range []string{"table-id", "token", "view-id"} {
		if v := attrs[k]; v != "" {
			parts = append(parts, fmt.Sprintf("%s=%q", k, v))
		}
	}
	return strings.Join(parts, " ")
}

// bitableURL builds an online link to the bitable, using the host of the
// document's own URL (the tenant host). Returns "" when the host is unknown.
func bitableURL(docURL, appToken, tableID, viewID string) string {
	if docURL == "" || appToken == "" {
		return ""
	}
	u, err := url.Parse(docURL)
	if err != nil || u.Host == "" {
		return ""
	}
	link := fmt.Sprintf("%s://%s/base/%s", u.Scheme, u.Host, appToken)
	q := url.Values{}
	if tableID != "" {
		q.Set("table", tableID)
	}
	if viewID != "" {
		q.Set("view", viewID)
	}
	if e := q.Encode(); e != "" {
		link += "?" + e
	}
	return fmt.Sprintf("[Open the bitable in Feishu](%s)", link)
}
