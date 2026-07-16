package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/layout"
)

// configAdapter is a fully in-memory adapter whose enumerated docs and bodies
// are set per test, so link-rewrite scenarios (same-run, late-target, wiki
// alias, external target) can be driven without a platform.
type configAdapter struct {
	docs   []adapter.RemoteDoc
	bodies map[string]adapter.FetchResult
}

func (c *configAdapter) Platform() string { return "feishu" }

func (c *configAdapter) Enumerate(ctx context.Context) (<-chan adapter.RemoteDoc, <-chan error) {
	out := make(chan adapter.RemoteDoc)
	errc := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errc)
		for _, d := range c.docs {
			out <- d
		}
	}()
	return out, errc
}

func (c *configAdapter) FetchMarkdown(ctx context.Context, doc adapter.RemoteDoc) (adapter.FetchResult, error) {
	return c.bodies[doc.ID], nil
}

func (c *configAdapter) DownloadAsset(ctx context.Context, ref adapter.AssetRef, dest string) error {
	return nil
}

const (
	urlDocxB = "https://x.feishu.cn/docx/objB"
	urlWikiC = "https://x.feishu.cn/wiki/nodeC"
	urlExt   = "https://x.feishu.cn/docx/EXTERNALTOKEN"
)

// bodyA is doc A's body: a docx link to B, a wiki link to C (by node_token), and
// an external link whose target is never mirrored.
const bodyA = "# 首页\n\n见 [手册](" + urlDocxB + ") 与 [权限](" + urlWikiC + ")，外部 [x](" + urlExt + ")。\n"

// fullDocSet is A (leaf), B (dir with README, has child C), C (leaf, wiki alias).
func fullDocSet() *configAdapter {
	return &configAdapter{
		docs: []adapter.RemoteDoc{
			{ID: "root", Type: adapter.TypeFolder, Title: "wiki-空间"},
			{ID: "objA", Type: adapter.TypeDocx, ParentID: "root", Title: "首页", URL: "https://x.feishu.cn/docx/objA", EditedAt: "2026-07-14T09:00:00Z"},
			{ID: "objB", Type: adapter.TypeDocx, ParentID: "root", Title: "手册", URL: urlDocxB, EditedAt: "2026-07-14T10:00:00Z"},
			{ID: "objC", AltID: "nodeC", Type: adapter.TypeDocx, ParentID: "objB", Title: "权限", URL: urlWikiC, EditedAt: "2026-07-14T11:00:00Z"},
		},
		bodies: map[string]adapter.FetchResult{
			"objA": {Markdown: bodyA, Body: bodyA, Links: []adapter.DocRef{
				{TargetID: "objB", RawURL: urlDocxB},
				{TargetID: "nodeC", RawURL: urlWikiC},
				{TargetID: "EXTERNALTOKEN", RawURL: urlExt},
			}},
			"objB": {Markdown: "# 手册\n正文B\n", Body: "# 手册\n正文B\n"},
			"objC": {Markdown: "# 权限\n正文C\n", Body: "# 权限\n正文C\n"},
		},
	}
}

func TestLinkRewriteSameRun(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)

	res := runOnce(t, l, fullDocSet())
	if res.Stats.LinksRewritten != 2 {
		t.Fatalf("links_rewritten = %d, want 2 (docx + wiki); stats=%+v", res.Stats.LinksRewritten, res.Stats)
	}

	body := readFile(t, root, "feishu/wiki-空间/首页.md")
	// docx link -> relative to B's README; wiki link (node_token) -> relative to C.
	if !strings.Contains(body, "[手册](手册/README.md)") {
		t.Errorf("docx link not rewritten:\n%s", body)
	}
	if !strings.Contains(body, "[权限](手册/权限.md)") {
		t.Errorf("wiki(node_token) link not rewritten:\n%s", body)
	}
	// External target (never mirrored) stays as the original platform URL.
	if !strings.Contains(body, urlExt) {
		t.Errorf("external link should be left untouched:\n%s", body)
	}
	// The frontmatter url: line (a docx URL) must not be rewritten.
	if !strings.Contains(body, `url: "https://x.feishu.cn/docx/objA"`) {
		t.Errorf("frontmatter url must not be rewritten:\n%s", body)
	}

	// content_hash stability: an immediate re-sync must skip everything and
	// rewrite nothing (the body already holds relative paths).
	res2 := runOnce(t, l, fullDocSet())
	if res2.Stats.Added != 0 || res2.Stats.Updated != 0 {
		t.Fatalf("re-sync should add/update nothing, got %+v", res2.Stats)
	}
	if res2.Stats.LinksRewritten != 0 {
		t.Fatalf("re-sync should rewrite no links, got %d", res2.Stats.LinksRewritten)
	}
	if res2.Stats.Skipped == 0 {
		t.Fatalf("re-sync should skip docs, got %+v", res2.Stats)
	}
	body2 := readFile(t, root, "feishu/wiki-空间/首页.md")
	if body2 != body {
		t.Fatalf("re-sync changed the file (false dirty):\nbefore:\n%s\nafter:\n%s", body, body2)
	}
}

