# W0 probe results — agy one-shot print provider

**Date**: 2026-07-08
**環境**: 本機 macOS agy v1.0.10 ／ oracle-vps Linux agy v1.0.14（部署目標；VPS 指令經用戶授權執行）
**參照**: `docs/agy-oneshot-print-provider.md` §3 W0

## 總表

| Probe | 結果 | 對 spec 的影響 |
|-------|------|---------------|
| P1 GEMINI.md print-mode 讀取 | ❌ **兩版本都不讀** | §2.2 改用 P1b seed-turn |
| P1b seed-turn system prompt | ✅ | 採用（= claude_cli k-2 cold-seed 同型） |
| P2 per-run config 隔離（O1） | ✅ Linux 全過（macOS 僅 config 半過） | O1 定案，O2 不需要 |
| P3 per-turn 延遲 | ✅ VPS 中位 ~5s（本機 1.0.10 不穩） | 過關；hard-kill timer 必要 |
| P4 argv 長 prompt | ✅ 9.8KB 完整 | tmux paste-buffer 不需移植 |
| P5 工具型 turn stdout | ⚠️ 絕對路徑入 prompt 才乾淨 | cwd/--add-dir 非 workspace；1.0.14 要 strip Summary 尾段 |
| P6 長對話 resume | 部分（<10 turn 正常） | 深度（20+）移 W2 觀測 |

## 逐項紀錄

### P1 — GEMINI.md：print mode 完全不載（兩版本）

workdir 放 `GEMINI.md`（規則：回覆以 `[FOX-P1]` 開頭），cwd 在該目錄跑 `agy -p "Say hello"`：

- 本機 1.0.10：三次（含二次 run 排除 trust gate）皆無 tag；直問「你被要求用什麼 tag?」答 `NONE`；`--log-file` 全文零提及 GEMINI.md/trust/context
- VPS 1.0.14：同樣無 tag

**根因（P5 交叉證實）**：print mode 沒有 workspace 概念 — cwd 不被載入，interactive 模式的 GEMINI.md 機制（PR #27）在 print 路徑不存在。

### P1b — seed-turn 遞送 system prompt：通過

turn 1 = `SYSTEM INSTRUCTIONS (standing rules...): ...[FOX-P1] rule... Acknowledge with exactly one word: OK`（輸出棄置）：

- turn 1 → `[FOX-P1] OK`；turn 2（resume）`Say hello` → `[FOX-P1] Hello, ...`；turn 3 問對話內容 → 有 tag、描述「established custom formatting rules」但**不逐字回聲**

### P2 — O1 fake-HOME 隔離：Linux（部署目標）通過

fake HOME：`~/.gemini` 逐項 symlink（**除 `config/` 用獨立目錄**，內放 mcp_config.json 指向 marker script）：

- **VPS Linux**：`HOME=$FH agy -p "PONG test"` → 回 `PONG`（**auth 經 symlink 共用成功**，token file = `antigravity-oauth-token`）+ marker 被 spawn（**隔離 config 被讀**）
- 本機 macOS：config 隔離證實（marker spawn），但 auth 失敗（`You are not logged into Antigravity`，兩種 symlink 拓撲都敗 — auth 走 Keychain/HOME 交互，macOS 特有；不影響部署目標）

**結論**：O1 成立。W1 bridge 隔離 = per-session fake HOME（symlink 全部、獨立 `config/`），不需 O2 全域序列化。

### P3 — 延遲：VPS 穩定 ~5s/turn；本機 1.0.10 高 wedge 率

- VPS 1.0.14 連續 5 輪 resume：`5s / 63s / 4s / 4s / 5s`，5/5 有答案
- 本機 1.0.10：turn1 4s、turn2 22s、turn3 **wedge >7min 且 `--print-timeout 90s` 未生效**（被外層 kill）；後續迴圈 9m50s 只推進 2 輪
- **雙重發現**：(a) `--print-timeout` 不可靠 → provider 必須 `exec.CommandContext` hard-kill；(b) **被 kill 的 turn 在 server 端可能已完成**（wedge 的 turn3 被 kill 後，下一輪回「4」= 對話已含 3）→ kill-and-retry 有**重複執行**語意，禁止自動重試有副作用的 turn

### P4 — argv 長 prompt：通過

9,786 bytes、120 行、含 CJK/emoji/雙單引號，`-p "$(cat prompt.txt)"` 送入，要求逐字回最後一行 → 內容完整重現。tmux paste-buffer workaround（PR #25）不需移植。

### P5 — 工具型 turn stdout 形狀

| 變體 | 結果 |
|------|------|
| `secret.txt`（相對）、預設權限 | stdout 混 4 行敘事 + `Error: timeout`（找不到檔案 — cwd 非 workspace，跑去翻 scratch/~/.gemini） |
| + `--add-dir <dir>`、預設權限 | 零輸出 timeout（權限 review 卡住工具） |
| + `--sandbox`（自動執行） | 工具會跑但**仍不看 --add-dir**，全家目錄亂搜（Desktop/Documents）後 timeout |
| **絕對路徑入 prompt** + `--sandbox` | ✅ stdout = `MARMALADE-7743`，乾淨無敘事 |

**規則**：檔案引用一律絕對路徑寫進 prompt（fox 附圖同法）；answer 提取需容忍 flail 時的敘事行前綴（可複用 PR #29 strip）；**1.0.14 新增**：正常回覆會附 `**Summary of work:**` 尾段 → 需 strip。

### P6 — 長對話（部分）

本機對話推進到 6+ 後 resume 記憶正確；VPS 對話跨 2 個 session（相隔 ~1h）續 resume 正常。20+ turn 深度未完成（本機 wedge 率使成本過高）→ W2 於 VPS 以真流量觀測。

## 遺留

- macOS fake-HOME auth 失效原因未深究（不影響部署，若要本機 e2e 測 O1 需先解）
- VPS 殘留 `/tmp/agy-p1`、`/tmp/agy-p2`、`/tmp/agy-resume-test`（/tmp 自清）
- 1.0.14 `Summary of work` 尾段的確切觸發條件（是否僅對話型 turn）待 W1 測試界定
