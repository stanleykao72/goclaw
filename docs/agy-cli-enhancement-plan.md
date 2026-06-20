# agy-cli 水管庫補強方案(參考 Hermes Agent antigravity-cli skill)

> 規劃文件,**不含實作**。目標:用 Hermes(`NousResearch/hermes-agent`)已驗證的做法補強我們的
> `internal/providers/agycli/` 水管庫。範圍仍維持「驗證過的水管,不接 Provider/config/註冊」。

## 0. Hermes 給我們的關鍵情報(來源:實際抓 repo source)

- Hermes 的 `antigravity-cli` skill **是 LLM operator 參考文件,不是程式驅動器**:記錄 agy 路徑/flags/auth/sandbox/slash 指令/驗證序列,但 **turn-completion 偵測完全沒寫**,delegate 給 codex/claude-code 姊妹 skill。
- 真正的互動驅動機制(在 `tools/process_registry.py` + `claude-code/SKILL.md`):
  1. **PTY 由 Python `ptyprocess` 配置**(背景 session),reader thread `io.Copy`-style 累積。
  2. **tmux 不在 tool 層** — 是 skill 把 `tmux new-session / send-keys / capture-pane -p -S -50` 當 shell 字串丟進通用 `terminal` tool。**capture-pane 先 render 螢幕狀態再回傳**,然後統一 `strip_ansi`(完整 ECMA-48 regex,跟我們 `StripANSI` 同級)。
  3. **turn 偵測是啟發式**:互動模式靠輪詢 `capture-pane` + `sleep N` + **肉眼找 prompt marker `❯`**(claude-code);print 模式靠 process 結束 + JSON `subtype`。**沒有程式化 idle/turn detector。**
  4. **session id 雙軌**:Hermes 自鑄 OS process id(`proc_<uuid>`)做 plumbing;**對話續接靠各 CLI 原生 `--resume`/`--continue`/`--conversation`**,Hermes 只是 shell out。
- agy 專屬情報(Hermes 文件 + 我們實測):
  - 路徑:`~/.gemini/antigravity-cli/{settings.json, keybindings.json, log/cli-*.log, conversations/, brain/, history.jsonl, plugins/}`
  - flags:`--print`/`-p`、`--print-timeout`、`--prompt`、`--prompt-interactive`/`-i`、`--continue`/`-c`、`--conversation`、`--add-dir`、`--sandbox`、`--dangerously-skip-permissions`、`--log-file`、`--version`
  - in-session slash:`/resume`(`/switch`)、`/rewind`、`/clear`、`/fork`、`/reset`、`/new`、`/model`、`/mcp`、`/permissions`、`/config`…
  - permission modes:`request-review`(預設)、`always-proceed`、`strict`、`proceed-in-sandbox`;sandbox = `settings.json.enableTerminalSandbox`(預設 false),`--sandbox`/`--dangerously-skip-permissions` 為 launch-time override
  - auth:OS keyring → 找不到則 browser Google sign-in(SSH 印 URL 貼回 code);`/logout` 清除。**Hermes 自身不需 auth agy**
  - 診斷:失敗第一站 `~/.gemini/antigravity-cli/log/cli-*.log`;`agy --version` 安全非互動,`agy version` 是互動會無 TTY 失敗

## 1. Gap 分析(Hermes 已驗證做法 ↔ 我們現況)

| 能力 | Hermes 做法(已驗證) | 我們 `agycli` 現況 | gap 嚴重度 |
|------|---------------------|-------------------|-----------|
| **agentic TUI 輸出捕捉** | `tmux capture-pane`(螢幕已 render)→ strip | 裸 PTY bytes → `parse.go` 啟發式(對抗 cursor-up/spinner 殘影) | 🔴 高 — tmux 直接繞掉我們 parser 最難的失敗模式 |
| **互動多輪** | 持久背景 PTY + stdin 寫入 + `process(poll/submit)` | 純 one-shot `--print`,master 不回寫 | 🔴 高 — 結構性缺 |
| **turn-completion 偵測** | 啟發式(輪詢截圖找 `❯` / print JSON subtype / process exit) | 只有 EOF/timeout + `looksTruncated`(事後) | 🟠 中 — 無 live turn-ready |
| **slash 指令 / 回答澄清** | stdin send-keys 進 live session | 無 stdin path | 🟠 中 — 無法回答 agy 反問 |
| **resume 續接** | CLI 原生 `--conversation`/`--continue` | snapshot-diff 只「觀察」id,沒回饋、**未驗證** | 🟠 中 — 已捕捉 id 但沒接回去 |
| **auth/health 探測** | `agy --version` + 讀 settings.json/log | 無 | 🟡 低 |
| **錯誤診斷** | 讀最新 `log/cli-*.log` | 只有 stderr | 🟡 低 |
| **typed options(model/sandbox/timeout/dirs)** | skill 組 flags | 裸 `Args []string`(易犯 argv 順序 bug) | 🟡 低 |
| **MCP 設定發現路徑** | plugins + `/mcp`(skill);codegraph 寫 `~/.gemini/config/mcp_config.json` | 我們寫 `<ws>/.agents/mcp_config.json` | 🔴 **未驗證 — 可能寫錯位置** |

