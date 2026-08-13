// Package runner executes one automation end to end: provision a workspace,
// start the agent, submit the prompt (or delegate to herdr-workflows), and
// record every state transition in the history log.
package runner

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DnzzL/herdr-automations/internal/config"
	"github.com/DnzzL/herdr-automations/internal/herdr"
	"github.com/DnzzL/herdr-automations/internal/history"
)

// inFlight guards against overlapping runs of the same automation: if the
// 9:00 run is still working at 10:00, the 10:00 tick is skipped, not queued.
var inFlight sync.Map

// gate bounds how many automations run at once. Its limit is set from the
// config on every scheduler tick, so raising it releases waiters immediately.
//
// It is per-process, which means it bounds the daemon's scheduled runs and
// nothing else: `herdr-automations run` and the board's `r` key each execute
// in their own process, where the gate starts unlimited. Bounding those too
// would mean routing manual runs through the daemon.
var gate = &limiter{}

type limiter struct {
	mu            sync.Mutex
	wake          *sync.Cond
	active, limit int
}

// SetLimit changes how many runs may be in flight; 0 means unlimited.
func SetLimit(n int) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.limit == n {
		return // the scheduler calls this every tick; don't wake waiters to re-check
	}
	gate.limit = n
	if gate.wake != nil {
		gate.wake.Broadcast()
	}
}

// acquire blocks until a slot is free. Waiting rather than skipping is the
// point: a run delayed by a busy minute still happens, and the catch-up
// window is what decides when it is too late to bother.
func (l *limiter) acquire() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.wake == nil {
		l.wake = sync.NewCond(&l.mu)
	}
	for l.limit > 0 && l.active >= l.limit {
		l.wake.Wait()
	}
	l.active++
}

func (l *limiter) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	if l.wake != nil {
		l.wake.Broadcast()
	}
}

// Busy reports whether any automation is mid-run.
func Busy() bool {
	busy := false
	inFlight.Range(func(_, _ any) bool { busy = true; return false })
	return busy
}

// Run executes the automation synchronously. trigger is "cron", "catchup" or
// "manual".
func Run(a config.Automation, trigger string) error {
	j := newJournal(a, trigger)
	if _, busy := inFlight.LoadOrStore(a.Name, true); busy {
		j.record(history.StatusSkipped, "previous run still in flight")
		return fmt.Errorf("%s: previous run still in flight, skipped", a.Name)
	}
	defer inFlight.Delete(a.Name)

	// Retirement is bookkeeping about previous runs, so it happens before the
	// gate: holding one of only max_concurrent slots to close old panes would
	// delay every run queued behind this one.
	retire(a)

	// Recorded before queueing. A run waiting on the gate is already committed
	// — Busy() counts it, and without this the board would show it as idle.
	j.record(history.StatusScheduled, "")
	gate.acquire()
	defer gate.release()

	if err := j.provision(a); err != nil {
		j.record(history.StatusFailed, err.Error())
		return err
	}
	j.record(history.StatusRunning, "")

	if err := j.execute(a); err != nil {
		j.capture()
		j.record(history.StatusFailed, err.Error())
		j.relabel(history.StatusFailed)
		return err
	}
	j.capture()
	j.record(history.StatusDone, "")
	j.relabel(history.StatusDone)
	if a.KeepCount() == 0 {
		// Nothing here is meant to be read, so don't make the user wait until
		// the next occurrence for the pane to go.
		if err := release(j.rec); err != nil {
			log.Printf("%s: closing the finished run: %v", a.Name, err)
		}
	}
	return nil
}

// runLabel names a run's pane. The clock time is the load-bearing part: tabs
// cannot be ordered through the CLI, and keep_failed leaves several runs of one
// automation side by side, so the label is the only thing telling them apart.
func runLabel(name string, start time.Time, st history.Status) string {
	glyph := "▶"
	switch st {
	case history.StatusDone:
		glyph = "✓"
	case history.StatusFailed:
		glyph = "✗"
	}
	return fmt.Sprintf("%s %s %s", glyph, name, start.Format("15:04"))
}

