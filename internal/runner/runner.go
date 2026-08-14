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

// How a run was triggered, as recorded in the history log.
const (
	TriggerCron    = "cron"
	TriggerCatchUp = "catchup"
	TriggerManual  = "manual"
)

// Run executes the automation synchronously, as though it were due now.
func Run(a config.Automation, trigger string) error {
	return RunDue(a, trigger, time.Now())
}

// RunDue executes the automation for the occurrence due at occ. The scheduler
// passes the occurrence rather than "now" so lateness is measured from when
// the run was meant to happen, across both the wait for the machine to wake
// and the wait for a concurrency slot.
func RunDue(a config.Automation, trigger string, occ time.Time) error {
	j := newJournal(a, trigger)
	if _, busy := inFlight.LoadOrStore(a.Name, true); busy {
		j.record(history.StatusSkipped, "previous run still in flight")
		return fmt.Errorf("%s: previous run still in flight, skipped", a.Name)
	}
	defer inFlight.Delete(a.Name)

	// Recorded before queueing. A run waiting on the gate is already committed
	// — Busy() counts it, and without this the board would show it as idle.
	j.record(history.StatusScheduled, "")
	gate.acquire()
	defer gate.release()

	// The queue can be long: max_concurrent 2 against ten automations sharing
	// an occurrence, each bounded only by timeout_minutes. The scheduler
	// checked the window before queueing; re-check it against the occurrence
	// now that the wait is known, rather than starting a run hours past its
	// point.
	// Manual runs are exempt: "run it now" carries no occurrence to be late
	// for, and with catch_up_minutes: -1 the window is zero, so every one of
	// them would be recorded missed instead of running.
	if late := time.Since(occ); trigger != TriggerManual && late > a.CatchUp() {
		j.record(history.StatusMissed, fmt.Sprintf("%s late by the time a slot was free, "+
			"past the %s catch-up window", late.Round(time.Minute), a.CatchUp()))
		return fmt.Errorf("%s: queued past its catch-up window", a.Name)
	}

	// Retiring the previous panes only once this run is certain to happen: a
	// run that turns out to be missed must not have taken away the pane its
	// predecessor was still showing.
	retire(a)

	if err := j.provision(a); err != nil {
		j.record(history.StatusFailed, err.Error())
		return err
	}
	j.record(history.StatusRunning, "")

	if err := j.execute(a); err != nil {
		saved := j.capture()
		j.record(history.StatusFailed, err.Error())
		j.relabel(history.StatusFailed)
		j.retireNow(a.KeepFailedCount(), saved)
		return err
	}
	saved := j.capture()
	j.record(history.StatusDone, "")
	j.relabel(history.StatusDone)
	j.retireNow(a.KeepCount(), saved)
	return nil
}

// retireNow closes this run's own pane when its budget is zero — nothing here
// is meant to be read, so don't make the user wait for the next occurrence.
// A pane whose output could not be saved is kept regardless: it is the only
// remaining copy, and the next run will try again.
func (j *journal) retireNow(keep int, saved bool) {
	if keep != 0 {
		return
	}
	if !saved {
		log.Printf("%s: keeping the pane, its output was not saved", j.rec.Automation)
		return
	}
	if err := release(j.rec); err != nil {
		log.Printf("%s: closing the finished run: %v", j.rec.Automation, err)
	}
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
	// A negative budget means never retire, for failures as much as for
	// successes. Passing it through as a count would retire every failure
	// instead — the exact opposite of what the setting asks for — while
	// keep: 0 still has to retire everything, so the two cases cannot share
	// the same negative number.
	keepAll := map[history.Status]bool{
		history.StatusDone:   keep < 0,
		history.StatusFailed: keepFailed < 0,
	}
	budget := map[history.Status]int{
		history.StatusDone:   max(0, keep-1),
		history.StatusFailed: max(0, keepFailed),
	}
	var out []history.Record
	for _, r := range runs {
		left, tracked := budget[r.Status]
		if !tracked || r.PaneID == "" {
			continue // still running, or never got as far as a pane
		}
		if keepAll[r.Status] {
			continue
		}
		if left > 0 {
			budget[r.Status] = left - 1
			continue
		}
		out = append(out, r)
	}
	return out
}

// paneState is everything observed about a run's pane before deciding to close
// it. Gathering it into one value keeps the decision itself pure, which is what
// makes the decision testable — and closing the wrong pane is the worst thing
// this program can do.
type paneState struct {
	onScreen bool   // the user is looking at this exact pane right now
	agent    string // herdr's agent status; "" when the pane holds no agent
	busy     bool   // a foreground command is running
}

// closeTarget names what release would close, or reports that it must not.
type closeTarget struct {
	tabID       string
	workspaceID string
}

