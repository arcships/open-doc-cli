package layout

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRootPrecedence(t *testing.T) {
	const home = "/home/tester"
	homeDir := func() (string, error) { return home, nil }

	tests := []struct {
		name     string
		flagRoot string
		env      map[string]string
		want     string
	}{
		{
			name:     "flag wins over env and default",
			flagRoot: "/explicit/root",
			env:      map[string]string{"OPENDOC_ROOT": "/env/root"},
			want:     "/explicit/root",
		},
		{
			name:     "env wins over default",
			flagRoot: "",
			env:      map[string]string{"OPENDOC_ROOT": "/env/root"},
			want:     "/env/root",
		},
		{
			name:     "default when nothing set",
			flagRoot: "",
			env:      map[string]string{},
			want:     filepath.Join(home, DefaultRootName),
		},
		{
			name:     "empty env falls through to default",
			flagRoot: "",
			env:      map[string]string{"OPENDOC_ROOT": ""},
			want:     filepath.Join(home, DefaultRootName),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(k string) string { return tt.env[k] }
			got, err := ResolveRoot(tt.flagRoot, getenv, homeDir)
			if err != nil {
				t.Fatalf("ResolveRoot: %v", err)
			}
			if got != tt.want {
				t.Fatalf("ResolveRoot = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveRootMakesAbsolute(t *testing.T) {
	got, err := ResolveRoot("relative/path", func(string) string { return "" }, func() (string, error) { return "/home", nil })
	if err != nil {
		t.Fatalf("ResolveRoot: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("ResolveRoot = %q, want absolute", got)
	}
}

func TestLayoutPaths(t *testing.T) {
	l := For("/root")
	cases := map[string]string{
		l.Internal:       "/root/.internal",
		l.ConfigPath():   "/root/.internal/config.toml",
		l.ManifestPath(): "/root/.internal/manifest.sqlite",
		l.LogsDir():      "/root/.internal/logs",
		l.TrashDir():     "/root/.internal/trash",
		l.AssetsDir():    "/root/assets",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	}
}

func TestEnsureInternalCreatesStructure(t *testing.T) {
	root := t.TempDir()
	l := For(filepath.Join(root, "mirror"))
	if err := l.EnsureInternal(); err != nil {
		t.Fatalf("EnsureInternal: %v", err)
	}
	for _, dir := range []string{l.Internal, l.LogsDir(), l.TrashDir(), l.AssetsDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
	}
	// Idempotent: second call must not error.
	if err := l.EnsureInternal(); err != nil {
		t.Fatalf("EnsureInternal (second call): %v", err)
	}
}
