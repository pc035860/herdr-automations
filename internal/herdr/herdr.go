// Package herdr is a thin client over the herdr CLI (which itself fronts the
// socket API). Herdr injects HERDR_BIN_PATH for plugins; outside a plugin
// context we fall back to `herdr` on PATH.
package herdr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

func bin() string {
	if b := os.Getenv("HERDR_BIN_PATH"); b != "" {
		return b
	}
	return "herdr"
}

// The error codes this package reacts to rather than merely reports.
const (
	codePaneBusy      = "agent_pane_busy"
	codePromptStalled = "agent_prompt_stalled"
	codeWorkspaceGone = "workspace_not_found"
	codeTabGone       = "tab_not_found"
	codeAgentGone     = "agent_not_found"
	codePaneGone      = "pane_not_found"
)

// APIError is herdr's structured error, kept structured. Matching on the
// formatted message instead would misread any command whose output merely
// quotes a code — reading back an agent's terminal, for one.
type APIError struct {
	Command string
	Code    string
	Message string
}

func (e *APIError) Error() string {
	detail := e.Code
	switch {
	case e.Code == "":
		detail = e.Message
	case e.Message != "":
		detail = e.Code + ": " + e.Message
	}
	return e.Command + ": " + detail
}

// hasCode reports whether err is an API error carrying one of codes.
func hasCode(err error, codes ...string) bool {
	var api *APIError
	return errors.As(err, &api) && slices.Contains(codes, api.Code)
}

