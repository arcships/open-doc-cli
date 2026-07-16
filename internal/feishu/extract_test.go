package feishu

import "testing"

// sampleXML mirrors the real lark-cli --doc-format xml output shape (from the
// docs/P0-test-results.md ground truth): an <img> with src token + temporary href,
// and an <a> to another Feishu doc.
const sampleXML = `<title>Doc</title><p>text <a href="https://acn93vuxm5pn.feishu.cn/docx/ZEeldSCTloh5wwx466EcXn4Qnac">ref</a> and <a href="https://example.com/external">ext</a></p>` +
	`<img name="test.jpg" href="https://internal-api-drive-stream.feishu.cn/space/api/box/stream/download/authcode/?code=ABC&amp;x=1" mime="image/jpeg" scale="1.000000" src="SYOJbGqUmo5wVSx4g0cc9hNYndc"/>` +
	`<img src="SYOJbGqUmo5wVSx4g0cc9hNYndc"/>` + // duplicate token, must dedupe
	`<a href="https://x.feishu.cn/wiki/WWWnodeToken123">wiki link</a>`

func TestExtractAssets(t *testing.T) {
	assets := extractAssets(sampleXML)
	if len(assets) != 1 {
		t.Fatalf("expected 1 deduped asset, got %d: %+v", len(assets), assets)
	}
	a := assets[0]
	if a.RemoteKey != "SYOJbGqUmo5wVSx4g0cc9hNYndc" {
		t.Errorf("RemoteKey = %q, want the src token", a.RemoteKey)
	}
	if a.Filename != "test.jpg" {
		t.Errorf("Filename = %q, want test.jpg", a.Filename)
	}
	// href entity should be unescaped.
	if wantSub := "code=ABC&x=1"; !contains(a.URL, wantSub) {
		t.Errorf("URL = %q, want unescaped href containing %q", a.URL, wantSub)
	}
}

func TestExtractLinks(t *testing.T) {
	links := extractLinks(sampleXML)
	got := map[string]bool{}
	for _, l := range links {
		got[l.TargetID] = true
	}
	if !got["ZEeldSCTloh5wwx466EcXn4Qnac"] {
		t.Errorf("missing docx link target; got %+v", links)
	}
	if !got["WWWnodeToken123"] {
		t.Errorf("missing wiki link target; got %+v", links)
	}
	if len(links) != 2 {
		t.Errorf("expected exactly 2 feishu links (external ignored), got %d: %+v", len(links), links)
	}
}

func TestExtractLinksIgnoresExternal(t *testing.T) {
	links := extractLinks(`<a href="https://github.com/foo/bar">gh</a><a href="https://example.com">e</a>`)
	if len(links) != 0 {
		t.Fatalf("external links must be ignored, got %+v", links)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
