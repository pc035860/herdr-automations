package herdr

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPIErrorReadsTheEnvelopeFromEitherStream(t *testing.T) {
	envelope := `{"error":{"code":"agent_prompt_stalled","message":"no state change"},"id":"cli:agent:prompt"}`

	// herdr writes the envelope to stderr for some subcommands and stdout for
	// others. Matching the formatted message instead of the parsed code hid
	// this: the raw JSON happened to contain the code as a substring.
	for name, err := range map[string]error{
		"stdout": apiError("agent prompt", []byte(envelope), ""),
		"stderr": apiError("agent prompt", nil, envelope),
	} {
		if !hasCode(err, codePromptStalled) {
			t.Errorf("%s: hasCode false for %v", name, err)
		}
	}

	// Plain text still surfaces, just without a code to branch on.
	plain := apiError("workspace list", nil, "herdr: not running")
	if hasCode(plain, codePromptStalled) {
		t.Error("plain stderr reported a code")
	}
	if plain.Error() != "workspace list: herdr: not running" {
		t.Errorf("plain error = %q", plain.Error())
	}
}

// fakeHerdr installs a stand-in for the herdr CLI that answers each subcommand
// from a script of canned stdout, so the payload shapes this package decodes
// are pinned by a test rather than by whatever happened to work once.
func fakeHerdr(t *testing.T, replies map[string]string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "herdr")
	var cases strings.Builder
	for sub, out := range replies {
		fmt.Fprintf(&cases, "  %q) cat <<'JSON'\n%s\nJSON\n  ;;\n", sub, out)
	}
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" + cases.String() +
		"  *) echo '{\"error\":{\"code\":\"unexpected\",\"message\":\"'\"$1 $2\"'\"}}' >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_BIN_PATH", path)
}

func TestLiveTargetsReadsWhatHerdrStillHolds(t *testing.T) {
	fakeHerdr(t, map[string]string{
		"workspace list": `{"id":"cli:workspace:list","result":{"workspaces":[{"workspace_id":"w9"},{"workspace_id":"w2"}]}}`,
		"tab list":       `{"id":"cli:tab:list","result":{"tabs":[{"tab_id":"w9:t1"},{"tab_id":"w9:tX"}]}}`,
	})

	tabs, workspaces, err := LiveTargets()
	if err != nil {
		t.Fatalf("LiveTargets = %v", err)
	}
	if !tabs["w9:t1"] || !tabs["w9:tX"] || len(tabs) != 2 {
		t.Errorf("tabs = %v, want both ids herdr listed", tabs)
	}
	if !workspaces["w9"] || !workspaces["w2"] || len(workspaces) != 2 {
		t.Errorf("workspaces = %v, want both ids herdr listed", workspaces)
	}
}

func TestLiveTargetsReturnsNothingWhenEitherListFails(t *testing.T) {
	// A partial answer is worse than none: callers use these sets to decide
	// what still exists, and a missing half reads as "closed" for every run in
	// it. Both orders are checked because either call can be the one that fails.
	ws := `{"result":{"workspaces":[{"workspace_id":"w9"}]}}`
	tabs := `{"result":{"tabs":[{"tab_id":"w9:t1"}]}}`
	for name, replies := range map[string]map[string]string{
		"workspace list fails": {"tab list": tabs},
		"tab list fails":       {"workspace list": ws},
	} {
		fakeHerdr(t, replies)
		gotTabs, gotWorkspaces, err := LiveTargets()
		if err == nil {
			t.Errorf("%s: LiveTargets returned no error", name)
		}
		if gotTabs != nil || gotWorkspaces != nil {
			t.Errorf("%s: LiveTargets = %v, %v, want nothing alongside the error",
				name, gotTabs, gotWorkspaces)
		}
	}
}
