# CLAUDE.md

herdr-automations — Herdr plugin：cron 排程觸發 coding agent（prompt + cron，一則 YAML entry 一個 automation）。單一 static Go binary。

## Commands

```bash
go build -o bin/herdr-automations . && go test ./...   # 正式 build（herdr 執行的就是 bin/ 這顆）
herdr plugin link .                                     # 用本 checkout 當已安裝 plugin
scripts/smoke.sh                                        # smoke test
```

## ⚠️ Gotchas

- **改完程式一定要 `go build -o bin/herdr-automations .`**：plugin 以 `herdr plugin link .` 連到 checkout，herdr 實際執行 `bin/` 內的 binary（`~/.local/bin/herdr-automations` 也 symlink 到它）。只跑 `go build ./...` 驗證編譯不會更新 `bin/`，pane 會繼續跑舊版。
- board pane 每 2 秒重刷（`internal/pane/pane.go` 的 `tea.Tick`），顯示欄位每次 refresh 重算——相對時間類顯示不需額外刷新機制。
- `scheduleCol` 常數必須等於 schedule 欄最寬的 rendering；改 `scheduleText` 的格式時要一起重算，且 name 欄寬度測試（`TestNameColumnGrowsIntoASpaciousPane`）的期望值會連動。
- `once: true` 的語意是「工作要完成」不是「時刻要用掉」：failed run 不 spend，下一個 occurrence 會重試；board 的 `spent()` 以最後一筆 `StatusDone` 判斷。
- shared placement 的 run 若沒拿到 tab，它記的 workspace 是**大家共用的那個**——改名或關掉會波及所有 automation。`closable` / `relabelRun` 都對此 fail-closed，只認明確的 `PlacementWorkspace`；`Placement` 為空的舊記錄一律不碰。
- 任何「隨時間才變 stale」的顯示（例如過夜 run 的 label 要補日期）必須由 daemon 時鐘驅動，不要掛在 run 或 retire 上——`keep: -1` 與 `once:` 的 automation 不會再進 retire，正好是最需要補的那些。掃描對象問 `herdr.LiveTargets()`，不要用 history 的固定筆數窗口。
- daemon 是 herdr server 的子行程，server 一停它的 stdout/stderr pipe 就斷。`Run()` 必須用 `signal.Notify` 收下 SIGPIPE，否則下一次 `log.Printf` 會當場殺掉 process、`defer` 全部跳過（關機那行 log 就是這樣把 lock 留下的）。不要改成 `signal.Ignore`——SIG_IGN 會被 exec 出去的 herdr 子行程繼承。
- 單例靠 `internal/daemon/lock.go` 的 **flock**，不是 `daemon.pid` 裡的數字：pid 只是給人看的診斷，程式不拿它做判斷。lock 檔刻意不刪（避免 unlink 掉別人正持有的 inode），`restart()` 也不主動釋放——靠 fd 的 close-on-exec 交棒，exec 失敗時舊 daemon 仍持鎖。要加「pid 還活著嗎」這類檢查前先想清楚：pid 會被系統回收，那正是排程整天不跑的老 bug。
- **動到 `config.Automation` 的欄位就要同步 `skills/creating-automations/SKILL.md` 與 `automations.example.yaml`**：兩份都沒有測試護著，schema 加欄位時很容易只改 struct。SKILL.md 是 agent 唯一讀得到的格式來源（`internal/skill` 把它 symlink 進 `~/.claude/skills`），它漏掉的欄位等於 agent 不知道存在——2026-08-15 就發現 `env` / `once` / `keep` / `keep_failed` / `placement` 五個全漏了。

## Structure

- `internal/config` — YAML automations 設定與 cron parser
- `internal/daemon` — 排程 daemon（flock 單例、self-update、overlap guard、catch-up）
- `internal/runner` — 實際啟 agent：worktree/root 模式、pane 生命週期
- `internal/pane` — Herdr overlay board（bubbletea）：排程、上次狀態、run now、跳 workspace
- `internal/history` — append-only JSONL run log（scheduled → running → done|failed|skipped|missed）
- `internal/herdr` — Herdr API client；`internal/wizard` — `add` 互動精靈；`internal/skill` — 給 agent 自行排程用的 bundled skill

## Conventions

- 註解走敘事風格：說明「為什麼／不這樣會怎樣」，不是逐行翻譯程式；新 code 要跟上這個密度。
- Commit message：`area: 小寫句子`（如 `board: …`、`runner: …`、`daemon: …`），描述行為改變而非實作。
- 測試命名描述行為（`TestScheduleFieldSaysWhatTheDaemonWillActuallyDo`），斷言訊息說出 want 的理由。

## Recent Significant Changes

- daemon：單例鎖改用 kernel flock，pid 只當診斷；daemon 無論怎麼死都不會再留下卡住排程的鎖。
- daemon：`once: true` automation 跑完一次自動 retire（記在 schedule state，YAML 免改）。
- runner：run 完成判定收緊——確認 agent 真的收到 prompt、agent 只是 blocked 不算 done、關 pane fail-closed。
- config：automation 可指定 agent 啟動環境（env）。
- runner/daemon：tab label 改 `✓ 07:00 name`，過夜的 run 由 daemon 逐日補上日期；pane 改用 agent 自報的 terminal title 命名。
