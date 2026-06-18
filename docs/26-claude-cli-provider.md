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

1. **I — RESOLVED (2026-06-18): Option A** (force-route external MCP through the goclaw-bridge for runtime grant recheck). e-smith-hub uses odoo-prod (sensitive, per-user). Requires building the bridge dynamic-proxy substrate (i-3a) — full design in §10.
2. K — replay transport: serialize prior turns as a stream-json transcript via buildStreamJSONInput (claude_cli_session.go:228) vs a flattened text preamble before the latest user message. Stream-json preserves role structure; text preamble is simpler but lossy.
3. F/cost — whether to surface CLI-reported cost_usd (claude_cli_types.go:13,30) as an informational, non-enforced metric distinct from the cap system, given claude_cli is subscription-billed (caps intentionally skipped, service.go:243). Decide before building the analytics panel display.
4. G — reconcile PreToolUse matchers with --disallowedTools: either document that PreToolUse only matters in the disable-MCP/summoner path (built-ins disabled on the normal bridge chat path, claude_cli_session.go:75), or move security enforcement onto the MCP-bridge tools the CLI actually calls. Also decide whether to add PostToolUse/Stop/SessionStart lifecycle hooks.
5. C — whether skill_manage/publish_skill should be bridge-exposed (bridge_server.go) so the CLI agent can author/evolve skills in-loop, or keep authoring native-only (CLI/HTTP). Adding use_skill to the bridge is needed regardless to capture self-evolution metrics on the CLI path.
6. SenderID source-of-truth — confirm the consumer field to thread (cmd/gateway_consumer_normal.go effectiveSenderID / MetaOriginSenderID) and whether the HMAC fallback levels in VerifyBridgeContext must add a new backward-compat tier for pre-SenderID sessions.

## 9. Blocking fixes (summary)

- K respawn-amnesia — provider discards loop's compacted history (extractFromMessages claude_cli_session.go:127 keeps last-user-msg only); cold/respawned subprocess loses context. v3.14 session_reset.go is MCP-server reconnection, unrelated. Provider-side-only fix: return full []Message + seed stream-json transcript when sessionFileExists==false (claude_cli_session.go:200,228).
- J per-sender file-writer gate — no OptSenderID / BridgeContext.SenderID / X-Sender-ID header; write_file/exec resolve actor as group-scoped UserID (bridge_tool.go:190), not the individual sender. v3.14 agent-key injection (server.go:301) does NOT cover SenderID. Multi-file thread: const + struct field + Options populate (loop_pipeline_callbacks.go:303) + HMAC-covered header + middleware read + store.WithSenderID.
- I external-MCP enforcement — buildMCPServerLookup (gateway_providers.go:213-241) drops info.ToolAllow/ToolDeny and passes empty userID to ListAccessible (:218); per-agent external servers are direct-injected into --mcp-config and connected directly (claude_cli_session.go:51-52), bypassing IsToolAllowed/grant_checker (bridge-path-only). claude_cli imports zero internal/mcp. Fix via Option A (force-route through bridge) or Option B (filter-at-write + per-user cred resolution).


## 10. Fix I — Option A bridge dynamic-proxy substrate design (i-3a / i-3b)

> Decision (2026-06-18): Fix I = **Option A** — force-route per-agent external MCP servers THROUGH the goclaw-bridge so grant recheck + per-user creds apply on every call. Design verified against mcp-go v0.44.0 internals. Implementation gated by the §10.7 security checklist ([ESC:arch] + [ESC:sec], opus + odoo-security-reviewer).

### 10.1 Recommended design

SYNTHESIS = "Candidate A's request-local-map isolation" + "Candidate B's mcp-go-native dispatch" — grounded on the FACT (verified in mcp-go v0.44.0) that Candidate A's load-bearing premise is false and Candidate B's global-union premise is unsafe.

CORE: NewBridgeServer gains deps (mcpStore store.MCPServerStore, pool *Pool, grantChecker GrantChecker). The builtin AddTool loop over BridgeToolNames stays EXACTLY as today (bridge_server.go:64-75) — these are shared, no per-user state. For EXTERNAL per-agent MCP tools, do NOT touch the global registry and do NOT build a JSON-RPC shim. Use mcp-go's documented stateless extension point: NewStreamableHTTPServer(srv, WithStateLess(true), WithHTTPContextFunc(seedExternalTools)).

