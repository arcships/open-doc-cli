package notion

import "testing"

func TestExtractAssetsKeyDerivation(t *testing.T) {
	// A real-shaped S3 signed URL: the stable key is the path without the query.
	md := "text\n" +
		"![](https://prod-files-secure.s3.us-west-2.amazonaws.com/5aced000-0000-4000-8000-000000000012/f11e0000-0000-4000-8000-000000000013/image.png?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Expires=3600&X-Amz-Signature=deadbeef)\n" +
		"![](https://example.com/external.png)\n" // external image is ignored
	refs := extractAssets(md)
	if len(refs) != 1 {
		t.Fatalf("want 1 asset (external ignored), got %d: %+v", len(refs), refs)
	}
	r := refs[0]
	wantKey := "5aced000-0000-4000-8000-000000000012/f11e0000-0000-4000-8000-000000000013/image.png"
	if r.RemoteKey != wantKey {
		t.Errorf("RemoteKey = %q, want %q", r.RemoteKey, wantKey)
	}
	if r.Filename != "image.png" {
		t.Errorf("Filename = %q, want image.png", r.Filename)
	}
	if r.BodyURL == "" || r.BodyURL != r.URL {
		t.Errorf("BodyURL/URL should be the full signed URL: %q / %q", r.BodyURL, r.URL)
	}
}

func TestExtractAssetsDedup(t *testing.T) {
	// Same file path with two different signatures dedups on the path key.
	base := "https://prod-files-secure.s3.us-west-2.amazonaws.com/ws/uuid/a.png"
	md := "![](" + base + "?X-Amz-Signature=one)\n![](" + base + "?X-Amz-Signature=two)\n"
	if refs := extractAssets(md); len(refs) != 1 {
		t.Fatalf("want 1 deduped asset, got %d", len(refs))
	}
}

func TestExtractLinksPageAndDatabase(t *testing.T) {
	md := `<page url="https://app.notion.com/p/facefeed00004000800000000000000a">Term 1</page>
<database url="https://app.notion.com/p/dbdbdbdb00004000800000000000000f" inline="true" data-source-url="collection://c0ffee00-0000-4000-8000-00000000000e">Subjects</database>
plain text with an external https://example.com/x link`
	links := extractLinks(md)
	got := map[string]bool{}
	for _, l := range links {
		got[l.TargetID] = true
	}
	// <page> target = canonicalized page id.
	if !got["facefeed-0000-4000-8000-00000000000a"] {
		t.Errorf("page link target missing: %+v", links)
	}
	// <database> target = the data-source id (the database node's manifest id),
	// NOT the container database id in the url attribute.
	if !got["c0ffee00-0000-4000-8000-00000000000e"] {
		t.Errorf("database link target (data-source id) missing: %+v", links)
	}
	if len(links) != 2 {
		t.Errorf("want exactly 2 links, got %d: %+v", len(links), links)
	}
}
