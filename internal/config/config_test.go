package config

import (
	"os"
	"path/filepath"
	"testing"
)

func withConfig(t *testing.T, yaml string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", dir)
	if yaml != "" {
		if err := os.WriteFile(filepath.Join(dir, "automations.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	withConfig(t, "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Automations) != 0 {
		t.Fatalf("expected empty config, got %d entries", len(cfg.Automations))
	}
}

func TestLoadDefaultsAndValidation(t *testing.T) {
	withConfig(t, `
automations:
  - name: triage
    cron: "0 9 * * 1-5"
    repo: ~/Projects/foo
    prompt: "Triage the issues"
`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Automations[0]
	if a.Workspace != WorkspaceWorktree || a.Agent != "claude" || a.TimeoutMinutes != 60 {
		t.Fatalf("defaults not applied: %+v", a)
	}
	home, _ := os.UserHomeDir()
	if a.Repo != filepath.Join(home, "Projects/foo") {
		t.Fatalf("home not expanded: %s", a.Repo)
	}
}

func TestPlacementDefaultsPerWorkspaceMode(t *testing.T) {
	withConfig(t, `
automations:
  - {name: shared, cron: "@daily", repo: /x, prompt: p, workspace: root}
  - {name: own, cron: "@daily", repo: /x, prompt: p, workspace: worktree}
  - {name: opted-out, cron: "@daily", repo: /x, prompt: p, workspace: root, placement: workspace}
`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Placement{
		"shared":    PlacementShared,
		"own":       PlacementWorkspace, // a worktree brings its own workspace
		"opted-out": PlacementWorkspace,
	}
	for name, w := range want {
		if got := cfg.Find(name).Placement; got != w {
			t.Errorf("%s: placement = %q, want %q", name, got, w)
		}
	}
}

func TestKeepDistinguishesUnsetFromZero(t *testing.T) {
	withConfig(t, `
automations:
  - {name: default, cron: "@daily", repo: /x, prompt: p}
  - {name: transient, cron: "@daily", repo: /x, prompt: p, keep: 0}
  - {name: forever, cron: "@daily", repo: /x, prompt: p, keep: -1, keep_failed: 10}
`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if a := cfg.Find("default"); a.KeepCount() != 1 || a.KeepFailedCount() != 3 || !a.Retires() {
		t.Errorf("default: keep=%d keep_failed=%d retires=%v, want 1/3/true",
			a.KeepCount(), a.KeepFailedCount(), a.Retires())
	}
	// The distinction the pointer exists for: 0 means "close it", not "unset".
	if a := cfg.Find("transient"); a.KeepCount() != 0 || !a.Retires() {
		t.Errorf("transient: keep=%d retires=%v, want 0/true", a.KeepCount(), a.Retires())
	}
	if a := cfg.Find("forever"); a.Retires() || a.KeepFailedCount() != 10 {
		t.Errorf("forever: retires=%v keep_failed=%d, want false/10", a.Retires(), a.KeepFailedCount())
	}
}

func TestLineOfFindsTheEntry(t *testing.T) {
	withConfig(t, `automations:
  - name: first
    cron: "@daily"
    repo: /x
    prompt: p
  - name: second
    cron: "@daily"
    repo: /x
    prompt: p
`)
	if got := LineOf("second"); got != 6 {
		t.Fatalf("LineOf(second) = %d, want 6", got)
	}
	if got := LineOf("first"); got != 2 {
		t.Fatalf("LineOf(first) = %d, want 2", got)
	}
	if got := LineOf("missing"); got != 0 {
		t.Fatalf("LineOf(missing) = %d, want 0", got)
	}
}

func TestLoadRejectsBadEntries(t *testing.T) {
	cases := map[string]string{
		"bad cron": `
automations:
  - {name: a, cron: "not a cron", repo: /x, prompt: p}`,
		"prompt and workflow": `
automations:
  - {name: a, cron: "@daily", repo: /x, prompt: p, workflow: w}`,
		"neither prompt nor workflow": `
automations:
  - {name: a, cron: "@daily", repo: /x}`,
		"duplicate names": `
automations:
  - {name: a, cron: "@daily", repo: /x, prompt: p}
  - {name: a, cron: "@daily", repo: /x, prompt: p}`,
		"bad workspace": `
automations:
  - {name: a, cron: "@daily", repo: /x, prompt: p, workspace: sandbox}`,
		"bad placement": `
automations:
  - {name: a, cron: "@daily", repo: /x, prompt: p, placement: floating}`,
		"worktree cannot share a workspace": `
automations:
  - {name: a, cron: "@daily", repo: /x, prompt: p, workspace: worktree, placement: shared}`,
	}
	for label, yaml := range cases {
		t.Run(label, func(t *testing.T) {
			withConfig(t, yaml)
			if _, err := Load(); err == nil {
				t.Fatalf("expected error for %s", label)
			}
		})
	}
}