PER-REQUEST FLOW (seedExternalTools): derive agentID/userID/tenantID from r.Context() (already populated by bridgeContextMiddleware at server.go:290/311/317) → call a NEW shared helper resolveExternalBridgeTools that LIFTS the proven getUserMCPTools body (loop_mcp_user.go:115-220) into internal/mcp (it currently lives on *Loop in internal/agent and cannot be imported): ListAccessible(ctx, agentID, userID) (mcp_store.go:110) → per enabled accessible server resolve creds, choose pool.Acquire vs AcquireUser by hasUserCreds, immediately Release/ReleaseUser (loop_mcp_user.go:178-181), filter via IsToolAllowed(info.ToolAllow, info.ToolDeny) (tool_filter.go), build NewBridgeTool(...serverID, grantChecker) (bridge_tool.go:65). Convert each *BridgeTool to an mcp-go ServerTool whose Handler calls bt.Execute (which already does grantChecker.IsAllowed recheck at bridge_tool.go:189-203 + clientPtr.Load + CallTool + 401 connected=false flip).

CRITICAL: external tools must reach handleListTools (server.go:1364-1394) AND handleToolCall (server.go:1438-1448) — both consult SessionWithTools.GetSessionTools() — via a PER-REQUEST-UNIQUE session identity, NOT the shared sessionToolsStore keyed by the client-supplied X-Mcp-Session-Id (the sessionID="" clobber hazard: StatelessSessionIdManager.Generate()="" at streamable_http.go:1327, header-read at :343, store keyed by id at :957-975). Plus request-end cleanup of the store entry (stateless ephemeral path never deletes → unbounded growth, streamable_http.go:695 only on terminate).

