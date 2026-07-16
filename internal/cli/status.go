package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/arcships/open-doc-cli/internal/engine"
	"github.com/arcships/open-doc-cli/internal/layout"
	"github.com/arcships/open-doc-cli/internal/manifest"
)

// statusReport is the machine-friendly `opendoc status` payload (the shape emitted by
// --json and rendered as lines by the default human view). It is a read-only
// overview of the manifest: it never mutates the ledger.
type statusReport struct {
	Root      string           `json:"root"`
	Manifest  bool             `json:"manifest"`
	Documents docStats         `json:"documents"`
	Assets    assetStats       `json:"assets"`
	Platforms []platformStatus `json:"platforms"`
	SyncRuns  int              `json:"sync_runs"`
}

// docStats is the document population, split by platform and by status.
type docStats struct {
	Total      int            `json:"total"`
	ByPlatform map[string]int `json:"by_platform"`
	ByStatus   map[string]int `json:"by_status"`
}

// assetStats is the asset pool population.
type assetStats struct {
	Total   int `json:"total"`
	Pending int `json:"pending"`
}

// platformStatus is one platform's last-sync marker and the degradation counts
// recorded on its most recent run.
type platformStatus struct {
	Platform     string           `json:"platform"`
	LastSyncedAt string           `json:"last_synced_at,omitempty"`
	LastMode     string           `json:"last_mode,omitempty"`
	Degradations degradationStats `json:"degradations"`
}

// degradationStats is the per-run degradation breakdown surfaced in status. It
// mirrors the degradation fields of engine.Stats.
type degradationStats struct {
	Total            int `json:"total"`
	UnknownBlocks    int `json:"unknown_blocks"`
	BitablesRendered int `json:"bitables_rendered"`
	BitablesOversize int `json:"bitables_oversize"`
	BitablesFailed   int `json:"bitables_failed"`
	TruncatedPages   int `json:"truncated_pages"`
	UnknownBlockIDs  int `json:"unknown_block_ids"`
}

// runStatus implements `opendoc status`: a read-only manifest overview. It resolves
// the root, and — only if a manifest already exists — opens it to count
// documents/assets and read the last sync and degradation counts per platform.
// When no manifest exists yet it reports an empty overview rather than creating
// one, so the command never mutates state.
func runStatus(env Env, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	root := fs.String("root", "", "mirror root (overrides OPENDOC_ROOT and ~/.opendoc)")
	asJSON := fs.Bool("json", false, "emit the overview as JSON")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: opendoc status [flags]\n\nPrint a read-only manifest overview.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if len(fs.Args()) > 0 {
		fmt.Fprintf(env.Stderr, "opendoc status: too many arguments\n")
		return ExitUsage
	}

	l, err := layout.Resolve(*root)
	if err != nil {
		fmt.Fprintf(env.Stderr, "opendoc status: %v\n", err)
		return ExitError
	}
	if code := requireInitialized(env, l, "status"); code != -1 {
		return code
	}

	report, err := buildStatus(l)
	if err != nil {
		fmt.Fprintf(env.Stderr, "opendoc status: %v\n", err)
		return ExitError
	}

	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(env.Stderr, "opendoc status: %v\n", err)
			return ExitError
		}
		return ExitOK
	}
	printStatus(env, report)
	return ExitOK
}