// closable decides whether a finished run's pane can be taken away, and what
// to close. Every uncertainty answers no: an agent herdr cannot classify, a
// pane still running something, a record that never got the tab it claimed.
// Keeping one pane too long is a nuisance; closing a live session is not.
func closable(r history.Record, s paneState) (closeTarget, bool) {
	if r.WorkspaceID == "" || s.onScreen || s.busy {
		return closeTarget{}, false
	}
	// Only a pane whose agent has demonstrably stopped, or which never had one,
	// may go. "working" is obvious; "blocked" is an agent waiting on a question
	// the user can still answer; "unknown" is herdr saying it cannot tell.
	switch s.agent {
	case "", herdr.StatusIdle, herdr.StatusDone:
	default:
		return closeTarget{}, false
	}

	switch {
	case r.TabID != "":
		return closeTarget{tabID: r.TabID}, true
	case r.Placement == string(config.PlacementShared):
		return closeTarget{}, false // claimed the shared workspace, never got its tab
	case r.Placement == string(config.PlacementWorkspace):
		return closeTarget{workspaceID: r.WorkspaceID}, true
	default:
		// Written before placement was recorded. A workspace id on its own
		// cannot be told apart from a shared run that died between claiming
		// the workspace and opening its tab — and closing that workspace takes
		// every other automation's tab with it. Leave it; it ages out.
		return closeTarget{}, false
	}
}

// release closes whatever a finished run is holding — its tab under shared
// placement, its whole workspace otherwise — once nothing says it is still in
// use. It captures the run's output first if that never happened, since
// closing the pane destroys the only copy.
func release(r history.Record) error {
	if r.WorkspaceID == "" {
		return nil
	}
	// Capture first: reading the terminal and writing it out takes long enough
	// for the user to open the tab or the agent to pick up work, so the state
	// the decision rests on is observed after it, not before.
	if r.PaneID != "" && !history.HasOutput(r.RunID) {
		text, err := herdr.PaneTail(r.PaneID, capturedLines)
		if err != nil {
			return err
		}
		if err := history.SaveOutput(r.RunID, text); err != nil {
			return err
		}
	}
	s, err := observe(r)
	if errors.Is(err, herdr.ErrGone) {
		return nil // closed by hand already
	}
	if err != nil {
		return err
	}
	target, ok := closable(r, s)
	if !ok {
		return nil // next run gets another chance
	}
	if target.tabID != "" {
		return herdr.TabClose(target.tabID)
	}
	return herdr.WorkspaceClose(target.workspaceID)
}

// observe gathers a pane's state. Focus is read last, so the window between
// looking and closing is as short as the CLI allows.
func observe(r history.Record) (paneState, error) {
	var s paneState
	if r.PaneID != "" {
		agent, err := herdr.AgentStatus(r.PaneID)
		if err != nil {
			return s, err
		}
		s.agent = agent

		// An agentless pane can still be running something — a delegated
		// workflow, or whatever the user typed into a pane they reopened.
		// Anything but a confirmed idle prompt counts as in use.
		if agent == "" {
			activity, err := herdr.PaneActivity(r.PaneID)
			if err != nil {
				return s, err
			}
			s.busy = activity != herdr.ActivityIdle
		}
	}
	focused, activeTab, err := herdr.WorkspaceView(r.WorkspaceID)
	if err != nil {
		return s, err
	}
	s.onScreen = focused && (r.TabID == "" || activeTab == r.TabID)
	return s, nil
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
		// Delegation: herdr-workflows owns multi-step execution. `pane run`
		// returns once the command is typed, so without waiting for the shell
		// to come back the run would be called done while hwf is still going —
		// and with keep: 0 its pane would be closed underneath it.
		if err := herdr.PaneRun(paneID, "hwf", "run", a.Workflow); err != nil {
			return err
		}
		return waitForShell(paneID, timeout)
	}

	args := a.AgentArgs
	if a.MCPConfig != "" {
		args = append([]string{"--mcp-config", a.MCPConfig}, args...)
	}
	// Exported into the shell rather than passed to `agent start`, which takes
	// no environment: the agent inherits it from the pane it launches in.
	for _, line := range a.EnvExports() {
		if err := herdr.PaneRun(paneID, line); err != nil {
			return fmt.Errorf("set environment: %w", err)
		}
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
	if err := submit(paneID, a.Prompt, timeout); err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	// Settling covers idle, done *and* blocked, and a blocked agent is waiting
	// on a question nobody is there to answer. Calling that done would retire
	// its pane — with keep: 0, immediately — and throw the question away.
	//
	// Unlike the close decision, an unrecognised status is accepted here:
	// herdr reports "unknown" for agents it cannot classify, and refusing
	// those would fail every run of an agent kind it does not detect. Saying
	// "done" when it isn't costs a wrong status; retention stays cautious on
	// its own account.
	status, err := herdr.AgentStatus(paneID)
	switch {
	case err != nil:
		log.Printf("%s: cannot confirm the agent settled: %v", a.Name, err)
	case status == herdr.StatusBlocked:
		return fmt.Errorf("agent is blocked waiting for input")
	case status == herdr.StatusWorking:
		return fmt.Errorf("agent was still working when the prompt returned")
	}
	return nil
}

