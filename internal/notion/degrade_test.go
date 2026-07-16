package notion

import (
	"strings"
	"testing"
)

func TestDegradeCleanBody(t *testing.T) {
	md := "<page url=\"https://app.notion.com/p/aaaa\">child</page>\n<empty-block/>\nhello"
	body, deg := degrade(md, false, nil)
	if body != md {
		t.Errorf("clean body should pass through unchanged:\n got %q\nwant %q", body, md)
	}
	if deg.Total() != 0 {
		t.Errorf("clean body should have no degradation, got %+v", deg)
	}
}

func TestDegradeEmptyBodyTrusted(t *testing.T) {
	body, deg := degrade("", false, nil)
	if body != "" {
		t.Errorf("empty page body should stay empty, got %q", body)
	}
	if deg.Total() != 0 {
		t.Errorf("empty page is not a degradation, got %+v", deg)
	}
}

func TestDegradeTruncatedMarker(t *testing.T) {
	body, deg := degrade("content", true, nil)
	if deg.TruncatedPages != 1 {
		t.Errorf("TruncatedPages = %d, want 1", deg.TruncatedPages)
	}
	if !strings.Contains(body, truncatedMarker) {
		t.Errorf("truncated marker missing from body: %q", body)
	}
	if !strings.HasPrefix(body, truncatedMarker) {
		t.Errorf("truncated marker should lead the body: %q", body)
	}
	if !strings.Contains(body, "content") {
		t.Errorf("original content must be preserved: %q", body)
	}
}

func TestDegradeUnknownBlockIDsMarker(t *testing.T) {
	body, deg := degrade("content", false, []string{"blk-1", "blk-2"})
	if deg.UnknownBlockIDs != 2 {
		t.Errorf("UnknownBlockIDs = %d, want 2", deg.UnknownBlockIDs)
	}
	if !strings.Contains(body, `opendoc:unknown-blocks`) || !strings.Contains(body, "blk-1") || !strings.Contains(body, "blk-2") {
		t.Errorf("unknown-blocks marker missing/incomplete: %q", body)
	}
}

func TestDegradeUnknownBlocksOnEmptyPage(t *testing.T) {
	// A truncated empty page still gets a marker so the loss is visible in-file.
	body, deg := degrade("", true, []string{"x"})
	if deg.TruncatedPages != 1 || deg.UnknownBlockIDs != 1 {
		t.Errorf("counts wrong: %+v", deg)
	}
	if !strings.Contains(body, truncatedMarker) || !strings.Contains(body, "opendoc:unknown-blocks") {
		t.Errorf("markers missing on empty truncated page: %q", body)
	}
}

func TestCountUnknownBlocks(t *testing.T) {
	md := "recognized <page url=\"x\">a</page> and <span>b</span>\n" +
		"unknown <mystery-block foo=\"1\"> and <another/>\n" +
		"```\n<in-code-fence> should not count\n```\n" +
		"inline `<also-not>` code"
	if got := countUnknownBlocks(md); got != 2 {
		t.Errorf("countUnknownBlocks = %d, want 2 (mystery-block, another)", got)
	}
}