// buildStatus assembles the overview from the manifest. It opens the manifest
// only when its file already exists (never creating one), so a status on a fresh
// or missing root reports an empty overview without side effects.
func buildStatus(l layout.Layout) (statusReport, error) {
	report := statusReport{
		Root:     l.Root,
		Manifest: false,
		Documents: docStats{
			ByPlatform: map[string]int{},
			ByStatus:   map[string]int{},
		},
		Platforms: []platformStatus{},
	}

	if !fileExists(l.ManifestPath()) {
		return report, nil
	}
	report.Manifest = true

	db, err := manifest.Open(l.ManifestPath())
	if err != nil {
		return statusReport{}, err
	}
	defer db.Close()

	counts, err := db.GroupedDocumentCounts()
	if err != nil {
		return statusReport{}, err
	}
	for _, c := range counts {
		report.Documents.Total += c.Count
		report.Documents.ByPlatform[c.Platform] += c.Count
		report.Documents.ByStatus[c.Status] += c.Count
	}

	total, err := db.CountAssets()
	if err != nil {
		return statusReport{}, err
	}
	pending, err := db.CountAssetsByStatus(statusAssetPendingLit)
	if err != nil {
		return statusReport{}, err
	}
	report.Assets = assetStats{Total: total, Pending: pending}

	runs, err := db.CountSyncRuns()
	if err != nil {
		return statusReport{}, err
	}
	report.SyncRuns = runs

	latest, err := db.LatestSyncRunPerPlatform()
	if err != nil {
		return statusReport{}, err
	}
	for _, p := range latest {
		ps := platformStatus{Platform: p.Platform, LastSyncedAt: p.FinishedAt}
		if p.Stats != "" {
			var s engine.Stats
			if err := json.Unmarshal([]byte(p.Stats), &s); err == nil {
				ps.LastMode = s.Mode
				ps.Degradations = degradationStats{
					Total:            s.Degradations,
					UnknownBlocks:    s.UnknownBlocks,
					BitablesRendered: s.BitablesRendered,
					BitablesOversize: s.BitablesOversize,
					BitablesFailed:   s.BitablesFailed,
					TruncatedPages:   s.TruncatedPages,
					UnknownBlockIDs:  s.UnknownBlockIDs,
				}
			}
		}
		report.Platforms = append(report.Platforms, ps)
	}
	return report, nil
}

// statusAssetPendingLit is the asset "pending" status literal (mirrors the
// engine's statusAssetPending, kept local so status does not depend on engine
// internals).
const statusAssetPendingLit = "pending"

// printStatus renders the overview as compact human/agent-readable lines.
func printStatus(env Env, r statusReport) {
	fmt.Fprintf(env.Stdout, "root: %s\n", r.Root)
	if !r.Manifest {
		fmt.Fprintf(env.Stdout, "manifest: none (no sync has run yet)\n")
		return
	}
	fmt.Fprintf(env.Stdout, "documents: %d total\n", r.Documents.Total)
	fmt.Fprintf(env.Stdout, "  by platform: %s\n", joinCounts(r.Documents.ByPlatform))
	fmt.Fprintf(env.Stdout, "  by status:   %s\n", joinCounts(r.Documents.ByStatus))
	fmt.Fprintf(env.Stdout, "assets: %d pending / %d total\n", r.Assets.Pending, r.Assets.Total)
	fmt.Fprintf(env.Stdout, "sync runs: %d\n", r.SyncRuns)
	if len(r.Platforms) == 0 {
		fmt.Fprintf(env.Stdout, "last sync: none\n")
		return
	}
	fmt.Fprintf(env.Stdout, "per platform:\n")
	for _, p := range r.Platforms {
		last := p.LastSyncedAt
		if last == "" {
			last = "-"
		}
		mode := p.LastMode
		if mode == "" {
			mode = "-"
		}
		fmt.Fprintf(env.Stdout, "  [%s] last_sync=%s mode=%s degradations=%d (unknown_blocks=%d bitable_rendered=%d bitable_oversize=%d bitable_failed=%d truncated=%d unknown_block_ids=%d)\n",
			p.Platform, last, mode, p.Degradations.Total, p.Degradations.UnknownBlocks,
			p.Degradations.BitablesRendered, p.Degradations.BitablesOversize, p.Degradations.BitablesFailed,
			p.Degradations.TruncatedPages, p.Degradations.UnknownBlockIDs)
	}
}

// joinCounts renders a count map as "key=n key=n" in sorted key order.
func joinCounts(m map[string]int) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%s=%d", k, m[k])
	}
	return out
}

// fileExists reports whether path is an existing regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
