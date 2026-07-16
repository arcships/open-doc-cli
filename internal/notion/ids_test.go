package notion

import "testing"

func TestCanonicalID(t *testing.T) {
	cases := map[string]string{
		// dashless 32-hex → hyphenated (the form embedded in <page url>).
		"facefeed00004000800000000000000a": "facefeed-0000-4000-8000-00000000000a",
		// already hyphenated → unchanged (lowercased).
		"DECAFBAD-0000-4000-8000-00000000000B": "decafbad-0000-4000-8000-00000000000b",
		// hyphenated and dashless of the same id map to the same canonical value.
		"decafbad-0000-4000-8000-00000000000b": "decafbad-0000-4000-8000-00000000000b",
	}
	for in, want := range cases {
		if got := canonicalID(in); got != want {
			t.Errorf("canonicalID(%q) = %q, want %q", in, got, want)
		}
	}
	// The dashless and hyphenated forms of one id are canonically equal — the
	// property the alias table relies on to resolve a body URL to its target.
	if canonicalID("facefeed00004000800000000000000a") != canonicalID("facefeed-0000-4000-8000-00000000000a") {
		t.Fatal("dashless and hyphenated forms must canonicalize equal")
	}
}

func TestDashless(t *testing.T) {
	if got := dashless("FACEFEED-0000-4000-8000-00000000000A"); got != "facefeed00004000800000000000000a" {
		t.Errorf("dashless = %q", got)
	}
}
