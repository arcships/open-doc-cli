// Package engine holds the sync pipeline shell: it resolves the
// root, loads config, opens/creates the manifest, materialises the internal
// directory structure, drives the platform adapters, and records sync_runs
// rows. With no adapters registered it still exercises every structural side
// effect around the empty middle.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/config"
	"github.com/arcships/open-doc-cli/internal/layout"
	"github.com/arcships/open-doc-cli/internal/manifest"
)

// Document status values stored in manifest documents.status.
const (
	statusActive        = "active"
	statusPendingAssets = "pending_assets"
	statusTrashed       = "trashed"
)

// Asset status values stored in manifest assets.status.
const (
	statusAssetDone    = "done"
	statusAssetPending = "pending"
)

// Stats is the machine-readable sync report. It is serialised into
// sync_runs.stats and printed as the command summary.
type Stats struct {
	Added   int `json:"added"`
	Updated int `json:"updated"`
	Skipped int `json:"skipped"`
	Moved   int `json:"moved"`
	Deleted int `json:"deleted"`
	// TrashPurged counts trash entries aged out past trash_keep_days this run.
	TrashPurged      int `json:"trash_purged"`
	AssetsDownloaded int `json:"assets_downloaded"`
	AssetsPending    int `json:"assets_pending"`
	LinksRewritten   int `json:"links_rewritten"`
	// Degradations is the total count of lossy conversions — the last link in
	// the "lossy but traceable" red line — broken down by the fields below.
	Degradations int `json:"degradations"`
	// UnknownBlocks counts unrecognized tags preserved verbatim.
	UnknownBlocks int `json:"unknown_blocks"`
	// BitablesRendered counts bitables rendered inline as a table.
	BitablesRendered int `json:"bitables_rendered"`
	// BitablesOversize counts bitables degraded to schema + link (over threshold).
	BitablesOversize int `json:"bitables_oversize"`
	// BitablesFailed counts bitables whose API fetch failed (degraded to link).
	BitablesFailed int `json:"bitables_failed"`
	// TruncatedPages counts Notion pages the markdown endpoint reported truncated.
	TruncatedPages int `json:"truncated_pages"`
	// UnknownBlockIDs counts Notion blocks the markdown endpoint could not render.
	UnknownBlockIDs int `json:"unknown_block_ids"`
	// Failures lists per-document failures that did not abort the run.
	Failures []string `json:"failures"`
	// Warnings lists loud, non-fatal conditions the operator must see — chiefly
	// the permission-jitter guard aborting the delete step.
	Warnings []string `json:"warnings"`
	// Adapters lists the platform tags that participated in this run.
	Adapters []string `json:"adapters"`
	// Mode is the enumeration mode this platform ran in: "full" (complete
	// enumeration + delete/move reconciliation) or "incremental" (only docs
	// changed since the checkpoint; no delete/move detection). It is persisted in
	// sync_runs.stats so a later run can count rounds since the last full round.
	// Empty on the aggregate summary and on empty (no-adapter) runs.
	Mode string `json:"mode,omitempty"`
}

// addDegradation folds a per-document degradation breakdown into the stats.
func (s *Stats) addDegradation(d adapter.Degradation) {
	s.UnknownBlocks += d.UnknownBlocks
	s.BitablesRendered += d.BitablesRendered
	s.BitablesOversize += d.BitablesOversize
	s.BitablesFailed += d.BitablesFailed
	s.TruncatedPages += d.TruncatedPages
	s.UnknownBlockIDs += d.UnknownBlockIDs
	s.Degradations += d.Total()
}

// merge folds another platform's Stats into s (used to build the aggregate
// summary across adapters).
func (s *Stats) merge(o Stats) {
	s.Added += o.Added
	s.Updated += o.Updated
	s.Skipped += o.Skipped
	s.Moved += o.Moved
	s.Deleted += o.Deleted
	s.TrashPurged += o.TrashPurged
	s.AssetsDownloaded += o.AssetsDownloaded
	s.AssetsPending += o.AssetsPending
	s.LinksRewritten += o.LinksRewritten
	s.Degradations += o.Degradations
	s.UnknownBlocks += o.UnknownBlocks
	s.BitablesRendered += o.BitablesRendered
	s.BitablesOversize += o.BitablesOversize
	s.BitablesFailed += o.BitablesFailed
	s.TruncatedPages += o.TruncatedPages
	s.UnknownBlockIDs += o.UnknownBlockIDs
	s.Failures = append(s.Failures, o.Failures...)
	s.Warnings = append(s.Warnings, o.Warnings...)
	s.Adapters = append(s.Adapters, o.Adapters...)
}

