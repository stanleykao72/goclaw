# agy-cli 水管庫 (plumbing library) — LOCKED 實作規格

> 範圍決定(使用者 2026-06-19):**只做驗證過的水管,不接 Provider/config/註冊。**
> 原因:實測證明 agy `-p` 是自主 coding agent(會反問、跑工具、卡互動、空 workspace 觸發澄清),
> 不是 claude-cli 那種對任意 prompt 回乾淨 `result` 的 chat 引擎。語意前提不成立,故暫緩完整 Provider。
> 但底層 I/O 水管全部實測可行,做成獨立有測試的元件,日後 agy 行為可靠或用例確認時再接線。

## Package
- 路徑:`internal/providers/agycli/`
- 宣告:`package agycli`
- module:`github.com/nextlevelbuilder/goclaw`(import path `github.com/nextlevelbuilder/goclaw/internal/providers/agycli`)
- 依賴:stdlib + `github.com/creack/pty`(已加為 direct require,v1.1.24)。**不得** import `internal/providers`(保持水管庫零耦合、可獨立測試)。
- go 1.26.0

## 實測基準(寫進 doc.go 註解,agy v1.0.9 (originally probed on v1.0.5))
1. 非 TTY 下 `agy -p` hang 到 print-timeout 且 stdout 全空(issue #76,至 v1.0.9 未修)→ 必須 PTY。
2. PTY 下 `agy -p` 回**純文字**(無 `--output-format`、無 stream-json、無結構化 envelope)。
3. `--conversation <自鑄uuid>` **不** resume-or-create;agy 自生 conversation id,存成 `~/.gemini/antigravity-cli/conversations/<uuid>.db`。→ session 用 snapshot-diff 偵測 agy 自生 id。
4. agy 高度 agentic:即使瑣碎 prompt 也可能跑工具/反問/TIMEOUT。parser 須能容忍大量 tool UI 與「沒有乾淨答案」。

## agy argv contract(寫 RunOptions.Args 必讀)
`-p`/`--print` 是 **string-valued flag**(Go flag pkg):它**吃下一個 token 當值**。所以 prompt **必須**是 `-p` 的值。
- ✅ 正確:`["-p", prompt, "--dangerously-skip-permissions", "--model", "gemini-3-pro"]`
- ✅ 正確(等價):`["--print=" + prompt, "--dangerously-skip-permissions", ...]`
- ❌ 錯誤:`["-p", "--dangerously-skip-permissions", prompt]` → `--print` 被設成 `"--dangerously-skip-permissions"`,真正的 prompt 被丟掉(實測:hang 到 print-timeout 或回答關於該 flag 的內容)。

---

## 檔案分工與 LOCKED 簽章

### `doc.go`（Foundation 階段建立）
package doc + 上述實測基準 + 「這是未接線的水管庫」聲明。無程式邏輯。

### `types.go`（Foundation 階段建立 — 其他元件都 import 這裡的型別）
```go
package agycli

import "time"

// Confidence signals how trustworthy the extracted answer is.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"   // sentinel-delimited answer matched
	ConfidenceMedium Confidence = "medium" // clean last-block heuristic
	ConfidenceLow    Confidence = "low"    // fallback: returned ANSI-stripped full text
)

// RunOptions configures a single PTY-driven agy invocation.
type RunOptions struct {
	Workdir string // cwd for the process (agy needs an active workspace)
	// Args is the full argv after the binary. WARNING: agy's -p/--print is a
	// string-valued flag (Go flag pkg) — it consumes the NEXT token as its value,
	// so the prompt MUST be -p's value. Correct:
	// ["-p", prompt, "--dangerously-skip-permissions", "--model", "gemini-3-pro"]
	// or ["--print=" + prompt, "--dangerously-skip-permissions", ...].
	// WRONG: ["-p","--dangerously-skip-permissions", prompt] sets
	// --print="--dangerously-skip-permissions" and drops the real prompt.
	Args     []string
	Timeout  time.Duration // hard wall-clock cap; 0 => DefaultRunTimeout
	ExtraEnv []string      // KEY=VALUE pairs merged over the neutralized base env
}

// RunResult is the raw outcome of a PTY run (no parsing applied).
type RunResult struct {
	Output   []byte        // raw bytes read from the pty master (still contains ANSI/control)
	ExitCode int           // process exit code; -1 if unknown/killed
	TimedOut bool          // true if killed due to Timeout/ctx
	Duration time.Duration
}

// ParseResult is the salvaged final answer from raw agy PTY output.
type ParseResult struct {
	Content    string
	Confidence Confidence
	Truncated  bool // true when output appears cut off (timeout/EOF mid-answer)
}

// DefaultRunTimeout mirrors agy --print-timeout default (5m). agy is agentic and slow.
const DefaultRunTimeout = 5 * time.Minute
```

### `pty.go` — PTY driver（解 #76）
LOCKED:
```go
// RunWithPTY runs `binary` under a pseudo-terminal so agy (which silently drops
// stdout on a non-TTY, issue #76) believes it is attached to a terminal.
// It merges a noise-neutralizing base env (TERM=dumb,NO_COLOR=1,CI=1,PAGER=cat,
// COLUMNS=200) over os.Environ(), then opts.ExtraEnv over that. Reads the pty
// master until EOF / ctx cancel / opts.Timeout, then kills the process group and
// drains. Never blocks indefinitely. Returns the raw (unparsed) bytes.
func RunWithPTY(ctx context.Context, binary string, opts RunOptions) (RunResult, error)
```
要求:用 `pty.Start(cmd)`;`cmd.Dir=opts.Workdir`;timeout 用 `context.WithTimeout` 包 ctx;kill 後 `cmd.Wait()` 有界(WaitDelay);關閉 pty fd 不洩漏;TimedOut 正確標記;讀取迴圈用 buffer 累積。binary 不存在 → 回 error。

### `parse.go` — 純文字 salvage parser
LOCKED:
```go
// ParseAgyOutput salvages the most plausible final-answer text from raw agy PTY
// output. Never panics. Pipeline: CR-overwrite collapse -> ANSI/OSC/control strip
// -> spinner/box/tool-UI line denylist -> sentinel-or-last-block answer location
// -> cleanup. Empty result falls back to the ANSI-stripped full text (ConfidenceLow).
func ParseAgyOutput(raw []byte) ParseResult

// StripANSI removes CSI/OSC/2-byte ESC sequences and lone control chars (keeps \n,\t).
func StripANSI(raw []byte) []byte
```
要求:
- Stage1:`\r\n`→`\n`;裸 `\r` 重置當前邏輯行(後寫覆蓋,殺 spinner repaint)。
- Stage2:CSI `ESC[...@-~`、OSC `ESC]...(BEL|ESC\)`(保留 OSC-8 內可見文字)、`ESC(`/`ESC=`/`ESC>`、孤立控制字元(留 `\n`/`\t`);**零寬取代**(`foo\x1b[0mbar`→`foobar`)。rune-safe(用 `\x{2800}-\x{28FF}` 等 unicode class,非 byte range)。
- Stage3:整行 denylist(維護成版本標註的常數區塊,鏡像 `claude_cli_deny_patterns.go` 做法):純 Braille `[\x{2800}-\x{28FF}]` / `[|/\-\\]` spinner 行、box-drawing `[\x{2500}-\x{257F}]` 主導行、tool 狀態前綴(`✓ `/`✗ `/`⏺ `/`● `/`⎿ `/`Running…`/`Tool:`/`(1.2s)` 類)、`^\s*\d+%` 進度行。**保守**:只在「符合 noise pattern 且幾無 alpha 答案內容」才丟;絕不因短就丟;code fence 內不丟。
- Stage4:① 若有 sentinel `<<<AGY_ANSWER_BEGIN>>>`…`<<<AGY_ANSWER_END>>>` → 取中間,ConfidenceHigh。② 否則取最後一段連續非 noise 文字塊(最後一個 tool/spinner 區塊之後),ConfidenceMedium。
- Stage5:壓縮 3+ 空行為 1、TrimSpace、去尾端 prompt echo。空 → 回 Stage2 全文 + ConfidenceLow。
- 失敗模式守則見 contracts 文件 §Part2(ANSI 跨界、答案含表格字元、cursor-up repaint、prompt echo、TUI 字串漂移、locale/UTF-8)。

### `session.go` — snapshot-diff conversation 捕捉
LOCKED:
```go
// ConversationsDir returns agy's conversation store dir.
// Default ~/.gemini/antigravity-cli/conversations; override via env AGY_CONVERSATIONS_DIR (tests).
func ConversationsDir() string

// SnapshotConversations returns the set of conversation UUIDs (<uuid>.db basenames) currently on disk.
func SnapshotConversations(dir string) (map[string]struct{}, error)

// DetectNewConversation diffs two snapshots. If exactly one new id appeared, returns (id, true).
// If multiple appeared, returns the newest by file mtime with ok=true (best-effort, logs ambiguity to caller via bool=false only when none).
// Returns ("", false) if none appeared.
func DetectNewConversation(beforeDir string, before map[string]struct{}, after map[string]struct{}) (id string, ok bool)
```
要求:`<uuid>.db` 檔名解析;UUID 格式驗證(過濾雜檔);多個新檔時用 mtime 取最新;純檔案系統、可用 temp dir + AGY_CONVERSATIONS_DIR 測試。

### `mcp.go` — agy MCP 設定檔轉譯
LOCKED:
```go
// AgyMCPConfigPath returns the workspace MCP config path agy discovers.
func AgyMCPConfigPath(workspace string) string // <workspace>/.agents/mcp_config.json

// WriteAgyMCPConfig atomically writes {"mcpServers": mcpServers} to AgyMCPConfigPath.
// Dir 0700, file 0600, temp+rename, skip-write if byte-identical. Returns the path
// ("" if mcpServers empty). The caller is responsible for building the goclaw-bridge
// entry + signed X-* headers via the existing providers.SignBridgeContext (NOT done
// here — this library stays decoupled from package providers).
func WriteAgyMCPConfig(workspace string, mcpServers map[string]any) (string, error)
```
要求:`json.MarshalIndent`;atomic temp(`.tmp`)+`os.Rename`;skip-if-unchanged;空 map → 回 `("",nil)`;mkdir `.agents` 0700。

---

## 測試要求（每元件一個 *_test.go，white-box `package agycli`）
- `parse_test.go`:**table-driven golden**。固定樣本(testdata fixtures,含 ESC bytes):clean(TERM=dumb)、full-ANSI、spinner-heavy、tool-panel-before-answer、truncated、answer-contains-table-chars、sentinel-wrapped。斷言 Content + Confidence。純函式,不跑 binary。
- `session_test.go`:temp dir + 假 `<uuid>.db` 檔,測 snapshot/detect(單新/多新取最新/無新);UUID 過濾雜檔。
- `mcp_test.go`:temp workspace,測寫入路徑/JSON shape/atomic/skip-if-unchanged/空 map。
- `pty_test.go`:單元用**假命令**(寫一個 printf 的 shell stub 或用 `/bin/echo`)驗 RunWithPTY 從 pty 收到輸出、timeout 行為(用 `sleep` stub)、binary 不存在回 error。**不**在單元測試跑真 agy。
- `e2e_test.go`:真 agy 煙測,`if os.Getenv("AGY_E2E")==""{ t.Skip(...) }`。在 temp workspace 跑 `agy -p "Reply with exactly: PONG" --dangerously-skip-permissions`(注意 argv 順序:`-p` 是 string-valued,prompt 必須緊跟其後當值),經 RunWithPTY + ParseAgyOutput 取回非空、snapshot-diff 抓到新 .db。CI 無 auth 時自動 skip。

## 綠燈門檻（Integrate 階段保證）
- `gofmt -l` 無輸出(全格式化)
- `go vet ./internal/providers/agycli/...` 乾淨
- `go build ./internal/providers/agycli/...` exit 0
- `go test ./internal/providers/agycli/...` exit 0(e2e 自動 skip)