// provision opens the pane this run will work in, recording what it claimed as
// it goes so a failure part-way through still says what has to be cleaned up.
func (j *journal) provision(a config.Automation) error {
	label := j.label(history.StatusRunning)
	if a.Placement == config.PlacementShared {
		ws, err := sharedWorkspace()
		j.rec.WorkspaceID = ws
		if err != nil {
			return err
		}
		j.rec.TabID, j.rec.PaneID, err = herdr.TabCreate(ws, a.Repo, label)
		return err
	}
	var err error
	switch a.Workspace {
	case config.WorkspaceWorktree:
		branch := fmt.Sprintf("auto/%s-%s", slug(a.Name), time.Now().Format("20060102-1504"))
		j.rec.WorkspaceID, j.rec.PaneID, err = herdr.WorktreeCreate(a.Repo, branch, label)
	case config.WorkspaceRoot:
		j.rec.WorkspaceID, j.rec.PaneID, err = herdr.WorkspaceCreate(a.Repo, label)
	}
	return err
}

// retireScanDepth bounds how far back retirement looks. Anything older than
// this has been retired already; history itself stays complete.
const retireScanDepth = 50

// retire closes the panes of this automation's earlier runs, keeping the most
// recent few.
//
// It runs when a new run starts, not when one ends, and that is the whole
// trick: a retained pane then lasts exactly one period. A weekly automation's
// output stays up for a week and an hourly one's for an hour, from a single
// `keep: 1` and with no TTL to tune. A global "keep the last 20 runs" cannot
// do this — the hourly automations would evict the weekly one within hours.
func retire(a config.Automation) {
	if !a.Retires() {
		return
	}
	runs, err := history.Runs(a.Name, retireScanDepth)
	if err != nil {
		log.Printf("%s: cannot read history to retire old runs: %v", a.Name, err)
		return
	}
	for _, r := range expired(runs, a.KeepCount(), a.KeepFailedCount()) {
		if err := release(r); err != nil {
			log.Printf("%s: retiring run %s: %v", a.Name, r.RunID, err)
		}
	}
}

// expired picks which of an automation's past runs, newest first, have to give
// their pane back. The run that is about to start is budgeted as a success;
// failures keep their full allowance, erring towards the runs worth reading.
func expired(runs []history.Record, keep, keepFailed int) []history.Record {
	budget := map[history.Status]int{
		history.StatusDone:   keep - 1,
		history.StatusFailed: keepFailed,
	}
	var out []history.Record
	for _, r := range runs {
		left, tracked := budget[r.Status]
		if !tracked || r.PaneID == "" {
			continue // still running, or never got as far as a pane
		}
		if left > 0 {
			budget[r.Status] = left - 1
			continue
		}
		out = append(out, r)
	}
	return out
}

// release closes whatever a finished run is holding — its tab under shared
// placement, its whole workspace otherwise — unless it is on screen.
func release(r history.Record) error {
	if r.WorkspaceID == "" {
		return nil
	}
	focused, activeTab, err := herdr.WorkspaceView(r.WorkspaceID)
	if errors.Is(err, herdr.ErrGone) {
		return nil // closed by hand already
	}
	if err != nil {
		return err
	}

	// Records written before placement was stored carry a tab only when they
	// were shared, so the id doubles as the discriminator for those.
	if r.Placement == string(config.PlacementShared) || r.TabID != "" {
		if r.TabID == "" {
			return nil // claimed the shared workspace but never got its tab
		}
		if focused && activeTab == r.TabID {
			return nil
		}
		return herdr.TabClose(r.TabID)
	}
	if focused {
		return nil // the user is in it; next run gets another chance
	}
	return herdr.WorkspaceClose(r.WorkspaceID)
}

// sharedMu serialises find-then-create. Two automations on the same
// occurrence would otherwise both find nothing, both create, and split their
// tabs across two workspaces that nothing ever merges back.
var sharedMu sync.Mutex

// sharedWorkspace finds the automations workspace, creating it on first use.
// Its own root tab is left alone: it is the anchor that keeps the workspace
// alive once every run's tab has been retired.
func sharedWorkspace() (string, error) {
	sharedMu.Lock()
	defer sharedMu.Unlock()

	id, err := herdr.WorkspaceFind(config.SharedWorkspaceLabel)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	id, _, err = herdr.WorkspaceCreate(home, config.SharedWorkspaceLabel)
	return id, err
}

// agentReadyWait bounds how long an agent may take to reach its first prompt.
// Generous: a cold Claude Code loading MCP servers is well past ten seconds.
const agentReadyWait = 2 * time.Minute