// PlatformRun is the recorded outcome for a single platform within a Sync.
type PlatformRun struct {
	Platform  string
	SyncRunID int64
	Stats     Stats
}

// Result is the outcome of a Sync call, returned for the CLI to render.
type Result struct {
	// Root is the resolved mirror root.
	Root string
	// SyncRunID is the id of the sync_runs row recorded for this run.
	SyncRunID int64
	// StartedAt / FinishedAt bound the run.
	StartedAt  time.Time
	FinishedAt time.Time
	// Stats is the aggregate report across all platforms in this run.
	Stats Stats
	// Platforms holds the per-platform outcomes (one sync_runs row each).
	Platforms []PlatformRun
}

// Engine drives the sync pipeline for a resolved mirror root.
type Engine struct {
	layout   layout.Layout
	config   config.Config
	adapters []adapter.Adapter
	// full forces a re-mirror: incremental skip paths (remote_edited and
	// content_hash) are bypassed so every document is refetched and rewritten
	// (`--full`). It also forces every platform's round to run in full
	// reconciliation mode. Rate limits still apply.
	full bool
	// now returns the current time; a field so tests can inject a fake clock for
	// the daily-first-run reconciliation trigger. Defaults to time.Now.
	now func() time.Time
}

// Options configures a New engine.
type Options struct {
	// Adapters are the registered platform adapters; may be empty.
	Adapters []adapter.Adapter
	// Full requests a forced re-mirror (ignore incremental skips).
	Full bool
}

// New opens the mirror at l: it ensures the internal directory structure, opens
// (creating if needed) the manifest, and loads the config if present. The
// caller must Close the returned engine.
//
// The manifest handle is opened lazily per Sync run to keep the engine value
// cheap; New only validates that the structure can be created and config read.
func New(l layout.Layout, opts Options) (*Engine, error) {
	if err := l.EnsureInternal(); err != nil {
		return nil, err
	}
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
	return &Engine{layout: l, config: cfg, adapters: opts.Adapters, full: opts.Full, now: time.Now}, nil
}

// Config returns the loaded configuration.
func (e *Engine) Config() config.Config { return e.config }

// Sync runs one pass of the pipeline shell and records a sync_runs row.
//
// Pipeline shape (the adapter-driven middle is empty when no adapters
// are registered):
//
//	ensure structure -> open manifest -> [enumerate -> fetch -> assets ->
//	write -> link rewrite -> trash -> INDEX] -> record run -> report
func (e *Engine) Sync(ctx context.Context) (Result, error) {
	started := e.now()

	// Re-ensure structure so a Sync is self-sufficient even if the tree was
	// disturbed between construction and the run.
	if err := e.layout.EnsureInternal(); err != nil {
		return Result{}, err
	}

	db, err := manifest.Open(e.layout.ManifestPath())
	if err != nil {
		return Result{}, err
	}
	defer db.Close()

	aggregate := Stats{Failures: []string{}, Warnings: []string{}, Adapters: []string{}}
	var platforms []PlatformRun
	var lastID int64

	// No adapters registered: record a single empty run so the pipeline shell
	// is still exercised.
	if len(e.adapters) == 0 {
		id, err := recordRun(db, "", started, time.Now(), "", aggregate)
		if err != nil {
			return Result{}, err
		}
		finished := time.Now()
		return Result{Root: e.layout.Root, SyncRunID: id, StartedAt: started, FinishedAt: finished, Stats: aggregate}, nil
	}

	// Decide each platform's mode (full reconciliation vs incremental) up front,
	// with sequential manifest reads, before launching the concurrent workers.
	// Feishu has no incremental capability, so it is always full.
	plans := make([]platformPlan, len(e.adapters))
	for i, a := range e.adapters {
		mode, prevCk, err := e.decidePlatformMode(db, a, started)
		if err != nil {
			return Result{}, err
		}
		plans[i] = platformPlan{adapter: a, mode: mode, prevCheckpoint: prevCk}
	}

	// Run the platforms CONCURRENTLY: each drains its own enumeration, writes its
	// own documents, and records its own sync_runs row. The
	// manifest handle is shared but serialized (SetMaxOpenConns(1)); every platform
	// touches a disjoint id space, so there is no cross-platform data race. A fatal
	// error from one platform is isolated to its own run, aborting neither the other
	// platform nor the whole-library finalize.
	runs := make([]PlatformRun, len(plans))
	var wg sync.WaitGroup
	for i := range plans {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runs[i] = e.runPlatform(ctx, plans[i], db)
		}(i)
	}
	wg.Wait()

	for _, r := range runs {
		lastID = r.SyncRunID
		platforms = append(platforms, r)
		aggregate.merge(r.Stats)
	}

	// Whole-library finalize. Trash aging runs once (the trash dir is shared
	// across platforms): entries older than trash_keep_days are reclaimed from
	// disk and their manifest tombstones purged. Then INDEX.md is
	// regenerated, after every platform's documents are written and internal
	// links rewritten.
	finished := time.Now()
	purged, err := e.purgeAgedTrash(db, finished)
	if err != nil {
		return Result{}, err
	}
	aggregate.TrashPurged += purged
	if len(platforms) > 0 {
		platforms[len(platforms)-1].Stats.TrashPurged += purged
	}
	if err := e.generateIndex(db, finished); err != nil {
		return Result{}, err
	}

	return Result{
		Root:       e.layout.Root,
		SyncRunID:  lastID,
		StartedAt:  started,
		FinishedAt: finished,
		Stats:      aggregate,
		Platforms:  platforms,
	}, nil
}

