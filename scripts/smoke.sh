#!/bin/bash
# End-to-end acceptance for the custom branch. Exercises the whole chain
# against the live herdr daemon: shared placement, retention, labelling,
# output capture, concurrency and the run log.
#
# Assumes ./bin/herdr-automations is built and the daemon is running.
set -uo pipefail
cd /Users/pc035860/code/herdr-plugins/herdr-automations

BIN=./bin/herdr-automations
CFG="$(herdr plugin config-dir dnzzl.automations)/automations.yaml"
STATE="$HOME/.local/state/herdr/plugins/dnzzl.automations"
BACKUP=$(mktemp)
cp "$CFG" "$BACKUP"
pass=0; fail=0

check() { # check <label> <condition-result>
  if [ "$2" = "0" ]; then echo "  PASS  $1"; pass=$((pass+1));
  else echo "  FAIL  $1"; fail=$((fail+1)); fi
}

ws_count() { herdr workspace list | python3 -c "import json,sys;print(len(json.load(sys.stdin)['result']['workspaces']))"; }
shared_id() { herdr workspace list | python3 -c "
import json,sys
w=[x for x in json.load(sys.stdin)['result']['workspaces'] if x['label']=='Automations']
print(w[0]['workspace_id'] if w else '')"; }
tabs_of() { herdr tab list --workspace "$1" | python3 -c "
import json,sys
[print(t.get('tab_id'),t.get('label','')) for t in json.load(sys.stdin)['result']['tabs']]"; }

cleanup() {
  cp "$BACKUP" "$CFG"; rm -f "$BACKUP"
  echo; echo "config restored"
}
trap cleanup EXIT

cat >> "$CFG" <<'YAML'

  - name: smoke-a
    cron: "0 5 * * *"
    repo: /Users/pc035860/code/herdr-plugins/herdr-automations
    workspace: root
    agent: claude
    prompt: Reply with exactly SMOKE-A-OK and nothing else. Do not use any tools.
    agent_args: [--dangerously-skip-permissions]
    timeout_minutes: 5
    disabled: true

  - name: smoke-b
    cron: "0 5 * * *"
    repo: /Users/pc035860/code/herdr-plugins/herdr-automations
    workspace: root
    agent: claude
    prompt: Reply with exactly SMOKE-B-OK and nothing else. Do not use any tools.
    agent_args: [--dangerously-skip-permissions]
    timeout_minutes: 5
    disabled: true
YAML

echo "== config parses =="
listing=$($BIN list)
echo "$listing" | grep -q smoke-a; check "smoke-a registered" $?
echo "$listing" | grep -q smoke-b; check "smoke-b registered" $?

before_ws=$(ws_count)
echo "== run 1: smoke-a and smoke-b =="
$BIN run smoke-a >/dev/null 2>&1; a1=$?
$BIN run smoke-b >/dev/null 2>&1; b1=$?
check "smoke-a run 1 exited clean" $a1
check "smoke-b run 1 exited clean" $b1

SW=$(shared_id)
[ -n "$SW" ]; check "shared Automations workspace exists" $?
after_ws=$(ws_count)
[ "$after_ws" -le $((before_ws + 1)) ]; check "two automations added at most one workspace (was $before_ws, now $after_ws)" $?

tabs_of "$SW" | grep -q "smoke-a"; check "smoke-a landed as a tab" $?
tabs_of "$SW" | grep -q "smoke-b"; check "smoke-b landed as a tab" $?
tabs_of "$SW" | grep -q "✓ smoke-a"; check "smoke-a tab stamped with the success glyph" $?
tabs_of "$SW" | grep -qE "✓ smoke-a [0-9]{2}:[0-9]{2}"; check "smoke-a tab stamped with the time" $?

echo "== output captured =="
grep -rl "SMOKE-A-OK" "$STATE/output/" >/dev/null 2>&1; check "smoke-a actually answered (marker in captured output)" $?
grep -rl "SMOKE-B-OK" "$STATE/output/" >/dev/null 2>&1; check "smoke-b actually answered" $?
first_a=$(ls -t "$STATE"/output/smoke-a-*.log 2>/dev/null | head -1)

echo "== run 2: retention retires the previous pane =="
$BIN run smoke-a >/dev/null 2>&1; check "smoke-a run 2 exited clean" $?
[ "$(tabs_of "$SW" | grep -c 'smoke-a')" -eq 1 ]; check "smoke-a holds exactly one tab (keep: 1)" $?
[ -s "$first_a" ]; check "run 1's output survived its pane being retired" $?
grep -q "SMOKE-A-OK" "$first_a"; check "retired run's output is still readable" $?

echo "== history =="
hist=$($BIN history)
echo "$hist" | grep -q smoke-a; check "history lists smoke-a" $?
[ "$(echo "$hist" | grep -c 'smoke-[ab] *done')" -ge 3 ]; check "history records three successful runs" $?
# The run that exposed this reported done having never received its prompt, so
# a clean exit is not evidence: the agent's own reply is.
# Scoped to this run's automations: the log also holds unrelated history.
[ "$(echo "$hist" | grep 'smoke-[ab]' | grep -c 'failed')" -eq 0 ]; check "no smoke run failed" $?

echo
echo "passed: $pass   failed: $fail"
[ "$fail" -eq 0 ]
