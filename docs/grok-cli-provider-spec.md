# Grok CLI Provider — Spec & Task List

> Status: **Draft / in progress**
> Base: `esmith/main` (0 behind main, +91 esmith commits, compiles green)
> Branch: `grok-cli-provider` (worktree `~/Documents/goclaw-grok`)
> Precedents in-repo:
> - `internal/providers/claude_cli*.go` — subprocess Provider, subscription auth (structural template)
> - `internal/plugins/esmith-ocr/vision_runner.go` — **already shells out to the grok CLI** (invocation template)

## 1. Goal

Add a `grok_cli` provider that drives the local **Grok Build CLI** (`grok`, in `cmux.app`)
as a subprocess, mirroring `claude_cli`. Routes agent turns to Grok via the user's
**grok.com subscription** (no xAI API key). CLI manages session/tools/context; GoClaw
forwards the latest prompt and streams back text + thinking.

Distinct from the existing **`xai` HTTP provider** (`cmd/gateway_providers.go:94`,
OpenAI-compatible, needs `GOCLAW_XAI_API_KEY`). `xai` = Grok model API; this = Grok CLI subprocess.

## 2. Phase 0 — empirical findings (probed on this mac)

`grok`: `/Applications/cmux.app/Contents/Resources/bin/grok` · logged in with grok.com ·
default model `grok-4.5` · subscription (no API key).

### 2.1 Single-turn (`--output-format json`)
```
grok -p "<prompt>" --output-format json --disable-web-search
```
→ single JSON object:
```json
{ "text":"PONG", "stopReason":"EndTurn", "sessionId":"019f…", "thought":"…",
  "usage":{"input_tokens":13393,"cache_read_input_tokens":1024,
           "output_tokens":35,"reasoning_tokens":29,"total_tokens":14452} }
```
Mapping → `ChatResponse`: `text`→Content · `thought`→thinking · `stopReason=="EndTurn"`→FinishReason `"stop"` (else `"error"`) · `usage.*`→Usage.

### 2.2 Streaming (`--output-format streaming-json`)
Line-delimited JSON, exactly three event types:
```json
{"type":"thought","data":"…"}   // → StreamChunk{Thinking}
{"type":"text","data":"…"}      // → StreamChunk{Content}, accumulate
{"type":"end","stopReason":"EndTurn","sessionId":"…","usage":{…}}  // → final ChatResponse
```
Simpler than claude stream-json (no message-envelope nesting).

### 2.3 Tool use is internalized
A tool-forcing prompt with permission bypass still emits **only** `thought`/`text`/`end` —
no `tool_use`/`tool_result` stream events. The CLI executes tools internally (same
"thin proxy" model as `claude_cli`). → tool/MCP = pass flags, NOT parse tool events.

### 2.4 Proven invocation (from `vision_runner.go:211`, already in esmith/main)
```go
// grok -p <prompt> --permission-mode bypassPermissions --output-format plain [--model m]
args := []string{"-p", prompt, "--permission-mode", "bypassPermissions", "--output-format", "plain"}
args = appendModel(args, "--model", cfg.Model)  // omit flag when model==""
cmd := exec.CommandContext(ctx, cfg.Bin, spec.Args...); out, _ := cmd.Output()
```
Confirms: `-p` single-turn, `--permission-mode bypassPermissions` (claude-compat alias,
accepted though not in `--help`), `--model` optional. For the provider swap
`--output-format plain` → `json` (Chat) / `streaming-json` (ChatStream).

### 2.5 CLI surface (`grok --help`)
| Need | Flag |
|---|---|
| single-turn headless | `-p, --single <PROMPT>` |
| output format | `--output-format plain\|json\|streaming-json` |
| structured output | `--json-schema <SCHEMA>` (implies json) |
| session (fixed UUID) | `-s, --session-id <UUID>` (valid UUID, must not pre-exist) |
| resume | `-r, --resume [<ID>]`, `-c, --continue`, `--fork-session` |
| working dir | `--cwd <CWD>` |
| permissions | `--permission-mode bypassPermissions` (proven) / `--allow` / `--deny` / `--always-approve` |
| disable tools | `--disallowed-tools`, `--disable-web-search` |
| system prompt / agent | `--agent <NAME\|file>`, `--agents <JSON>` |
| MCP | `grok mcp …` |

## 3. Design

Mirror `claude_cli` structurally (Provider + CapabilitiesAware + io.Closer, subprocess,
no HTTP adapter). Lift the grok argv from `vision_runner.go` §2.4.

