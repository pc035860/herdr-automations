package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DnzzL/herdr-automations/internal/config"
	"github.com/DnzzL/herdr-automations/internal/history"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestDueNotYet(t *testing.T) {
	sched, _ := config.CronParser.Parse("0 9 * * 1") // Mondays at 09:00
	_, _, ok := due(sched, at(t, "2026-08-10 09:00"), at(t, "2026-08-10 15:00"))
	if ok {
		t.Fatal("nothing should be due before the next Monday")
	}
}

func TestDueAfterSleepReturnsTheOccurrence(t *testing.T) {
	sched, _ := config.CronParser.Parse("0 9 * * 1")
	// Laptop asleep from Sunday evening; wakes Monday afternoon.
	occ, skipped, ok := due(sched, at(t, "2026-08-09 20:00"), at(t, "2026-08-10 17:13"))
	if !ok {
		t.Fatal("Monday 09:00 should be due")
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	if want := at(t, "2026-08-10 09:00"); !occ.Equal(want) {
		t.Fatalf("occurrence = %s, want %s", occ, want)
	}
}

func TestDueCountsEveryMissedOccurrence(t *testing.T) {
	sched, _ := config.CronParser.Parse("0 9 * * *") // daily at 09:00
	// Away for three days: three occurrences passed, the latest is today's.
	occ, skipped, ok := due(sched, at(t, "2026-08-07 12:00"), at(t, "2026-08-10 10:00"))
	if !ok {
		t.Fatal("expected due")
	}
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2 (the 8th and the 9th)", skipped)
	}
	if want := at(t, "2026-08-10 09:00"); !occ.Equal(want) {
		t.Fatalf("occurrence = %s, want %s", occ, want)
	}
}

func TestCatchUpWindowDefaults(t *testing.T) {
	a := config.Automation{}
	if got := a.CatchUp(); got != 0 {
		t.Fatalf("a zero-value automation has no window until defaults apply, got %s", got)
	}
	a.CatchUpMinutes = -1
	if got := a.CatchUp(); got != 0 {
		t.Fatalf("negative means never catch up, got %s", got)
	}
	a.CatchUpMinutes = 120
	if got := a.CatchUp(); got != 2*time.Hour {
		t.Fatalf("CatchUp() = %s, want 2h", got)
	}
}

func TestOnceIsSpentOnlyByARunThatFinished(t *testing.T) {
	// A one-time automation exists to get work done. Attempts that failed or
	// never started leave it owed, so the next occurrence still gets a turn.
	unfinished := []history.Record{
		{Status: history.StatusFailed},
		{Status: history.StatusMissed},
		{Status: history.StatusSkipped},
		{Status: history.StatusScheduled},
		{Status: history.StatusRunning},
	}
	if usedUp(unfinished) {
		t.Error("retired a one-time automation that never completed a run")
	}
	if usedUp(nil) {
		t.Error("retired a one-time automation that never ran at all")
	}
	// Newest first, as history.Runs returns them: a failed retry after the
	// run that succeeded must not bring the automation back.
	retried := []history.Record{
		{Status: history.StatusFailed},
		{Status: history.StatusDone},
	}
	if !usedUp(retried) {
		t.Error("a finished one-time run did not retire the automation")
	}
}

func TestStateFilesWrittenBeforeOnceStillLoad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)
	old := `{"last_occurrence":{"nightly":"2026-08-14T07:00:00+08:00"}}`
	if err := os.WriteFile(filepath.Join(dir, "schedule.json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	s := loadState()
	if len(s.LastOccurrence) != 1 {
		t.Fatalf("lost the existing schedule: %+v", s.LastOccurrence)
	}
	// Writing to a nil map panics, and evaluate() writes here the first time
	// a one-time automation finishes.
	s.Completed["nightly"] = time.Now()
}

func TestDayRolloverOnlyFiresGoingForward(t *testing.T) {
	swept := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)

	// Later the same day: the labels written this morning still say today.
	if _, rolled := dayRolledOver(swept, swept.Add(23*time.Hour)); rolled {
		t.Error("rolled over within the same day, sweeping for nothing")
	}
	// Midnight: yesterday's kept panes are about to sit beside today's runs.
	next, rolled := dayRolledOver(swept, swept.AddDate(0, 0, 1))
	if !rolled || !next.Equal(swept.AddDate(0, 0, 1)) {
		t.Errorf("dayRolledOver at midnight = %v, %v, want the new day", next, rolled)
	}
	// Asleep for a week: one sweep on waking covers all of it.
	if _, rolled := dayRolledOver(swept, swept.AddDate(0, 0, 7)); !rolled {
		t.Error("a week's sleep did not roll over")
	}
	// A clock corrected backwards past midnight and then forward again, as the
	// daemon would see it: 8/14 → 8/13 → 8/14. Dating is one-way, so going
	// backwards must not sweep. Coming back must, though — runs started while
	// the clock was behind need dating too — which only works if the caller
	// remembers the day it went back to.
	back := swept.AddDate(0, 0, -1)
	day, rolled := dayRolledOver(swept, back)
	if rolled {
		t.Error("a backwards clock rolled over, dating panes that are from today")
	}
	if !day.Equal(back) {
		t.Errorf("dayRolledOver going backwards returned %v, want the day to follow "+
			"the clock back to %v, or its return is not seen as a rollover", day, back)
	}
	if _, rolled := dayRolledOver(day, swept); !rolled {
		t.Error("returning to the original day did not roll over")
	}
}
