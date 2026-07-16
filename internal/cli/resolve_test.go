package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/arcships/open-doc-cli/internal/manifest"
)

const (
	nCanonical = "11112222-3333-4444-5555-666677778888"
	nDashless  = "11112222333344445555666677778888"
)

// seedResolveManifest seeds a Notion page (keyed by canonical UUID, dashless
// alias) and a Feishu wiki doc (keyed by obj_token, node_token alias), and writes
// the Notion page's on-disk file with a frontmatter url.
func seedResolveManifest(t *testing.T, db *manifest.DB, root string) {
	t.Helper()
	if err := db.UpsertDocument(manifest.Document{
		ID: nCanonical, Platform: "notion", Type: "page", Title: "Notion Page",
		LocalPath: "notion/page.md", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAlias(nDashless, nCanonical); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertDocument(manifest.Document{
		ID: "objFeishu", Platform: "feishu", Type: "docx", Title: "Feishu Doc",
		LocalPath: "feishu/doc.md", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAlias("nodeFeishu", "objFeishu"); err != nil {
		t.Fatal(err)
	}
	// Write the Notion page file with a frontmatter url so resolve can read it back.
	abs := filepath.Join(root, "notion", "page.md")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\n" +
		"id: \"" + nCanonical + "\"\n" +
		"source: \"notion\"\n" +
		"url: \"https://www.notion.so/Notion-Page-" + nDashless + "\"\n" +
		"title: \"Notion Page\"\n" +
		"---\nbody\n"
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveByID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	db := newManifest(t, root)
	seedResolveManifest(t, db, root)
	db.Close()

	cases := []struct {
		name      string
		query     string
		wantID    string
		wantMatch string
	}{
		{"notion canonical id", nCanonical, nCanonical, "id"},
		{"notion dashless id", nDashless, nCanonical, "id"},
		{"notion url", "https://www.notion.so/Notion-Page-" + nDashless, nCanonical, "url"},
		{"feishu obj token", "objFeishu", "objFeishu", "id"},
		{"feishu wiki node alias", "nodeFeishu", "objFeishu", "alias"},
		{"feishu wiki url", "https://acme.feishu.cn/wiki/nodeFeishu?from=x", "objFeishu", "url"},
		{"local path root-relative", "notion/page.md", nCanonical, "local_path"},
		{"local path dot-relative", "./feishu/doc.md", "objFeishu", "local_path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, out, errb := newEnv("resolve", "--root", root, "--json", tc.query)
			if code := Run(env); code != ExitOK {
				t.Fatalf("resolve %q = %d, want 0; stderr=%s", tc.query, code, errb.String())
			}
			var r resolveResult
			if err := json.Unmarshal(out.Bytes(), &r); err != nil {
				t.Fatalf("resolve JSON invalid: %v; got %q", err, out.String())
			}
			if r.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", r.ID, tc.wantID)
			}
			if r.MatchedBy != tc.wantMatch {
				t.Errorf("MatchedBy = %q, want %q", r.MatchedBy, tc.wantMatch)
			}
		})
	}
}

func TestResolveURLFromFrontmatter(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	db := newManifest(t, root)
	seedResolveManifest(t, db, root)
	db.Close()

	env, out, _ := newEnv("resolve", "--root", root, "--json", nCanonical)
	if code := Run(env); code != ExitOK {
		t.Fatalf("resolve = %d, want 0", code)
	}
	var r resolveResult
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	want := "https://www.notion.so/Notion-Page-" + nDashless
	if r.URL != want {
		t.Errorf("URL = %q, want %q (from frontmatter)", r.URL, want)
	}
	if r.LocalPath != "notion/page.md" {
		t.Errorf("LocalPath = %q, want notion/page.md", r.LocalPath)
	}
}

func TestResolveNotFound(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	db := newManifest(t, root)
	seedResolveManifest(t, db, root)
	db.Close()

	env, _, errb := newEnv("resolve", "--root", root, "nonexistent-id")
	if code := Run(env); code != ExitError {
		t.Fatalf("resolve nonexistent = %d, want %d", code, ExitError)
	}
	if errb.Len() == 0 {
		t.Errorf("expected a not-found message on stderr")
	}
}

func TestResolveUsageError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	newManifest(t, root).Close()

	env, _, _ := newEnv("resolve", "--root", root)
	if code := Run(env); code != ExitUsage {
		t.Fatalf("resolve with no query = %d, want %d", code, ExitUsage)
	}
}
