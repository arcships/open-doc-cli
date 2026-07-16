package cli

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/arcships/open-doc-cli/internal/layout"
	"github.com/arcships/open-doc-cli/internal/manifest"
)

// resolveResult is the machine-friendly `opendoc resolve` payload: the matched
// document's online identity and local mirror location.
type resolveResult struct {
	Platform  string `json:"platform"`
	ID        string `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	URL       string `json:"url,omitempty"`
	LocalPath string `json:"local_path,omitempty"`
	Status    string `json:"status"`
	// MatchedBy records which input form matched: id | alias | url | local_path.
	MatchedBy string `json:"matched_by"`
}

// feishuDocURLRe mirrors engine.docURLRe: a Feishu/Lark document URL, capturing
// the trailing token (obj_token for /docx//docs/, node_token for /wiki/, or the
// resource token for the typed surfaces). Kept local so the cli package stays
// decoupled from the engine's internals.
var feishuDocURLRe = regexp.MustCompile(
	`https?://[A-Za-z0-9.\-]+\.(?:feishu\.cn|larksuite\.com|feishu\.net)/(?:docx|docs|wiki|sheets|base|bitable|file|minutes|slides|mindnotes)/([A-Za-z0-9]+)`)

// notionDocURLRe mirrors engine.notionURLRe: a Notion page/database URL,
// capturing the trailing 32-hex (dashless) id.
var notionDocURLRe = regexp.MustCompile(
	`https?://(?:www\.notion\.so|notion\.so|app\.notion\.com)/(?:p/)?(?:[^/\s"'<>)\]]*-)?([0-9a-fA-F]{32})`)

// runResolve implements `opendoc resolve <id|url|path>`: a bidirectional lookup
// between a document's online identity and its local mirror path, querying the
// manifest (documents + doc_aliases). Exit codes: 0 found, 1 not found, 2 usage.
func runResolve(env Env, args []string) int {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	root := fs.String("root", "", "mirror root (overrides OPENDOC_ROOT and ~/.opendoc)")
	asJSON := fs.Bool("json", false, "emit the result as JSON")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: opendoc resolve [flags] <id|url|path>\n\nLook up a document by platform id, online URL, or local path.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	// Extract the leading positional query before flag parsing (Go's flag package
	// stops at the first non-flag arg).
	query := ""
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		query = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	// A trailing positional (after flags) is also accepted, e.g. `resolve --json <q>`.
	if query == "" && len(fs.Args()) > 0 {
		query = fs.Args()[0]
		if len(fs.Args()) > 1 {
			fmt.Fprintf(env.Stderr, "opendoc resolve: too many arguments\n")
			return ExitUsage
		}
	} else if query != "" && len(fs.Args()) > 0 {
		fmt.Fprintf(env.Stderr, "opendoc resolve: too many arguments\n")
		return ExitUsage
	}
	query = strings.TrimSpace(query)
	if query == "" {
		fmt.Fprintf(env.Stderr, "opendoc resolve: missing <id|url|path> argument\n")
		return ExitUsage
	}

	l, err := layout.Resolve(*root)
	if err != nil {
		fmt.Fprintf(env.Stderr, "opendoc resolve: %v\n", err)
		return ExitError
	}
	if code := requireInitialized(env, l, "resolve"); code != -1 {
		return code
	}

	if !fileExists(l.ManifestPath()) {
		fmt.Fprintf(env.Stderr, "opendoc resolve: no manifest at %s (run opendoc sync first)\n", l.ManifestPath())
		return ExitError
	}
	db, err := manifest.Open(l.ManifestPath())
	if err != nil {
		fmt.Fprintf(env.Stderr, "opendoc resolve: %v\n", err)
		return ExitError
	}
	defer db.Close()

	doc, matchedBy, found, err := resolveDocument(db, l.Root, query)
	if err != nil {
		fmt.Fprintf(env.Stderr, "opendoc resolve: %v\n", err)
		return ExitError
	}
	if !found {
		fmt.Fprintf(env.Stderr, "opendoc resolve: no mirrored document matches %q\n", query)
		return ExitError
	}

	res := resolveResult{
		Platform:  doc.Platform,
		ID:        doc.ID,
		Type:      doc.Type,
		Title:     doc.Title,
		URL:       frontmatterURL(l.Root, doc.LocalPath),
		LocalPath: doc.LocalPath,
		Status:    doc.Status,
		MatchedBy: matchedBy,
	}

	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			fmt.Fprintf(env.Stderr, "opendoc resolve: %v\n", err)
			return ExitError
		}
		return ExitOK
	}
	printResolve(env, res)
	return ExitOK
}

// resolveDocument matches query against the manifest, trying (in order) URL
// extraction, local-path lookup, and bare-id/alias lookup. matchedBy names the
// form that matched.
func resolveDocument(db *manifest.DB, root, query string) (doc manifest.Document, matchedBy string, found bool, err error) {
	switch {
	case strings.Contains(query, "://"):
		// Online URL: extract the platform token(s) and resolve via id or alias.
		for _, tok := range urlTokens(query) {
			doc, found, err = lookupByIDOrAlias(db, tok)
			if err != nil || found {
				return doc, "url", found, err
			}
		}
		return manifest.Document{}, "", false, nil

	case looksLikePath(query):
		for _, cand := range pathCandidates(root, query) {
			doc, found, err = db.GetDocumentByLocalPath(cand)
			if err != nil || found {
				return doc, "local_path", found, err
			}
		}
		return manifest.Document{}, "", false, nil

	default:
		// Bare id or alias. Try the id-space (raw + Notion-canonical) then aliases
		// (raw + dashless), reporting which succeeded.
		for _, cand := range dedup([]string{query, canonicalNotionID(query)}) {
			doc, found, err = db.GetDocument(cand)
			if err != nil || found {
				return doc, "id", found, err
			}
		}
		for _, cand := range dedup([]string{query, notionDashless(query)}) {
			var docID string
			docID, found, err = db.DocIDForAlias(cand)
			if err != nil {
				return manifest.Document{}, "", false, err
			}
			if found {
				doc, found, err = db.GetDocument(docID)
				return doc, "alias", found, err
			}
		}
		return manifest.Document{}, "", false, nil
	}
}

