package notion

import "strings"

// Notion IDs travel in two forms: the API returns canonical hyphenated UUIDs
// (8-4-4-4-12), while URLs embedded in page bodies (<page url>, <database url>)
// carry the dashless 32-hex form. The engine keys documents on the canonical
// form, so enumeration stores canonical IDs and records the dashless form as an
// alias, letting the two-phase link rewrite resolve a body URL to its target.

// canonicalID normalizes any Notion ID (hyphenated or dashless, mixed case) to
// the canonical lowercase hyphenated UUID. A 32-hex input is regrouped as
// 8-4-4-4-12; anything else is returned lowercased and trimmed unchanged (so a
// non-UUID token never panics).
func canonicalID(s string) string {
	h := dashless(s)
	if len(h) != 32 || !isHex(h) {
		return strings.ToLower(strings.TrimSpace(s))
	}
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// dashless strips hyphens and lowercases, yielding the 32-hex form used in body
// URLs. It is the alias key stored for each node.
func dashless(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
}

// isHex reports whether s is all hexadecimal digits.
func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return len(s) > 0
}
