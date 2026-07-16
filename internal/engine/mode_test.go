package engine

import (
	"testing"
	"time"
)

func tm(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// TestDecideNotionMode exercises the reconciliation-cadence policy with an
// injected clock: first run, no checkpoint, --full, daily-first-run,
// and the every-N-runs streak logic.
func TestDecideNotionMode(t *testing.T) {
	now := tm("2026-07-14T10:00:00Z")
	inc := runMeta{startedAt: tm("2026-07-14T09:00:00Z"), mode: modeIncremental}
	full := runMeta{startedAt: tm("2026-07-14T08:00:00Z"), mode: modeFull}
	yesterday := runMeta{startedAt: tm("2026-07-13T23:00:00Z"), mode: modeIncremental}

	cases := []struct {
		name      string
		forceFull bool
		hasCk     bool
		everyRuns int
		recent    []runMeta
		want      string
	}{
		{"first-ever run → full", false, false, 0, nil, modeFull},
		{"no checkpoint yet → full", false, false, 0, []runMeta{inc}, modeFull},
		{"--full forces full", true, true, 0, []runMeta{inc}, modeFull},
		{"same day, daily-only → incremental", false, true, 0, []runMeta{full}, modeIncremental},
		{"last run was yesterday → full (day's first run)", false, true, 0, []runMeta{yesterday}, modeFull},
		{"everyRuns=1 → always full", false, true, 1, []runMeta{full}, modeFull},
		{"everyRuns=2, streak 0 → incremental", false, true, 2, []runMeta{full}, modeIncremental},
		{"everyRuns=2, streak 1 → full", false, true, 2, []runMeta{inc, full}, modeFull},
		{"everyRuns=3, streak 1 → incremental", false, true, 3, []runMeta{inc, full}, modeIncremental},
		{"everyRuns=3, streak 2 → full", false, true, 3, []runMeta{inc, inc, full}, modeFull},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideNotionMode(now, c.forceFull, c.hasCk, c.everyRuns, c.recent)
			if got != c.want {
				t.Errorf("decideNotionMode = %q, want %q", got, c.want)
			}
		})
	}
}