func TestLinkRewriteLateTarget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)

	// Run 1: only A (and its container). Targets B and C are not enumerated, so
	// A's links stay as platform URLs.
	full := fullDocSet()
	run1 := &configAdapter{
		docs:   []adapter.RemoteDoc{full.docs[0], full.docs[1]}, // root + objA
		bodies: full.bodies,
	}
	res1 := runOnce(t, l, run1)
	if res1.Stats.LinksRewritten != 0 {
		t.Fatalf("run1 should rewrite nothing (targets absent), got %d", res1.Stats.LinksRewritten)
	}
	body1 := readFile(t, root, "feishu/wiki-空间/首页.md")
	if !strings.Contains(body1, urlDocxB) || !strings.Contains(body1, urlWikiC) {
		t.Fatalf("run1 body should keep original URLs:\n%s", body1)
	}

	// Run 2: B and C arrive. A itself is unchanged (skipped), but the whole-library
	// finalize scan must still rewrite A's body now that the targets exist.
	res2 := runOnce(t, l, fullDocSet())
	if res2.Stats.LinksRewritten != 2 {
		t.Fatalf("run2 should rewrite A's 2 links via late-target scan, got %d; stats=%+v", res2.Stats.LinksRewritten, res2.Stats)
	}
	body2 := readFile(t, root, "feishu/wiki-空间/首页.md")
	if !strings.Contains(body2, "[手册](手册/README.md)") || !strings.Contains(body2, "[权限](手册/权限.md)") {
		t.Fatalf("run2 body not rewritten:\n%s", body2)
	}
}

func TestRewriteBodyLinksShapesAndDepths(t *testing.T) {
	// Resolver maps known tokens to local paths at varying depths.
	targets := map[string]string{
		"objB":  "feishu/wiki-空间/手册/README.md",
		"nodeC": "feishu/wiki-空间/手册/权限.md",
		"objD":  "feishu/其他/说明.md",
	}
	resolve := func(tok string) (string, bool) { p, ok := targets[tok]; return p, ok }

	cases := []struct {
		name, from, url, want string
	}{
		{"docx same dir up", "feishu/wiki-空间/首页.md", urlDocxB, "手册/README.md"},
		{"wiki node_token", "feishu/wiki-空间/首页.md", urlWikiC, "手册/权限.md"},
		{"legacy docs shape", "feishu/wiki-空间/首页.md", "https://x.feishu.cn/docs/objB", "手册/README.md"},
		{"cross-subtree up", "feishu/wiki-空间/手册/权限.md", "https://x.feishu.cn/docx/objD", "../../其他/说明.md"},
		{"from README depth", "feishu/wiki-空间/手册/README.md", "https://x.feishu.cn/docx/objD", "../../其他/说明.md"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := "see [t](" + c.url + ") end"
			out, n := rewriteBodyLinks(body, c.from, resolve)
			if n != 1 {
				t.Fatalf("rewrote %d links, want 1 (%s)", n, out)
			}
			if !strings.Contains(out, "[t]("+c.want+")") {
				t.Fatalf("got %q, want relative path %q", out, c.want)
			}
		})
	}

	// Unknown token is left untouched and not counted.
	out, n := rewriteBodyLinks("x [e](https://x.feishu.cn/docx/UNKNOWN) y", "feishu/a.md", resolve)
	if n != 0 || !strings.Contains(out, "docx/UNKNOWN") {
		t.Fatalf("unknown target should be untouched: out=%q n=%d", out, n)
	}
	// Non-feishu URL ignored.
	out, n = rewriteBodyLinks("[g](https://github.com/x/y)", "feishu/a.md", resolve)
	if n != 0 || !strings.Contains(out, "github.com") {
		t.Fatalf("external URL should be ignored: out=%q n=%d", out, n)
	}
}

func TestSplitAfterFrontmatter(t *testing.T) {
	content := "---\nid: \"x\"\nurl: \"https://x.feishu.cn/docx/objA\"\n---\n\n# body\n"
	head, body := splitAfterFrontmatter(content)
	if !strings.Contains(head, "url:") || strings.Contains(body, "url:") {
		t.Fatalf("frontmatter not isolated:\nhead=%q\nbody=%q", head, body)
	}
	if head+body != content {
		t.Fatalf("split is not lossless")
	}
	// No frontmatter: whole string is body.
	if h, b := splitAfterFrontmatter("# just body\n"); h != "" || b != "# just body\n" {
		t.Fatalf("no-frontmatter split wrong: h=%q b=%q", h, b)
	}
}
