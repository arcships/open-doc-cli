// Package layout resolves the mirror-root location and describes the on-disk
// layout of an opendoc mirror (see docs/dev/architecture.md).
//
// The mirror root is where the Markdown tree and the internal state directory
// live. It is resolved with the precedence: --root flag > OPENDOC_ROOT env >
// default ~/.opendoc. The internal state directory is always <root>/.internal,
// independent of where the root itself sits.
package layout

import (
	"fmt"
	"os"
	"path/filepath"
)

// InternalDirName is the fixed name of the internal state directory under the
// mirror root. It is deliberately ".internal" and not ".opendoc" so the default
// root does not nest as ~/.opendoc/.opendoc.
const InternalDirName = ".internal"

// DefaultRootName is the mirror root directory created under the user's home
// directory when neither --root nor OPENDOC_ROOT is provided.
const DefaultRootName = ".opendoc"

// Layout holds the resolved absolute paths for a mirror root.
type Layout struct {
	// Root is the absolute mirror-root path.
	Root string
	// Internal is <Root>/.internal.
	Internal string
}

// ResolveRoot resolves the mirror root using the precedence
// flagRoot > OPENDOC_ROOT env > ~/.opendoc. flagRoot is the value of the --root flag
// (empty when unset). The returned path is absolute and cleaned but is not
// created on disk.
func ResolveRoot(flagRoot string, getenv func(string) string, homeDir func() (string, error)) (string, error) {
	var raw string
	switch {
	case flagRoot != "":
		raw = flagRoot
	case getenv("OPENDOC_ROOT") != "":
		raw = getenv("OPENDOC_ROOT")
	default:
		home, err := homeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir for default root: %w", err)
		}
		raw = filepath.Join(home, DefaultRootName)
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("make root absolute: %w", err)
	}
	return abs, nil
}

// For builds a Layout from an already-resolved absolute root path.
func For(root string) Layout {
	return Layout{
		Root:     root,
		Internal: filepath.Join(root, InternalDirName),
	}
}

// Resolve is a convenience wrapper that resolves the root from the process
// environment and returns the corresponding Layout.
func Resolve(flagRoot string) (Layout, error) {
	root, err := ResolveRoot(flagRoot, os.Getenv, os.UserHomeDir)
	if err != nil {
		return Layout{}, err
	}
	return For(root), nil
}

// ConfigPath returns <Internal>/config.toml.
func (l Layout) ConfigPath() string { return filepath.Join(l.Internal, "config.toml") }

// ManifestPath returns <Internal>/manifest.sqlite.
func (l Layout) ManifestPath() string { return filepath.Join(l.Internal, "manifest.sqlite") }

// EnvFilePath returns <Internal>/env, the private shell-style env file opendoc reads
// natively to resolve token variables (e.g. NOTION_TOKEN) that are not present in
// the process environment — the same file the launchd wrapper historically
// sourced. Keep it chmod 600.
func (l Layout) EnvFilePath() string { return filepath.Join(l.Internal, "env") }

// LogsDir returns <Internal>/logs.
func (l Layout) LogsDir() string { return filepath.Join(l.Internal, "logs") }

// TrashDir returns <Internal>/trash.
func (l Layout) TrashDir() string { return filepath.Join(l.Internal, "trash") }

// TrashRelDir returns the trash directory relative to the mirror root
// (".internal/trash", forward-slashed). It is the prefix under which a deleted
// document's original relative path is preserved.
func (l Layout) TrashRelDir() string { return InternalDirName + "/trash" }

// AssetsDir returns <Root>/assets, the global content-addressed asset pool.
func (l Layout) AssetsDir() string { return filepath.Join(l.Root, "assets") }

// EnsureInternal creates the internal state directory tree (.internal, logs,
// trash) and the assets pool, idempotently. It does not create config or the
// manifest — those are managed by their own packages.
func (l Layout) EnsureInternal() error {
	for _, dir := range []string{l.Root, l.Internal, l.LogsDir(), l.TrashDir(), l.AssetsDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}