// lookupByIDOrAlias resolves a token to a document by its id first, then via the
// alias table (any status), so a URL-extracted token matches whichever the
// manifest keys it under.
func lookupByIDOrAlias(db *manifest.DB, token string) (manifest.Document, bool, error) {
	if token == "" {
		return manifest.Document{}, false, nil
	}
	if doc, found, err := db.GetDocument(token); err != nil || found {
		return doc, found, err
	}
	docID, found, err := db.DocIDForAlias(token)
	if err != nil || !found {
		return manifest.Document{}, false, err
	}
	return db.GetDocument(docID)
}

// urlTokens extracts the candidate document tokens from an online URL: for Notion
// both the canonical hyphenated id and its dashless form (aliases key on the
// dashless id); for Feishu the raw path token (obj_token or wiki node_token).
func urlTokens(u string) []string {
	var toks []string
	if m := notionDocURLRe.FindStringSubmatch(u); m != nil {
		toks = append(toks, canonicalNotionID(m[1]), notionDashless(m[1]))
	}
	if m := feishuDocURLRe.FindStringSubmatch(u); m != nil {
		toks = append(toks, m[1])
	}
	return dedup(toks)
}

// looksLikePath reports whether query is a filesystem path rather than a bare id:
// it contains a path separator or ends in .md. Bare platform ids carry neither.
func looksLikePath(query string) bool {
	return strings.ContainsAny(query, "/\\") || strings.HasSuffix(query, ".md")
}

// pathCandidates normalises a local-path query into the forward-slashed,
// root-relative forms stored in documents.local_path. It handles absolute paths
// under the root, paths already relative to the root, and a leading "./".
func pathCandidates(root, query string) []string {
	var cands []string
	add := func(p string) {
		p = filepath.ToSlash(strings.TrimPrefix(filepath.ToSlash(p), "./"))
		if p != "" {
			cands = append(cands, p)
		}
	}
	if filepath.IsAbs(query) {
		if rel, err := filepath.Rel(root, query); err == nil && !strings.HasPrefix(rel, "..") {
			add(rel)
		}
	} else {
		add(query)
		// Also handle a path that redundantly repeats the root's base name or was
		// given relative to the current directory but still lives under the root.
		if abs, err := filepath.Abs(query); err == nil {
			if rel, rerr := filepath.Rel(root, abs); rerr == nil && !strings.HasPrefix(rel, "..") {
				add(rel)
			}
		}
	}
	return dedup(cands)
}

// frontmatterURL reads the online URL from a mirrored file's frontmatter `url:`
// field — the authoritative place opendoc stores each document's canonical URL
// (frontmatter.Doc.URL; the manifest does not hold URLs). It returns "" when the
// file is absent (e.g. a pending/trashed document) or has no url line.
func frontmatterURL(root, localPath string) string {
	if localPath == "" {
		return ""
	}
	abs := filepath.Join(root, filepath.FromSlash(localPath))
	f, err := os.Open(abs)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	fences := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "---" {
			fences++
			if fences >= 2 {
				break // past the frontmatter block
			}
			continue
		}
		if fences != 1 {
			continue
		}
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "url:"); ok {
			val := strings.TrimSpace(rest)
			if unq, err := strconv.Unquote(val); err == nil {
				return unq
			}
			return strings.Trim(val, `"`)
		}
	}
	return ""
}

// canonicalNotionID normalises a Notion id (hyphenated or dashless, any case) to
// the canonical lowercase 8-4-4-4-12 UUID used as documents.id. Non-UUID input is
// returned trimmed/lowercased unchanged, so a Feishu token passes through untouched.
func canonicalNotionID(s string) string {
	h := notionDashless(s)
	if len(h) != 32 || !isHex32(h) {
		return strings.ToLower(strings.TrimSpace(s))
	}
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// notionDashless strips hyphens and lowercases, yielding the 32-hex form Notion
// URLs carry and aliases key on.
func notionDashless(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
}

// isHex32 reports whether s is all hexadecimal digits.
func isHex32(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return len(s) > 0
}

// dedup returns xs with empty strings and later duplicates removed, order preserved.
func dedup(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}

// printResolve renders the result as compact human/agent-readable lines.
func printResolve(env Env, r resolveResult) {
	fmt.Fprintf(env.Stdout, "platform:   %s\n", r.Platform)
	fmt.Fprintf(env.Stdout, "id:         %s\n", r.ID)
	fmt.Fprintf(env.Stdout, "type:       %s\n", r.Type)
	fmt.Fprintf(env.Stdout, "title:      %s\n", r.Title)
	if r.URL != "" {
		fmt.Fprintf(env.Stdout, "url:        %s\n", r.URL)
	}
	if r.LocalPath != "" {
		fmt.Fprintf(env.Stdout, "local_path: %s\n", r.LocalPath)
	}
	fmt.Fprintf(env.Stdout, "status:     %s\n", r.Status)
	fmt.Fprintf(env.Stdout, "matched_by: %s\n", r.MatchedBy)
}
