package naming

import (
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{"chinese preserved", "租户IP白名单", "租户IP白名单"},
		{"interior spaces kept", "Behind the Prompt", "Behind the Prompt"},
		{"illegal stripped", `a/b\c:d*e?f"g<h>i|j`, "abcdefghij"},
		{"leading trailing ws collapsed", "  hello  ", "hello"},
		{"slash with spaces", "示例知识库 / Wiki samples", "示例知识库  Wiki samples"},
		{"control chars stripped", "a\tb\nc", "abc"},
		{"all illegal -> empty", `/\:*?"<>|`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Slug(tt.title); got != tt.want {
				t.Fatalf("Slug(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

func TestComponentDuplicateTitles(t *testing.T) {
	used := map[string]bool{}
	// First arrival keeps the clean name; later ones take the ID suffix.
	first := Component("权限设计", "3752d1deABCDEF", used)
	if first != "权限设计" {
		t.Fatalf("first = %q, want 权限设计", first)
	}
	second := Component("权限设计", "aaaabbbbCCCC", used)
	if second != "权限设计-aaaabbbb" {
		t.Fatalf("second = %q, want 权限设计-aaaabbbb", second)
	}
}

func TestComponentCasefoldDuplicate(t *testing.T) {
	used := map[string]bool{}
	a := Component("Design", "1111aaaa2222", used)
	b := Component("design", "3333bbbb4444", used)
	if a != "Design" {
		t.Fatalf("a = %q, want Design", a)
	}
	if b != "design-3333bbbb" {
		t.Fatalf("b = %q, want design-3333bbbb (casefold collision)", b)
	}
}

func TestComponentReservedNames(t *testing.T) {
	for _, name := range []string{"README", "readme", "_index", "INDEX", "assets", "_orphans", ".internal", "CLAUDE", "AGENTS"} {
		used := map[string]bool{}
		got := Component(name, "deadbeef1234", used)
		if !strings.HasSuffix(got, "-deadbeef") {
			t.Fatalf("reserved %q -> %q, want an ID suffix", name, got)
		}
	}
}

func TestComponentEmptyTitle(t *testing.T) {
	used := map[string]bool{}
	got := Component("", "abcd1234wxyz", used)
	if got != "untitled-abcd1234" {
		t.Fatalf("empty title -> %q, want untitled-abcd1234", got)
	}
	// A second empty title must not collide.
	got2 := Component("", "ffff0000eeee", used)
	if got2 != "untitled-ffff0000" {
		t.Fatalf("second empty title -> %q, want untitled-ffff0000", got2)
	}
}

func TestComponentOverlength(t *testing.T) {
	long := strings.Repeat("あ", 100) // 300 bytes (3 bytes/rune)
	used := map[string]bool{}
	got := Component(long, "0123456789ab", used)
	if len(got) > MaxComponentBytes {
		t.Fatalf("component length = %d bytes, want <= %d", len(got), MaxComponentBytes)
	}
	if !strings.HasSuffix(got, "-01234567") {
		t.Fatalf("overlength component %q missing ID suffix", got)
	}
	// The base must remain valid UTF-8 (no split rune).
	if !isValidRuneBoundary(got) {
		t.Fatalf("overlength component split a rune: %q", got)
	}
}

func TestComponentShortIDPrefix(t *testing.T) {
	used := map[string]bool{}
	got := Component("README", "ab", used)
	if got != "README-ab" {
		t.Fatalf("short id -> %q, want README-ab", got)
	}
}

func TestIDPrefixSyntheticID(t *testing.T) {
	// Synthetic root IDs must yield the unique native segment, never the
	// constant "feishu:w" (which also carries the illegal ':').
	if got := IDPrefix("feishu:wiki:7540928398949236755"); got != "75409283" {
		t.Fatalf("synthetic wiki id -> %q, want 75409283", got)
	}
	if got := IDPrefix("feishu:my-library:7540923485661626387"); got != "75409234"[:8] {
		t.Fatalf("synthetic my-library id -> %q, want 75409234", got)
	}
	// Distinct spaces must get distinct prefixes.
	if IDPrefix("feishu:wiki:7540928398949236755") == IDPrefix("feishu:wiki:7540923394481520644") {
		t.Fatal("two distinct space ids produced the same prefix")
	}
	// Plain platform-native IDs are unaffected.
	if got := IDPrefix("Fj5VdNtPnoWlFcxfHYbcKvJKnKb"); got != "Fj5VdNtP" {
		t.Fatalf("native id -> %q, want Fj5VdNtP", got)
	}
	// A trailing colon must not produce an empty prefix.
	if got := IDPrefix("weird:"); got == "" {
		t.Fatal("trailing-colon id produced empty prefix")
	}
}

func isValidRuneBoundary(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
