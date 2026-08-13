package runner

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DnzzL/herdr-automations/internal/history"
)

func TestExpiredKeepsTheNewestAndAllowsFailuresMore(t *testing.T) {
	// Newest first, as history.Runs returns them.
	runs := []history.Record{
		{RunID: "6", Status: history.StatusRunning, PaneID: "p6"},
		{RunID: "5", Status: history.StatusDone, PaneID: "p5"},
		{RunID: "4", Status: history.StatusFailed, PaneID: "p4"},
		{RunID: "3", Status: history.StatusDone, PaneID: "p3"},
		{RunID: "2", Status: history.StatusMissed}, // never got a pane
		{RunID: "1", Status: history.StatusFailed, PaneID: "p1"},
	}
	ids := func(rs []history.Record) []string {
		out := []string{}
		for _, r := range rs {
			out = append(out, r.RunID)
		}
		return out
	}

	// keep 1: the incoming run takes the single success slot, so both earlier
	// successes go; two failures fit inside an allowance of three.
	if got := ids(expired(runs, 1, 3)); !slices.Equal(got, []string{"5", "3"}) {
		t.Errorf("expired(keep 1, keepFailed 3) = %v, want [5 3]", got)
	}
	// keep 2: the newest success survives alongside the incoming run.
	if got := ids(expired(runs, 2, 3)); !slices.Equal(got, []string{"3"}) {
		t.Errorf("expired(keep 2) = %v, want [3]", got)
	}
	// keepFailed 1: the older failure is retired too.
	if got := ids(expired(runs, 1, 1)); !slices.Equal(got, []string{"5", "3", "1"}) {
		t.Errorf("expired(keepFailed 1) = %v, want [5 3 1]", got)
	}
	// keep 0 retires everything finished, including the newest success.
	if got := ids(expired(runs, 0, 0)); !slices.Equal(got, []string{"5", "4", "3", "1"}) {
		t.Errorf("expired(keep 0) = %v, want [5 4 3 1]", got)
	}
}

func TestAgentNameIsUniquePerRunAndFitsHerdrsLimit(t *testing.T) {
	long := "a-very-long-automation-name-that-overflows-the-limit"
	_, firstToken := newRunID(long)
	_, secondToken := newRunID(long)
	first, second := agentName(long, firstToken), agentName(long, secondToken)

	for _, got := range []string{first, second} {
		if len(got) > 32 {
			t.Errorf("agentName = %q (%d chars), want at most 32", got, len(got))
		}
		if strings.Trim(got, "abcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
			t.Errorf("agentName = %q, contains characters herdr rejects", got)
		}
	}
	if first == second {
		// Two runs of one automation coexist whenever the earlier pane is
		// retained; sharing a name makes the second one fail to start.
		t.Errorf("agentName is not unique per run: %q twice", first)
	}
}

func TestSlugProducesValidBranchNames(t *testing.T) {
	cases := map[string]string{
		"Weekly sprint planning": "weekly-sprint-planning",
		"issue-triage":           "issue-triage",
		"Deps  bump!!":           "deps-bump",
		"  ~weird/name~  ":       "weird-name",
		"???":                    "automation",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLimiterBoundsConcurrentRuns(t *testing.T) {
	l := &limiter{limit: 2}
	var mu sync.Mutex
	active, peak := 0, 0

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.acquire()
			mu.Lock()
			active++
			if active > peak {
				peak = active
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			active--
			mu.Unlock()
			l.release()
		}()
	}
	wg.Wait()

	if peak > 2 {
		t.Errorf("peak concurrency = %d, want at most 2", peak)
	}
	if peak < 2 {
		t.Errorf("peak concurrency = %d, want the limit to actually be used", peak)
	}
}

func TestRunLabelCarriesTimeAndOutcome(t *testing.T) {
	start := time.Date(2026, 8, 14, 7, 3, 0, 0, time.UTC)
	cases := map[history.Status]string{
		history.StatusRunning: "▶ daily-graph-dream 07:03",
		history.StatusDone:    "✓ daily-graph-dream 07:03",
		history.StatusFailed:  "✗ daily-graph-dream 07:03",
	}
	for st, want := range cases {
		if got := runLabel("daily-graph-dream", start, st); got != want {
			t.Errorf("runLabel(%s) = %q, want %q", st, got, want)
		}
	}
}
