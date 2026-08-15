// Package daemon is the long-running scheduler started by the plugin's startup
// hook. It re-reads automations.yaml as it changes, fires occurrences off the
// wall clock (so a sleeping laptop delays runs instead of losing them), and
// re-executes itself when the plugin binary is upgraded underneath it.
package daemon

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/DnzzL/herdr-automations/internal/config"
	"github.com/DnzzL/herdr-automations/internal/history"
	"github.com/DnzzL/herdr-automations/internal/runner"
)

// tickInterval is how often the wall clock is consulted. Short enough that a
// run resumes within a minute of the machine waking up.
const tickInterval = 30 * time.Second

const (
	// historyMaxAge is how far back the run log reaches. Panes are budgeted
	// tightly; these lines are not, because they are all that is left once a
	// run's pane is gone.
	historyMaxAge = 90 * 24 * time.Hour
	// pruneInterval keeps the rewrite off the hot path — the log grows by a
	// handful of lines per run.
	pruneInterval = 24 * time.Hour
)

func Run() error {
	log.SetPrefix("[herdr-automations] ")

	release, err := acquireLock()
	if err != nil {
		return err
	}
	defer release()

	log.Printf("daemon starting, config=%s", config.Path())
	state := loadState()
	binary := binaryStamp()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	prune := time.NewTicker(pruneInterval)
	defer prune.Stop()

	evaluate(state) // don't wait a full tick to notice what is already due
	// Before pruning, not after: a daemon that was down longer than the history
	// window would otherwise drop the records of panes that are still on screen,
	// and a pane whose record is gone can no longer be relabelled.
	// Before the prune, whose window a long-stopped daemon would otherwise take
	// the records of still-open panes with. RestampOrphans reads history here
	// and relabels in the background, so the ordering holds without a wedged
	// herdr call keeping the daemon out of its loop.
	day := startOfDay(time.Now())
	runner.RestampOrphans(time.Now())
	pruneHistory()

	for {
		select {
		case <-prune.C:
			pruneHistory()
		case <-tick.C:
			if stamp := binaryStamp(); stamp != binary && stamp != "" {
				restart(release)
			}
			// Yesterday's kept panes are labelled with a bare clock time, which
			// stops being unambiguous the moment today's run puts an identically
			// labelled tab beside it. A ticker cannot be used for this — it would
			// drift off midnight — so the rollover is watched for directly, and
			// only forwards: a clock corrected backwards past midnight would
			// otherwise date panes that are once again from today, and nothing
			// takes a date back off.
			//
			// The day is remembered even when it went backwards, so a clock
			// corrected back and then forward again sweeps on its return —
			// runs started meanwhile need dating too, and sweeping a day twice
			// costs a few renames that write what is already there.
			// The new day is only taken as swept once a sweep actually starts.
			// One that spans midnight turns the next rollover away, and giving
			// the day up regardless would cost that day its sweep entirely.
			if d, rolled := dayRolledOver(day, time.Now()); !rolled {
				day = d
			} else if runner.RestampStale(time.Now()) {
				day = d
			}
			evaluate(state)
		case s := <-sigs:
			log.Printf("received %v, shutting down", s)
			return nil
		}
	}
}

// startOfDay is the calendar day a moment falls in, kept as a time so two of
// them can be compared for direction and not merely for difference.
func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// dayRolledOver reports whether now falls on a later calendar day than the one
// last swept, along with the day to remember. Direction matters: dating a label
// is one-way, so a clock corrected backwards past midnight must not stamp a
// date onto panes that turn out to be from today after all.
func dayRolledOver(swept, now time.Time) (time.Time, bool) {
	d := startOfDay(now)
	return d, d.After(swept)
}

