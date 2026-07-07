# agy one-shot print provider — change spec

**Status**: PROPOSED（待 probe wave 後開工）
**Branch**: `feature/agy-oneshot-print`
**Supersedes**: `docs/agy-cli-enhancement-plan.md` Phase 0 Q1 結論、`internal/providers/agycli/doc.go` empirical baseline 第 1、3 點
**靈感來源**: [PeterPanSwift/fox-ai-roundtable](https://github.com/PeterPanSwift/fox-ai-roundtable)（零相依 `spawn` 驅動 `agy -p` + `--conversation` 續談，實證可行）

## 0. 背景 — Phase 0 的兩條基石結論被推翻

現行 `agy_cli_provider.go` 的持久互動 session 架構（tmux + PTY + `parse.go` TUI 刮取 + idle reaper，agycli 套件 ~6400 行）建立在 Phase 0（agy v1.0.10）的兩條結論上：

1. 「非 TTY `-p` 會 hang 到 print-timeout 且 stdout 全空（issue #76）→ PTY 是必要的」（doc.go 第 1 點）
2. 「`--print` 模式 resume 完全失效：`--conversation <真實id>` 不載回前文、每次呼叫新生 conversation db → 多輪記憶只能靠持久互動 session」（enhancement-plan Q1）

**2026-07-08 雙環境實測推翻兩者**：

| 環境 | agy 版本 | 測試 | 結果 |
|------|---------|------|------|
| 本機 macOS | v1.0.10（Phase 0 同版本） | `agy -p "Remember codeword: PINEAPPLE42..." --log-file t1.log < /dev/null` | 回 `OK`，exit 0，**非 TTY 沒 hang**；log 含 `Created conversation 5642d9cd-...` |
| 本機 macOS | v1.0.10 | `agy --conversation 5642d9cd-... -p "What is the codeword?"` | 回 `PINEAPPLE42` — **跨行程記憶正常** |
| 本機 macOS | v1.0.10 | 第三輪 append 測試 + `--log-file` | 回 `PINEAPPLE42BANANA`；log **無**新生 conversation（複用同 id） |
| oracle-vps | v1.0.14（prod） | 同組三步（用戶授權執行） | `OK` → `Created conversation 1ea82809-...` → `PINEAPPLE42` |

**Phase 0 為何錯**：Q1 僅 2 次 attempt，一次 ~5m timeout、一次跑去自我探索 — 症狀與後來確診的 headless OAuth token 失效（`error getting token source` → timeout + 0 token，見 2026-06-22 esmith-general 事故）完全一致。實驗極可能被 stale token 污染，把 auth 故障誤判為功能缺失。

**注意**：純文字 turn 在預設 permission mode（request-review）下即可完成，實測**未帶** `--dangerously-skip-permissions`。

## 1. 目標 / 非目標

**目標**：
- 在 `AgyCLIProvider` 增加 **one-shot print 模式**（config opt-in，預設 off）：每輪 `agy --conversation <id> -p <msg>`，不再維持常駐 tmux/agy 行程
- 與現行 interactive（tmux）路徑**並存**，可用 config flag A/B，prod 驗證後另開 change 退役 tmux 路徑
- 消滅驅動 TUI 的整類衍生 bug：PR #25（tmux paste buffer 長 prompt）、#27（system prompt 回聲）、#28（互動選單卡死）、#29（agentic 進度 UI 刮除）在 one-shot 路徑天然不存在

**非目標**：
- 不動 claude_cli / ACP provider
- 不在本 change 刪除 tmux/PTY/parse 程式碼（退役是後續 change，遵循 stage-first 驗證紀律）
- 不解決 headless OAuth token 週期性失效（auth 層問題，與執行模式正交；health.go 探測照舊）

## 2. 設計

### 2.1 Config 表面

`AgyCLIConfig`（`internal/config/config_channels.go`）加一欄：

| 欄位 | 型別 | 預設 | 語意 |
|------|------|------|------|
| `one_shot` | bool | false | true → provider 走 one-shot print 路徑；false → 現行 interactive tmux 路徑，行為 bit-for-bit 不變 |

`registerAgyCLIFromConfig`（`cmd/gateway_providers.go`）對應加 `WithAgyCLIOneShot(true)` option。**Rollback = flag 關掉 + restart**，零 migration。

### 2.2 Provider 內部（one-shot 模式）

複用現有骨架，`sessionEntry` 語意改變：

| 元件 | interactive（現行） | one-shot（新） |
|------|--------------------|---------------|
| `sessionEntry.sess` | 常駐 `agycli.Session`（tmux 行程） | **nil** — 改存 `conversationID string` + `workDir string` |
| turn 執行 | `sess.SendPrompt`（send-keys + capture-pane） | `exec.CommandContext` 跑 `agy` 一次，stdout 即答案 |
| system prompt | GEMINI.md at session create | 同機制：首 turn 前寫 `<workDir>/GEMINI.md`（probe P1 驗證 print mode 會讀） |
| 首 turn | NewSession 啟動 tmux | 帶 `--log-file <tmpfile>`，結束後 regex `Created conversation ([a-f0-9-]+)` 存入 entry（取代 session.go snapshot-diff — 該法自我降級為觀測用，log-file 法 race-free） |
| 續 turn | 同一 tmux 行程 | `--conversation <id> -p <msg>` |
| 併發 | `entry.mu` 序列化同 session | 同 — `entry.mu` 序列化同 conversation 的 turn |
| idle reaper | 關 tmux 行程 + bridge listener | 只清 map entry + bridge listener（無行程可關，成本更低） |
| ephemeral（空 session_key） | 建即用即關 | 單次 `agy -p`，不帶 `--conversation`，無 ID 捕捉 |

### 2.3 argbuilder 擴充

`PrintOptions` 加 `Conversation string`（→ `--conversation <id>`）與 `LogFile string`（→ `--log-file <path>`），維持「`-p` 的值永遠是 prompt、flags 順序固定」的既有 argv 契約。長/多行 prompt 走 argv 傳遞（probe P4 驗證上限），tmux paste-buffer workaround 不需要。

### 2.4 輸出處理

實測純文字 turn 的 stdout 是乾淨答案（無 TUI 殘影）。保留 `StripANSI` 作為安全網；`parse.go` 的重型啟發式（cursor-up/spinner 對抗）不進 one-shot 路徑。**工具型 turn 的 stdout 形狀未知 → probe P5**。

### 2.5 設計難點：MCP bridge 的 per-run config race（本 change 最大風險）

Interactive 模式的安全前提（provider 內註解，2026-06-22 實測）：agy **只在啟動時**讀全域 `~/.gemini/config/mcp_config.json`，session 建立是低頻事件，重疊安全。

One-shot 模式**每輪 spawn 都重新讀 config**，而 bridge entry 是**單一固定 key**（`BuildAgyBridgeServers` → `bridgeServerName`）指向 per-session loopback URL（port 即身分）。兩個 session 併發時：

```
session A: write config (A 的 URL) → spawn agy_A
session B: write config (B 的 URL) → spawn agy_B
                ↑ 若發生在 A spawn 之後、agy_A 讀檔之前 → agy_A 連到 B 的 bridge = 跨用戶身分混淆
```

| 選項 | 做法 | 評估 |
|------|------|------|
| **O1 per-run 隔離 config**（首選，待 probe P2） | spawn 時以 env 重導 agy 的 config 讀取位置（候選：`HOME` 指向 per-session fake home + symlink 真實 auth/settings；或 agy 原生 config-dir env——若存在） | 徹底消除共享，並順帶隔離 conversations db。風險：auth token 在真 `~/.gemini` 下，fake HOME 需 symlink 帶入；agy 是否跟隨 symlink 未知 |
| O2 全域 write→spawn→exit 鎖 | provider 級 mutex 罩住「寫 config → 行程結束」 | 跨 session 的 turn 全序列化（turn 10–60s），多 bot 併發直接排隊；且嚴格說 agy 讀檔時點在 spawn 後，鎖到 exit 才安全 → 吞吐最差但絕對正確 |
| ~~O3 per-session entry name~~ | `goclaw-bridge-<hash>` 各 session 一條 | **否決** — agy 啟動連**所有** configured servers，session A 的 agy 會連到 B 的 listener（bearer 共用、port 即身分）= 跨用戶工具存取，security no-go |

**決策規則**：P2 證實可隔離 → O1；否則 fallback O2（VPS 目前流量低，可接受），並在 doc 記明吞吐代價。`AGY_CONFIG_DIR` 是 goclaw 測試 seam（只影響 goclaw 的 writer），**不是** agy 原生 env，不可誤用。

## 3. Wave 計畫

### W0 — Probes（不寫產品碼；每項留 test-evidence）

| # | 問題 | 方法 | 過關準則 |
|---|------|------|---------|
| P1 | print mode 讀 GEMINI.md？ | workdir 放 GEMINI.md（指示固定回覆格式）→ `agy -p` 觀察是否遵循且**不回聲** | 遵循 + 不回聲 |
| P2 | config 可 per-run 隔離？ | fake HOME + symlink auth → `agy -p` 能 auth 且讀到 fake home 的 mcp_config.json（放一個可觀測的 stdio server 驗證載入） | auth 成功 + 只載入 fake home 的 server |
| P3 | per-turn 延遲 | 同一 conversation 連續 5 turn 計時，對比 interactive 路徑 | 中位數增量 < 5s（否則記錄並評估） |
| P4 | 長/多行 prompt 經 argv | 8KB 多行 prompt 含引號/emoji/CJK | 完整送達（回覆可驗證尾段內容） |
| P5 | 工具型 turn 的 stdout 形狀 | 要求 agy 讀一個檔案再回答 → 檢查 stdout 是否混入工具進度 | 界定答案可提取的規則（若髒，定義最小 strip） |
| P6 | `--conversation` 對已滿/巨大 conversation 的行為 | 20+ turn 後 resume | 無退化（記憶仍在、延遲可控） |

### W1 — 實作（P1–P5 過關後）

1. `argbuilder.go`：`PrintOptions.Conversation` / `.LogFile` + tests
2. `agycli` 新增 one-shot runner（`RunPrint(ctx, binary, opts) (stdout, conversationID, err)`，內含 log-file 建立/regex/清理；只依賴 stdlib，維持套件零耦合）+ tests
3. `agy_cli_provider.go`：`oneShot bool` + `WithAgyCLIOneShot`；`runTurn` 分流；`sessionEntry` 加 `conversationID`/`workDir`；reaper 適配 + tests（fake runner seam，比照現有 `withAgyCLISessionFactory`）
4. `config_channels.go` + `cmd/gateway_providers.go` 接線
5. bridge 隔離：依 P2 結果實作 O1 或 O2 + 併發測試（`go test -race`）
6. Gate：`go build ./...` + `go vet` + `go test ./internal/providers/... ./internal/config`

### W2 — VPS A/B 驗證

1. Deploy（照慣例：`git pull --ff-only` + embedui build + `systemctl --user restart goclaw`；備份 `goclaw.bak.pre-oneshot`）
2. esmith-general（agy-cli bot）開 `one_shot: true` → LINE WORKS 實測多輪記憶、bridge 工具呼叫（若該 bot 有配）、長訊息
3. 觀測 24–48h：`v3.run.completed` 的 duration/token 對比、無 0-token turn
4. 過關 → 另開 change 退役 tmux 路徑；不過 → flag off 回滾，記錄失敗模式

## 4. 驗收準則

- one-shot 模式下：同 session_key 跨 turn 記憶正常（三輪 codeword 測試綠）、system prompt 生效且不回聲、bridge 工具呼叫身分正確（雙用戶併發無串線）
- `one_shot: false`（預設）時現行為 bit-for-bit 不變（既有測試全綠）
- 已知風險記錄：headless token stale 照舊存在（症狀=timeout+空答案），health.go 探測不受影響
