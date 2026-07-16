package notion_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/engine"
	"github.com/arcships/open-doc-cli/internal/layout"
	"github.com/arcships/open-doc-cli/internal/notion"
)

// mockNotion answers the search, markdown, and asset endpoints from an in-memory
// fixture so a full Sync can run with no network. It reproduces the real API
// shapes (P0-verified): a search inventory, per-page markdown bodies, and an S3
// asset download.
type mockNotion struct {
	search   []string          // one JSON body per search page
	markdown map[string]string // path id (hyphenated) -> markdown endpoint JSON
	query    map[string]string // data_source id -> query endpoint JSON
	asset    []byte            // bytes served for any asset GET
	searchN  int
}

func (m *mockNotion) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	resp := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
	}
	switch {
	case req.URL.Path == "/v1/search":
		i := m.searchN
		if i >= len(m.search) {
			i = len(m.search) - 1
		}
		m.searchN++
		return resp(200, m.search[i]), nil
	case strings.HasPrefix(req.URL.Path, "/v1/pages/") && strings.HasSuffix(req.URL.Path, "/markdown"):
		id := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/v1/pages/"), "/markdown")
		body, ok := m.markdown[id]
		if !ok {
			return resp(404, `{"message":"not found"}`), nil
		}
		return resp(200, body), nil
	case strings.HasPrefix(req.URL.Path, "/v1/data_sources/") && strings.HasSuffix(req.URL.Path, "/query"):
		id := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/v1/data_sources/"), "/query")
		body, ok := m.query[id]
		if !ok {
			return resp(200, `{"object":"list","results":[],"has_more":false,"next_cursor":null}`), nil
		}
		return resp(200, body), nil
	default:
		// Asset download (S3 signed URL): serve the fixture bytes.
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(m.asset)))}, nil
	}
}

// mdResp builds a markdown-endpoint JSON response.
func mdResp(md string, truncated bool, unknownIDs string) string {
	ids := "[]"
	if unknownIDs != "" {
		ids = unknownIDs
	}
	tr := "false"
	if truncated {
		tr = "true"
	}
	b, _ := jsonMarshalString(md)
	return `{"object":"page_markdown","id":"x","markdown":` + b + `,"truncated":` + tr + `,"unknown_block_ids":` + ids + `}`
}

// jsonMarshalString returns a JSON-quoted string.
func jsonMarshalString(s string) (string, error) {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}

// tinyPNG is a minimal valid PNG so content sniffing yields a .png extension.
var tinyPNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00,
	0x0A, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49,
	0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestFullMirrorTreeOrphanLinkAsset exercises the whole pipeline against a fixture
