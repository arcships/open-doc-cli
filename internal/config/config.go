// Package config defines the on-disk configuration for an opendoc mirror root and
// the helpers to read and write it. The configuration lives inside the mirror
// root's internal state directory (<root>/.internal/config.toml).
//
// The [feishu], [sync], and [notion] sections are materialised. The Notion
// adapter is enabled iff the [notion] section is present and its
// token_env environment variable is non-empty at runtime.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Default values for the [sync] section.
const (
	// DefaultBitableInlineMaxRows is the row-count threshold above which a
	// Feishu bitable is stored as schema + link instead of a rendered table.
	DefaultBitableInlineMaxRows = 200
	// DefaultTrashKeepDays is how long tombstoned documents are retained in
	// <root>/.internal/trash before sync reclaims them.
	DefaultTrashKeepDays = 30
	// DefaultNotionTokenEnv is the environment variable that holds the Notion
	// internal-integration token when the [notion] section names none.
	DefaultNotionTokenEnv = "NOTION_TOKEN"
	// DefaultNotionReconcileEveryRuns is the default cadence for forced Notion
	// reconciliation rounds. 0 means "daily-only" (reconcile only on the first run
	// of each day, plus first-ever run and --full).
	DefaultNotionReconcileEveryRuns = 0
)

// Feishu holds the Feishu (Lark) mirroring scope. Credentials are fully
// delegated to the embedded lark engine (config + keychain under ~/.lark-cli)
// and never stored here.
type Feishu struct {
	// WikiSpaces lists the wiki space IDs to mirror.
	WikiSpaces []string `toml:"wiki_spaces"`
	// DriveFolders lists the drive folder tokens to mirror.
	DriveFolders []string `toml:"drive_folders"`
	// IncludeMyLibrary mirrors the user's personal drive ("my library").
	IncludeMyLibrary bool `toml:"include_my_library"`
}

// Notion holds the Notion mirroring configuration. The mirror scope is the full
// set of pages the internal integration is connected to (no per-page config).
// The integration token is never stored here; only the name of the environment
// variable that holds it.
type Notion struct {
	// TokenEnv is the environment variable that carries the integration token
	// (e.g. "NOTION_TOKEN"). An empty value means the platform is disabled.
	TokenEnv string `toml:"token_env"`
}

// Enabled reports whether the Notion section names a token environment variable.
// Whether that variable is actually set at runtime is checked separately (so a
// configured-but-missing token yields a helpful error rather than silent skip).
func (n Notion) Enabled() bool { return n.TokenEnv != "" }

// Sync holds engine-wide tuning knobs.
type Sync struct {
	// BitableInlineMaxRows is the inline-render row threshold for bitables.
	BitableInlineMaxRows int `toml:"bitable_inline_max_rows"`
	// TrashKeepDays is the retention window for trashed documents, in days.
	TrashKeepDays int `toml:"trash_keep_days"`
	// NotionReconcileEveryRuns forces a full Notion reconciliation round every N
	// runs, on top of the always-on triggers (first run, --full, first run of a
	// day). 0 (default) means daily-only; 1 makes every round a reconciliation.
	NotionReconcileEveryRuns int `toml:"notion_reconcile_every_runs"`
}

// Config is the full parsed configuration document.
type Config struct {
	Feishu Feishu `toml:"feishu"`
	Notion Notion `toml:"notion"`
	Sync   Sync   `toml:"sync"`
}

// Default returns a Config populated with the defaults and empty Feishu scope.
// The Notion section is left empty (disabled) by default. Callers may override
// fields before writing.
func Default() Config {
	return Config{
		Feishu: Feishu{
			WikiSpaces:       []string{},
			DriveFolders:     []string{},
			IncludeMyLibrary: false,
		},
		Notion: Notion{
			TokenEnv: "",
		},
		Sync: Sync{
			BitableInlineMaxRows:     DefaultBitableInlineMaxRows,
			TrashKeepDays:            DefaultTrashKeepDays,
			NotionReconcileEveryRuns: DefaultNotionReconcileEveryRuns,
		},
	}
}

// Load reads and decodes the config file at path.
func Load(path string) (Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return Config{}, fmt.Errorf("load config %s: %w", path, err)
	}
	return c, nil
}

// Write encodes c to path atomically (temp file + rename). Parent directories
// must already exist. It never overwrites via a partial write: a crash leaves
// either the old file or the fully written new one.
func Write(path string, c Config) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.toml.tmp")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename.
	defer func() { _ = os.Remove(tmpName) }()

	enc := toml.NewEncoder(tmp)
	if err := enc.Encode(c); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename config into place: %w", err)
	}
	return nil
}

// Exists reports whether a regular file exists at path.
func Exists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