// shellPoll is how often a delegated workflow's pane is checked for the shell
// prompt coming back.
const shellPoll = 2 * time.Second

// promptAcceptWait is how long the agent has to visibly start working after a
// recovered submission. Only reached when the first attempt already stalled,
// so an agent that answers faster than this cannot be mistaken for a silent
// one — it would not have stalled in the first place.
const promptAcceptWait = 15 * time.Second

// submit gets the prompt into the agent and proves the agent took it.
//
// An agent that never received a prompt sits at idle, which is also what a
// finished one looks like, so settling proves nothing on its own. A run was
// recorded done having asked the agent nothing at all: herdr reports the agent
// idle while its TUI is still mounting, and typing into that window loses the
// text outright, not just the Enter that follows it.
func submit(paneID, prompt string, timeout time.Duration) error {
	return submitWith(promptOps{
		prompt:  func() error { return herdr.AgentPrompt(paneID, prompt, timeout) },
		enter:   func() error { return herdr.AgentSubmit(paneID) },
		working: func() error { return herdr.AgentWorking(paneID, promptAcceptWait) },
		settle:  func() error { return herdr.AgentWait(paneID, timeout) },
	}, time.Now, promptRetryWindow)
}

// promptOps are the herdr calls submit drives, named for what they mean to the
// submission rather than the subcommand behind them. Injected so the retry
// loop can be exercised without a live agent.
type promptOps struct {
	prompt  func() error // type the prompt, then wait for the agent to settle
	enter   func() error // press Enter on text already in the input
	working func() error // wait for visible evidence the agent took it
	settle  func() error // wait out the work the agent accepted
}

// promptRetryWindow bounds how long submit keeps re-offering a prompt the
// agent will not take. One retry is not enough: herdr calls an agent idle once
// its process is up, but a Claude Code still connecting MCP servers has no
// input to type into yet, and that gap is as long as the servers take to
// answer. A run on 2026-08-14 spent 35 seconds there — every MCP server timing
// out at once — and giving up inside that window recorded a failure for a run
// that went on to do the work.
const promptRetryWindow = 2 * time.Minute

func submitWith(ops promptOps, now func() time.Time, window time.Duration) error {
	deadline := now().Add(window)
	for {
		err := ops.prompt()
		if !herdr.PromptStalled(err) {
			return err // accepted, or failed for a reason worth reporting
		}

		// The text may be sitting in the input with only its Enter swallowed,
		// so press Enter before considering typing it again.
		if err := ops.enter(); err != nil {
			return err
		}
		if ops.working() == nil {
			return ops.settle()
		}

		// Nothing started, so the text never landed either and the input is
		// empty: safe to type it again, and necessary — otherwise this run
		// reports success having done nothing.
		if !now().Before(deadline) {
			return fmt.Errorf("the agent never accepted it: %w", err)
		}
	}
}

// startGrace is how long the shell has to pick up a submitted command before
// an idle pane is believed.
const startGrace = 15 * time.Second

// waitForShell blocks until a pane's foreground command exits. It waits to see
// the command running first: `pane run` only submits the line, so polling
// immediately can catch the shell before it has started and call a workflow
// that has not begun finished.
func waitForShell(paneID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	started := time.Now()
	seenRunning := false
	for {
		activity, err := herdr.PaneActivity(paneID)
		if err != nil {
			return err
		}
		switch {
		case activity == herdr.ActivityRunning:
			seenRunning = true
		case activity == herdr.ActivityIdle && (seenRunning || time.Since(started) > startGrace):
			// Either it ran and finished, or it never started within the
			// grace — a command that fast leaves nothing to wait for. An
			// unobservable pane is neither, so it keeps polling.
			return nil
		}
		if time.Now().Add(shellPoll).After(deadline) {
			return fmt.Errorf("still running after %s", timeout)
		}
		time.Sleep(shellPoll)
	}
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
// It reports whether the output is safely on disk, because retiring a pane
// whose tail was never saved destroys the only copy.
func (j *journal) capture() bool {
	if j.rec.PaneID == "" {
		return false
	}
	text, err := herdr.PaneTail(j.rec.PaneID, capturedLines)
	if err != nil {
		log.Printf("%s: reading the run's output: %v", j.rec.Automation, err)
		return false
	}
	if err := history.SaveOutput(j.rec.RunID, text); err != nil {
		log.Printf("%s: saving the run's output: %v", j.rec.Automation, err)
		return false
	}
	return true
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