// workspace: a nested page tree, a database placeholder with a row, an internal
// <page> link, an inline image, an empty page, and one orphan row. It asserts the
// resulting on-disk tree, the rewritten relative link, the downloaded asset, and
// the empty-page file.
func TestFullMirrorTreeOrphanLinkAsset(t *testing.T) {
	root := "decafbad-0000-4000-8000-00000000000b"
	s1 := "facefeed-0000-4000-8000-00000000000a"
	dsID := "c0ffee00-0000-4000-8000-00000000000e"
	dbID := "dbdbdbdb-0000-4000-8000-00000000000f"
	ads := "ab1e0000-0000-4000-8000-000000000010"
	empty := "ba5eba11-0000-4000-8000-000000000011"
	orphan := "beefcafe-0000-4000-8000-00000000000c"

	dashless := func(s string) string { return strings.ReplaceAll(s, "-", "") }

	search := notionSearch([]string{
		obj("page", root, "School", "workspace", "", ""),
		obj("page", s1, "Term 1", "page_id", root, ""),
		obj("page", empty, "Notes", "page_id", root, ""),
		dsObj(dsID, dbID, s1, "Subjects"),
		row(ads, dsID, dbID, "Advanced Topics"),
		row(orphan, "faded0ff-0000-4000-8000-00000000000d", dbID, "Behind the Prompt"),
	})

	// School links to Term 1 via a <page> tag → must be rewritten to a relative
	// path once both are mirrored.
	schoolMD := `<page url="https://app.notion.com/p/` + dashless(s1) + `">Term 1</page>`
	// Term 1 embeds an image (asset) and the Subjects database.
	s1MD := "Semester overview\n\n" +
		"![](https://prod-files-secure.s3.us-west-2.amazonaws.com/ws/uuid/pic.png?X-Amz-Signature=sig)\n\n" +
		`<database url="https://app.notion.com/p/` + dashless(dbID) + `" inline="true" data-source-url="collection://` + dsID + `">Subjects</database>`

	// The Subjects data source query yields the ADS row's properties (a select, a
	// status, and a checkbox), flattened into the row's frontmatter.
	adsProps := `{"状态":{"type":"status","status":{"name":"已完成"}},` +
		`"学期":{"type":"select","select":{"name":"Term 1"}},` +
		`"完成":{"type":"checkbox","checkbox":true},` +
		`"Name":{"type":"title","title":[{"plain_text":"Advanced Topics"}]}}`
	queryResp := `{"object":"list","results":[` +
		`{"object":"page","id":"` + ads + `","properties":` + adsProps + `}` +
		`],"has_more":false,"next_cursor":null}`

	m := &mockNotion{
		search: []string{search},
		asset:  tinyPNG,
		markdown: map[string]string{
			root:   mdResp(schoolMD, false, ""),
			s1:     mdResp(s1MD, false, ""),
			empty:  mdResp("", false, ""), // empty page: markdown "" is trusted
			ads:    mdResp("R-tree lecture notes.", false, ""),
			orphan: mdResp("Book review.", false, ""),
		},
		query: map[string]string{dsID: queryResp},
	}

	dir := t.TempDir()
	l := layout.For(dir)
	eng, err := engine.New(l, engine.Options{Adapters: []adapter.Adapter{notion.NewAdapter("tok", m)}})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Close()

	res, err := eng.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	// (a) Tree matches the hierarchy; the orphan lands under notion/_orphans/.
	mustExist := []string{
		"notion/School/README.md",
		"notion/School/Term 1/README.md",
		"notion/School/Term 1/Subjects/_index.md",          // db → directory + row index
		"notion/School/Term 1/Subjects/Advanced Topics.md", // db row (leaf)
		"notion/School/Notes.md",                           // empty page (leaf)
		"notion/_orphans/Behind the Prompt.md",             // orphan row
	}
	for _, rel := range mustExist {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("expected file missing: %s (%v)", rel, err)
		}
	}

	// (b) Empty page is a valid file with empty body and correct frontmatter.
	emptyFile := readFile(t, filepath.Join(dir, "notion/School/Notes.md"))
	if !strings.Contains(emptyFile, "source: \"notion\"") {
		t.Errorf("empty page frontmatter wrong:\n%s", emptyFile)
	}
	if _, body, _ := strings.Cut(emptyFile, "\n---\n"); strings.TrimSpace(body) != "" {
		t.Errorf("empty page should have an empty body, got %q", body)
	}

	// (c) The <page> link in School is rewritten to a working relative path.
	schoolFile := readFile(t, filepath.Join(dir, "notion/School/README.md"))
	_, schoolBody, _ := strings.Cut(schoolFile, "\n---\n")
	wantRel := "Term 1/README.md"
	if !strings.Contains(schoolBody, wantRel) {
		t.Errorf("internal link not rewritten to %q:\n%s", wantRel, schoolFile)
	}
	// The frontmatter url: field legitimately keeps the online URL; only the body
	// link must be rewritten off the notion URL.
	if strings.Contains(schoolBody, "app.notion.com") {
		t.Errorf("body notion URL should be gone after rewrite:\n%s", schoolBody)
	}
	// The rewritten target resolves on disk relative to the source document.
	if _, err := os.Stat(filepath.Join(dir, "notion/School", filepath.FromSlash(wantRel))); err != nil {
		t.Errorf("rewritten link does not resolve on disk: %v", err)
	}

	// (d) The image landed in the asset pool; the body points at a local path.
	s1File := readFile(t, filepath.Join(dir, "notion/School/Term 1/README.md"))
	if strings.Contains(s1File, "amazonaws.com") {
		t.Errorf("asset URL not rewritten to local path:\n%s", s1File)
	}
	if !strings.Contains(s1File, "assets/") {
		t.Errorf("asset link should point into the pool:\n%s", s1File)
	}
	if res.Stats.AssetsDownloaded != 1 {
		t.Errorf("AssetsDownloaded = %d, want 1", res.Stats.AssetsDownloaded)
	}

	// The database rendered as a directory with a generated `_index.md` row index
	// that links to the row's local file.
	indexFile := readFile(t, filepath.Join(dir, "notion/School/Term 1/Subjects/_index.md"))
	if !strings.Contains(indexFile, "opendoc:generated") {
		t.Errorf("_index.md missing generated marker:\n%s", indexFile)
	}
	if !strings.Contains(indexFile, "Advanced Topics.md") {
		t.Errorf("_index.md should link to the row file:\n%s", indexFile)
	}
	if strings.Contains(indexFile, "README.md") {
		t.Errorf("database should not have a README placeholder:\n%s", indexFile)
	}

	// (b) The db row's frontmatter carries the flattened properties (the title
	// property is omitted — it is already the frontmatter title).
	rowFile := readFile(t, filepath.Join(dir, "notion/School/Term 1/Subjects/Advanced Topics.md"))
	for _, want := range []string{"properties:", "状态: \"已完成\"", "学期: \"Term 1\"", "完成: \"true\""} {
		if !strings.Contains(rowFile, want) {
			t.Errorf("row frontmatter missing %q:\n%s", want, rowFile)
		}
	}
	if strings.Contains(rowFile, "Name:") {
		t.Errorf("title property should be omitted from properties:\n%s", rowFile)
	}

	// Rerun → nothing dirty: every doc skipped.
	res2, err := eng.Sync(context.Background())
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res2.Stats.Added != 0 || res2.Stats.Updated != 0 {
		t.Errorf("rerun should be clean: added=%d updated=%d", res2.Stats.Added, res2.Stats.Updated)
	}
	if res2.Stats.Skipped == 0 {
		t.Errorf("rerun should skip documents, skipped=%d", res2.Stats.Skipped)
	}
}

