# GoClaw `claude_cli` Provider — Spec / Plan (v3.14.0)

> Verified against `esmith/main` worktree at `v3.14.0-55-g601b65d4` (merge commit `601b65d4 "Merge upstream v3.14.0 into esmith/main"`). All file:line citations re-pinned against actual code and call chains (not filenames). DB provider type constant `claude_cli` (`internal/store/provider_store.go:26`); registry name `claude-cli` (`cmd/gateway_providers.go:449`).

## 1. Context — why `claude_cli`

e-smith-hub currently runs on the **ACP** provider (`acp`, `internal/store/provider_store.go:32`), an Agent Client Protocol subprocess driven via a JS adapter. The switch target is the **Go-native `claude_cli` provider**, which shells out to the real `claude` CLI directly:

| Dimension | ACP (`acp`) | `claude_cli` |
|-----------|-------------|--------------|
| Transport to model | JS adapter → ACP JSON-RPC → subprocess | Go → `claude --print --output-format stream-json` |
| JS hop | Yes (adapter process) | **No** — pure Go subprocess management |
| Model access | Whatever the adapter pins | **Opus 4.8 via the system `claude` binary** (subscription-billed) |
| Session history | Adapter-managed | CLI-managed `.jsonl` under `~/.claude/projects/<enc>/<uuid>.jsonl` |
| Tool execution | ACP tool calls | CLI-internal + GoClaw MCP bridge (`--mcp-config`) |

Decision: switch e-smith-hub's agent rows from provider `acp` → `claude_cli` to drop the JS hop and gain Opus 4.8 through the system binary. The provider is intentionally **subscription-billed**, so dollar-cost enforcement is excluded by design (`internal/usage/caps/service.go:243` lists `ProviderClaudeCLI` among non-enforced providers).

The provider's own docstring frames the contract (`internal/providers/claude_cli.go:41-43`): *"It acts as a thin proxy: CLI manages session history, tool execution, and context. GoClaw only forwards the latest user message and streams back the response."* That thin-proxy assumption is the root of the K respawn-amnesia regression (§4).

## 2. Architecture

### 2.1 Request flow

```
agent loop (loop_pipeline_callbacks.go)
  → ChatRequest{ Messages[], Options{agent_id,user_id,channel,...} }
  → ClaudeCLIProvider.ChatStream (claude_cli_chat.go:83)
      → extractFromMessages  (claude_cli_session.go:127)  // keeps last user msg + system + images
      → buildArgs            (claude_cli_session.go:40)    // --mcp-config / --resume|--session-id / --disallowedTools / --settings
      → exec: claude --print --output-format stream-json -- <userMsg>
      → parse stream events  (claude_cli_chat.go:214)      // Usage{Prompt,Completion,Total}
```

### 2.2 Three wiring surfaces the default must enforce

1. **MCP bridge** (`--mcp-config`, `claude_cli_session.go:51-52`). Per-session config written by `WriteMCPConfig` (`claude_cli_mcp.go`); injects the `goclaw-bridge` HTTP server (`claude_cli_mcp.go:166-173`) with per-request identity headers + HMAC (`SignBridgeContext`, `claude_cli_mcp.go:163`). When `--mcp-config` is present, `buildArgs` **disables all CLI built-in tools** via `--disallowedTools Bash,Edit,Read,Write,Glob,Grep,...` (`claude_cli_session.go:72-76`) so *all* tool execution is forced through the controlled bridge.
2. **Hooks** (`--settings`, `claude_cli_session.go:79`). `BuildCLIHooksConfig` emits a PreToolUse security hook (`claude_cli_hooks.go:14`). v3.14 made deny-patterns configurable (`denyPatternSets ...[]*regexp.Regexp`, `claude_cli_hooks.go:17`; wired at `cmd/gateway_providers.go:446-447,468`).
3. **Usage parse** (`claude_cli_parse.go` + stream result event, `claude_cli_chat.go:214-220`). Maps input/output tokens into `Usage`.

### 2.3 Invariant: never fall back to no-MCP

