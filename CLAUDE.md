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

## Structure

- `internal/config` — YAML automations 設定與 cron parser
- `internal/daemon` — 排程 daemon（PID lock、self-update、overlap guard、catch-up）
- `internal/runner` — 實際啟 agent：worktree/root 模式、pane 生命週期
- `internal/pane` — Herdr overlay board（bubbletea）：排程、上次狀態、run now、跳 workspace
- `internal/history` — append-only JSONL run log（scheduled → running → done|failed|skipped|missed）
- `internal/herdr` — Herdr API client；`internal/wizard` — `add` 互動精靈；`internal/skill` — 給 agent 自行排程用的 bundled skill

## Conventions

- 註解走敘事風格：說明「為什麼／不這樣會怎樣」，不是逐行翻譯程式；新 code 要跟上這個密度。
- Commit message：`area: 小寫句子`（如 `board: …`、`runner: …`、`daemon: …`），描述行為改變而非實作。
- 測試命名描述行為（`TestScheduleFieldSaysWhatTheDaemonWillActuallyDo`），斷言訊息說出 want 的理由。

## Recent Significant Changes

- board：schedule 欄改相對時間顯示（`in 18h 30m`；≥7 天 fallback 到 `8/15` 日期），欄寬 21→16，name 欄多 5 格。
- daemon：`once: true` automation 跑完一次自動 retire（記在 schedule state，YAML 免改）。
- runner：run 完成判定收緊——確認 agent 真的收到 prompt、agent 只是 blocked 不算 done、關 pane fail-closed。
- config：automation 可指定 agent 啟動環境（env）。
