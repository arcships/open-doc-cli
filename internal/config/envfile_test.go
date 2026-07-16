package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	content := `# a comment line
export NOTION_TOKEN="ntn_double_quoted"
export SINGLE='sq_value'
BARE=bare_value
export WITH_EXPORT=export_ok
  INDENTED=indented_ok
TRAILING=has_trailing_ws
INLINE=value # inline comment stripped
HASH_IN_VALUE=abc#notacomment
QUOTED_HASH="a # b"
EMPTY=

garbage line without equals
=noKey
LATER=first
LATER=second
`
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := ParseEnvFile(path)
	if err != nil {
		t.Fatalf("ParseEnvFile: %v", err)
	}
	want := map[string]string{
		"NOTION_TOKEN":  "ntn_double_quoted",
		"SINGLE":        "sq_value",
		"BARE":          "bare_value",
		"WITH_EXPORT":   "export_ok",
		"INDENTED":      "indented_ok",
		"TRAILING":      "has_trailing_ws",
		"INLINE":        "value",
		"HASH_IN_VALUE": "abc#notacomment", // '#' not preceded by whitespace ⇒ literal
		"QUOTED_HASH":   "a # b",           // '#' inside quotes is literal
		"EMPTY":         "",
		"LATER":         "second", // last assignment wins
	}
	for k, wv := range want {
		if gv, ok := m[k]; !ok || gv != wv {
			t.Errorf("%s = %q (present=%v), want %q", k, gv, ok, wv)
		}
	}
	// Garbage lines and empty-key lines must not appear.
	if _, ok := m["garbage line without equals"]; ok {
		t.Errorf("garbage line was parsed")
	}
	if len(m) != len(want) {
		t.Errorf("parsed %d keys, want %d: %v", len(m), len(want), m)
	}
}

func TestParseEnvFileMissing(t *testing.T) {
	if _, err := ParseEnvFile(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Errorf("ParseEnvFile on a missing file = nil error, want error")
	}
}

func TestResolveEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	if err := os.WriteFile(path, []byte(`FOO="from_file"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Environment wins over the file.
	getenvSet := func(string) string { return "from_env" }
	res := ResolveEnv("FOO", getenvSet, path)
	if res.Value != "from_env" || res.Source != SourceEnvironment {
		t.Errorf("env precedence: value=%q source=%q, want from_env/%s", res.Value, res.Source, SourceEnvironment)
	}
	// The file is still stat'd (FileExists/FileHas populated) even when env wins.
	if !res.FileExists || !res.FileHas {
		t.Errorf("FileExists=%v FileHas=%v, want both true", res.FileExists, res.FileHas)
	}

	// Empty environment ⇒ fall back to the file.
	getenvEmpty := func(string) string { return "" }
	res = ResolveEnv("FOO", getenvEmpty, path)
	if res.Value != "from_file" || res.Source != SourceFile {
		t.Errorf("file fallback: value=%q source=%q, want from_file/%s", res.Value, res.Source, SourceFile)
	}

	// Neither channel ⇒ unresolved.
	res = ResolveEnv("MISSING", getenvEmpty, path)
	if res.Value != "" || res.Source != "" {
		t.Errorf("unresolved: value=%q source=%q, want empty/empty", res.Value, res.Source)
	}
}

func TestResolveEnvLoosePerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	if err := os.WriteFile(path, []byte(`FOO=bar`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := ResolveEnv("FOO", func(string) string { return "" }, path)
	if !res.FileLoose {
		t.Errorf("FileLoose = false for a 0644 env file, want true")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	res = ResolveEnv("FOO", func(string) string { return "" }, path)
	if res.FileLoose {
		t.Errorf("FileLoose = true for a 0600 env file, want false")
	}
}

func TestResolveEnvNoFile(t *testing.T) {
	// Empty envPath and a missing file both behave as "file absent".
	res := ResolveEnv("FOO", func(string) string { return "v" }, "")
	if res.FileExists {
		t.Errorf("FileExists = true for empty envPath, want false")
	}
	res = ResolveEnv("FOO", func(string) string { return "" }, filepath.Join(t.TempDir(), "nope"))
	if res.FileExists || res.Value != "" {
		t.Errorf("missing file: FileExists=%v value=%q, want false/empty", res.FileExists, res.Value)
	}
}