The default chat path **must** carry `--mcp-config`. If `mcpConfigPath` is empty the CLI runs with native built-in tools un-gated (the `--disallowedTools` branch at `claude_cli_session.go:74` only fires when `mcpConfigPath != ""`). The `disableTools`/summoner path (`claude_cli_session.go:71`) is the only legitimate no-bridge mode. Wiring MUST guarantee `AgentMCPLookup` + base bridge config are always present for normal agents (`cmd/gateway_providers.go:471`).

## 3. Coverage on v3.14.0 — 13-category table (`claude_cli` vs ACP)

| # | Category | ACP | `claude_cli` v3.14 | v3.14 delta |
|---|----------|-----|--------------------|-------------|
| A | Agent loop integration | yes | **yes** | — (loop builds + compacts full history, `loop_pipeline_callbacks.go:74-84`) |
| B | Streaming output | partial | **yes** | — (`stream-json` events, `claude_cli_chat.go:214`) |
| C | Skills (search/use/manage) in bridge | partial | **partial** | runtime hardening added (`internal/skills/dep_installer.go`, `target_path_validator.go`, mig `000079`); bundled `skills/goclaw/`. Bridge still exposes only `skill_search` (`bridge_server.go:33`); `use_skill`/`skill_manage`/`publish_skill` NOT bridged → skill activations on CLI path emit **no** self-evolution metrics (`recordSkillUsage` fires only on native loop `use_skill`, `loop_pipeline_tool_callbacks.go:289`) |
| D | Image / multimodal input | partial | **yes** | `buildStreamJSONInput` (`claude_cli_session.go:228`), `--input-format stream-json` |
| E | Memory → KG | yes | **yes** | `memory_search`/`memory_get` bridged (`bridge_server.go:29-31`) |
| F | Token / cost / cache usage | partial | **partial** | pricing stack added (mig `000070-72`, `internal/usage/pricing/*`) but parser unchanged — `cliUsage` still input/output only (`claude_cli_types.go:18-21`); `cost_usd` captured (`claude_cli_types.go:13,30`) but **never** copied to `Usage` |
| G | Hook lifecycle / matcher coverage | partial | **partial** | deny-patterns now configurable + `claude_cli_hooks_test.go` (3 tests); still PreToolUse-only with Bash/Write/Edit/Read matchers (`claude_cli_hooks.go:52-72`), no PostToolUse/Stop/SessionStart; **inert on bridge chat path** because those built-ins are disabled (`claude_cli_session.go:75`) |
| H | Channel routing context | yes | **yes** | channel/chatID/peerKind/localKey threaded as headers (`claude_cli_mcp.go:128-164`) |
| I | External-MCP enforcement (ToolAllow/Deny + per-user creds) | partial | **no (partial primitives)** | v3.14 added `tool_filter.go` `IsToolAllowed` + `grant_checker.IsAllowed`, but invoked **only** on bridge/pool path (`manager_connect.go:156,249`; `bridge_tool.go:192`). CLI direct-inject path (`gateway_providers.go:213-241`) drops ToolAllow/ToolDeny and passes empty userID — **unenforced** |
| J | Identity threading (HMAC, agent-key, SenderID) | partial | **partial** | **v3.14 closed agent-key**: `WithToolAgentKey` injected in bridge middleware (`server.go:301`). HMAC hardened (localKey in signature). **SenderID still unthreaded** — no `OptSenderID`, no `X-Sender-ID`, no `BridgeContext.SenderID` |
| K | Respawn / history durability | no | **no** | `session_reset.go` (v3.14 new) is **MCP-server** HTTP reconnection (`session_reset.go:14-40`), unrelated. Provider still forwards last-user-msg only (`claude_cli_session.go:127`); relies on subprocess `.jsonl` via `--resume` |
| L | Auth / credential injection | yes | **yes** | `claude_cli_auth.go`; per-session config dir outside workDir (`mcpConfigBaseDir`) |
| M | Tool-event observability | no | **no** | bridge processes tool results internally; agent loop never sees CLI-internal tool calls (`bridge_server.go:116-117`) |