// recordRun serialises stats and inserts one sync_runs row, returning its id. The
// checkpoint is the platform's persisted incremental high-water mark (max
// last_edited_time; "" for platforms without incremental enumeration).
func recordRun(db *manifest.DB, platform string, started, finished time.Time, checkpoint string, stats Stats) (int64, error) {
	statsJSON, err := json.Marshal(stats)
	if err != nil {
		return 0, fmt.Errorf("marshal stats: %w", err)
	}
	return db.InsertSyncRun(manifest.SyncRun{
		Platform:   platform,
		StartedAt:  started,
		FinishedAt: finished,
		Checkpoint: checkpoint,
		Stats:      string(statsJSON),
	})
}

// platformPlan is one platform's pre-decided run parameters: the adapter, the
// enumeration mode ("full" | "incremental"), and the checkpoint an incremental
// round resumes from.
type platformPlan struct {
	adapter        adapter.Adapter
	mode           string
	prevCheckpoint string
}

// runPlatform executes one platform's pipeline and records its sync_runs row,
// returning the outcome. A fatal error is isolated onto this platform's run (it
// is surfaced in the stats, not propagated) so the other platform and the
// whole-library finalize still complete (per-platform isolation).
func (e *Engine) runPlatform(ctx context.Context, plan platformPlan, db *manifest.DB) PlatformRun {
	a := plan.adapter
	platformStarted := time.Now()
	stats, checkpoint, err := e.runAdapter(ctx, a, db, plan.mode, plan.prevCheckpoint)
	if err != nil {
		// Isolate: keep the failure on this platform's report and its checkpoint
		// untouched (fall back to the previous one so a failed round never advances
		// the high-water mark).
		stats.Failures = append(stats.Failures, fmt.Sprintf("%s: %v", a.Platform(), err))
		stats.Warnings = append(stats.Warnings, fmt.Sprintf("%s: platform run failed; other platforms and finalize continue", a.Platform()))
		checkpoint = plan.prevCheckpoint
	}
	id, rerr := recordRun(db, a.Platform(), platformStarted, time.Now(), checkpoint, stats)
	if rerr != nil {
		// Recording the run itself failed: surface it, but there is nothing else to
		// do — return a zero id so the aggregate still reflects the work done.
		stats.Failures = append(stats.Failures, fmt.Sprintf("%s: record sync_run: %v", a.Platform(), rerr))
	}
	return PlatformRun{Platform: a.Platform(), SyncRunID: id, Stats: stats}
}

// Close releases engine resources; currently none beyond per-run handles.
func (e *Engine) Close() error { return nil }
