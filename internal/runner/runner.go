// Package runner executes one automation end to end: provision a workspace,
// start the agent, submit the prompt (or delegate to herdr-workflows), and
// record every state transition in the history log.
package runner

import (
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

// Busy reports whether any automation is mid-run.
func Busy() bool {
	busy := false
	inFlight.Range(func(_, _ any) bool { busy = true; return false })
	return busy
}

// Run executes the automation synchronously. trigger is "cron", "catchup" or
// "manual".
func Run(a config.Automation, trigger string) error {
	j := &journal{id: runID(a.Name), name: a.Name, trigger: trigger}
	if _, busy := inFlight.LoadOrStore(a.Name, true); busy {
		j.record(history.StatusSkipped, "previous run still in flight")
		return fmt.Errorf("%s: previous run still in flight, skipped", a.Name)
	}
	defer inFlight.Delete(a.Name)

	j.record(history.StatusScheduled, "")

	workspaceID, tabID, paneID, err := provision(a)
	j.workspaceID, j.tabID, j.paneID = workspaceID, tabID, paneID
	if err != nil {
		j.record(history.StatusFailed, err.Error())
		return err
	}
	j.record(history.StatusRunning, "")

	if err := execute(a, j.id, paneID); err != nil {
		j.record(history.StatusFailed, err.Error())
		return err
	}
	j.record(history.StatusDone, "")
	return nil
}

func provision(a config.Automation) (workspaceID, tabID, paneID string, err error) {
	label := "auto: " + a.Name
	if a.Placement == config.PlacementShared {
		workspaceID, err = sharedWorkspace()
		if err != nil {
			return "", "", "", err
		}
		tabID, paneID, err = herdr.TabCreate(workspaceID, a.Repo, label)
		return workspaceID, tabID, paneID, err
	}
	switch a.Workspace {
	case config.WorkspaceWorktree:
		branch := fmt.Sprintf("auto/%s-%s", slug(a.Name), time.Now().Format("20060102-1504"))
		workspaceID, paneID, err = herdr.WorktreeCreate(a.Repo, branch, label)
	case config.WorkspaceRoot:
		workspaceID, paneID, err = herdr.WorkspaceCreate(a.Repo, label)
	}
	return workspaceID, "", paneID, err
}

// sharedWorkspace finds the automations workspace, creating it on first use.
// Its own root tab is left alone: it is the anchor that keeps the workspace
// alive once every run's tab has been retired.
func sharedWorkspace() (string, error) {
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

func execute(a config.Automation, runID, paneID string) error {
	timeout := time.Duration(a.TimeoutMinutes) * time.Minute

	if a.Workflow != "" {
		// Delegation: herdr-workflows owns multi-step execution.
		return herdr.PaneRun(paneID, "hwf", "run", a.Workflow)
	}

	args := a.AgentArgs
	if a.MCPConfig != "" {
		args = append([]string{"--mcp-config", a.MCPConfig}, args...)
	}
	// Herdr requires agent names to be lowercase, 1-32 chars, [a-z0-9-_].
	if err := herdr.AgentStart(agentName(a.Name, runID), a.Agent, paneID, args); err != nil {
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
func agentName(automation, runID string) string {
	token := runToken(runID)
	s := slug(automation)
	if room := 32 - len(token) - 1; len(s) > room {
		s = strings.Trim(s[:room], "-")
	}
	return s + "-" + token
}

// runToken compacts a run id's nanosecond stamp into a few base-36 characters.
func runToken(runID string) string {
	token := runID
	if i := strings.LastIndex(runID, "-"); i >= 0 {
		token = runID[i+1:]
	}
	if n, err := strconv.ParseInt(token, 10, 64); err == nil {
		token = strconv.FormatInt(n, 36)
	}
	if len(token) > 6 {
		token = token[len(token)-6:]
	}
	return token
}

func runID(name string) string {
	return fmt.Sprintf("%s-%d", name, time.Now().UnixNano())
}

// journal accumulates a run's identifiers as provisioning discovers them, so
// every transition is logged with everything known at that point.
type journal struct {
	id, name, trigger          string
	workspaceID, tabID, paneID string
}

func (j *journal) record(st history.Status, errMsg string) {
	err := history.Append(history.Record{
		RunID: j.id, Automation: j.name, Trigger: j.trigger, Status: st, At: time.Now(),
		WorkspaceID: j.workspaceID, TabID: j.tabID, PaneID: j.paneID, Error: errMsg,
	})
	if err != nil {
		log.Printf("history append failed: %v", err)
	}
}