Net vs the docs/25 §12 ACP gap inventory: `claude_cli` **wins** on A/B/D/E/H and partial-wins F/G; **v3.14 closed J-agentkey**; **three blocking regressions persist** — K, J-SenderID, I.

## 4. Blocking pre-switch fixes

### 4.1 K — Respawn-amnesia (STILL NEEDED, v3.14 changed nothing)

**Root cause.** The loop builds and compacts a full `[]Message` history (`loadSessionHistory → buildMessages → compactMessages`, `loop_pipeline_callbacks.go:74-84`) and hands it to the provider; the provider discards everything but the last user message (`extractFromMessages`, `claude_cli_session.go:127-142`) and reconstructs context purely via `--resume`/`--session-id` against the subprocess `.jsonl` (`claude_cli_session.go:55-63`). On a cold/respawned subprocess the `.jsonl` is absent → compacted history is lost.

**Minimal fix (provider-side only; no loop changes):**
1. In `extractFromMessages` (`claude_cli_session.go:127`): return the full ordered `[]Message` (system already split out) instead of collapsing to last-user-msg.
2. In `ChatStream`/`Chat` (`claude_cli_chat.go:83,17`): when `sessionFileExists(...) == false` (`claude_cli_session.go:200`), seed the cold subprocess with the loop's prior turns — extend `buildStreamJSONInput` (`claude_cli_session.go:228`) to serialize prior user/assistant turns as a stream-json transcript, or flatten a text preamble before the latest user message. When the `.jsonl` exists, keep last-msg-only + `--resume` (no double-replay).
3. Gate the replay on the `sessionFileExists==false` signal so it fires exactly once per (re)spawn.

### 4.2 J — Per-sender file-writer gate (STILL NEEDED)

**Root cause.** `BridgeContext` (`claude_cli_mcp.go:86-95`) and `bridgeContextFromOpts` (`claude_cli_session.go:171-182`) have no `SenderID`. The loop populates Options without it (`loop_pipeline_callbacks.go:303-311`; `req.SenderID` is used only for events at `loop_pipeline_callbacks.go:34`). `WriteMCPConfig` emits no `X-Sender-ID` (`claude_cli_mcp.go:128-164`); `bridgeContextMiddleware` reads none (`server.go:259-266`). So `write_file`/`exec` bridge tools resolve the actor via `store.UserIDFromContext` (`bridge_tool.go:190`) = group-scoped UserID, not the individual sender. The v3.14 agent-key injection (`server.go:301`) fixed session-tool identity, **not** SenderID. The PreToolUse hook keys only on tool_name/tool_input (`claude_cli_hooks.go`) and is disabled on the bridge path anyway (`claude_cli_session.go:75`).

**Minimal fix (multi-file thread):**
1. Add `OptSenderID` const (`claude_cli.go`, alongside `OptUserID:17`) + `SenderID` field to `BridgeContext` (`claude_cli_mcp.go:86`) + wire in `bridgeContextFromOpts` (`claude_cli_session.go:171`). The consumer already has the real sender (`cmd/gateway_consumer_normal.go` effectiveSenderID / `MetaOriginSenderID`); populate `chatReq.Options[OptSenderID]` at `loop_pipeline_callbacks.go:303`.
2. Emit `X-Sender-ID` in `WriteMCPConfig` (`claude_cli_mcp.go:128`) **and fold it into the HMAC** via `SignBridgeContext` (`claude_cli_mcp.go:163,267`) so it can't be forged; extend `VerifyBridgeContext` (`claude_cli_mcp.go:284`) with a backward-compat fallback level.
3. In `bridgeContextMiddleware` (`server.go:259`) read `X-Sender-ID`, require it be HMAC-covered, inject via a new `store.WithSenderID`.
4. The file-writer/exec gate then reads `store.SenderIDFromContext` for per-sender authorization (same pattern as `memory_vault.go:47`, `skill_manage.go:50`, `sessions_send.go:116`).

### 4.3 I — External-MCP enforcement bypass (STILL NEEDED — `partial`: primitives exist, not wired)

