package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Env-file token fallback.
//
// A opendoc mirror root may carry a private, shell-style env file at
// <root>/.internal/env holding the Notion integration token (and any other env
// the engine consumes). This is the exact file the launchd wrapper historically
// sourced before exec'ing the binary — same path, same format — so opendoc now reads
// it natively and the wrapper is no longer required.
//
// The parser is deliberately NOT a shell: it does no variable expansion
// ($VAR / ${VAR}), no command substitution ($(...) / backticks), and no escape
// processing inside quotes. It only recognises simple `KEY=value` assignments so
// a token file behaves predictably regardless of the surrounding shell.

// Sources of a resolved environment value, reported by ResolveEnv for diagnostics.
const (
	// SourceEnvironment means the value came from the real process environment.
	SourceEnvironment = "environment"
	// SourceFile means the value came from the <root>/.internal/env fallback file.
	SourceFile = "file"
)

// EnvResolution is the outcome of resolving one environment variable through the
// real environment first, then the env-file fallback. It carries enough detail
// for `opendoc doctor` N1 to name the source and warn
// on loose file permissions.
type EnvResolution struct {
	// Name is the variable that was resolved.
	Name string
	// Value is the resolved value, or "" when neither channel supplied one.
	Value string
	// Source is SourceEnvironment, SourceFile, or "" when unresolved.
	Source string
	// FilePath is the env-file path that was checked (may be "").
	FilePath string
	// FileExists reports whether the env file exists (as a regular file).
	FileExists bool
	// FileLoose reports whether the env file exists with permissions looser than
	// 0600 (any group/other bit set) — a security smell N1 warns about.
	FileLoose bool
	// FileHas reports whether the env file defined Name (regardless of precedence).
	FileHas bool
}

// ResolveEnv resolves name from the real environment (via getenv) first, falling
// back to the env file at envPath. The real environment always wins; the file is
// only consulted when getenv returns empty. It stats the file regardless (so the
// loose-permission warning fires even when the value came from the environment)
// and tolerates a missing file or empty envPath by treating the file as absent.
func ResolveEnv(name string, getenv func(string) string, envPath string) EnvResolution {
	res := EnvResolution{Name: name, FilePath: envPath}
	if v := getenv(name); v != "" {
		res.Value = v
		res.Source = SourceEnvironment
	}
	if envPath == "" {
		return res
	}
	info, err := os.Stat(envPath)
	if err != nil || info.IsDir() {
		return res
	}
	res.FileExists = true
	if info.Mode().Perm()&0o077 != 0 {
		res.FileLoose = true
	}
	m, perr := ParseEnvFile(envPath)
	if perr != nil {
		return res
	}
	if fv, ok := m[name]; ok {
		res.FileHas = true
		if res.Source == "" {
			res.Value = fv
			res.Source = SourceFile
		}
	}
	return res
}

// ParseEnvFile reads a shell-style env file and returns its KEY=value pairs. The
// accepted syntax is intentionally minimal (no shell evaluation):
//
//   - Blank lines and lines whose first non-space character is '#' are ignored.
//   - A leading `export ` prefix is tolerated and stripped.
//   - Each remaining line is split on the first '=' into KEY and value; a line
//     without '=' (or with an empty KEY) is skipped as garbage.
//   - A value wrapped in matching single or double quotes has the quotes removed
//     and its contents taken verbatim — no escape or variable expansion, and any
//     trailing text after the closing quote is discarded.
//   - An unquoted value is taken up to an inline comment (whitespace followed by
//     '#') and then trimmed of surrounding whitespace.
//
// Later assignments of the same key win. A read error is returned; malformed
// individual lines are skipped, never fatal.
func ParseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open env file %s: %w", path, err)
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	// Raise the line limit so an unusually long token value is not truncated.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		line = strings.TrimLeft(line, " \t")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			continue
		}
		out[key] = parseEnvValue(line[eq+1:])
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read env file %s: %w", path, err)
	}
	return out, nil
}

// parseEnvValue interprets the right-hand side of a KEY=value assignment per the
// ParseEnvFile contract: quoted verbatim, or unquoted up to an inline comment.
func parseEnvValue(raw string) string {
	v := strings.TrimLeft(raw, " \t")
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		quote := v[0]
		if end := strings.IndexByte(v[1:], quote); end >= 0 {
			return v[1 : 1+end]
		}
		// Unterminated quote: fall through and treat the rest literally.
		return v[1:]
	}
	// Unquoted: strip an inline comment introduced by whitespace + '#'.
	if i := indexInlineComment(v); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// indexInlineComment returns the index of an inline comment marker (whitespace
// immediately followed by '#') in an unquoted value, or -1 if there is none.
func indexInlineComment(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return i - 1
		}
	}
	return -1
}
