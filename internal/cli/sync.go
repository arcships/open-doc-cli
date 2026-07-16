package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/config"
	"github.com/arcships/open-doc-cli/internal/engine"
	"github.com/arcships/open-doc-cli/internal/feishu"
	"github.com/arcships/open-doc-cli/internal/layout"
	"github.com/arcships/open-doc-cli/internal/notion"
)

// syncSummary is the structured summary printed by `opendoc sync`.
type syncSummary struct {
	Root       string       `json:"root"`
	SyncRunID  int64        `json:"sync_run_id"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at"`
	Stats      engine.Stats `json:"stats"`
}

// runSync implements `opendoc sync`: it resolves the root, loads config, opens/creates
// the manifest, materialises the internal structure, runs the pipeline for each
// registered adapter, records the sync_runs rows, prints a structured summary,
// and exits 0.
func runSync(env Env, args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	root := fs.String("root", "", "mirror root (overrides OPENDOC_ROOT and ~/.opendoc)")
	asJSON := fs.Bool("json", false, "emit the summary as JSON")
	// --full forces a re-mirror: incremental skip paths are bypassed so every
	// document is refetched and rewritten. Rate limits still apply.
	full := fs.Bool("full", false, "force a full re-mirror (ignore incremental skips)")

	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: opendoc sync [platform] [flags]\n\nRun one pass of the sync pipeline.\n\nFlags:\n")
		fs.PrintDefaults()
	}

	// Accept an optional leading positional platform filter (Go's flag package
	// stops at the first non-flag arg, so extract it before parsing flags).
	platformFilter := ""
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		platformFilter = args[0]
		args = args[1:]
	}

	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if len(fs.Args()) > 0 {
		fmt.Fprintf(env.Stderr, "opendoc sync: too many arguments\n")
		return ExitUsage
	}
	if platformFilter != "" && platformFilter != "feishu" && platformFilter != "notion" {
		fmt.Fprintf(env.Stderr, "opendoc sync: unknown platform %q (want feishu|notion)\n", platformFilter)
		return ExitUsage
	}

	l, err := layout.Resolve(*root)
	if err != nil {
		fmt.Fprintf(env.Stderr, "opendoc sync: %v\n", err)
		return ExitError
	}
	if code := requireInitialized(env, l, "sync"); code != -1 {
		return code
	}

	adapters, err := buildAdapters(l, platformFilter)
	if err != nil {
		fmt.Fprintf(env.Stderr, "opendoc sync: %v\n", err)
		return ExitError
	}

	eng, err := engine.New(l, engine.Options{Adapters: adapters, Full: *full})
	if err != nil {
		fmt.Fprintf(env.Stderr, "opendoc sync: %v\n", err)
		return ExitError
	}
	defer eng.Close()

	res, err := eng.Sync(context.Background())
	if err != nil {
		fmt.Fprintf(env.Stderr, "opendoc sync: %v\n", err)
		return ExitError
	}

	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(syncSummary{
			Root:       res.Root,
			SyncRunID:  res.SyncRunID,
			StartedAt:  res.StartedAt,
			FinishedAt: res.FinishedAt,
			Stats:      res.Stats,
		}); err != nil {
			fmt.Fprintf(env.Stderr, "opendoc sync: %v\n", err)
			return ExitError
		}
		return ExitOK
	}

	printSyncSummary(env, res)
	return ExitOK
}