## 2. 策略性 reframe

Hermes 證明:agentic TUI CLI 的正解是 **持久 PTY + tmux-render 捕捉 + 啟發式 turn 偵測 + 原生 resume**,
**不是** one-shot-print + 啟發式 scrape。我們現有水管庫是「one-shot 子集」。

兩個最高槓桿洞察:
1. **tmux capture-pane 取代「把 parse.go 做得更聰明」** — 我們 parser 的最難問題(cursor-up repaint、spinner 殘影)正是 tmux render 免費解掉的。與其繼續加 parser 啟發式,不如改用 tmux 捕捉。
2. **turn-completion 偵測是天生難題** — Hermes 是「LLM 在迴圈裡肉眼看截圖」。純程式庫(迴圈裡沒有 LLM)必須用啟發式(prompt-ready marker + idle-timeout + optional sentinel)取代那個判斷,**這是本方案最大風險點**,必須誠實對待。

## 3. 開放問題 — Phase 0 已實測解決(2026-06-19,真 agy v1.0.10 + tmux)

| # | 結論 | 證據 / 影響 |
|---|------|-----------|
| **Q1 resume** | 🔴 **`--print` 模式 resume 完全失效**。`--conversation <真實id>` 與 `-c`/`--continue` 都**不載回前文**,且**每次呼叫都新生一個 conversation db**(捕捉到的 `55fd78f3` 從未被重用;turn2/3 又生 5 個新 uuid)。2 次 attempt:1 次 ~5m timeout、1 次跑去自我探索 conversations。agy 自跑 `agy --help` 後自承沒載入舊脈絡。 | **砍掉 E4(--conversation/-c resume)**。多輪記憶**只能**靠 Phase 3 持久 live session(同一 agy TUI 行程 send-keys 多輪),不能用分開的 --print。session.go 的 snapshot-diff 降級為「觀測用」(記錄某次 run 生了哪個 db),不再宣稱可 resume。 |
| **Q2 MCP 路徑** | 🔴 **我們 mcp.go 寫錯位置**。agy 實際讀 **`~/.gemini/config/mcp_config.json`**(home 全域),top-level `"mcpServers"`,**stdio shape** `{command, args, env}`。佐證:agy 載入的 tool cache `~/.gemini/antigravity-cli/mcp/{odoo,odoo-stage38}/` 的 server 集**只**對上該檔(且最新 mtime);settings.json 的 mcpServers 與 IDE 變體(httpUrl shape)都沒被 agy-cli 載入。 | **mcp.go 修正升為 Phase 1 優先(correctness bug)**:改寫到 `~/.gemini/config/mcp_config.json`、**merge 既有 entry 不覆蓋**、stdio shape、`MkdirAll`、路徑可經 env 覆寫供測試(**測試/e2e 絕不可動使用者真檔**)。 |
| **Q3 prompt marker** | ✅ **turn 偵測解出**。idle/等待輸入 = capture-pane dump **含 `? for shortcuts` 且不含 `esc to cancel`**(兩字串互斥,footer 切換)。busy 時為 `esc to cancel`。`>` 提示符兩態都在,**不可**用。建議**連兩次 idle**(或 byte-stable)防 sub-second 抖動。**首次 untrusted workspace 有 trust gate**(「Do you trust…」預設 Yes,`send-keys Enter` 一次跳過;trust 畫面兩 marker 皆無 → 自然判 not-ready,但必須按 Enter)。footer 右側恆有 `Gemini 3.x Flash (Medium)` model label 可當 footer 定位錨。 | Phase 3 `AwaitTurn` 有了確定依據;tmux 驅動 pattern(`new-session -d -x 200 -y 50` / `send-keys` / `capture-pane -p`/`-e`)完全可行。 |

## 4. 補強項目(分階段,各標 why=Hermes 證據 / risk / dep)

