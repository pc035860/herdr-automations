package runner

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DnzzL/herdr-automations/internal/herdr"
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
	// Negative means never retire — not "a negative budget", which would
	// retire every failure instead. keep 0 must still retire, so the two
	// cannot be the same number internally.
	if got := ids(expired(runs, 1, -1)); !slices.Equal(got, []string{"5", "3"}) {
		t.Errorf("expired(keepFailed -1) = %v, want the failures kept", got)
	}
	if got := expired(runs, -1, -1); len(got) != 0 {
		t.Errorf("expired(all negative) = %v, want nothing retired", ids(got))
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

func TestClosableRefusesEveryUncertainty(t *testing.T) {
	shared := history.Record{
		RunID: "r1", WorkspaceID: "w9", TabID: "w9:t2", PaneID: "w9:p2",
		Placement: "shared",
	}
	own := history.Record{RunID: "r2", WorkspaceID: "w5", PaneID: "w5:p1", Placement: "workspace"}
	idle := paneState{agent: "idle"}

	if got, ok := closable(shared, idle); !ok || got.tabID != "w9:t2" {
		t.Errorf("settled shared run: got %+v ok=%v, want its tab closed", got, ok)
	}
	if got, ok := closable(own, idle); !ok || got.workspaceID != "w5" {
		t.Errorf("settled own-workspace run: got %+v ok=%v, want its workspace closed", got, ok)
	}
	// A pane with no agent left is closeable; that is the ordinary case once
	// the agent has exited.
	if _, ok := closable(shared, paneState{}); !ok {
		t.Error("pane with no agent was kept")
	}

	refused := map[string]paneState{
		"on screen":          {agent: "idle", onScreen: true},
		"agent working":      {agent: "working"},
		"agent blocked":      {agent: "blocked"},
		"agent unclassified": {agent: "unknown"},
		"foreground command": {busy: true},
	}
	for why, s := range refused {
		if _, ok := closable(shared, s); ok {
			t.Errorf("%s: closed anyway", why)
		}
	}

	// A shared run that claimed the workspace but never got its tab must never
	// fall through to closing the workspace — that is everyone else's tabs.
	orphan := history.Record{RunID: "r3", WorkspaceID: "w9", Placement: "shared"}
	if got, ok := closable(orphan, idle); ok {
		t.Errorf("shared run without a tab closed %+v", got)
	}
	// Records predating the placement field look exactly like that orphan, so
	// they get the same answer rather than a guess.
	legacy := history.Record{RunID: "r5", WorkspaceID: "w9", PaneID: "w9:p3"}
	if got, ok := closable(legacy, idle); ok {
		t.Errorf("record without placement closed %+v", got)
	}
	if _, ok := closable(history.Record{RunID: "r4"}, idle); ok {
		t.Error("record with no workspace closed something")
	}
}

// stalled is herdr refusing to call a submission observed: the text reached
// the agent's input and nothing happened.
func stalled() error { return &herdr.APIError{Code: "agent_prompt_stalled"} }

// tick hands submitWith a clock that advances a fixed step per reading, so a
// test can say how long the agent stayed unreachable without sleeping.
func tick(step time.Duration) func() time.Time {
	now := time.Now()
	return func() time.Time {
		now = now.Add(step)
		return now
	}
}

func TestSubmitKeepsOfferingThePromptWhileTheAgentIsStillComingUp(t *testing.T) {
	// The 2026-08-14 07:00 run: herdr called the agent idle while its TUI was
	// still connecting MCP servers, so the early passes had nowhere to type.
	// Retrying once gave up inside that window and failed a run that worked.
	prompts, enters := 0, 0
	err := submitWith(promptOps{
		prompt: func() error {
			prompts++
			if prompts < 4 {
				return stalled()
			}
			return nil
		},
		enter:   func() error { enters++; return nil },
		working: func() error { return stalled() },
		settle: func() error {
			t.Error("settled without the agent ever starting work")
			return nil
		},
	}, tick(20*time.Second), 2*time.Minute)
	if err != nil {
		t.Fatalf("gave up on an agent that took the prompt: %v", err)
	}
	if prompts != 4 {
		t.Errorf("offered the prompt %d times, want 4", prompts)
	}
	if enters != 3 {
		t.Errorf("pressed Enter %d times, want one per stall (3)", enters)
	}
}

func TestSubmitStopsOnceTheWindowIsSpent(t *testing.T) {
	// An agent that is genuinely wedged must still fail the run rather than
	// retype the prompt forever.
	prompts := 0
	err := submitWith(promptOps{
		prompt:  func() error { prompts++; return stalled() },
		enter:   func() error { return nil },
		working: func() error { return stalled() },
		settle:  func() error { return nil },
	}, tick(30*time.Second), time.Minute)
	if err == nil {
		t.Fatal("a permanently stalled agent reported success")
	}
	if !strings.Contains(err.Error(), "never accepted it") {
		t.Errorf("unhelpful failure: %v", err)
	}
	if prompts > 5 {
		t.Errorf("typed the prompt %d times into a wedged agent", prompts)
	}
}

func TestSubmitTakesARecoveredEnterAsAcceptance(t *testing.T) {
	// The text landed and only its Enter was swallowed: pressing Enter is
	// enough, and typing it a second time would run the prompt twice.
	prompts, settles := 0, 0
	err := submitWith(promptOps{
		prompt:  func() error { prompts++; return stalled() },
		enter:   func() error { return nil },
		working: func() error { return nil },
		settle:  func() error { settles++; return nil },
	}, tick(time.Second), time.Minute)
	if err != nil {
		t.Fatalf("recovered submission reported failure: %v", err)
	}
	if prompts != 1 || settles != 1 {
		t.Errorf("prompted %d times and settled %d, want 1 and 1", prompts, settles)
	}
}