// output runs a herdr subcommand and returns its stdout. Failures come back as
// *APIError so callers can branch on the code.
func output(args ...string) ([]byte, error) {
	cmd := exec.Command(bin(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Only the subcommand names the failure. The rest of argv holds the
		// automation's prompt, which would otherwise reach the run log, the
		// board's status line and the daemon's stderr on every failure.
		return nil, apiError(subcommand(args), stdout.Bytes(), stderr.String())
	}
	return stdout.Bytes(), nil
}

// subcommand is the leading verb pair of an argv ("agent prompt"), which is
// all of it that is safe to quote back.
func subcommand(args []string) string {
	return strings.Join(args[:min(2, len(args))], " ")
}

// run executes a herdr subcommand and decodes the socket-API JSON envelope
// ({"id": ..., "result": {...}}) into out when out is non-nil.
func run(out any, args ...string) error {
	stdout, err := output(args...)
	if err != nil || out == nil {
		return err
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(stdout, &envelope); err != nil {
		return fmt.Errorf("herdr %v: unexpected output %q: %w", args, stdout, err)
	}
	raw := envelope.Result
	if raw == nil {
		raw = stdout // some commands print the result object bare
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("herdr %v: decode result: %w", args, err)
	}
	return nil
}

// tolerant runs a command whose target may already be gone. Retirement is
// best-effort bookkeeping, so a workspace you closed by hand is a success.
func tolerant(args ...string) error {
	if err := run(nil, args...); err != nil && !hasCode(err, codeWorkspaceGone, codeTabGone) {
		return err
	}
	return nil
}

// PaneTail reads back what the agent printed. Retiring a pane throws its
// terminal away, so this is what lets a run's output outlive it.
func PaneTail(paneID string, lines int) (string, error) {
	out, err := output("pane", "read", paneID,
		"--source", "recent-unwrapped",
		"--lines", strconv.Itoa(lines),
		"--format", "text")
	return string(out), err
}

// apiError turns herdr's JSON error envelope into a typed error. Without this
// the raw payload ends up in logs and, worse, in the board's status line.
func apiError(command string, stdout []byte, stderr string) error {
	e := &APIError{Command: command}
	// The envelope arrives on stderr for some subcommands and stdout for
	// others, so both are tried before falling back to the raw text.
	for _, stream := range [][]byte{stdout, []byte(stderr)} {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(stream, &envelope) == nil && envelope.Error.Code != "" {
			e.Code, e.Message = envelope.Error.Code, envelope.Error.Message
			return e
		}
	}
	if s := strings.TrimSpace(stderr); s != "" {
		e.Message = s
		return e
	}
	e.Message = strings.TrimSpace(string(stdout))
	return e
}

// createResult matches both worktree_created and workspace_created payloads:
// the new workspace plus its initial pane, ready for `agent start`.
type createResult struct {
	Workspace struct {
		WorkspaceID string `json:"workspace_id"`
	} `json:"workspace"`
	RootPane struct {
		PaneID string `json:"pane_id"`
		TabID  string `json:"tab_id"`
	} `json:"root_pane"`
}

func (r createResult) ids(what string) (string, string, error) {
	if r.Workspace.WorkspaceID == "" || r.RootPane.PaneID == "" {
		return "", "", fmt.Errorf("%s returned no workspace/pane id", what)
	}
	return r.Workspace.WorkspaceID, r.RootPane.PaneID, nil
}

// WorktreeCreate provisions a fresh git worktree workspace off repo and
// returns its workspace and root pane IDs.
func WorktreeCreate(repo, branch, label string) (workspaceID, paneID string, err error) {
	var res createResult
	err = run(&res, "worktree", "create",
		"--cwd", repo, "--branch", branch, "--label", label, "--no-focus")
	if err != nil {
		return "", "", err
	}
	return res.ids("worktree create")
}

// WorkspaceCreate opens a workspace directly on a directory (root mode).
func WorkspaceCreate(cwd, label string) (workspaceID, paneID string, err error) {
	var res createResult
	err = run(&res, "workspace", "create", "--cwd", cwd, "--label", label, "--no-focus")
	if err != nil {
		return "", "", err
	}
	return res.ids("workspace create")
}

// WorkspaceFind returns the id of the workspace carrying label, or "" when
// none does. Labels are how the shared workspace is rediscovered after a
// daemon restart — a stored id would go stale the moment you close it.
func WorkspaceFind(label string) (string, error) {
	var res struct {
		Workspaces []struct {
			WorkspaceID string `json:"workspace_id"`
			Label       string `json:"label"`
		} `json:"workspaces"`
	}
	if err := run(&res, "workspace", "list"); err != nil {
		return "", err
	}
	for _, w := range res.Workspaces {
		if w.Label == label {
			return w.WorkspaceID, nil
		}
	}
	return "", nil
}

// TabCreate opens a tab inside an existing workspace and returns its tab and
// root pane ids. This is what lets many automations share one workspace
// instead of each spawning its own.
func TabCreate(workspaceID, cwd, label string) (tabID, paneID string, err error) {
	var res createResult
	err = run(&res, "tab", "create",
		"--workspace", workspaceID, "--cwd", cwd, "--label", label, "--no-focus")
	if err != nil {
		return "", "", err
	}
	if res.RootPane.TabID == "" || res.RootPane.PaneID == "" {
		return "", "", fmt.Errorf("tab create returned no tab/pane id")
	}
	return res.RootPane.TabID, res.RootPane.PaneID, nil
}

// A pane created moments ago is still spawning its shell, and herdr refuses to
// start an agent in it until the prompt is up (agent_pane_busy). Profile load
// dominates that wait, so allow for a slow ~/.zshrc rather than one poll.
const (
	agentStartWait = 30 * time.Second
	agentStartPoll = 500 * time.Millisecond
)

// AgentStart launches an interactive agent in a pane sitting at a shell
// prompt. extraArgs are forwarded to the agent executable (e.g. --mcp-config).
// It retries while the pane is still coming up.
func AgentStart(name, kind, paneID string, extraArgs []string) error {
	args := []string{"agent", "start", name, "--kind", kind, "--pane", paneID}
	if len(extraArgs) > 0 {
		args = append(args, "--")
		args = append(args, extraArgs...)
	}
	deadline := time.Now().Add(agentStartWait)
	for {
		err := run(nil, args...)
		if err == nil || !hasCode(err, codePaneBusy) {
			return err
		}
		if !time.Now().Add(agentStartPoll).Before(deadline) {
			return err
		}
		time.Sleep(agentStartPoll)
	}
}

// AgentWait blocks until the agent settles (idle, done or blocked) — the
// signal that its TUI has finished mounting and will accept a prompt.
func AgentWait(target string, timeout time.Duration) error {
	return run(nil, "agent", "wait", target,
		"--timeout", strconv.FormatInt(timeout.Milliseconds(), 10))
}

// AgentPrompt submits a prompt and waits for the agent to settle (idle, done
// or blocked), bounded by timeout.
func AgentPrompt(target, text string, timeout time.Duration) error {
	return run(nil, "agent", "prompt", target, text,
		"--wait", "--timeout", strconv.FormatInt(timeout.Milliseconds(), 10))
}

// PromptStalled reports whether err is herdr refusing to call a submission
// observed: the text reached the agent's input but nothing happened.
func PromptStalled(err error) bool { return hasCode(err, codePromptStalled) }

// AgentSubmit presses Enter in the agent's input, then waits for it to settle.
// Recovery for a stalled prompt: the text is already typed, so re-prompting
// would type it a second time.
func AgentSubmit(target string, timeout time.Duration) error {
	if err := run(nil, "agent", "send-keys", target, "Enter"); err != nil {
		return err
	}
	return AgentWait(target, timeout)
}

// Agent states herdr reports. StatusBlocked is the one that matters most: an
// agent waiting on a question has stopped, but it is not finished.
const (
	StatusWorking = "working"
	StatusBlocked = "blocked"
)

// AgentStatus reports the state of the agent in a pane, or "" when the pane
// holds no agent — a plain shell, or one whose agent has exited.
func AgentStatus(paneID string) (string, error) {
	var res struct {
		Agent struct {
			Status string `json:"agent_status"`
		} `json:"agent"`
	}
	if err := run(&res, "agent", "get", paneID); err != nil {
		if hasCode(err, codeAgentGone, codePaneGone) {
			return "", nil
		}
		return "", err
	}
	return res.Agent.Status, nil
}

// PaneBusy reports whether a pane is still running a foreground command rather
// than sitting at its shell prompt. This is how a delegated workflow — which
// is a command, not an agent — is known to be finished.
func PaneBusy(paneID string) (bool, error) {
	var res struct {
		ProcessInfo struct {
			ForegroundGroup int `json:"foreground_process_group_id"`
			ShellPID        int `json:"shell_pid"`
		} `json:"process_info"`
	}
	if err := run(&res, "pane", "process-info", "--pane", paneID); err != nil {
		return false, err
	}
	info := res.ProcessInfo
	return info.ForegroundGroup != 0 && info.ForegroundGroup != info.ShellPID, nil
}

// ErrGone means the run's workspace no longer exists — the expected outcome
// once you've reviewed and closed it, not a failure worth a stack trace.
var ErrGone = errors.New("workspace already closed")

// WorkspaceView reports whether a workspace is the one on screen and which of
// its tabs is active there — together, whether the user is looking at a given
// tab right now. Retiring a pane out from under them is worse than keeping one
// pane too many.
func WorkspaceView(workspaceID string) (focused bool, activeTabID string, err error) {
	var res struct {
		Workspace struct {
			Focused     bool   `json:"focused"`
			ActiveTabID string `json:"active_tab_id"`
		} `json:"workspace"`
	}
	if err := run(&res, "workspace", "get", workspaceID); err != nil {
		if hasCode(err, codeWorkspaceGone) {
			return false, "", ErrGone
		}
		return false, "", err
	}
	return res.Workspace.Focused, res.Workspace.ActiveTabID, nil
}

// TabRename restamps a run's tab, which is how a finished run reports itself
// in the shared workspace.
func TabRename(tabID, label string) error { return tolerant("tab", "rename", tabID, label) }

// WorkspaceRename does the same for a run that has a workspace to itself.
func WorkspaceRename(workspaceID, label string) error {
	return tolerant("workspace", "rename", workspaceID, label)
}

// TabClose retires one run's tab.
func TabClose(tabID string) error { return tolerant("tab", "close", tabID) }

// WorkspaceClose retires a run that had a workspace to itself.
func WorkspaceClose(workspaceID string) error {
	return tolerant("workspace", "close", workspaceID)
}

// Focus brings a run's workspace to the front, then its agent pane when one
// is known — the "jump to what this automation did" move.
func Focus(workspaceID, paneID string) error {
	if workspaceID != "" {
		if err := run(nil, "workspace", "focus", workspaceID); err != nil {
			if hasCode(err, codeWorkspaceGone) {
				return ErrGone
			}
			return err
		}
	}
	if paneID != "" {
		// Best-effort: the pane may be gone while the workspace lives on.
		_ = run(nil, "agent", "focus", paneID)
	}
	return nil
}

// PaneRun executes a shell command in a pane (used to delegate to hwf).
func PaneRun(paneID string, command ...string) error {
	return run(nil, append([]string{"pane", "run", paneID}, command...)...)
}