Simplifications vs claude_cli:
- No image stream-json stdin path (defer vision).
- Session ID: reuse `deriveSessionUUID(sessionKey)` from `claude_cli_session.go` → `--session-id`; `--resume` on later turns.
- Permissions: start with `--permission-mode bypassPermissions` (proven), workspace confinement via `--cwd` + security hooks parity later.

## 4. Task list

### Provider files (`internal/providers/`)
- [ ] **T1** `grok_cli.go` — `GrokCLIProvider` struct, `NewGrokCLIProvider(cliPath, opts…)`, options (`WithGrokCLIName/Model/WorkDir/PermMode`), `Name/DefaultModel`(`grok-4.5`)`/Capabilities`(Streaming+ToolCalling+Thinking; Vision false)`/Close/lockSession`. Adapt `claude_cli.go`.
- [ ] **T2** `grok_cli_chat.go` — `Chat` (`--output-format json`) + `ChatStream` (`--output-format streaming-json`, scanner over `thought`/`text`/`end`). Adapt `claude_cli_chat.go`, minus image stdin. Argv per §2.4.
- [ ] **T3** `grok_cli_parse.go` — `grokJSONResponse` + `parseGrokJSONResponse`; `grokStreamEvent{Type,Data,StopReason,SessionId,Usage}` + mappers.
- [ ] **T4** `grok_cli_test.go` — table tests: parse (§2.1/§2.2 fixtures), argv builder, session UUID reuse.

### Wiring (mirror every `ProviderClaudeCLI` touch point)
- [ ] **T5** `internal/store/provider_store.go` — `ProviderGrokCLI = "grok_cli"` + both validity maps.
- [ ] **T6** `internal/config/config_channels.go` — `GrokCLIConfig{CLIPath,Model,PermMode,BaseWorkDir}`, `GrokCLI` field `json:"grok_cli"`, `HasAnyProvider()` clause.
- [ ] **T7** `internal/config/config_load.go` — env `GOCLAW_GROK_CLI_PATH/_MODEL/_WORK_DIR`.
- [ ] **T8** `cmd/gateway_providers.go` — (a) startup: `if cfg.Providers.GrokCLI.CLIPath != "" {…Register}`; (b) DB path: `case store.ProviderGrokCLI:` + binary-exists check + `RegisterForTenant`.
- [ ] **T9** `internal/http/providers.go` `registerInMemory` — `ProviderGrokCLI` case before the API-key guard (mirror ClaudeCLI block).
- [ ] **T10** `internal/http/provider_models.go` — `grokCLIModels()` + `ProviderGrokCLI` branch.
- [ ] **T11** (opt) `internal/http/provider_verify.go` — verify branch + `grok-cli/auth-status` (shell `grok models`).

### Docs
- [ ] **T12** schema_profile: reuse/verify `xai` profile applies to `--json-schema` (defer if unused).
- [ ] **T13** `docs/02-providers.md` + `.env` sample: `GOCLAW_GROK_CLI_PATH`.

## 5. Phasing
1. **MVP** (T1-T5, T8a, T9): sync `Chat` + registration → agent bound to `grok-cli` replies via subscription.
2. **Streaming** (T2 stream, T4).
3. **Sessions** (`--session-id`/`--resume`).
4. **Tools/MCP + security** (T6 perm, T11, T12).

## 6. Open questions (resolve in P0 probes)
- **System prompt**: no CLAUDE.md equivalent confirmed. Options: `--agent <file>` gen def, or prepend to prompt. Needs probe.
- **`--session-id` stability**: caller-supplied UUID for new session + `--resume` continuation. Needs probe.
- **Deny-pattern parity**: grok `--deny` grammar vs claude hook patterns — may need translation or `--disallowed-tools` + restricted `--allow`.
- Cost (`total_cost_usd`) surfacing; per-session dir locking (reuse `lockSession`); binary PATH discovery.

## 7. Test plan
- Unit: parse fixtures (§2.1/§2.2), argv builder, session UUID (T4).
- Integration (manual, logged-in): MVP prompt through gateway; streaming smoke; multi-turn continuity.
- Regression: `go build ./...` + `go test ./internal/providers/... ./internal/config/... ./cmd/...` green.

## 8. Non-goals (this pass)
Vision/image input, `--best-of-n`, `--check`, worktree mode, `--json-schema` structured-output plumbing beyond profile, plugin/marketplace.
