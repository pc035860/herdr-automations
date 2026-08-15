// Package history persists one JSONL record per run under the plugin state
// dir. Append-only: readers reconstruct the latest state per run ID.
package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/DnzzL/herdr-automations/internal/config"
)

type Status string

const (
	StatusScheduled Status = "scheduled"
	StatusRunning   Status = "running"
	StatusDone      Status = "done"
	StatusFailed    Status = "failed"
	StatusSkipped   Status = "skipped"
	// StatusMissed records an occurrence the scheduler could not run at all —
	// typically the machine was asleep past the catch-up window. Recording it
	// is the point: a silently absent run is worse than a visible failure.
	StatusMissed Status = "missed"
)

type Record struct {
	RunID      string    `json:"run_id"`
	Automation string    `json:"automation"`
	Status     Status    `json:"status"`
	At         time.Time `json:"at"`
	// Started is when the run began, as opposed to At, which moves with every
	// transition. Labels are stamped with it, so relabelling a run long after
	// it finished — dating a pane that outlived its day — reproduces the same
	// clock time instead of drifting to whenever the relabel happened.
	// omitzero, not omitempty: a time.Time is a struct, and omitempty would
	// write the year-1 sentinel into every record instead of leaving it out.
	Started     time.Time `json:"started,omitzero"`
	Trigger     string    `json:"trigger,omitempty"` // cron | manual
	WorkspaceID string    `json:"workspace_id,omitempty"`
	// Placement records whether this run owned its workspace or only a tab in
	// the shared one. Retirement has to know: inferring it from an empty TabID
	// would close the shared workspace — every other automation's tab with it —
	// for a run that failed between claiming the workspace and getting a tab.
	Placement string `json:"placement,omitempty"`
	TabID     string `json:"tab_id,omitempty"`
	PaneID    string `json:"pane_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

func path() string { return filepath.Join(config.StateDir(), "history.jsonl") }

// mu serialises appends against the rewrite Prune does, so a run recorded
// mid-prune is not lost to the rename. Only within this process: Prune runs in
// the daemon, and a record appended by the CLI or the board in the same
// millisecond can still be dropped. Readers need no lock — an open fd survives
// the rename.
var mu sync.Mutex

// outputDir holds one file per run: what the agent printed, captured before
// its pane can be retired. It stays out of history.jsonl deliberately — the
// log is scanned end to end on every board render, and a few hundred lines of
// terminal per run would make that expensive.
func outputDir() string { return filepath.Join(config.StateDir(), "output") }

// OutputPath is where a run's captured output lives, for handing to a pager.
func OutputPath(runID string) string {
	return filepath.Join(outputDir(), runID+".log")
}

// SaveOutput stores a run's terminal output.
func SaveOutput(runID, text string) error {
	if err := os.MkdirAll(outputDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(OutputPath(runID), []byte(text), 0o644)
}

// HasOutput reports whether a run left anything to read.
func HasOutput(runID string) bool {
	st, err := os.Stat(OutputPath(runID))
	return err == nil && st.Size() > 0
}

// Prune drops records older than maxAge. The window is deliberately generous:
// a line of JSON costs nothing next to a pane, and history is what answers
// "did the weekly one run at all last month" long after the panes are gone.
func Prune(maxAge time.Duration) error {
	mu.Lock()
	defer mu.Unlock()

	cutoff := time.Now().Add(-maxAge)

	f, err := os.Open(path())
	if os.IsNotExist(err) {
		// No log to rewrite, but captured output can outlive it — a log
		// trimmed by hand, or a state dir restored without one.
		return pruneOutput(cutoff)
	}
	if err != nil {
		return err
	}
	defer f.Close()
	var kept []byte
	dropped := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var r Record
		// An unreadable line has no date to judge; keeping it is the safer bet.
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.At.Before(cutoff) {
			dropped++
			continue
		}
		kept = append(append(kept, sc.Bytes()...), '\n')
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if dropped == 0 {
		return pruneOutput(cutoff)
	}

	tmp := path() + ".tmp"
	if err := os.WriteFile(tmp, kept, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path()); err != nil {
		return err
	}
	return pruneOutput(cutoff)
}

// pruneOutput drops captured output past the window. It goes by file age
// rather than by the log, so output whose record is already gone — from an
// older window, or a hand-edited log — is collected too.
func pruneOutput(cutoff time.Time) error {
	entries, err := os.ReadDir(outputDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// One undeletable entry must not abandon the sweep — it would stall every
	// future prune on the same file, silently.
	var failed []error
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(outputDir(), e.Name())); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// Append writes one record; failures are returned but callers generally just
// log them — history must never break a run.
func Append(r Record) error {
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(config.StateDir(), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, string(line))
	return err
}

// Runs returns the latest record per run, newest first, optionally filtered
// by automation name, capped at limit (0 = no cap).
func Runs(automation string, limit int) ([]Record, error) {
	f, err := os.Open(path())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	latest := map[string]Record{}
	order := []string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue // tolerate a torn write rather than losing the file
		}
		if automation != "" && r.Automation != automation {
			continue
		}
		if _, ok := latest[r.RunID]; !ok {
			order = append(order, r.RunID)
		}
		latest[r.RunID] = r
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	out := make([]Record, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		out = append(out, latest[order[i]])
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// Latest returns the most recent record for every automation in one pass. The
// board renders every two seconds; calling LastRun per automation instead
// rescans the whole log once per row, which at ten hourly automations is
// hundreds of thousands of decodes a second on the UI's own goroutine.
func Latest() (map[string]Record, error) {
	runs, err := Runs("", 0)
	if err != nil {
		return nil, err
	}
	latest := make(map[string]Record, len(runs))
	for _, r := range runs {
		// Runs is newest first, so the first sighting of a name wins.
		if _, seen := latest[r.Automation]; !seen {
			latest[r.Automation] = r
		}
	}
	return latest, nil
}

// LastRun returns the most recent record for an automation, or nil.
func LastRun(automation string) (*Record, error) {
	runs, err := Runs(automation, 1)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return &runs[0], nil
}