### Phase 1 — 低風險打磨(無新依賴,強化現有 one-shot 路徑)
- **E1 typed RunOptions + ArgBuilder**:`Model`/`Sandbox bool`/`SkipPermissions bool`/`PrintTimeout`/`AddDirs`/`Conversation`/`Continue`,由 builder 產出正確 argv(把 `-p` string-valued 契約寫進程式,杜絕我們踩過的 argv 順序 bug)。*risk 低*。
- **E2 agy 路徑模組 + `ReadLatestLog()`**:`~/.gemini/antigravity-cli/*` 常數 + 讀最新 `log/cli-*.log` 做錯誤診斷(Hermes:失敗第一站)。*risk 低*。
- **E3 `AuthStatus()`/`Healthcheck()`**:`agy --version`(安全非互動)+ creds/keyring/settings.json 探測,鏡像 Hermes 驗證序列。*risk 低*。
- ~~**E4 resume 接線**~~ **已砍**(Phase 0 Q1 證明 --print 模式 resume 失效)。改為:`session.go` 的 snapshot-diff **降級為觀測用**(`ObserveNewConversation`,僅供 log/debug 記錄某次 run 生成的 db),doc 明載「--print 無跨輪記憶,多輪走 Phase 3 持久 session」。
- **E5-mcp(從 Phase 2 提前到 Phase 1,correctness bug)**:依 Phase 0 Q2 改寫 `mcp.go` — 目標改為 `~/.gemini/config/mcp_config.json`(經 `AgyConfigDir()` env 可覆寫供測試)、**讀現有檔 merge 不覆蓋**、stdio `{command,args,env}` shape、`MkdirAll`。**所有測試/e2e 用 temp HOME,嚴禁碰使用者真 `~/.gemini/config/mcp_config.json`**(內有 odoo/odoo-stage38)。*risk 中(merge 邏輯 + 安全)*。

### Phase 2 — tmux-render 捕捉(+1 外部依賴 tmux)
- **E5 `RunWithTmux`(或 RunWithPTY 的 capture mode)**:agy 跑在 tmux session,`capture-pane -p` 取**已 render 螢幕** → 只需簡單 ANSI-strip,繞掉 `parse.go` 的 cursor-up/spinner 弱點;保留裸 PTY 路徑當 fallback。**Hermes 已驗證的招**。*risk 中(需偵測 tmux 是否安裝;Hermes 也是 graceful fallback)*。

### Phase 3 — 互動多輪 session(結構性擴張,最大工 + 最大風險)
- **E6 `Session` 型別**:持久 PTY(保留 master 可寫)、`SendPrompt(text)`、`AwaitTurn()`(啟發式:prompt-ready marker + idle-timeout + optional sentinel)、`Capture()`、`SendSlash(cmd)`、`Close()`。解鎖多輪 + slash + 回答 agy 反問。**`AwaitTurn` 的「無 LLM 判斷下偵測 turn 結束」是核心風險**;E5 的 tmux render 讓 AwaitTurn 讀螢幕較可靠。*risk 高*。

### Phase X — 明確不做(維持範圍紀律)
- 不建完整 goclaw Provider/config/註冊(仍暫停 — agy 是 agentic 非 chat,前次決議)。
- 不把 `parse.go` 做成 TUI 螢幕重建(改用 tmux capture)。

## 5. 建議排序

1. **Phase 0(Q1/Q2/Q3 實測)= 必做且便宜**,尤其 **Q2 可能揭露我們 mcp.go 現在就寫錯路徑**(correctness bug)。先做。
2. **Phase 1 打磨**:高 CP 值、低風險,讓現有 one-shot 路徑更穩、更好用。
3. **Phase 2 tmux 捕捉**:單一最高槓桿穩健度提升(殺掉 parser 最糟失敗模式)。
4. **Phase 3 互動多輪**:只有在有**具體需要多輪/回答澄清**的用例時才做(成本與 turn-偵測風險最高)。

## 6. 誠實的前提

Hermes 的「互動驅動」本質是 **LLM 在迴圈裡肉眼判讀 tmux 截圖**;它的 antigravity-cli skill 也只是給 LLM 看的文件。
我們若要做成**純程式庫**(迴圈裡沒有 LLM),turn-completion 必須用啟發式取代那層判斷 —— 這在 Phase 3 是真風險。
若未來 agy 接線進 goclaw 時迴圈裡本來就有 LLM(agent loop),則可考慮「薄程式驅動 + LLM 判讀截圖」的混合,把 turn 判斷交回 LLM(更貼近 Hermes,風險更低)。此選擇待 Provider 接線階段再定。
