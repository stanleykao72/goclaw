# agy-cli Provider 實作計畫(與 claude-cli 同等級)

> 目標:在 goclaw 新增一個 `agy-cli` provider,以薄代理方式驅動 Google Antigravity CLI(`agy`),
> 結構對齊既有的 `claude-cli` provider(`internal/providers/claude_cli*.go`,11 檔 ~1886 LOC)。
>
> ⚠️ **政策前提(非工程問題)**:Google Antigravity ToS 明文禁止「第三方軟體/工具/服務存取 Antigravity」。
> goclaw 以 subprocess 驅動 `agy` 屬此範疇,風險為帳號封禁 / OAuth 撤銷。本計畫只處理工程面;
> 是否上線由使用者承擔 ToS 風險決定。動工前建議直讀 antigravity.google 官方 ToS/FAQ 原文。

---

## 0. 實測基準(本機 agy v1.0.5,2026-06-19 驗證)

所有設計都建立在以下實機觀測,不是文件推測:

| 測項 | 指令 | 結果 | 對設計的影響 |
|------|------|------|-------------|
| **非 TTY stdout(#76)** | `agy -p "..." > file`(redirect) | **exit 124(hang 到 timeout)、stdout=0、stderr=0** | 🔴 裸 exec 完全失效,**必須 PTY** |
| **PTY workaround** | Python `pty.openpty()` 跑 `agy -p` | **exit 0、輸出乾淨 `PONG`(純文字)** | ✅ Go `creack/pty` 可解 |
| **輸出格式** | `agy --help` | **無 `--output-format` flag** | 🔴 無 stream-json/JSON,只有純文字 → 無 token streaming |
| **session 建立** | `agy --help` | 無 caller 指定 id 的 flag | ⚠️ id 由 agy 自生 |
| **session resume** | `--conversation <id>` / `-c`(最近) | ✅ 有 | resume 可行 |
| **conversation id 取回** | `~/.gemini/antigravity-cli/conversations/<UUID>.db` | **檔名即 conversation UUID**,跑完即生成;另有 `cache/last_conversations.json` | ✅ **讀最新 .db 檔名即可,不需刮 stdout** |
| **權限旁路** | `--dangerously-skip-permissions` | ✅ 有(對應 claude `bypassPermissions`) | 但 all-or-nothing |
| **MCP** | `~/.gemini/antigravity-cli/mcp/<server>/*.json`(已有 odoo server 在用) | ✅ 檔案式設定 | ⚠️ 非 per-call flag |
| **auth** | `~/.gemini/oauth_creds.json`(OAuth)/ 或 `GEMINI_API_KEY`/`ANTIGRAVITY_API_KEY` env | ✅ 訂閱式,無需 per-call key | 同 claude-cli「訂閱式」定位 |
| **其他 flag** | `--add-dir`、`--print-timeout`(預設 5m)、`--sandbox`、`--model` | — | timeout / workspace 可控 |

**一句話結論**:agy 的 headless 介面比 claude **薄一大截**。能做到「同等級的 provider 骨架」,
但有兩處先天無法等價:**① 無 token 串流(只能 buffer-and-emit);② 無 per-call 工具閘門/hooks 安全層**。

---

## 1. claude-cli 驅動契約 → agy 對映(差異總表)

| claude-cli 依賴 | agy v1.0.5 對應 | 策略 |
|---|---|---|
| `-p`(print) | `-p`/`--print`/`--prompt` | 直用 |
| `--output-format stream-json` | ❌ 無 | 改:PTY 收純文字 → strip ANSI → 取 final answer |
| `--input-format stream-json`(多模態 stdin) | ❌ 無 | 多模態先不支援(Vision=false);圖片走 `--add-dir` 引用檔案(後續) |
| `--model <m>` | `--model <m>` | 直用 |
| `--session-id <uuid>`(自鑄) | ❌ 無 | agy 自生;goclaw 存「goclaw sessionKey → agy conversation UUID」映射 |
| `--resume <uuid>` | `--conversation <uuid>` | 從映射取 id;首輪不帶、跑完讀最新 `.db` 回填映射 |
| `--permission-mode bypassPermissions` | `--dangerously-skip-permissions`(或 `--sandbox`) | 直用 |
| `--mcp-config <path>`(per-session) | 檔案式 `~/.gemini/.../mcp/` | 改:啟動時寫 global MCP config,bridge 同 claude(見 §4) |
| `--disallowedTools ...` | ❌ 無 | **無法等價**;只能靠 `--sandbox` 全域限制 |
| `--settings <hooks.json>` | ❌ 無 | **無法等價**;deny-patterns/hooks 安全層不可移植 |
| exec + 捕捉 stdout | 🔴 #76 非 TTY 壞 | **PTY 包裝(creack/pty)** |

---

## 2. 架構決策

1. **薄代理 + PTY**:沿用 claude-cli 的 subprocess 薄代理模型,但所有 exec 改走 PTY(`github.com/creack/pty`)。
   無 PTY → agy hang+空輸出(已實測)。
2. **純文字解析,非串流**:`Capabilities.Streaming=false`。`ChatStream` 內部 buffer 完整輸出後,一次性
   經 `onChunk` 吐出(或分段吐 buffer),保持介面相容但不保證 token-by-token。strip ANSI 後取最終回應段。
3. **session 映射 by .db 檔名**:不刮 stdout 找 id。流程:
   - 首輪:不帶 `--conversation`,跑完記錄 `conversations/` 內**最新** `<UUID>.db` → 存進 `sessionMu` 旁的
     `sessionID sync.Map[goclawSessionKey]agyConvUUID`。
   - 續輪:帶 `--conversation <存的 UUID>`。
   - `/reset`:清除映射,下輪重新建 conversation。
   - 並發保護:沿用 claude-cli 的 `lockSession(sessionKey)` per-session mutex(避免同 conversation 並行)。
4. **MCP**:啟動時用既有 `BuildCLIMCPConfigData`(外部 MCP servers + GoClaw bridge),序列化成 agy 認得的
   `~/.gemini/config/...` / workspace `.agents/mcp_config.json` 格式寫入。bridge 身分注入沿用 claude 同套 Opt*。
5. **權限/安全**:預設 `--dangerously-skip-permissions`(對齊 claude bypass)或 `--sandbox`(較安全)。
   **明確標註**:claude-cli 的 shell deny-patterns + path-restriction hooks **在 agy 無對應**,這層保護消失,
   需在文件與 config 註明風險,並建議搭 `--sandbox` + 受限 workspace。

---

## 3. 檔案結構(對映 claude_cli*.go)

新增於 `internal/providers/`,命名 `agy_cli*.go`:

| 新檔 | 對映 claude-cli | 內容 / 差異 |
|------|----------------|------------|
| `agy_cli.go` | `claude_cli.go` | `AgyCLIProvider` struct、`AgyCLIOption`、`NewAgyCLIProvider`、`Name/DefaultModel/Capabilities/Close`、`lockSession`、新增 `sessionID sync.Map`、`ptyTimeout`。`Capabilities{Streaming:false, ToolCalling:true, StreamWithTools:false, Vision:false, MaxContextWindow: 1_000_000(Gemini), TokenizerID:"cl100k_base"(近似)}` |
| `agy_cli_chat.go` | `claude_cli_chat.go` | `Chat`/`ChatStream`,改用 **PTY exec**(`pty.Start(cmd)` + 讀 master 至 EOF/timeout)。`ChatStream` buffer 後吐。 |
| `agy_cli_session.go` | `claude_cli_session.go` | `buildArgs`(`-p`,`--model`,`--dangerously-skip-permissions`,`--print-timeout`,選擇性 `--conversation`,`--add-dir`);workdir 建立;**conversation id 解析**(掃 `conversations/*.db` 取 mtime 最新)。 |
| `agy_cli_parse.go` | `claude_cli_parse.go` | 解析**純文字 + ANSI**:strip ANSI/控制碼/spinner、去除工具執行 UI、抽取 final answer。(無 JSON,需啟發式;以 `--print` 的輸出尾段為主)。 |
| `agy_cli_types.go` | `claude_cli_types.go` | Opt 常數沿用(session_key/agent_id/...);agy 專屬常數。 |
| `agy_cli_auth.go` | `claude_cli_auth.go` | OAuth creds 偵測(`~/.gemini/oauth_creds.json`)或 env key 檢查。 |
| `agy_cli_mcp.go` | `claude_cli_mcp.go` | 把 `MCPConfigData` 寫成 agy 的 MCP 設定檔格式(file-based);path resolve。 |
| `agy_cli_pty.go` | (新增,claude 無) | PTY 封裝:`runWithPTY(ctx, cmd) ([]byte, error)`,含 timeout/kill/drain。 |
| `agy_cli_sanitize.go` | 部分對映 `claude_cli_deny_patterns.go` | **降級版**:因無 hooks,改提供 prompt 層警示 + workspace 限制說明;deny-patterns 改由 `--sandbox` 承擔(標註限制)。 |
| `agy_cli_*_test.go` | 對映各 `_test.go` | buildArgs / session-id 解析 / parse(ANSI strip)/ mcp 寫檔 單元測試(用假 `agy` stub script 模擬 PTY 輸出)。 |

> `claude_cli_hooks.go`(207 LOC,settings.json + 安全 hooks)**無對映** — agy 不吃 `--settings`。
> 這是「無法同等級」的最大一塊,須在 PR 與 docs 明確標示。

---

## 4. 設定與註冊接線

### 4.1 Config(`internal/config`)
新增 `cfg.Providers.AgyCLI`,對映 `ClaudeCLI`:
```
AgyCLI struct {
  CLIPath      string  // default "agy"
  Model        string  // e.g. "gemini-3-pro"
  BaseWorkDir  string
  Sandbox      bool    // true → --sandbox; false → --dangerously-skip-permissions
  PrintTimeout string  // default "5m"
}
```

### 4.2 註冊點(對映 claude-cli 三處)
- `cmd/gateway_providers.go` ~L218:新增 `if cfg.Providers.AgyCLI.CLIPath != "" { registry.Register(NewAgyCLIProvider(...)) }`
- `cmd/gateway_providers.go` ~L327:per-tenant 同樣 `RegisterForTenant`
- `internal/http/providers.go` ~L172:per-tenant HTTP 註冊
- (選擇性)`internal/tools/read_image.go` / `read_document.go` provider 優先序:agy 暫不入(Vision=false)

### 4.3 Dockerfile
claude 走 npm;agy 走官方安裝腳本。新增:
```
RUN curl -fsSL https://antigravity.google/cli/install.sh | bash   # 安裝 agy 到 ~/.local/bin
```
並確保 `PATH` 含 `~/.local/bin`;auth 走掛載的 `~/.gemini/oauth_creds.json` 或 `GEMINI_API_KEY` env。

---

## 5. 能力對照(誠實版:哪裡「同等級」、哪裡不是)

| 能力 | claude-cli | agy-cli | 等級 |
|------|-----------|---------|------|
| 薄代理 / session 續接 | ✅ | ✅(by .db 映射) | 同級 |
| MCP 工具(含 GoClaw bridge) | ✅ per-session flag | ✅ file-based | 同級(機制不同) |
| 權限旁路 | ✅ | ✅ | 同級 |
| 多租戶註冊 | ✅ | ✅ | 同級 |
| **token 串流** | ✅ stream-json | ❌ buffer-and-emit | **降級** |
| **per-call 工具閘門 `--disallowedTools`** | ✅ | ❌ | **缺** |
| **shell deny-patterns / hooks 安全層** | ✅ | ❌(只能 `--sandbox`) | **缺** |
| **多模態 / Vision** | claude 經 stream-json input | ❌(初版不做,後續 `--add-dir`) | **缺** |
| 非 TTY 穩定性 | ✅ 原生 | ⚠️ 需 PTY workaround(#76) | 有 workaround |

---

## 6. 風險清單

1. **ToS(最高)**:第三方驅動 Antigravity 違反 ToS → 封號/撤 token。非工程可解。
2. **#76 未修**:依賴 PTY workaround;若上游改了 TTY 偵測或輸出格式,parser 易碎。
3. **純文字 parser 脆弱**:無結構化輸出,final-answer 抽取靠啟發式;agy 版本升級(每日更新)可能改 UI → parser 需維護。
4. **conversation .db race**:並發跑多個 `-p` 時「最新 .db」可能抓錯;以 per-session lock + 跑前後 diff `conversations/` 目錄緩解。
5. **安全層消失**:無 hooks/deny-patterns,惡意 prompt 可能讓 agy 執行危險操作;務必 `--sandbox` + 受限 workspace + 文件警示。
6. **無 token streaming**:長回應使用者體驗退化(等完整輸出才一次出現)。

---

## 7. 里程碑

- **M1 PoC**:`agy_cli.go`+`agy_cli_pty.go`+`agy_cli_chat.go` 最小版,單輪 `-p` 經 PTY 取回純文字答案,過 `Provider` 介面。(已用 Python 驗證 PTY 路徑可行)
- **M2 Session**:`.db` 映射 + `--conversation` 續接 + `/reset`;per-session lock。
- **M3 MCP**:`agy_cli_mcp.go` 寫 MCP 設定檔 + GoClaw bridge 身分注入;接通既有 odoo MCP。
- **M4 註冊/設定/Docker**:config 結構 + 三處註冊 + Dockerfile 安裝。
- **M5 測試/文件**:單元測試(假 agy stub)+ ToS/安全限制文件 + CHANGELOG。

---

## 8. 建議

- 若需求只是「goclaw 裡要 Gemini agentic CLI」→ **不必做這個**,goclaw 既有 ACP 跑 `gemini --acp` 已合規且功能完整。
- 若需求是「Antigravity 專屬編排」→ 本計畫可交付一個**功能上接近但非全等**(無串流/無工具閘門/無 hooks 安全層)的 provider,
  且須承擔 ToS 風險。
- 動工前置條件:(1) 接受 ToS 風險的明確決定;(2) 鎖定 agy 版本(每日更新會衝擊 parser)。
