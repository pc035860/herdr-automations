package pane

import (
	"testing"
	"time"

	"github.com/DnzzL/herdr-automations/internal/history"
)

func TestStatusTextSaysWhenNotJustWhat(t *testing.T) {
	if got := statusText(row{}); got != "never" {
		t.Errorf("statusText with no run = %q, want %q", got, "never")
	}

	// "done" alone cannot distinguish this morning from last week.
	at := time.Now()
	r := row{last: &history.Record{Status: history.StatusDone, At: at}}
	if got, want := statusText(r), "done "+at.Format("15:04"); got != want {
		t.Errorf("statusText = %q, want %q", got, want)
	}
}

func TestShortTimeShowsTheDateOnceItIsNotToday(t *testing.T) {
	now := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	if got := shortTime(now.Add(-2*time.Hour), now); got != "07:00" {
		t.Errorf("today = %q, want a clock reading", got)
	}
	// A clock reading for an older run reads as "this morning" and lies.
	if got := shortTime(now.AddDate(0, 0, -6), now); got != "08 Aug" {
		t.Errorf("last week = %q, want a date", got)
	}
	if got := shortTime(now.AddDate(-1, 0, 0), now); got != "14 Aug" {
		t.Errorf("last year = %q, want a date", got)
	}
}
