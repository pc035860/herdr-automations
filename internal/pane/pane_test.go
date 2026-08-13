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

	at := time.Date(2026, 8, 14, 7, 3, 0, 0, time.UTC)
	r := row{last: &history.Record{Status: history.StatusDone, At: at}}
	// "done" alone cannot distinguish this morning from last week.
	if got := statusText(r); got != "done 07:03" {
		t.Errorf("statusText = %q, want %q", got, "done 07:03")
	}
}