// TestTruncatedAndUnknownBlocksRecorded drives the truncated / unknown_block_ids
// path (unreachable with the real workspace) via a fixture, asserting the markers
// land in-file and the counters roll up into the sync stats.
func TestTruncatedAndUnknownBlocksRecorded(t *testing.T) {
	root := "decafbad-0000-4000-8000-00000000000b"
	search := notionSearch([]string{obj("page", root, "Big Page", "workspace", "", "")})
	m := &mockNotion{
		search:   []string{search},
		asset:    tinyPNG,
		markdown: map[string]string{root: mdResp("partial content", true, `["blk-a","blk-b"]`)},
	}
	dir := t.TempDir()
	eng, err := engine.New(layout.For(dir), engine.Options{Adapters: []adapter.Adapter{notion.NewAdapter("tok", m)}})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Close()
	res, err := eng.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Stats.TruncatedPages != 1 {
		t.Errorf("TruncatedPages = %d, want 1", res.Stats.TruncatedPages)
	}
	if res.Stats.UnknownBlockIDs != 2 {
		t.Errorf("UnknownBlockIDs = %d, want 2", res.Stats.UnknownBlockIDs)
	}
	file := readFile(t, filepath.Join(dir, "notion/Big Page.md"))
	if !strings.Contains(file, "opendoc:truncated") || !strings.Contains(file, "opendoc:unknown-blocks") {
		t.Errorf("loss markers missing from mirrored file:\n%s", file)
	}
}

