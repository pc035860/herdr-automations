package pane

import (
	"strings"
	"testing"
	"time"

	"github.com/DnzzL/herdr-automations/internal/config"
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

func rowsNamed(names ...string) []row {
	rs := make([]row, 0, len(names))
	for _, n := range names {
		rs = append(rs, row{auto: config.Automation{Name: n}})
	}
	return rs
}

func TestNameColumnGrowsIntoASpaciousPane(t *testing.T) {
	long := "ubereats-receipt-auto-saturday" // 30 — truncated at the old fixed 24
	rows := rowsNamed(long, "short")

	// Wide enough for the whole name and then some: show all of it, and no
	// more — padding past the longest name is dead space in every row.
	if got := nameWidth(rows, 160); got != len(long) {
		t.Errorf("nameWidth in a 160-col pane = %d, want %d", got, len(long))
	}
	// The pane the board was built for. With the model column in the line
	// there is no spare room left at 80, so the name falls back to its
	// minimum — the width it always had — rather than eating into a column
	// that has a fixed rendering to fit.
	if got := nameWidth(rows, 80); got != nameMin {
		t.Errorf("nameWidth in an 80-col pane = %d, want %d", got, nameMin)
	}
	// Narrower than the board's own layout. Truncating the name is the
	// accepted cost; shrinking below the old width is not, or the column the
	// user reads first is the one that suffers.
	if got := nameWidth(rows, 40); got != nameMin {
		t.Errorf("nameWidth in a 40-col pane = %d, want %d", got, nameMin)
	}
	// Names shorter than the minimum never shrink the column either.
	if got := nameWidth(rowsNamed("a"), 200); got != nameMin {
		t.Errorf("nameWidth with only short names = %d, want %d", got, nameMin)
	}
	// A pane with nothing in it still has to produce a usable width.
	if got := nameWidth(nil, 100); got != nameMin {
		t.Errorf("nameWidth with no rows = %d, want %d", got, nameMin)
	}
}

func TestNameColumnLeavesTheOtherColumnsRoom(t *testing.T) {
	// The grown column must not push the schedule field off the edge: at any
	// viewport, the full line has to fit in it once it is wide enough to.
	rows := rowsNamed("an-automation-with-a-genuinely-long-name-here")
	for view := 60; view <= 200; view++ {
		line := 1 + nameWidth(rows, view) + 1 + cronCol + 1 + statusCol + 1 + modelCol + 1 + scheduleCol
		if view >= 1+nameMin+1+cronCol+1+statusCol+1+modelCol+1+scheduleCol && line > view {
			t.Fatalf("at width %d the row needs %d columns", view, line)
		}
	}
}

func TestScheduleFieldSaysWhatTheDaemonWillActuallyDo(t *testing.T) {
	once := config.Automation{Name: "audit", Cron: "0 13 15 8 *", Once: true}

	// A one-time automation that has not run yet still has a next occurrence,
	// and the board says it is the only one coming.
	if got := scheduleText(row{auto: once}); !strings.HasSuffix(got, "· once") {
		t.Errorf("pending one-time run = %q, want it marked once", got)
	}
	// Once it has run, the daemon will never fire it again — so advertising a
	// next occurrence would be a lie the board tells every refresh.
	done := row{auto: once, last: &history.Record{Status: history.StatusDone, At: time.Now()}}
	if got := scheduleText(done); got != "(once · done)" {
		t.Errorf("finished one-time run = %q, want %q", got, "(once · done)")
	}
	// A failed attempt does not spend it, matching the daemon.
	failed := row{auto: once, last: &history.Record{Status: history.StatusFailed, At: time.Now()}}
	if got := scheduleText(failed); !strings.HasSuffix(got, "· once") {
		t.Errorf("failed one-time run = %q, want it still scheduled", got)
	}
	// Disabled outranks everything: it is why the automation does not run.
	off := config.Automation{Name: "x", Cron: "@daily", Once: true, Disabled: true}
	if got := scheduleText(row{auto: off}); got != "(disabled)" {
		t.Errorf("disabled one-time run = %q, want %q", got, "(disabled)")
	}
	// A plain automation is unchanged by any of this. @daily is always under a
	// day away, so it renders as relative time.
	plain := row{auto: config.Automation{Name: "y", Cron: "@daily"}}
	if got := scheduleText(plain); !strings.HasPrefix(got, "in ") || strings.Contains(got, "once") {
		t.Errorf("recurring automation = %q, want a bare relative next-run", got)
	}
}

func TestUntilTextPicksAReadableGranularity(t *testing.T) {
	at := time.Date(2026, time.August, 15, 7, 0, 0, 0, time.UTC)
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "in <1m"},
		{45 * time.Minute, "in 45m"},
		{18*time.Hour + 30*time.Minute, "in 18h 30m"},
		{3*24*time.Hour + 2*time.Hour, "in 3d 2h"},
		// A week or more out, the date says more than a day count would.
		{7 * 24 * time.Hour, "8/15"},
		{40 * 24 * time.Hour, "8/15"},
	}
	for _, c := range cases {
		if got := untilText(c.d, at); got != c.want {
			t.Errorf("untilText(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestModelFieldReadsBackTheFlagTheAgentIsGiven(t *testing.T) {
	model := func(args ...string) string {
		return modelText(config.Automation{Name: "x", AgentArgs: args})
	}

	// The form every agent kind that takes a model understands, separated or
	// joined, plus the short form codex and gemini also accept.
	for _, args := range [][]string{
		{"--model", "opus"},
		{"--model=opus"},
		{"-m", "opus"},
		{"-m=opus"},
		{"--dangerously-skip-permissions", "--model", "opus"},
	} {
		if got := model(args...); got != "opus" {
			t.Errorf("modelText(%q) = %q, want %q", args, got, "opus")
		}
	}

	// claude's --fallback-model names a different model for a different
	// purpose. Matching on anything less than the whole token would report it
	// as the model this automation runs on.
	if got := model("--fallback-model", "sonnet"); got != "-" {
		t.Errorf("modelText with only a fallback model = %q, want %q", got, "-")
	}
	// An agent left on its default has no flag to read, and neither does a
	// --model someone left dangling with no value after it.
	if got := model(); got != "-" {
		t.Errorf("modelText with no args = %q, want %q", got, "-")
	}
	if got := model("--model"); got != "-" {
		t.Errorf("modelText with a valueless --model = %q, want %q", got, "-")
	}
}
