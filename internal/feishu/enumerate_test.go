package feishu

import (
	"context"
	"strings"
	"testing"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/config"
)

// fakeRunner returns canned lark-cli responses keyed by a substring match on the
// joined argument list, so enumeration parsing is tested without network.
type fakeRunner struct {
	responses []fakeResponse
	calls     [][]string
}

type fakeResponse struct {
	match string // substring that must appear in the joined args
	body  string
}

func (f *fakeRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	f.calls = append(f.calls, args)
	for _, r := range f.responses {
		if strings.Contains(joined, r.match) {
			return []byte(r.body), nil
		}
	}
	return nil, &notFoundError{joined}
}

func (f *fakeRunner) RunInDir(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return f.Run(ctx, args...)
}

type notFoundError struct{ args string }

func (e *notFoundError) Error() string { return "fakeRunner: no response for: " + e.args }

func ok(data string) string { return `{"ok":true,"data":` + data + `}` }

func TestEnumerateWikiAndDrive(t *testing.T) {
	f := &fakeRunner{responses: []fakeResponse{
		// Wiki space name.
		{"wiki spaces get --space-id S1", ok(`{"space":{"name":"工程","space_id":"S1"}}`)},
		// Children of nB — MUST precede the generic root match below, since the
		// root node-list args are a substring of this call's args.
		{"--parent-node-token nB",
			ok(`{"nodes":[
				{"has_child":false,"node_token":"nC","obj_token":"objC","obj_type":"sheet","parent_node_token":"nB","title":"排期表"}
			]}`)},
		// Wiki root nodes: one leaf docx, one docx with a child.
		{"wiki +node-list --space-id S1 --page-all --page-limit 0 --format json",
			ok(`{"nodes":[
				{"has_child":false,"node_token":"nA","obj_token":"objA","obj_type":"docx","parent_node_token":"","title":"欢迎"},
				{"has_child":true,"node_token":"nB","obj_token":"objB","obj_type":"docx","parent_node_token":"","title":"手册"}
			]}`)},
		// Drive folder name (metas for the folder token) and enrichment share the
		// same endpoint; disambiguate by the folder doc_type payload.
		{"metas/batch_query", ok(`{"metas":[
			{"doc_token":"FOLDER1","doc_type":"folder","title":"设计稿","url":"https://t.feishu.cn/drive/folder/FOLDER1"},
			{"doc_token":"objA","latest_modify_time":"1719392923","url":"https://t.feishu.cn/docx/objA"},
			{"doc_token":"objB","latest_modify_time":"1719392999","url":"https://t.feishu.cn/docx/objB"},
			{"doc_token":"objC","latest_modify_time":"1720000000","url":"https://t.feishu.cn/sheets/objC"},
			{"doc_token":"fileD","latest_modify_time":"1721000000","url":"https://t.feishu.cn/docx/fileD"}
		]}`)},
		// Drive folder listing: one docx file, no subfolders.
		{"drive/v1/files", ok(`{"files":[
			{"token":"fileD","name":"方案","type":"docx","url":"https://t.feishu.cn/docx/fileD","parent_token":"FOLDER1","modified_time":"1721000000"}
		],"has_more":false}`)},
	}}

	a := NewAdapter(config.Feishu{WikiSpaces: []string{"S1"}, DriveFolders: []string{"FOLDER1"}}, f, 0)
	docs, err := a.enumerate(context.Background())
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	byID := map[string]adapter.RemoteDoc{}
	for _, d := range docs {
		byID[d.ID] = d
	}

	// Synthetic wiki root exists and is a folder at top level.
	root, okRoot := byID["feishu:wiki:S1"]
	if !okRoot || root.Type != adapter.TypeFolder || root.ParentID != "" {
		t.Fatalf("wiki root missing/wrong: %+v", root)
	}
	if root.Title != "wiki-工程" {
		t.Errorf("wiki root title = %q, want wiki-工程", root.Title)
	}

	// Leaf docx parents onto the synthetic root; enrichment applied.
	if a := byID["objA"]; a.ParentID != "feishu:wiki:S1" || a.Type != adapter.TypeDocx ||
		a.URL != "https://t.feishu.cn/docx/objA" || a.EditedAt == "" {
		t.Errorf("objA wrong: %+v", byID["objA"])
	}

	// Child sheet parents onto objB (node_token->obj_token translation) and maps
	// to the placeholder sheet type.
	if c := byID["objC"]; c.ParentID != "objB" || c.Type != adapter.TypeSheet {
		t.Errorf("objC parent/type wrong: %+v", byID["objC"])
	}

	// Drive folder root uses its resolved name.
	if fr := byID["FOLDER1"]; fr.Type != adapter.TypeFolder || fr.Title != "drive-设计稿" || fr.ParentID != "" {
		t.Errorf("drive root wrong: %+v", byID["FOLDER1"])
	}
	// Drive file parents onto the folder.
	if fd := byID["fileD"]; fd.ParentID != "FOLDER1" || fd.Type != adapter.TypeDocx {
		t.Errorf("fileD wrong: %+v", byID["fileD"])
	}
}

func TestObjTypeToDocType(t *testing.T) {
	cases := map[string]adapter.DocType{
		"docx":     adapter.TypeDocx,
		"folder":   adapter.TypeFolder,
		"sheet":    adapter.TypeSheet,
		"bitable":  adapter.TypeBitable,
		"mindnote": adapter.TypeMindnote,
		"slides":   adapter.TypeSlides,
		"file":     adapter.TypeFile,
		"weird":    adapter.TypeFile,
	}
	for in, want := range cases {
		if got := objTypeToDocType(in); got != want {
			t.Errorf("objTypeToDocType(%q) = %q, want %q", in, got, want)
		}
	}
}
