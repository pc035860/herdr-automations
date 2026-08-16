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

// ids names the runs a selection picked, so a failure says which runs were
// chosen rather than dumping whole records.
func ids(rs []history.Record) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.RunID)
	}
	return out
}

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
	now := start.Add(20 * time.Minute)
	cases := map[history.Status]string{
		history.StatusRunning: "▶ 07:03 daily-graph-dream",
		history.StatusDone:    "✓ 07:03 daily-graph-dream",
		history.StatusFailed:  "✗ 07:03 daily-graph-dream",
	}
	for st, want := range cases {
		if got := runLabel("daily-graph-dream", start, now, st); got != want {
			t.Errorf("runLabel(%s) = %q, want %q", st, got, want)
		}
	}
}

func TestRunLabelDatesARunThatOutlivedItsDay(t *testing.T) {
	start := time.Date(2026, 8, 14, 7, 3, 0, 0, time.UTC)

	// The next morning a kept failure sits beside today's run of the same
	// automation. Without the date the two labels are identical but for the
	// glyph, which is exactly the case the date exists for.
	tomorrow := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	if got, want := runLabel("daily-graph-dream", start, tomorrow, history.StatusFailed),
		"✗ 8/14 07:03 daily-graph-dream"; got != want {
		t.Errorf("runLabel a day later = %q, want %q", got, want)
	}

	// A run that started before midnight and ended after it is still dated by
	// when it started, so its label matches the occurrence it belongs to.
	lateNight := time.Date(2026, 8, 14, 23, 50, 0, 0, time.UTC)
	if got, want := runLabel("nightly", lateNight, lateNight.Add(30*time.Minute), history.StatusDone),
		"✓ 8/14 23:50 nightly"; got != want {
		t.Errorf("runLabel across midnight = %q, want %q", got, want)
	}
}

func TestStalePicksOnlyLabelsThatWentAmbiguous(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 8, 14, 7, 0, 0, 0, time.UTC)

	rec := func(id string, st history.Status, started time.Time) history.Record {
		return history.Record{
			RunID: id, Automation: "daily-graph-dream", Status: st, Trigger: TriggerCron,
			Started: started, At: started, PaneID: "w9:p" + id, TabID: "w9:t" + id,
		}
	}
	runs := []history.Record{
		rec("today", history.StatusDone, now.Add(-2*time.Hour)),
		rec("yesterday", history.StatusFailed, yesterday),
		// A run whose tab the user closed by hand. History still has it; herdr
		// does not, and renaming it would be a rename aimed at nothing.
		rec("closed", history.StatusDone, yesterday),
		// Still going, and started before midnight: its label is a bare clock
		// time right now, and if the daemon dies mid-run nothing else ever
		// comes back for it.
		rec("running", history.StatusRunning, yesterday),
		{RunID: "nowhere", Status: history.StatusFailed, Started: yesterday, PaneID: "w9:pX"},
	}
	tabs := map[string]bool{"w9:tyesterday": true, "w9:trunning": true, "w9:ttoday": true}

	got := ids(stale(runs, tabs, nil, now, false))
	if !slices.Equal(got, []string{"yesterday"}) {
		t.Errorf("stale = %v, want [yesterday]: today's label is still unambiguous, "+
			"a closed tab is not there to rename, a run that never got a tab has "+
			"nowhere to put a label, and one in flight would have its ✓ overwritten "+
			"with ▶ if this got there second", got)
	}

	// At startup, though, a cron record stuck at running belongs to a run that
	// died with the daemon that started it. Nothing else will ever relabel it.
	got = ids(stale(runs, tabs, nil, now, true))
	if !slices.Equal(got, []string{"yesterday", "running"}) {
		t.Errorf("stale(orphans) = %v, want [yesterday running]", got)
	}
}