// buildAdapters constructs the platform adapters to run, honouring an optional
// platform filter and the mirror's config (only configured platforms register).
// It returns an empty slice when no platform is configured, so `opendoc sync` still
// runs the empty pipeline shell.
func buildAdapters(l layout.Layout, platformFilter string) ([]adapter.Adapter, error) {
	var cfg config.Config
	if config.Exists(l.ConfigPath()) {
		loaded, err := config.Load(l.ConfigPath())
		if err != nil {
			return nil, err
		}
		cfg = loaded
	} else {
		cfg = config.Default()
	}

	var adapters []adapter.Adapter
	if platformFilter == "" || platformFilter == "feishu" {
		// The Feishu engine is embedded in the opendoc binary itself (empty Bin ⇒
		// self-exec, see docs/dev/architecture.md). OPENDOC_LARK_CLI — from the process
		// environment or the <root>/.internal/env file — overrides it with an
		// external lark-cli binary as a debugging escape hatch.
		larkBin := config.ResolveEnv(larkCLIEnv, os.Getenv, l.EnvFilePath()).Value
		fa := feishu.NewAdapter(cfg.Feishu, feishu.ExecRunner{Bin: larkBin}, cfg.Sync.BitableInlineMaxRows)
		if fa.Configured() {
			adapters = append(adapters, fa)
		}
	}
	if platformFilter == "" || platformFilter == "notion" {
		if cfg.Notion.Enabled() {
			// Resolve the token from the process environment first, then fall back to
			// the private <root>/.internal/env file. This is
			// the same file the launchd wrapper used to source, so unattended runs no
			// longer need a wrapper to inject the token.
			token := config.ResolveEnv(cfg.Notion.TokenEnv, os.Getenv, l.EnvFilePath()).Value
			if token == "" {
				return nil, fmt.Errorf("notion is configured (token_env=%q) but %s is set neither in the environment nor in %s; export it or add it to that file",
					cfg.Notion.TokenEnv, cfg.Notion.TokenEnv, l.EnvFilePath())
			}
			adapters = append(adapters, notion.NewAdapter(token, nil))
		}
	}
	return adapters, nil
}

// printSyncSummary renders a compact human/agent-readable summary.
func printSyncSummary(env Env, res engine.Result) {
	dur := res.FinishedAt.Sub(res.StartedAt).Round(time.Millisecond)
	fmt.Fprintf(env.Stdout, "sync complete\n")
	fmt.Fprintf(env.Stdout, "  root:        %s\n", res.Root)
	fmt.Fprintf(env.Stdout, "  sync_run_id: %d\n", res.SyncRunID)
	fmt.Fprintf(env.Stdout, "  duration:    %s\n", dur)
	fmt.Fprintf(env.Stdout, "  adapters:    %d registered\n", len(res.Stats.Adapters))
	fmt.Fprintf(env.Stdout, "  added=%d updated=%d skipped=%d moved=%d deleted=%d trash_purged=%d\n",
		res.Stats.Added, res.Stats.Updated, res.Stats.Skipped, res.Stats.Moved, res.Stats.Deleted, res.Stats.TrashPurged)
	fmt.Fprintf(env.Stdout, "  assets: downloaded=%d pending=%d\n",
		res.Stats.AssetsDownloaded, res.Stats.AssetsPending)
	fmt.Fprintf(env.Stdout, "  degradations=%d (unknown=%d bitable_rendered=%d bitable_oversize=%d bitable_failed=%d truncated=%d unknown_block_ids=%d)\n",
		res.Stats.Degradations, res.Stats.UnknownBlocks, res.Stats.BitablesRendered,
		res.Stats.BitablesOversize, res.Stats.BitablesFailed, res.Stats.TruncatedPages, res.Stats.UnknownBlockIDs)
	fmt.Fprintf(env.Stdout, "  links_rewritten=%d failures=%d warnings=%d\n",
		res.Stats.LinksRewritten, len(res.Stats.Failures), len(res.Stats.Warnings))
	for _, w := range res.Stats.Warnings {
		fmt.Fprintf(env.Stdout, "  ⚠ %s\n", w)
	}
	for _, p := range res.Platforms {
		mode := p.Stats.Mode
		if mode == "" {
			mode = "-"
		}
		fmt.Fprintf(env.Stdout, "  [%s] mode=%s added=%d updated=%d skipped=%d moved=%d deleted=%d failures=%d (run %d)\n",
			p.Platform, mode, p.Stats.Added, p.Stats.Updated, p.Stats.Skipped, p.Stats.Moved, p.Stats.Deleted, len(p.Stats.Failures), p.SyncRunID)
	}
	for _, f := range res.Stats.Failures {
		fmt.Fprintf(env.Stdout, "  ! %s\n", f)
	}
}
