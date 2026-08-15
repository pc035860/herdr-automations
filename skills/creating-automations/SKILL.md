---
name: herdr-automations
description: Schedule a recurring agent task with herdr-automations (cron + prompt), or edit/inspect existing ones. Use whenever the user wants something to run on a schedule — "every morning", "each Monday", "nightly", "every week", "on a cron", "recurring", "automate this", "schedule an agent", "run this while I sleep" — and for triage/dependency-bump/report/standup chores that repeat. Also use to list, disable, or debug scheduled automations and their run history.
---

# Scheduling Herdr automations

An automation is a cron schedule plus a prompt. At the scheduled time the
daemon provisions a workspace on a repo — a fresh git worktree by default, or
the repo root — spawns the agent there and submits the prompt. Your job:
translate the user's intent into a valid YAML entry, write it, and tell them
when it will next run.

## Where the file lives

Ask the CLI rather than guessing:

```bash
herdr plugin config-dir dnzzl.automations    # → <dir>/automations.yaml
```

Fallback when Herdr isn't installed: `~/.config/herdr-automations/automations.yaml`.

## Entry format

```yaml
max_concurrent: 2                 # optional, top-level: runs in flight at once.
                                  # Default 2; ten entries sharing 07:00 would
                                  # otherwise start ten agents in one second.
automations:
  - name: issue-triage            # unique
    cron: "0 9 * * 1-5"           # 5-field crontab, or @daily / @hourly / @weekly
    repo: ~/Projects/myapp        # repo the agent works in
    workspace: worktree           # worktree (default, fresh branch per run) | root
    placement: shared             # shared (default for root) | workspace
    agent: claude                 # any kind `herdr agent start` supports
    prompt: |
      Triage new GitHub issues: label them, close duplicates,
      draft replies for the ones needing more info.
    mcp_config: ~/.config/mcp/github.json   # optional → passed as --mcp-config
    agent_args: ["--model", "opus"]         # optional, verbatim agent flags
    env:                          # optional, exported into the pane's shell
      MCP_TIMEOUT: "60000"        # before the agent starts
    timeout_minutes: 60           # optional, default 60
    catch_up_minutes: 120         # optional: how late a sleep-delayed run may start; -1 never
    keep: 1                       # optional: finished runs that keep their pane
    keep_failed: 3                # optional: same budget for failures
    # once: true                  # retire the automation after one run finishes
    # disabled: true              # keep the entry, stop scheduling it
```

Instead of `prompt`, an entry may delegate to a herdr-workflows workflow:

```yaml
    workflow: nightly-deps        # runs `hwf run nightly-deps` in the pane
```

Exactly one of `prompt` / `workflow` is required.

## Rules

- **Read the existing file first and append.** Never rewrite entries you didn't
  come to change.
- Names must be unique. Spaces are allowed; the branch name is slugified.
- Cron is validated on load, and one bad entry blocks the whole file — re-read
  it after writing.
- Times are local to the machine running the Herdr server.
- The daemon reloads within 30 seconds; no restart needed.
- Occurrences missed while the machine slept run on wake within `catch_up_minutes`
  (default 120), otherwise they appear as `missed` in the history.
- `workspace: worktree` means the agent never touches the user's working copy.
  Only choose `root` when the task must see uncommitted local state.
- Set `env: MCP_TIMEOUT: "60000"` on every entry whose agent loads MCP servers,
  which in practice is all of them — servers connect during agent startup no
  matter whether the prompt goes on to call an MCP tool, and the 30s default
  makes a slow morning look like `agent_prompt_stalled` rather than an MCP
  error. Don't try to work out which entries "use MCP".

## Panes and tabs

Each run opens its own pane; `keep` decides how long the finished one stays.
Old panes are retired **when the next run starts**, not when a run ends, so a
retained pane lasts exactly one period — a weekly automation's output stays up
for a week and an hourly one's for an hour, from the same `keep: 1`.

- `keep: 1` (default) — one finished run on screen at a time
- `keep: 0` — close the pane the moment the run ends
- `keep: -1` — never retire; for output meant to be read days later
- `keep_failed: 3` (default) — failures are kept longer; a failure is the run
  the user actually wants to open

`placement: shared` collects every run as a tab in one "Automations" workspace,
which is what keeps ten automations from burying the workspaces the user drives.
Worktree runs default to their own workspace; root runs default to shared.

## Choosing the schedule

Ask only when it's genuinely ambiguous. Otherwise translate directly:
"every morning" → `0 9 * * *`, "weekdays at 9" → `0 9 * * 1-5`,
"each Monday at 9am" → `0 9 * * 1`, "nightly" → `@daily`,
"every week" → `0 9 * * 1`.

## Verify

```bash
herdr-automations list             # entry present, cron parsed
herdr-automations run <name>       # optional immediate test run
herdr-automations history <name>   # what happened
```

Report the next scheduled run time and how to trigger it immediately.
