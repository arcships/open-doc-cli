package engine

import (
	"encoding/json"
	"time"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/manifest"
)

// Enumeration modes recorded in Stats.Mode and used to decide reconciliation
// cadence.
const (
	modeFull        = "full"
	modeIncremental = "incremental"
)

// recentRunLimit bounds how many prior runs the mode decision inspects. It only
// needs enough to count the incremental streak since the last full run; a full
// run always appears within a bounded window because the daily trigger forces one
// at least once a day.
const recentRunLimit = 64

// decidePlatformMode chooses a platform's enumeration mode for this run and, for
// an incremental round, the checkpoint to resume from. A platform without the
// IncrementalEnumerator capability (Feishu) is always full — its enumeration is
// inherently a complete inventory every round, so every round reconciles.
// now is the wall clock for the daily-first-run trigger.
func (e *Engine) decidePlatformMode(db *manifest.DB, a adapter.Adapter, now time.Time) (mode, prevCheckpoint string, err error) {
	if _, ok := a.(adapter.IncrementalEnumerator); !ok {
		return modeFull, "", nil
	}
	platform := a.Platform()
	prevCheckpoint, err = db.LatestCheckpoint(platform)
	if err != nil {
		return "", "", err
	}
	recent, err := db.RecentSyncRuns(platform, recentRunLimit)
	if err != nil {
		return "", "", err
	}
	metas := make([]runMeta, 0, len(recent))
	for _, r := range recent {
		metas = append(metas, runMeta{startedAt: parseRunTime(r.StartedAtRaw), mode: runModeOf(r.Stats)})
	}
	mode = decideNotionMode(now, e.full, prevCheckpoint != "", e.config.Sync.NotionReconcileEveryRuns, metas)
	return mode, prevCheckpoint, nil
}

// runMeta is the slice of a prior sync_runs row the mode decision needs: when it
// started (local-day comparison) and which mode it ran in (streak counting).
type runMeta struct {
	startedAt time.Time
	mode      string
}

// decideNotionMode is the pure reconciliation-cadence policy,
// factored out for direct unit testing with an injected clock. recent is the
// platform's prior runs, newest first. A run is FULL (complete search enumeration
// + delete/move detection) when any of these hold, else it is incremental:
//
//   - forceFull (--full),
//   - no usable checkpoint yet (first-ever run, or the checkpoint was lost),
//   - the most recent prior run started on an earlier local day (day's first run),
//   - everyRuns > 0 and this would be the everyRuns-th run since the last full one
//     (so everyRuns=1 ⇒ every round is full; everyRuns=2 ⇒ every other round).
func decideNotionMode(now time.Time, forceFull, hasCheckpoint bool, everyRuns int, recent []runMeta) string {
	if forceFull || !hasCheckpoint || len(recent) == 0 {
		return modeFull
	}
	// Day's first run: the last run happened on an earlier local calendar day.
	if !sameLocalDay(recent[0].startedAt, now) {
		return modeFull
	}
	// Every-N cadence: count the consecutive incremental runs immediately
	// preceding this one (a full run resets the streak). This run is the
	// (streak+1)-th since the last full, so reconcile when streak+1 >= everyRuns.
	if everyRuns > 0 {
		streak := 0
		for _, r := range recent {
			if r.mode == modeIncremental {
				streak++
				continue
			}
			break
		}
		if streak+1 >= everyRuns {
			return modeFull
		}
	}
	return modeIncremental
}

// sameLocalDay reports whether a and b fall on the same local calendar day. A
// zero time (unparseable prior started_at) is never "the same day", so it forces
// a reconciliation rather than risking a skipped daily reconcile.
func sameLocalDay(a, b time.Time) bool {
	if a.IsZero() {
		return false
	}
	al, bl := a.Local(), b.Local()
	ay, am, ad := al.Date()
	by, bm, bd := bl.Date()
	return ay == by && am == bm && ad == bd
}

// parseRunTime parses a stored RFC3339 started_at into a time, returning the zero
// time when empty/unparseable (which sameLocalDay treats as "force full").
func parseRunTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// runModeOf extracts the recorded mode from a sync_runs stats JSON blob. A blob
// without a mode reads as full, the safe default (it never inflates
// the incremental streak).
func runModeOf(statsJSON string) string {
	if statsJSON == "" {
		return modeFull
	}
	var s struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal([]byte(statsJSON), &s); err != nil || s.Mode == "" {
		return modeFull
	}
	return s.Mode
}
