package engine

import (
	"strings"
	"testing"
)

func TestContentPathBucketing(t *testing.T) {
	// sha256("") = e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
	const sha = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	got := contentPath(sha, ".png")
	want := "assets/e3/" + sha + ".png"
	if got != want {
		t.Fatalf("contentPath = %q, want %q", got, want)
	}
	// No extension is allowed (unknown type).
	if got := contentPath(sha, ""); got != "assets/e3/"+sha {
		t.Fatalf("contentPath(no ext) = %q", got)
	}
}

func TestRelAssetDepth(t *testing.T) {
	cases := []struct {
		body, asset, want string
	}{
		// Leaf two levels under root.
		{"feishu/drive-x/P0.md", "assets/ab/h.png", "../../assets/ab/h.png"},
		// README three levels deep.
		{"feishu/wiki-空间/手册/README.md", "assets/ab/h.png", "../../../assets/ab/h.png"},
		// Deeply nested leaf.
		{"feishu/w/成员手册/子/权限.md", "assets/cd/h.jpg", "../../../../assets/cd/h.jpg"},
	}
	for _, c := range cases {
		if got := relAsset(c.body, c.asset); got != c.want {
			t.Errorf("relAsset(%q,%q) = %q, want %q", c.body, c.asset, got, c.want)
		}
	}
}

func TestPickExt(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32))
	jpg := []byte("\xff\xd8\xff\xe0" + strings.Repeat("\x00", 32))
	gif := []byte("GIF89a" + strings.Repeat("\x00", 32))
	cases := []struct {
		name string
		data []byte
		want string
	}{
		// Sniffed type wins over a lying filename extension (test.jpg -> .ico case).
		{"photo.PNG", jpg, ".jpg"},
		{"", png, ".png"},
		{"", jpg, ".jpg"},
		{"nodots", gif, ".gif"},
		// Unsniffable content falls back to the filename extension (office docs).
		{"report.docx", []byte("PK\x03\x04 zip-ish office bytes here padding"), ".docx"},
		// Unknown content and no filename ext -> "".
		{"", []byte("not an image at all really"), ""},
	}
	for _, c := range cases {
		if got := pickExt(c.name, c.data); got != c.want {
			t.Errorf("pickExt(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestAppendPendingMarker(t *testing.T) {
	const url = "https://x.feishu.cn/stream/authcode/?code=ABC"
	body := "before\n![](" + url + ")\nafter"
	got := appendPendingMarker(body, url)
	if !strings.Contains(got, "![]("+url+")"+assetPendingMarker) {
		t.Fatalf("marker not appended after image:\n%s", got)
	}
	// Idempotent: a second pass does not double the marker.
	if again := appendPendingMarker(got, url); strings.Count(again, assetPendingMarker) != 1 {
		t.Fatalf("marker not idempotent:\n%s", again)
	}
}
