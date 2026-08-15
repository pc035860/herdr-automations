package history

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestRecordsSurviveTheAdditionOfStarted(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())

	// A line written before Started existed. The log is append-only and never
	// migrated, so every reader has to keep working against records like this
	// one — and the panes they describe are on screen right now.
	legacy := `{"run_id":"r1","automation":"triage","status":"done","at":"2026-08-14T07:12:00Z",` +
		`"tab_id":"w9:t1","pane_id":"w9:p1"}` + "\n"
	if err := os.WriteFile(path(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	runs, err := Runs("triage", 0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("Runs = %+v, %v, want the legacy record read back", runs, err)
	}
	if !runs[0].Started.IsZero() {
		t.Errorf("Started = %v, want the zero time so callers can tell it was never recorded",
			runs[0].Started)
	}
	if runs[0].Status != StatusDone || runs[0].PaneID != "w9:p1" {
		t.Errorf("legacy record decoded as %+v, want its other fields intact", runs[0])
	}

	// And a record that never gets a Started must not write the year-1 sentinel
	// back out, which is what omitempty on a time.Time would do.
	if err := Append(Record{RunID: "r2", Automation: "triage", Status: StatusMissed}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"started":"0001-01-01`) {
		t.Errorf("history holds a zero-time started field:\n%s", raw)
	}
}

func TestRunsCollapsesToLatestState(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	base := time.Now()
	steps := []Record{
		{RunID: "r1", Automation: "triage", Status: StatusScheduled, At: base},
		{RunID: "r1", Automation: "triage", Status: StatusRunning, At: base.Add(time.Second)},
		{RunID: "r1", Automation: "triage", Status: StatusDone, At: base.Add(time.Minute)},
		{RunID: "r2", Automation: "other", Status: StatusFailed, At: base.Add(2 * time.Minute), Error: "boom"},
	}
	for _, r := range steps {
		if err := Append(r); err != nil {
			t.Fatal(err)
		}
	}

	runs, err := Runs("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	if runs[0].RunID != "r2" || runs[0].Status != StatusFailed {
		t.Fatalf("newest first expected, got %+v", runs[0])
	}
	if runs[1].Status != StatusDone {
		t.Fatalf("r1 should collapse to done, got %s", runs[1].Status)
	}

	last, err := LastRun("triage")
	if err != nil || last == nil || last.RunID != "r1" {
		t.Fatalf("LastRun(triage) = %+v, %v", last, err)
	}
}

func TestPruneDropsOnlyWhatIsPastTheWindow(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	now := time.Now()
	for _, r := range []Record{
		{RunID: "ancient", Automation: "a", Status: StatusDone, At: now.Add(-100 * 24 * time.Hour)},
		{RunID: "recent", Automation: "a", Status: StatusDone, At: now.Add(-2 * time.Hour)},
	} {
		if err := Append(r); err != nil {
			t.Fatal(err)
		}
	}

	if err := Prune(90 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	runs, err := Runs("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].RunID != "recent" {
		t.Fatalf("after prune got %+v, want only the recent run", runs)
	}

	// Nothing left to drop: the log must survive a second pass untouched.
	if err := Prune(90 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if runs, _ := Runs("", 0); len(runs) != 1 {
		t.Fatalf("second prune changed the log: %+v", runs)
	}
}

func TestOutputSurvivesItsRunAndIsPrunedWithIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)

	if HasOutput("never-ran") {
		t.Error("HasOutput true for a run that captured nothing")
	}

	if err := SaveOutput("r1", "the agent said this\n"); err != nil {
		t.Fatal(err)
	}
	if !HasOutput("r1") {
		t.Error("HasOutput false right after SaveOutput")
	}
	got, err := os.ReadFile(OutputPath("r1"))
	if err != nil || string(got) != "the agent said this\n" {
		t.Fatalf("captured output = %q, %v", got, err)
	}

	// Age the file past the window: pruning the log must collect it too, or
	// output outlives the records that point at it.
	old := time.Now().Add(-100 * 24 * time.Hour)
	if err := os.Chtimes(OutputPath("r1"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := Prune(90 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if HasOutput("r1") {
		t.Error("output survived a prune that should have collected it")
	}
}
