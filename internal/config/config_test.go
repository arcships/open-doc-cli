package config

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	want := Config{
		Feishu: Feishu{
			WikiSpaces:       []string{"7384736370537086978", "123"},
			DriveFolders:     []string{"LudKfiCx7lNX2QdMcqBc2CL7nWb"},
			IncludeMyLibrary: true,
		},
		Notion: Notion{
			TokenEnv: "NOTION_TOKEN",
		},
		Sync: Sync{
			BitableInlineMaxRows:     200,
			TrashKeepDays:            30,
			NotionReconcileEveryRuns: 5,
		},
	}

	if err := Write(path, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !Exists(path) {
		t.Fatalf("Exists = false after Write")
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got = %+v\nwant = %+v", got, want)
	}
}

func TestDefaults(t *testing.T) {
	d := Default()
	if d.Sync.BitableInlineMaxRows != DefaultBitableInlineMaxRows {
		t.Errorf("BitableInlineMaxRows = %d, want %d", d.Sync.BitableInlineMaxRows, DefaultBitableInlineMaxRows)
	}
	if d.Sync.TrashKeepDays != DefaultTrashKeepDays {
		t.Errorf("TrashKeepDays = %d, want %d", d.Sync.TrashKeepDays, DefaultTrashKeepDays)
	}
	if d.Sync.NotionReconcileEveryRuns != DefaultNotionReconcileEveryRuns {
		t.Errorf("NotionReconcileEveryRuns = %d, want %d", d.Sync.NotionReconcileEveryRuns, DefaultNotionReconcileEveryRuns)
	}
	if d.Feishu.IncludeMyLibrary {
		t.Errorf("IncludeMyLibrary default = true, want false")
	}
	if d.Notion.Enabled() {
		t.Errorf("Notion should be disabled by default (empty token_env)")
	}
}

func TestNotionEnabled(t *testing.T) {
	if (Notion{TokenEnv: ""}).Enabled() {
		t.Error("empty token_env must be disabled")
	}
	if !(Notion{TokenEnv: "NOTION_TOKEN"}).Enabled() {
		t.Error("non-empty token_env must be enabled")
	}
}

func TestWriteAtomicOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	first := Default()
	first.Feishu.WikiSpaces = []string{"a"}
	if err := Write(path, first); err != nil {
		t.Fatalf("Write first: %v", err)
	}

	second := Default()
	second.Feishu.WikiSpaces = []string{"b", "c"}
	if err := Write(path, second); err != nil {
		t.Fatalf("Write second: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got.Feishu.WikiSpaces, []string{"b", "c"}) {
		t.Fatalf("overwrite failed, got %v", got.Feishu.WikiSpaces)
	}
}