// evaluate fires every automation whose occurrence has come due, and records
// the ones that came due too long ago to still be worth running.
func evaluate(state *scheduleState) {
	cfg, err := config.Load()
	if err != nil {
		log.Printf("config error, leaving the schedule untouched: %v", err)
		return
	}

	runner.SetLimit(cfg.Concurrency())

	now := time.Now()
	live := map[string]bool{}
	dirty := false

	for _, a := range cfg.Automations {
		live[a.Name] = true
		if a.Disabled {
			continue
		}
		if a.Once {
			if _, spent := state.Completed[a.Name]; spent {
				continue
			}
			// The run happens on another goroutine, so its outcome is read
			// back here on a later tick rather than written from there: every
			// write to the schedule state stays on this one.
			runs, err := history.Runs(a.Name, onceScanDepth)
			if err != nil {
				// Fail closed. Repeating a one-time run repeats whatever it
				// did; waiting for a readable log costs only the delay.
				log.Printf("%s: cannot tell whether its one-time run happened: %v", a.Name, err)
				continue
			}
			if usedUp(runs) {
				state.Completed[a.Name] = now
				dirty = true
				log.Printf("%s: one-time run finished, retiring it from the schedule", a.Name)
				continue
			}
		}
		sched, err := config.CronParser.Parse(a.Cron)
		if err != nil {
			log.Printf("%s: %v", a.Name, err) // Load validated it; be defensive
			continue
		}

		last, seen := state.LastOccurrence[a.Name]
		if !seen {
			// A new automation starts counting from now: adding one should
			// never retroactively fire this morning's occurrence.
			state.LastOccurrence[a.Name] = now
			dirty = true
			log.Printf("%s: scheduled, next run %s",
				a.Name, sched.Next(now).Format(time.RFC1123))
			continue
		}

		occ, skipped, ok := due(sched, last, now)
		if !ok {
			continue
		}
		state.LastOccurrence[a.Name] = occ
		dirty = true

		if skipped > 0 {
			recordMissed(a.Name, skipped, "machine unavailable")
			log.Printf("%s: %d earlier occurrence(s) missed", a.Name, skipped)
		}

		lateness := now.Sub(occ)
		if lateness > a.CatchUp() {
			window := fmt.Sprintf("past the %s catch-up window", a.CatchUp())
			if a.CatchUp() == 0 {
				window = "catch-up disabled"
			}
			recordMissed(a.Name, 1, fmt.Sprintf("due %s ago, %s",
				lateness.Round(time.Minute), window))
			log.Printf("%s: missed (%s late)", a.Name, lateness.Round(time.Minute))
			continue
		}

		trigger := "cron"
		if lateness > time.Minute {
			trigger = "catchup"
			log.Printf("%s: running %s late", a.Name, lateness.Round(time.Minute))
		}
		go func(a config.Automation, trigger string, occ time.Time) {
			if err := runner.RunDue(a, trigger, occ); err != nil {
				log.Printf("run %s: %v", a.Name, err)
			}
		}(a, trigger, occ)
	}

	// Forget automations that are gone, so re-adding one later starts clean.
	for name := range state.LastOccurrence {
		if !live[name] {
			delete(state.LastOccurrence, name)
			dirty = true
		}
	}
	for name := range state.Completed {
		if !live[name] {
			delete(state.Completed, name)
			dirty = true
		}
	}
	if dirty {
		if err := state.save(); err != nil {
			log.Printf("saving schedule state: %v", err)
		}
	}
}

// onceScanDepth bounds how far back the run log is read when deciding whether
// a one-time automation is spent. It only has to span the attempts made since
// the entry was added, and the answer is remembered once found.
const onceScanDepth = 20

// usedUp reports whether a one-time automation has had the run it was added
// for. A failed or missed attempt does not count: `once` says the work has to
// happen, not that the moment has to be spent, so the next occurrence should
// still get its turn.
func usedUp(runs []history.Record) bool {
	for _, r := range runs {
		if r.Status == history.StatusDone {
			return true
		}
	}
	return false
}

func pruneHistory() {
	if err := history.Prune(historyMaxAge); err != nil {
		log.Printf("pruning history: %v", err)
	}
}

func recordMissed(name string, count int, why string) {
	detail := why
	if count > 1 {
		detail = fmt.Sprintf("%d occurrences: %s", count, why)
	}
	err := history.Append(history.Record{
		RunID:      fmt.Sprintf("%s-missed-%d", name, time.Now().UnixNano()),
		Automation: name, Trigger: "cron", Status: history.StatusMissed,
		At: time.Now(), Error: detail,
	})
	if err != nil {
		log.Printf("history append failed: %v", err)
	}
}

// restart re-executes the daemon so a plugin upgrade takes effect without
// waiting for the Herdr server to be restarted.
func restart(release func()) {
	if runner.Busy() {
		return // let the in-flight run finish; we'll notice again next tick
	}
	exe, err := os.Executable()
	if err != nil {
		log.Printf("cannot locate the new binary: %v", err)
		return
	}
	log.Printf("binary changed, re-executing %s", exe)
	release() // the new process takes the lock
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		log.Printf("re-exec failed, continuing with the old build: %v", err)
	}
}

func binaryStamp() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	st, err := os.Stat(exe)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", st.ModTime().UnixNano(), st.Size())
}