func (j *journal) execute(a config.Automation) error {
	timeout := time.Duration(a.TimeoutMinutes) * time.Minute
	paneID := j.rec.PaneID

	if a.Workflow != "" {
		// Delegation: herdr-workflows owns multi-step execution.
		return herdr.PaneRun(paneID, "hwf", "run", a.Workflow)
	}

	args := a.AgentArgs
	if a.MCPConfig != "" {
		args = append([]string{"--mcp-config", a.MCPConfig}, args...)
	}
	// Herdr requires agent names to be lowercase, 1-32 chars, [a-z0-9-_].
	if err := herdr.AgentStart(agentName(a.Name, j.token), a.Agent, paneID, args); err != nil {
		return fmt.Errorf("start %s agent: %w", a.Agent, err)
	}
	// `agent start` returns while the agent's TUI is still mounting. Prompting
	// into that window types the text but loses the Enter, so wait for the
	// agent to settle — and if the submission is still swallowed, press Enter
	// on the text already sitting in the input rather than typing it twice.
	if err := herdr.AgentWait(paneID, agentReadyWait); err != nil {
		return fmt.Errorf("wait for %s agent: %w", a.Agent, err)
	}
	err := herdr.AgentPrompt(paneID, a.Prompt, timeout)
	if herdr.PromptStalled(err) {
		err = herdr.AgentSubmit(paneID, timeout)
	}
	if err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	return nil
}

// slug makes a name safe for a git branch: spaces and the characters
// git check-ref-format rejects would otherwise fail worktree creation.
func slug(name string) string {
	var b strings.Builder
	lastDash := true // also trims leading dashes
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "automation"
	}
	return out
}

// agentName fits an automation name into Herdr's agent-name rules: lowercase,
// [a-z0-9-_], at most 32 characters. The run token is what keeps it unique —
// once a finished run's pane is retained, its agent still holds the name, and
// naming the next run after the automation alone collides with it.
func agentName(automation, token string) string {
	s := slug(automation)
	if room := 32 - len(token) - 1; len(s) > room {
		s = strings.Trim(s[:room], "-")
	}
	return s + "-" + token
}

// newRunID mints a run's id and the short token its agent is named after. The
// name is slugged because the id becomes a filename under the state dir, where
// an automation called "reports/daily" would otherwise write outside it.
func newRunID(name string) (id, token string) {
	stamp := time.Now().UnixNano()
	token = strconv.FormatInt(stamp, 36)
	if len(token) > 6 {
		token = token[len(token)-6:]
	}
	return fmt.Sprintf("%s-%d", slug(name), stamp), token
}

// journal accumulates a run's record as provisioning discovers it, so every
// transition is logged with everything known at that point — and so retirement
// can be handed the same record the log holds.
type journal struct {
	rec   history.Record
	token string
	start time.Time
}

func newJournal(a config.Automation, trigger string) *journal {
	id, token := newRunID(a.Name)
	return &journal{
		rec: history.Record{
			RunID:      id,
			Automation: a.Name,
			Trigger:    trigger,
			Placement:  string(a.Placement),
		},
		token: token,
		start: time.Now(),
	}
}

// label names this run's pane at a given point in its life.
func (j *journal) label(st history.Status) string {
	return runLabel(j.rec.Automation, j.start, st)
}

// capturedLines is how much of the agent's terminal is kept. Enough for a
// summary and a stack trace; not the whole session.
const capturedLines = 200

// capture saves what the agent printed. Retirement closes the pane and throws
// its terminal away, so without this a finished run leaves nothing but a
// status word — and the failures are exactly the ones worth reading.
func (j *journal) capture() {
	if j.rec.PaneID == "" {
		return
	}
	text, err := herdr.PaneTail(j.rec.PaneID, capturedLines)
	if err != nil {
		log.Printf("%s: reading the run's output: %v", j.rec.Automation, err)
		return
	}
	if err := history.SaveOutput(j.rec.RunID, text); err != nil {
		log.Printf("%s: saving the run's output: %v", j.rec.Automation, err)
	}
}

// relabel restamps the run's pane with how it ended, so a glance at the shared
// workspace tells you which runs are worth opening.
func (j *journal) relabel(st history.Status) {
	label := j.label(st)
	var err error
	switch {
	case j.rec.TabID != "":
		err = herdr.TabRename(j.rec.TabID, label)
	case j.rec.WorkspaceID != "":
		err = herdr.WorkspaceRename(j.rec.WorkspaceID, label)
	}
	if err != nil {
		log.Printf("%s: relabelling the finished run: %v", j.rec.Automation, err)
	}
}

func (j *journal) record(st history.Status, errMsg string) {
	j.rec.Status, j.rec.At, j.rec.Error = st, time.Now(), errMsg
	if err := history.Append(j.rec); err != nil {
		log.Printf("history append failed: %v", err)
	}
}
