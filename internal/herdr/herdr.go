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
	"strings"
	"time"
)

func bin() string {
	if b := os.Getenv("HERDR_BIN_PATH"); b != "" {
		return b
	}
	return "herdr"
}

// run executes a herdr subcommand and decodes the socket-API JSON envelope
// ({"id": ..., "result": {...}}) into out when out is non-nil.
func run(out any, args ...string) error {
	cmd := exec.Command(bin(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %s", args[0]+" "+args[1], apiError(stdout.Bytes(), stderr.String()))
	}
	if out == nil {
		return nil
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		return fmt.Errorf("herdr %v: unexpected output %q: %w", args, stdout.String(), err)
	}
	raw := envelope.Result
	if raw == nil {
		raw = stdout.Bytes() // some commands print the result object bare
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("herdr %v: decode result: %w", args, err)
	}
	return nil
}

// runText executes a herdr subcommand that prints plain text rather than the
// JSON envelope — the terminal-reading commands.
func runText(args ...string) (string, error) {
	cmd := exec.Command(bin(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %s", args[0]+" "+args[1], apiError(stdout.Bytes(), stderr.String()))
	}
	return stdout.String(), nil
}

// PaneTail reads back what the agent printed. Retiring a pane throws its
// terminal away, so this is what lets a run's output outlive it.
func PaneTail(paneID string, lines int) (string, error) {
	return runText("pane", "read", paneID,
		"--source", "recent-unwrapped",
		"--lines", fmt.Sprintf("%d", lines),
		"--format", "text")
}

// apiError turns herdr's JSON error envelope into one readable line. Without
// this the raw payload ends up in logs and, worse, in the board's status line.
func apiError(stdout []byte, stderr string) string {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(stdout, &envelope) == nil && envelope.Error.Code != "" {
		if envelope.Error.Message != "" {
			return envelope.Error.Code + ": " + envelope.Error.Message
		}
		return envelope.Error.Code
	}
	if s := strings.TrimSpace(stderr); s != "" {
		return s
	}
	return strings.TrimSpace(string(stdout))
}

// createResult matches both worktree_created and workspace_created payloads:
// the new workspace plus its initial pane, ready for `agent start`.
type createResult struct {
	Workspace struct {
		WorkspaceID string `json:"workspace_id"`
	} `json:"workspace"`
	RootPane struct {
		PaneID string `json:"pane_id"`
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
	var res struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
			TabID  string `json:"tab_id"`
		} `json:"root_pane"`
	}
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
		if err == nil || !strings.Contains(err.Error(), "agent_pane_busy") {
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
		"--timeout", fmt.Sprintf("%d", timeout.Milliseconds()))
}

// AgentPrompt submits a prompt and waits for the agent to settle (idle, done
// or blocked), bounded by timeout.
func AgentPrompt(target, text string, timeout time.Duration) error {
	return run(nil, "agent", "prompt", target, text,
		"--wait", "--timeout", fmt.Sprintf("%d", timeout.Milliseconds()))
}

// PromptStalled reports whether err is herdr refusing to call a submission
// observed: the text reached the agent's input but nothing happened.
func PromptStalled(err error) bool {
	return err != nil && strings.Contains(err.Error(), "agent_prompt_stalled")
}

// AgentSubmit presses Enter in the agent's input, then waits for it to settle.
// Recovery for a stalled prompt: the text is already typed, so re-prompting
// would type it a second time.
func AgentSubmit(target string, timeout time.Duration) error {
	if err := run(nil, "agent", "send-keys", target, "Enter"); err != nil {
		return err
	}
	return AgentWait(target, timeout)
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
		if gone(err) {
			return false, "", ErrGone
		}
		return false, "", err
	}
	return res.Workspace.Focused, res.Workspace.ActiveTabID, nil
}

// TabRename restamps a run's tab, which is how a finished run reports itself
// in the shared workspace.
func TabRename(tabID, label string) error {
	if err := run(nil, "tab", "rename", tabID, label); err != nil && !gone(err) {
		return err
	}
	return nil
}

// WorkspaceRename does the same for a run that has a workspace to itself.
func WorkspaceRename(workspaceID, label string) error {
	if err := run(nil, "workspace", "rename", workspaceID, label); err != nil && !gone(err) {
		return err
	}
	return nil
}

// TabClose retires one run's tab. A tab that is already gone is a success:
// retirement is best-effort bookkeeping, not a transaction.
func TabClose(tabID string) error {
	if err := run(nil, "tab", "close", tabID); err != nil && !gone(err) {
		return err
	}
	return nil
}

// WorkspaceClose retires a run that had a workspace to itself.
func WorkspaceClose(workspaceID string) error {
	if err := run(nil, "workspace", "close", workspaceID); err != nil && !gone(err) {
		return err
	}
	return nil
}

func gone(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "workspace_not_found") ||
		strings.Contains(err.Error(), "tab_not_found"))
}

// Focus brings a run's workspace to the front, then its agent pane when one
// is known — the "jump to what this automation did" move.
func Focus(workspaceID, paneID string) error {
	if workspaceID != "" {
		if err := run(nil, "workspace", "focus", workspaceID); err != nil {
			if strings.Contains(err.Error(), "workspace_not_found") {
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
