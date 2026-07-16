// Package naming implements the mirror tree's directory/file naming and
// conflict rules. Titles become path components with the minimum transformation
// needed to be filesystem-safe: Chinese text and spaces are preserved (they are
// grep signal), only illegal characters are stripped, and stable IDs are
// appended only when a name would otherwise collide, be reserved, or be too
// long.
package naming

import (
	"strings"
	"unicode/utf8"
)

// MaxComponentBytes is the byte cap for a single path component.
// Names whose slug exceeds this are truncated and given an ID suffix.
const MaxComponentBytes = 200

// idPrefixLen is how many leading characters of the platform ID are appended
// on conflict/reserved/overlong.
const idPrefixLen = 8

// reserved holds the layout-reserved base names (compared casefolded, without
// extension). A title that slugs to one of these always gets an ID suffix so it
// can never shadow a structural file. CLAUDE/AGENTS are included so a
// mirrored doc can never be auto-loaded as an agent instruction file.
var reserved = map[string]bool{
	"readme":    true,
	"_index":    true,
	"index":     true,
	"assets":    true,
	"_orphans":  true,
	".internal": true,
	"claude":    true,
	"agents":    true,
}

// illegal reports whether r is a filesystem-illegal character that must be
// stripped from a slug: the POSIX/Windows reserved set plus control characters.
func illegal(r rune) bool {
	switch r {
	case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
		return true
	}
	return r < 0x20 // control characters
}

// Slug cleans a title into a filesystem-safe base name (no extension): illegal
// characters are dropped and leading/trailing whitespace is collapsed. Interior
// spaces and non-ASCII text are preserved verbatim. The result may be empty if
// the title consisted solely of illegal/whitespace characters.
func Slug(title string) string {
	var b strings.Builder
	for _, r := range title {
		if illegal(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// IDPrefix returns the leading idPrefixLen characters (runes) of id, used as the
// disambiguating suffix. Shorter IDs are returned whole.
//
// Synthetic engine IDs of the form "platform:kind:<native-id>" (e.g. wiki space
// roots) would all share the constant "platform:k" prefix — and carry the
// filesystem-illegal ':' — so the prefix is taken from the segment after the
// last ':', which is the unique platform-native part. Any remaining illegal
// characters are stripped before cutting.
func IDPrefix(id string) string {
	if i := strings.LastIndexByte(id, ':'); i >= 0 && i+1 < len(id) {
		id = id[i+1:]
	}
	id = Slug(id)
	if len(id) <= idPrefixLen {
		return id
	}
	// Cut on a rune boundary; platform IDs are ASCII in practice but be safe.
	return string([]rune(id)[:idPrefixLen])
}

// truncateToBytes returns s truncated to at most n bytes without splitting a
// UTF-8 rune.
func truncateToBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// casefold normalises a base name for duplicate detection. macOS is
// case-insensitive by default, so comparison is done casefolded.
func casefold(s string) string { return strings.ToLower(s) }

// Component computes the final path component (base name, without extension) for
// a node, applying every naming rule in order: slug, empty-title fallback,
// reserved-name suffixing, overlength truncation+suffix, and same-directory
// duplicate suffixing. used is the set of casefolded base names already taken in
// the target directory; the chosen name is added to it before returning so the
// caller can reuse the set across siblings.
func Component(title, id string, used map[string]bool) string {
	base := Slug(title)

	suffix := "-" + IDPrefix(id)

	// Empty title carries no signal — always disambiguate with the ID so the
	// name is both non-empty and unique.
	if base == "" {
		return claim("untitled"+suffix, used)
	}

	needsID := false

	// Reserved names must never appear bare.
	if reserved[casefold(base)] {
		needsID = true
	}

	// Overlength: truncate the slug leaving room for the suffix, then force the
	// ID on so the truncated name stays traceable and collision-resistant.
	if len(base) > MaxComponentBytes {
		base = truncateToBytes(base, MaxComponentBytes-len(suffix))
		needsID = true
	}

	if needsID {
		return claim(base+suffix, used)
	}

	// Same-directory duplicate (casefolded): the later arrival takes the ID
	// suffix; the first keeps the clean name.
	if used[casefold(base)] {
		return claim(base+suffix, used)
	}

	return claim(base, used)
}

// claim records name (casefolded) in used and returns it. If the suffixed name
// itself somehow collides (two nodes sharing an 8-char ID prefix in one dir), a
// numeric tiebreaker is appended to guarantee uniqueness.
func claim(name string, used map[string]bool) string {
	key := casefold(name)
	if !used[key] {
		used[key] = true
		return name
	}
	for i := 2; ; i++ {
		cand := name + "-" + itoa(i)
		ck := casefold(cand)
		if !used[ck] {
			used[ck] = true
			return cand
		}
	}
}

// itoa is a tiny strconv.Itoa to avoid the import for a single call site.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