// TestPropertyOnlyChangeDetected proves the content_hash folds in the row's
// properties: with an unchanged body but a changed property (and a bumped
// last_edited_time, as a real property edit produces), the row is counted
// updated; a rerun with nothing changed is skipped.
func TestPropertyOnlyChangeDetected(t *testing.T) {
	home := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	dsID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	dbID := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	rowID := "dddddddd-dddd-dddd-dddd-dddddddddddd"
	dashless := func(s string) string { return strings.ReplaceAll(s, "-", "") }

	rowObj := func(edited string) string {
		return `{"object":"page","id":"` + rowID + `","url":"https://app.notion.com/p/` + dashless(rowID) +
			`","last_edited_time":"` + edited + `","parent":{"type":"data_source_id","data_source_id":"` + dsID +
			`","database_id":"` + dbID + `"},"properties":{"Name":{"type":"title","title":[{"plain_text":"Row"}]}}}`
	}
	searchWith := func(rowEdited string) string {
		return notionSearch([]string{
			obj("page", home, "DB Home", "workspace", "", ""),
			dsObj(dsID, dbID, home, "Subjects"),
			rowObj(rowEdited),
		})
	}
	queryWith := func(status string) string {
		props := `{"状态":{"type":"status","status":{"name":"` + status + `"}},` +
			`"Name":{"type":"title","title":[{"plain_text":"Row"}]}}`
		return `{"object":"list","results":[{"object":"page","id":"` + rowID +
			`","properties":` + props + `}],"has_more":false,"next_cursor":null}`
	}

	m := &mockNotion{
		asset:  tinyPNG,
		search: []string{searchWith("2026-07-14T09:00:00.000Z")},
		markdown: map[string]string{
			home:  mdResp("Home", false, ""),
			rowID: mdResp("Row body", false, ""), // body constant across every run
		},
		query: map[string]string{dsID: queryWith("进行中")},
	}

	dir := t.TempDir()
	eng, err := engine.New(layout.For(dir), engine.Options{Adapters: []adapter.Adapter{notion.NewAdapter("tok", m)}})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Close()

	if _, err := eng.Sync(context.Background()); err != nil {
		t.Fatalf("sync 1: %v", err)
	}

	// Run 2: same body, property changed 进行中 -> 已完成, row last_edited bumped.
	m.search[0] = searchWith("2026-07-14T10:00:00.000Z")
	m.query[dsID] = queryWith("已完成")
	res2, err := eng.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if res2.Stats.Updated != 1 {
		t.Errorf("property-only change: Updated = %d, want 1", res2.Stats.Updated)
	}
	rowFile := readFile(t, filepath.Join(dir, "notion/DB Home/Subjects/Row.md"))
	if !strings.Contains(rowFile, "状态: \"已完成\"") {
		t.Errorf("row frontmatter did not pick up the new property:\n%s", rowFile)
	}

	// Run 3: nothing changed → the row is skipped, not re-updated.
	res3, err := eng.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync 3: %v", err)
	}
	if res3.Stats.Updated != 0 {
		t.Errorf("unchanged rerun: Updated = %d, want 0", res3.Stats.Updated)
	}
	if res3.Stats.Skipped == 0 {
		t.Errorf("unchanged rerun should skip documents, skipped=%d", res3.Stats.Skipped)
	}
}

// --- fixture builders (mirror the P0-verified API shapes) ---

func notionSearch(results []string) string {
	return `{"object":"list","results":[` + strings.Join(results, ",") + `],"has_more":false,"next_cursor":null}`
}

func obj(kind, id, title, parentType, parentID, _ string) string {
	dl := strings.ReplaceAll(id, "-", "")
	var parent string
	switch parentType {
	case "workspace":
		parent = `{"type":"workspace","workspace":true}`
	case "page_id":
		parent = `{"type":"page_id","page_id":"` + parentID + `"}`
	}
	return `{"object":"` + kind + `","id":"` + id + `","url":"https://app.notion.com/p/` + dl +
		`","last_edited_time":"2026-07-14T09:00:00.000Z","parent":` + parent +
		`,"properties":{"title":{"type":"title","title":[{"plain_text":"` + title + `"}]}}}`
}

func dsObj(id, dbID, parentPage, title string) string {
	return `{"object":"data_source","id":"` + id + `","url":"https://app.notion.com/p/` + strings.ReplaceAll(dbID, "-", "") +
		`","last_edited_time":"2026-07-14T09:00:00.000Z","parent":{"type":"database_id","database_id":"` + dbID +
		`"},"database_parent":{"type":"page_id","page_id":"` + parentPage + `"},"title":[{"plain_text":"` + title + `"}]}`
}

func row(id, dsID, dbID, title string) string {
	return `{"object":"page","id":"` + id + `","url":"https://app.notion.com/p/` + strings.ReplaceAll(id, "-", "") +
		`","last_edited_time":"2026-07-14T09:00:00.000Z","parent":{"type":"data_source_id","data_source_id":"` + dsID +
		`","database_id":"` + dbID + `"},"properties":{"Name":{"type":"title","title":[{"plain_text":"` + title + `"}]}}}`
}