NET: external tools live ONLY in a per-request session-tool map keyed by a unique id, never in process-global s.tools, never on a clobberable key. tools/list and tools/call both dispatch natively via mcp-go (no hand-rolled router — answers A's complexity cost), scoped intrinsically to THIS agent's ListAccessible∩allow set (no tenant union — answers B's blocker), zero shared-registry mutation (answers concurrency, matches loop_mcp_user.go:183-190 cross-user-leak constraint).

### 10.2 Why (rejected alternatives)

1. Reject Candidate A's SHIM, keep its ISOLATION. Verified in mcp-go v0.44.0: handleListTools (server.go:1364-1394) and handleToolCall (server.go:1438-1448) BOTH consult ClientSessionFromContext→SessionWithTools.GetSessionTools() before/alongside global s.tools; handlePost creates an ephemeral *streamableHttpSession per request (streamable_http.go:379) bound via WithContext (:383); *streamableHttpSession implements SessionWithTools with SetSessionTools (:1076). So A's claim "stateless mode cannot inject per-request tools / unavailable without RegisterSession" is factually wrong — its JSON-RPC shim is unnecessary build cost + upgrade-fragility. A's request-local-map isolation IS correct; we keep it.

2. Reject Candidate B's GLOBAL UNION, keep NATIVE DISPATCH. registeredName derives from the non-tenant-qualified server name (bridge_tool.go ensureMCPPrefix) but uniqueness is only UNIQUE(tenant_id,name) — two tenants collide on the same s.tools key with AddTools last-write-wins (server.go:704), so an allowed tools/call can resolve another tenant's captured serverID. That is a routing/isolation break, not a mild infoleak. Also verified: handleToolCall invokes ZERO filter (server.go:1438-1448), so WithToolFilter only scopes tools/list — a union exposes every tenant's tool name to direct invocation. We DROP the union and surface per-request scoped to (ctx tenant, agent grants), which is what corrected-B converges to and what A's per-request map already gives.

3. Reject Candidate C wholesale. Verified manager.go:369: LoadForAgent DEFERS requireUserCreds servers (continue, no connect) into userCredServers — odoo-prod/Notion/Bitrix (the whole target class) are NEVER registered into the clone. C's "ALL external tools registered / near-zero new logic" is false; C must still reimpl

### 10.3 i-3a — substrate implementation outline

1. STEP 1 — Lift the per-user resolution body into internal/mcp. getUserMCPTools is a *Loop method (internal/agent/loop_mcp_user.go:91-235) not importable by internal/mcp. Extract its inner per-server loop (loop_mcp_user.go:115-220) into a free helper: func ResolveExternalBridgeTools(ctx, store MCPServerStore, pool *Pool, gc GrantChecker, tenantID uuid.UUID, agentID uuid.UUID, userID string) ([]*BridgeTool, error). Reuse ListAccessible (mcp_store.go:110), ParseJSONBytesToStringSlice/Map (internal/mcp), Acquire/AcquireUser + Release/ReleaseUser (pool.go), IsToolAllowed (tool_filter.go), NewBridgeTool (bridge_tool.go:65), ParseToolHints. Refactor Loop.getUserMCPTools to call this shared helper (single source of truth, prevents drift).
2. STEP 2 — DECIDE credential source of truth [ESC:sec]. Inline getUserMCPTools (loop_mcp_user.go:122-145) drops context-scoped creds; manager.resolveServerCredentials (manager.go:221-326) handles them (ChannelContextScopeChainFromContext, manager.go:229). bridgeContextMiddleware (server.go:251-348) never injects ChannelContextScope, so context creds are unreachable on the bridge regardless. RECOMMENDATION: replicate resolveServerCredentials merge ORDER (server APIKey → contextCreds → userCreds, manager.go:258-308) and hasUserCreds→AcquireUser decision (manager.go:310-336) EXACTLY, so the AcquireUser decision is provably identical to the path that mints per-user sessions; document the context-creds gap.
3. STEP 3 — Change NewBridgeServer signature (bridge_server.go:58): add (mcpStore store.MCPServerStore, pool *Pool, grantChecker GrantChecker). Keep the builtin BridgeToolNames AddTool loop (bridge_server.go:64-75) unchanged.
4. STEP 4 — Add WithHTTPContextFunc seeding (bridge_server.go:79-81): NewStreamableHTTPServer(srv, WithStateLess(true), WithHTTPContextFunc(seedExternalTools)). seedExternalTools(ctx, r) derives agentID/userID/tenantID from r.Context(), calls ResolveExternalBridgeTools, converts each *BridgeTool to mcpserver.ServerTool{Tool: convertBridgeToMCPTool(bt), Handler: closure over bt.Execute}.
5. STEP 5 — FIX the sessionID clobber + leak (P0). Route external ServerTools to handleListTools+handleToolCall via a per-request-UNIQUE session identity, NOT the shared sessionToolsStore keyed by client X-Mcp-Session-Id (streamable_http.go:343,957-975, Generate()="" at 1327). Apply-time options: (a) custom SessionIdManager whose non-initialize id = hash(agentID|userID|requestNonce) so each concurrent request gets a distinct store key; (b) if not overridable post-construction, attach the external map to ctx + ctx-scoped ToolFilterFunc (WithToolFilter, server.go:317) for tools/list AND SetSessionTools on the ephemeral session so handleToolCall (server.go:1438-1448) finds them. Whichever path, ADD request-end cleanup deleting the per-request entry (ephemeral path never calls sessionTools.delete → unbounded growth, streamable_http.go:695 fires only on terminate).
6. STEP 6 — convertBridgeToMCPTool(bt *BridgeTool) mcpgo.Tool: marshal bt inputSchema/description into mcpgo.NewToolWithRawSchema(bt.RegisteredName(), bt.Description(), schema). Expose minimal accessors on BridgeTool if needed.
7. STEP 7 — Wire deps at call site (internal/gateway/server.go:219): NewBridgeServer(s.tools, "1.0.0", s.msgBus, s.mcpStore, s.mcpPool, s.mcpGrantChecker). If these aren't on gateway Server, thread from the same deps that build the agent-loop manager (resolver.go:316-323 uses deps.MCPStore/MCPPool/MCPGrantChecker).
8. STEP 8 — Latency/availability guard. In ResolveExternalBridgeTools a cold/unreachable server must NOT stall tools/list: cap per-server Acquire with UserAcquireTimeout (pool.go), on timeout/err SKIP that server (log + continue, mirroring loop_mcp_user.go:171-176). Optional short per-(agent,user) TTL cache (sync.Map) to avoid ListAccessible+Acquire every list; keep TTL short so revokes honored.
9. STEP 9 — 401 self-heal owner. Bridge rebuilds per request so it inherits purge IF ResolveExternalBridgeTools' Acquire-fail branch replicates isUnauthorized401 → DeleteUserCredentials (loop_mcp_user.go:158-167). Include that branch in the lifted helper so expired tokens trigger re-onboarding, not a permanent failure.
10. STEP 10 — Tests: unit-test ResolveExternalBridgeTools (allow/deny filter, hasUserCreds→AcquireUser, 401 purge, skip-on-cold-server); concurrency test two simultaneous fake-agent requests assert NO session-tool clobber + store cleanup; integration test tools/list returns only the agent's granted external tools and tools/call routes to the right serverID with grant recheck.

### 10.4 i-3b — force-route wiring (remove the enforcement bypass)

1. STEP A — Delete the enforcement-bypassing direct-inject: internal/providers/claude_cli_mcp.go:116-126, REMOVE the `if d.AgentMCPLookup != nil && agentID != "" { for _, srv := range d.AgentMCPLookup(...) { servers[srv.Name] = mcpServerEntryToConfig(srv) } }` block. This writes per-agent external servers directly into the CLI --mcp-config, bypassing ALL grant recheck + per-user cred resolution. After removal, the only non-builtin entry is the single 'goclaw-bridge' http entry (claude_cli_mcp.go:166-173).
2. STEP B — Remove the lookup plumbing so external tools can ONLY arrive via the bridge: delete MCPServerLookup type + AgentMCPLookup usage at internal/providers/claude_cli_mcp.go:30-40, cmd/gateway_providers.go:207-242 (buildMCPServerLookup) + :471 assignment, internal/http/providers.go:39/67-69/241-242 (field, SetMCPServerLookup, assignment). Keep mcpServerEntryToConfig only if still referenced by static d.Servers.
3. STEP C — Confirm the bridge entry still carries the HMAC-signed context (claude_cli_mcp.go:160-164 SignBridgeContext) so seedExternalTools can trust agentID/userID/tenantID. The CLI now connects to exactly ONE http MCP server (the bridge), which surfaces builtin + per-agent external tools, all gated by grantChecker.IsAllowed + per-user creds.
4. STEP D — fix-J coupling. Today bridgeContextMiddleware injects X-User-ID as-is (server.go:311) and the signed BridgeContext has NO sender field (SignBridgeContext args, claude_cli_mcp.go:267). For per-user-cred servers keyed by SenderID (resolveActorUserID, loop_mcp_user.go:71-85) the bridge must resolve creds by the ACTOR id. Add X-Sender-ID to the SIGNED BridgeContext (Sign/Verify extra fields) + middleware injection, and have ResolveExternalBridgeTools call resolveActorUserID(userID, senderID, peerKind, channelType) before GetUserCredentials. i-3b removal must NOT merge before the bridge can resolve the right user, or odoo-prod silently disappears for group/merged-contact users.
5. STEP E — Verify no other writer injects external MCP into the CLI config (grep --mcp-config / mcpServers / writeMCPConfig callers). Add a regression test asserting the written mcp-config contains ONLY goclaw-bridge (+ static d.Servers), never per-agent DB servers.

### 10.5 Open risks

- P0 mechanism uncertainty (STEP 5): whether mcp-go v0.44.0 lets us override SessionIdManagerResolver on an already-constructed StreamableHTTPServer to force per-request-unique ids. WithStateLess(true) hard-wires StatelessSessionIdManager (streamable_http.go:49/61/75). If not overridable, fall back to ctx-scoped ToolFilter (list) + ensure handleToolCall sees external tools — needs an apply-time spike. Until resolved, the cross-user clobber is latent P0.
- sessionToolsStore unbounded growth: stateless ephemeral path never deletes per-request entries (streamable_http.go:695 only on terminate). Per-request seeding MUST add explicit cleanup or the map leaks under sustained multi-user load.
- Credential-path divergence (STEP 2): inline getUserMCPTools (loop_mcp_user.go:122-145) drops context creds; resolveServerCredentials (manager.go:221-326) changes hasUserCreds semantics. Must prove the chosen merge order + AcquireUser decision are byte-for-byte equivalent to the per-user-session path, or risk serving one user on another's authenticated connection.
- fix-J ordering hazard (i-3b STEP D): removing AgentMCPLookup before the bridge resolves creds by the real actor id (SenderID) makes per-user servers vanish for group/merged-contact users. i-3b removal + SenderID-carry must land together or behind a flag.
- tools/list latency / cold-server availability: per-request ListAccessible + Acquire can stall on a flaky external server; STEP 8 mitigations add their own staleness surface (revoke/cred rotation served stale until TTL).
- 401 purge fidelity (STEP 9): omitting the DeleteUserCredentials-on-401 branch yields a permanent failure with no re-onboarding until pool idle-evicts (~15m).
- Pool refCount discipline: per-request Acquire/Release must be matched on ALL paths (timeout, grant-deny early return, panic) or refCount leaks pin connections and exhaust MaxUserConns (pool.go).
- search-mode / >threshold parity NOT replicated: getUserMCPTools path does not enter search mode; an agent with very many external tools will advertise all in tools/list with no deferral. Out of scope for i-3a but note for parity.

### 10.6 Critical coupling: J-before-I

i-3b removes `AgentMCPLookup` (the direct-inject of external servers). Per-user servers (odoo-prod) are keyed by the ACTOR id via `resolveActorUserID` (SenderID for group/merged-contact users, `loop_mcp_user.go:71-85`). So the bridge must carry **X-Sender-ID in the SIGNED BridgeContext** (fix J) before i-3b lands, or odoo-prod silently vanishes for group users. **Fix J must merge before fix I.**

### 10.7 Security review checklist (mandatory before any force-route code)

- [ESC:sec] Cred source of truth: prove ResolveExternalBridgeTools merge order (server APIKey → contextCreds → userCreds) and hasUserCreds→AcquireUser decision are identical to manager.resolveServerCredentials (manager.go:258-336); no user served on a shared per-agent connection carrying another principal's Authorization.
- [ESC:sec] Per-request session isolation: verify no two concurrent agents/users share a session-tools entry (sessionID="" clobber, streamable_http.go:343/957-975/1327). Adversarial concurrency test with omitted/duplicate X-Mcp-Session-Id asserting distinct external sets + correct per-user BridgeTool (right api_key/connection).
- [ESC:sec] tools/call gated by grantChecker.IsAllowed on EVERY call (bridge_tool.go:189-203, fail-closed grant_checker.go:99) keyed off AgentIDFromContext/UserIDFromContext; cannot be bypassed by guessing a prefixed name; handler re-resolves server from ctx, never a stale captured serverID.
- [ESC:sec] No tenant cross-visibility: external tools surfaced per-request scoped to ListAccessible(agentID,userID); confirm NO process-global union map (reject any residual Candidate-B union) so a same-named server in another tenant can never be advertised or dispatched.
- [ESC:arch] HMAC trust boundary: bridge trusts agentID/userID/tenantID only when X-Bridge-Sig verifies (VerifyBridgeContext, claude_cli_mcp.go:284; tenant only when tenantVerified, server.go:315-318). Confirm seedExternalTools reads from verified ctx, not raw headers.
- [ESC:sec] fix-J actor id: external creds resolved by resolveActorUserID (SenderID for bitrix24/group, loop_mcp_user.go:71-85), and X-Sender-ID added to the SIGNED BridgeContext (not an unsigned header) before being trusted for credential lookup.
- [ESC:sec] 401 handling: expired-token path purges creds (DeleteUserCredentials) and flips connected=false (bridge_tool.go:251-283) so re-onboarding triggers; no infinite retry; failing Authorization not logged.
- [ESC:arch] Pool refCount discipline: Acquire/AcquireUser matched by Release/ReleaseUser on all exit paths (success, deny, timeout, panic); no pinning that exhausts MaxUserConns.
- Bridge-disabled posture unchanged: with no gateway token /mcp/bridge returns 403 and context headers ignored (server.go:223-229, 269-273); confirm external-tool seeding also disabled in that mode.
- Input safety: registeredName routing strips prefix to OriginalName and sends req.Params.Name upstream (bridge_tool.go) with no injection; schema marshaling failures fall back safely.
- i-3b completeness: after removing AgentMCPLookup, assert (test) the written CLI mcp-config contains ONLY goclaw-bridge + static d.Servers, NEVER per-agent DB servers — enforcement-bypass provably closed.
- odoo-security-reviewer sign-off: per-user Odoo (odoo-prod) credential isolation end-to-end — two LINE WORKS users in the same group get their OWN odoo session, verified on stage38 with psql/SSH before merge.