func TestOrphansAreOnlyTheRunsTheDaemonItselfStarted(t *testing.T) {
	// The board and the CLI run manual automations in their own process, which
	// outlives a daemon restart — so a manual run recorded running may well
	// still be running, and dating it now would put ▶ back over the ✓ it is
	// about to write. A record too old to say how it was triggered gets the
	// same benefit of the doubt.
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	rec := func(id, trigger string) history.Record {
		return history.Record{
			RunID: id, Automation: "daily-graph-dream", Status: history.StatusRunning,
			Trigger: trigger, Started: now.AddDate(0, 0, -1),
			PaneID: "w9:p" + id, TabID: "w9:t" + id,
		}
	}
	runs := []history.Record{
		rec("cron", TriggerCron),
		rec("catchup", TriggerCatchUp),
		rec("manual", TriggerManual),
		rec("legacy", ""),
	}
	tabs := map[string]bool{"w9:tcron": true, "w9:tcatchup": true, "w9:tmanual": true, "w9:tlegacy": true}

	got := ids(stale(runs, tabs, nil, now, true))
	if !slices.Equal(got, []string{"cron", "catchup"}) {
		t.Errorf("stale(orphans) = %v, want only the runs the daemon ran itself", got)
	}
}

func TestOnlyOneSweepRunsAtATime(t *testing.T) {
	// The caller has a rollover to spend and has to be told the sweep did not
	// land, or a sweep spanning midnight quietly costs the next day its own.
	sweeping.Store(true)
	defer sweeping.Store(false)
	if restamp(time.Now(), false) {
		t.Error("restamp claimed to have started a sweep while one was running")
	}
}

func TestStaleReachesRunsNoRetirementWould(t *testing.T) {
	// The panes most likely to sit on screen for days belong to automations that
	// retire nothing or never run again, and there is no bound on how far back
	// they go — so what counts is whether herdr still holds the tab, not how
	// deep into history the run sits.
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	kept := history.Record{
		RunID: "old", Automation: "once-off", Status: history.StatusDone,
		Started: now.AddDate(0, 0, -400), PaneID: "w9:p1", TabID: "w9:t1",
	}
	tabs := map[string]bool{"w9:t1": true}
	if got := ids(stale([]history.Record{kept}, tabs, nil, now, false)); !slices.Equal(got, []string{"old"}) {
		t.Errorf("stale = %v, want the year-old kept run dated", got)
	}
}

func TestStaleOnlyNamesAWorkspaceItIsSureOf(t *testing.T) {
	// A run holding a workspace id and no tab is either an automation that owns
	// that workspace, or a shared run that died between claiming the shared one
	// and opening its tab. Renaming the latter names every other automation's
	// home after this one run, so only an explicit placement may be acted on —
	// the same ambiguity retirement fail-closes on.
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	yesterday := now.AddDate(0, 0, -1)
	rec := func(id, placement string) history.Record {
		return history.Record{
			RunID: id, Automation: "daily-graph-dream", Status: history.StatusFailed,
			Started: yesterday, WorkspaceID: "w9", Placement: placement,
		}
	}
	shared := rec("shared", "shared")
	legacy := rec("legacy", "") // written before placement was recorded
	own := rec("own", "workspace")
	workspaces := map[string]bool{"w9": true}

	got := ids(stale([]history.Record{shared, legacy, own}, nil, workspaces, now, false))
	if !slices.Equal(got, []string{"own"}) {
		t.Errorf("stale = %v, want [own]: neither a shared run nor one whose "+
			"placement was never recorded may name the workspace it holds", got)
	}
	for _, r := range []history.Record{shared, legacy} {
		if err := relabelRun(r, r.Status, now); err != nil {
			t.Errorf("relabelRun(%s) = %v, want it to decline silently", r.RunID, err)
		}
	}
}

func TestStartedAtFallsBackForOlderRecords(t *testing.T) {
	// Records written before Started existed still have to be datable, or the
	// very labels this fixes — yesterday's, already on screen — stay ambiguous.
	end := time.Date(2026, 8, 14, 7, 12, 0, 0, time.UTC)
	if got := startedAt(history.Record{At: end}); !got.Equal(end) {
		t.Errorf("startedAt(no Started) = %v, want the last transition %v", got, end)
	}
	start := end.Add(-12 * time.Minute)
	if got := startedAt(history.Record{At: end, Started: start}); !got.Equal(start) {
		t.Errorf("startedAt = %v, want the run's start %v", got, start)
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