**Root cause.** Per-agent external servers are injected **directly** into `--mcp-config`: `buildMCPServerLookup` (`gateway_providers.go:213-241`) copies `Name/Transport/Command/URL/Args/Headers/Env` into `MCPServerEntry` (`gateway_providers.go:229-238`) but drops `info.ToolAllow`/`info.ToolDeny` (the store **does** return them: `mcp_store.go:75-76` `MCPAccessInfo.ToolAllow/ToolDeny`), and calls `ListAccessible(ctx, aid, "")` with **empty userID** (`gateway_providers.go:218`) — bypassing per-user grant scoping and per-user credential resolution (the native path's `resolveServerCredentials(ctx, info, userID)`, `manager.go:357,375`, is unreachable). `MCPServerEntry` has no ToolAllow/Deny fields (`claude_cli_mcp.go:20-28`). These rows are written verbatim into `mcpServers` (`claude_cli_mcp.go:116-126`) and the `claude` subprocess connects **directly** (`claude_cli_session.go:51-52`). v3.14's `IsToolAllowed`/`grant_checker` run **only** on the bridge/pool path (`manager_connect.go:156,249`; `bridge_tool.go:192`) — `claude_cli` imports zero of `internal/mcp` (grep-confirmed).

**Two options:**
- **Option A (force-route, preferred for e-smith-hub security parity).** Stop injecting external servers into `--mcp-config`; proxy them through `goclaw-bridge` so every external tool call traverses `BridgeTool.Execute → grant_checker.IsAllowed` (`bridge_tool.go:189`) + `IsToolAllowed` (`tool_filter.go:12`), giving runtime revocation recheck. Requires building bridge proxying of non-builtin MCP tools (`bridge_server.go:20-51` currently exposes only builtins) and making `buildMCPServerLookup` stop emitting direct entries.
- **Option B (filter-at-write, lighter).** Add `ToolAllow`/`ToolDeny` to `MCPServerEntry` (`claude_cli_mcp.go:20`), carry `info.ToolAllow`/`info.ToolDeny` in `buildMCPServerLookup` (`gateway_providers.go:229`), pass real `req.UserID` to `ListAccessible` (`gateway_providers.go:218`) and resolve per-user creds before write, then append CLI-native `--allowedTools mcp__<server>__<tool>` / `--disallowedTools` derived from allow/deny (`buildArgs` already manages `--disallowedTools`, `claude_cli_session.go:69-76`). Note Option B has **no runtime revocation recheck** — for sensitive servers Option A is the correct minimal-blast-radius fix. Option B also requires changing the lookup signature `MCPServerLookup(ctx, agentID string)` (`claude_cli_mcp.go:32`) to add userID.

## 5. Follow-ups (non-blocking)

| Item | Action | Anchor |
|------|--------|--------|
| F — cache/cost tokens | Add `cache_creation_input_tokens`/`cache_read_input_tokens` to `cliUsage` (`claude_cli_types.go:18-21`); map into `Usage.CacheCreationTokens/CacheReadTokens` in `parseJSONArray`, `parseSingleJSONResult`, and stream result event (`claude_cli_chat.go:214-220`). Downstream already prices them (`pricing/decimal.go:31-32`, `tracing/cost.go:42-46`). Optionally surface `cost_usd` as informational (non-enforced) metric | `claude_cli_types.go:13,30` |
| F — analytics | Document that `claude_cli` appears in usage-event panel as **skip** events (`service.go:80-83`, reason `provider_not_billable_api`) with token-only data (cost=0, cache=0). Token counts do surface via `AccumulateTokens` (`loop_finalize.go:187`) | closed-by-design |
| M — tool-event observability | Add PostToolUse hook matchers for audit of CLI-internal tool results (currently invisible, `bridge_server.go:116-117`); or move enforcement onto bridge tools (the tools the CLI actually calls) rather than PreToolUse built-in matchers | `claude_cli_hooks.go:52` |
| C — `use_skill` in bridge | Add `use_skill` to `BridgeToolNames` (`bridge_server.go:33`) so CLI-path skill activations emit tracing + feed self-evolution metrics (`recordSkillUsage`, `loop_pipeline_tool_callbacks.go:292`); decide whether `skill_manage`/`publish_skill` should be bridge-exposed; verify bundled `skills/goclaw/` is discoverable via bridge `skill_search` index | `bridge_server.go:20-51` |
| `claude_cli_parse_test` | Add table-driven test for `parseJSONArray`/`parseSingleJSONResult` covering token mapping + new cache fields; parse.go is byte-identical to v3.12.0 (no test coverage added) | `claude_cli_parse.go` |

## 6. Migration from ACP

### 6.1 DB provider switch

Switch e-smith-hub agent rows: provider type `acp` → `claude_cli` (`internal/store/provider_store.go:32 → :26`). Registry resolves `claude_cli` (DB type) to the `claude-cli` registered provider (`cmd/gateway_providers.go:449`). Both types are in the enabled maps (`provider_store.go:76,82,217,218`).

### 6.2 Pre-switch verification

1. **MCP wiring** — confirm `AgentMCPLookup` is set (`gateway_providers.go:471`) and base bridge config present; verify no agent falls to the no-MCP path (§2.3).
2. **Identity** — bridge HMAC verifies (`server.go:280`, `VerifyBridgeContext`); agent-key injected (`server.go:301`); session tools resolve identity. **Confirm SenderID gate behavior** (currently fail-closed group-scoped — fix §4.2 before relying on per-sender writes).
3. **K replay** — smoke-test agent respawn: kill subprocess, ensure prior-turn context survives (requires §4.1 fix or accept `.jsonl`-only durability).
4. **External MCP** — if any e-smith-hub agent has sensitive external MCP servers with ToolAllow/ToolDeny or per-user creds, **do not switch until §4.3 is applied** (currently unenforced on CLI path).
5. **Usage** — verify usage-event panel shows `claude_cli` skip events with token counts (expected, not a regression).

### 6.3 Rollback

Provider switch is a DB-row change with no schema migration — revert by setting the agent rows back to `acp`. ACP provider remains registered (`gateway_providers.go:505,550`). No `.jsonl` cleanup required (CLI sessions are per-workdir and harmless if orphaned).

## 7. Affected files index

| File | Role |
|------|------|
| `internal/providers/claude_cli.go` | Provider struct + Opt* consts (`:17-40`); needs `OptSenderID` |
| `internal/providers/claude_cli_chat.go` | Chat/ChatStream; `extractFromMessages` call + usage mapping (`:214-220`) |
| `internal/providers/claude_cli_session.go` | `buildArgs` (`:40`), `extractFromMessages` (`:127`), `bridgeContextFromOpts` (`:171`), `sessionFileExists` (`:200`), `buildStreamJSONInput` (`:228`) |
| `internal/providers/claude_cli_mcp.go` | `MCPServerEntry`/`MCPServerLookup` (`:20-32`), `BridgeContext` (`:86`), `WriteMCPConfig` headers (`:128`), `SignBridgeContext`/`VerifyBridgeContext` (`:267,284`) |
| `internal/providers/claude_cli_types.go` | `cliUsage` (`:18-21`), `CostUSD` (`:13,30`) — needs cache fields |
| `internal/providers/claude_cli_parse.go` | token mapping (`:75-81,130-136`) — needs cache + test |
| `internal/providers/claude_cli_hooks.go` | `BuildCLIHooksConfig` (`:14`), `generateSettingsJSON` PreToolUse-only (`:52-72`), `hookDenyPatternStrings` (`:210`) |
| `cmd/gateway_providers.go` | `buildMCPServerLookup` (`:209-241`), `configuredShellDenyPatterns` wiring (`:446-468`), `AgentMCPLookup` (`:471`) |
| `internal/gateway/server.go` | `bridgeContextMiddleware` (`:259-345`), agent-key injection (`:301`), HMAC verify (`:280`) |
| `internal/mcp/bridge_server.go` | `BridgeToolNames` allowlist (`:20-51`); CLI-internal-tool note (`:116-117`) |
| `internal/mcp/tool_filter.go` | `IsToolAllowed` (`:12`) — bridge-path only |
| `internal/mcp/grant_checker.go` / `bridge_tool.go` | runtime grant recheck (`bridge_tool.go:189-202`) — bridge-path only |
| `internal/mcp/manager_connect.go` | `IsToolAllowed` call sites (`:156,249`) |
| `internal/mcp/session_reset.go` | MCP-server HTTP reconnection (NOT CLI history) |
| `internal/store/mcp_store.go` | `MCPAccessInfo.ToolAllow/ToolDeny` (`:75-76`) — available, unused on CLI path |
| `internal/store/provider_store.go` | provider type consts `claude_cli`/`acp` (`:26,32`) |
| `internal/usage/caps/service.go` | `ShouldEnforceProvider` skip (`:243`), Preflight short-circuit (`:78-83`) |
| `internal/usage/pricing/decimal.go` / `internal/tracing/cost.go` | cache-token pricing (already supports) |
| `internal/agent/loop_pipeline_callbacks.go` | Options population (`:303-311`); SenderID at `:34` (events only) |

## 8. Open decisions

1. I — choose Option A (force-route external MCP through goclaw-bridge for runtime grant recheck) vs Option B (filter-at-write via --allowedTools/--disallowedTools, no revocation recheck). For e-smith-hub sensitive servers Option A is recommended but requires building bridge proxying of non-builtin MCP tools (bridge_server.go:20-51 currently builtins-only).
2. K — replay transport: serialize prior turns as a stream-json transcript via buildStreamJSONInput (claude_cli_session.go:228) vs a flattened text preamble before the latest user message. Stream-json preserves role structure; text preamble is simpler but lossy.
3. F/cost — whether to surface CLI-reported cost_usd (claude_cli_types.go:13,30) as an informational, non-enforced metric distinct from the cap system, given claude_cli is subscription-billed (caps intentionally skipped, service.go:243). Decide before building the analytics panel display.
4. G — reconcile PreToolUse matchers with --disallowedTools: either document that PreToolUse only matters in the disable-MCP/summoner path (built-ins disabled on the normal bridge chat path, claude_cli_session.go:75), or move security enforcement onto the MCP-bridge tools the CLI actually calls. Also decide whether to add PostToolUse/Stop/SessionStart lifecycle hooks.
5. C — whether skill_manage/publish_skill should be bridge-exposed (bridge_server.go) so the CLI agent can author/evolve skills in-loop, or keep authoring native-only (CLI/HTTP). Adding use_skill to the bridge is needed regardless to capture self-evolution metrics on the CLI path.
6. SenderID source-of-truth — confirm the consumer field to thread (cmd/gateway_consumer_normal.go effectiveSenderID / MetaOriginSenderID) and whether the HMAC fallback levels in VerifyBridgeContext must add a new backward-compat tier for pre-SenderID sessions.

## 9. Blocking fixes (summary)

- K respawn-amnesia — provider discards loop's compacted history (extractFromMessages claude_cli_session.go:127 keeps last-user-msg only); cold/respawned subprocess loses context. v3.14 session_reset.go is MCP-server reconnection, unrelated. Provider-side-only fix: return full []Message + seed stream-json transcript when sessionFileExists==false (claude_cli_session.go:200,228).
- J per-sender file-writer gate — no OptSenderID / BridgeContext.SenderID / X-Sender-ID header; write_file/exec resolve actor as group-scoped UserID (bridge_tool.go:190), not the individual sender. v3.14 agent-key injection (server.go:301) does NOT cover SenderID. Multi-file thread: const + struct field + Options populate (loop_pipeline_callbacks.go:303) + HMAC-covered header + middleware read + store.WithSenderID.
- I external-MCP enforcement — buildMCPServerLookup (gateway_providers.go:213-241) drops info.ToolAllow/ToolDeny and passes empty userID to ListAccessible (:218); per-agent external servers are direct-injected into --mcp-config and connected directly (claude_cli_session.go:51-52), bypassing IsToolAllowed/grant_checker (bridge-path-only). claude_cli imports zero internal/mcp. Fix via Option A (force-route through bridge) or Option B (filter-at-write + per-user cred resolution).
