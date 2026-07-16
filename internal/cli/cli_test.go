package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcships/open-doc-cli/internal/config"
	"github.com/arcships/open-doc-cli/internal/layout"
)

// newEnv builds a test Env with buffer-backed I/O (non-interactive) and args.
func newEnv(args ...string) (Env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return Env{
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errb,
		Args:   args,
	}, &out, &errb
}

// writeDefaultConfig materialises <root>/.internal and a default config.toml, so
// a command under test sees an initialized mirror (sync/status/resolve return
// ExitNotInitialized without it).
func writeDefaultConfig(t *testing.T, root string) {
	t.Helper()
	l := layout.For(root)
	if err := l.EnsureInternal(); err != nil {
		t.Fatalf("EnsureInternal: %v", err)
	}
	if err := config.Write(l.ConfigPath(), config.Default()); err != nil {
		t.Fatalf("config.Write: %v", err)
	}
}

func TestInitNonInteractive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	env, out, errb := newEnv("init",
		"--root", root,
		"--feishu-wiki-spaces", "111,222",
		"--feishu-drive-folders", "aaa",
		"--include-my-library",
	)

	if code := Run(env); code != ExitOK {
		t.Fatalf("Run init = %d, want %d; stderr=%s", code, ExitOK, errb.String())
	}

	l := layout.For(root)
	if !config.Exists(l.ConfigPath()) {
		t.Fatalf("config not written at %s", l.ConfigPath())
	}
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Feishu.WikiSpaces; len(got) != 2 || got[0] != "111" || got[1] != "222" {
		t.Errorf("WikiSpaces = %v, want [111 222]", got)
	}
	if got := cfg.Feishu.DriveFolders; len(got) != 1 || got[0] != "aaa" {
		t.Errorf("DriveFolders = %v, want [aaa]", got)
	}
	if !cfg.Feishu.IncludeMyLibrary {
		t.Errorf("IncludeMyLibrary = false, want true")
	}
	if cfg.Sync.BitableInlineMaxRows != config.DefaultBitableInlineMaxRows {
		t.Errorf("BitableInlineMaxRows = %d, want default", cfg.Sync.BitableInlineMaxRows)
	}
	if !strings.Contains(out.String(), l.ConfigPath()) {
		t.Errorf("stdout missing config path: %q", out.String())
	}
}

func TestInitRefusesOverwrite(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")

	env1, _, errb1 := newEnv("init", "--root", root)
	if code := Run(env1); code != ExitOK {
		t.Fatalf("first init = %d; stderr=%s", code, errb1.String())
	}

	env2, _, errb2 := newEnv("init", "--root", root)
	if code := Run(env2); code != ExitError {
		t.Fatalf("second init = %d, want %d (refuse overwrite)", code, ExitError)
	}
	if !strings.Contains(errb2.String(), "--force") {
		t.Errorf("stderr should mention --force, got %q", errb2.String())
	}

	// --force succeeds.
	env3, _, errb3 := newEnv("init", "--root", root, "--force")
	if code := Run(env3); code != ExitOK {
		t.Fatalf("forced init = %d; stderr=%s", code, errb3.String())
	}
}

func TestSyncEmptyRunJSON(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	writeDefaultConfig(t, root)
	env, out, errb := newEnv("sync", "--root", root, "--json")
	if code := Run(env); code != ExitOK {
		t.Fatalf("sync = %d, want %d; stderr=%s", code, ExitOK, errb.String())
	}

	var summary syncSummary
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("summary not valid JSON: %v; got %q", err, out.String())
	}
	if summary.SyncRunID <= 0 {
		t.Errorf("SyncRunID = %d, want > 0", summary.SyncRunID)
	}
	if summary.Root != root {
		t.Errorf("Root = %q, want %q", summary.Root, root)
	}
}

// TestCommandsNotInitialized asserts that sync/status/resolve return
// ExitNotInitialized with a setup.md pointer on stderr when no config exists.
func TestCommandsNotInitialized(t *testing.T) {
	for _, cmd := range [][]string{
		{"sync"},
		{"status"},
		{"resolve", "some-id"},
	} {
		t.Run(cmd[0], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "mirror")
			args := append([]string{cmd[0], "--root", root}, cmd[1:]...)
			env, _, errb := newEnv(args...)
			if code := Run(env); code != ExitNotInitialized {
				t.Fatalf("%s (uninitialized) = %d, want %d; stderr=%s", cmd[0], code, ExitNotInitialized, errb.String())
			}
			if !strings.Contains(errb.String(), "setup.md") {
				t.Errorf("%s stderr missing setup.md pointer: %q", cmd[0], errb.String())
			}
		})
	}
}

// TestBuildAdaptersEnvFileFallback covers fix 4 end-to-end at the sync seam: when
// the Notion token is absent from the process environment but present in
// <root>/.internal/env, buildAdapters still constructs the Notion adapter.
func TestBuildAdaptersEnvFileFallback(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	l := layout.For(root)
	if err := l.EnsureInternal(); err != nil {
		t.Fatalf("EnsureInternal: %v", err)
	}
	// A token env var name unlikely to exist in the real environment.
	const tokenEnv = "OPENDOC_TEST_NOTION_TOKEN_FALLBACK"
	if v := os.Getenv(tokenEnv); v != "" {
		t.Skipf("%s is set in the environment; cannot test the file fallback", tokenEnv)
	}
	cfg := config.Default()
	cfg.Notion.TokenEnv = tokenEnv
	if err := config.Write(l.ConfigPath(), cfg); err != nil {
		t.Fatalf("config.Write: %v", err)
	}
	if err := os.WriteFile(l.EnvFilePath(),
		[]byte(`export `+tokenEnv+`="ntn_from_env_file"`+"\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	adapters, err := buildAdapters(l, "notion")
	if err != nil {
		t.Fatalf("buildAdapters: %v", err)
	}
	if len(adapters) != 1 {
		t.Fatalf("buildAdapters returned %d adapters, want 1 (Notion via env-file fallback)", len(adapters))
	}
	if adapters[0].Platform() != "notion" {
		t.Errorf("adapter platform = %q, want notion", adapters[0].Platform())
	}
}

func TestUnknownCommand(t *testing.T) {
	env, _, _ := newEnv("bogus")
	if code := Run(env); code != ExitUsage {
		t.Fatalf("unknown command = %d, want %d", code, ExitUsage)
	}
}

func TestNoArgs(t *testing.T) {
	env, _, _ := newEnv()
	if code := Run(env); code != ExitUsage {
		t.Fatalf("no args = %d, want %d", code, ExitUsage)
	}
}
