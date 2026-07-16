package notion

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/arcships/open-doc-cli/internal/adapter"
)

// The degradation contract for Notion turns the two loss
// signals the markdown endpoint can report — a truncated body and
// unrecognized-block IDs — into visible in-file markers plus report counters,
// and preserves any unknown enhanced-markdown tag verbatim while counting it
// (the red line: never silently drop).

// recognizedTags is the set of Notion enhanced-markdown / HTML tags opendoc knows.
// Any opening tag whose name is not here is an "unknown block": preserved
// verbatim and counted. <page>, <database>, <synced_block> are recognized (they
// are meaningful references or pass-through, not unknown). Keep in sync with
// references/degradation-tags.md.
var recognizedTags = map[string]bool{
	"page": true, "database": true, "synced_block": true, "empty-block": true,
	"column": true, "columns": true, "column_list": true, "callout": true,
	"toggle": true, "table": true, "table_row": true,
	// Standard HTML structure the endpoint emits (inline styling, breaks, links).
	"a": true, "p": true, "div": true, "span": true, "br": true, "hr": true,
	"b": true, "i": true, "u": true, "s": true, "strong": true, "em": true,
	"code": true, "pre": true, "blockquote": true, "mark": true, "del": true,
	"ins": true, "sub": true, "sup": true, "ul": true, "ol": true, "li": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"thead": true, "tbody": true, "tr": true, "td": true, "th": true,
	"colgroup": true, "col": true, "img": true,
}

// openTagRe matches an opening or self-closing tag (not a closing </tag>) and
// captures its name. Applied only to text outside code spans/fences/comments.
var openTagRe = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9_-]*)\b[^>]*>`)

// truncatedMarker / unknownBlocksMarker are the in-file loss markers so the loss
// is visible in the mirrored document itself (faithful red line).
const truncatedMarker = "<!-- opendoc:truncated -->"

// degrade applies the Notion degradation contract to a fetched body. It counts
// unknown tags, records the truncated / unknown_block_ids signals, and prepends
// the corresponding HTML-comment markers so the loss is visible in-file. The raw
// markdown is otherwise passed through unchanged (faithful-first).
func degrade(md string, truncated bool, unknownBlockIDs []string) (string, adapter.Degradation) {
	var deg adapter.Degradation
	deg.UnknownBlocks = countUnknownBlocks(md)

	var markers []string
	if truncated {
		deg.TruncatedPages = 1
		markers = append(markers, truncatedMarker)
	}
	if len(unknownBlockIDs) > 0 {
		deg.UnknownBlockIDs = len(unknownBlockIDs)
		markers = append(markers, fmt.Sprintf("<!-- opendoc:unknown-blocks n=%q ids=%q -->",
			fmt.Sprint(len(unknownBlockIDs)), strings.Join(unknownBlockIDs, ",")))
	}

	if len(markers) == 0 {
		return md, deg
	}
	prefix := strings.Join(markers, "\n")
	if md == "" {
		return prefix + "\n", deg
	}
	return prefix + "\n" + md, deg
}

// countUnknownBlocks counts opening tags whose name is not recognized, ignoring
// anything inside fenced/inline code or HTML comments so code samples and opendoc's
// own markers are never miscounted.
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
// tag scanner only sees prose. The returned string is used for scanning only.
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
