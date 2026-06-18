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

> **Authoritative spec: §11.** §10.0–§10.10 below is the design-evolution record (BLOCKED → revised → consolidated). Implementers follow **§11**.


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


### 10.8 R1 RESOLVED — per-request session identity (spike on mcp-go v0.44.0)

The §10.3/STEP-5 P0 (sessionID="" clobber) is trivially avoidable; the design's ctx-scoped-ToolFilter fallback is NOT needed.

- **Cause.** `WithStateLess(true)` forces `StatelessSessionIdManager` whose `Generate()` returns `""` (streamable_http.go:46-52, 1325-1329) — every concurrent request collapses onto the `""` key in the shared `sessionTools` store.
- **Fix.** Construct the bridge with `WithSessionIdManager(&StatelessGeneratingSessionIdManager{})` INSTEAD OF `WithStateLess(true)`. Its `Generate()` returns `idPrefix + uuid.New()` (streamable_http.go:1344) — unique per session, still stateless (format-only validation, no local tracking; works cross-instance). This is in fact mcp-go's default when `WithStateLess` is not set. `WithSessionIdManager(manager)` does honor the passed manager (streamable_http.go:58-66).
- **Injection is clean and reaches BOTH dispatch paths.** `handlePost` creates the ephemeral session (streamable_http.go:379) and binds it to ctx (:383) **before** running `contextFunc` (:385). So `seedExternalTools` (the `WithHTTPContextFunc`) retrieves the session via `ClientSessionFromContext` and calls `session.(SessionWithTools).SetSessionTools(externalTools)` (streamable_http.go:1076). `handleListTools` (server.go:1367-1368) AND `handleToolCall` (server.go:1441-1442) both consult `GetSessionTools()` → external tools are available for tools/list and tools/call. `WithToolFilter` is unnecessary (it scopes tools/list only, server.go:1398-1404).
- **R2 (store growth) still applies, minor.** `s.sessionTools` (shared, keyed by sessionID) accumulates one entry per one-shot `claude --print` session. Add explicit cleanup via `sessionToolsStore.delete` (streamable_http.go:973) on request-end, or a short TTL sweep. Not a blocker.

**Net:** i-3a STEP 5 is now concrete — drop `WithStateLess(true)`, add `WithSessionIdManager(&StatelessGeneratingSessionIdManager{})`, `SetSessionTools` inside `seedExternalTools`, add request-end cleanup. The design's last mechanism uncertainty is closed; **i-3a is ready to implement pending the §10.7 security sign-off.**


### 10.9 Security sign-off (§10.7 review) — VERDICT: **BLOCKED**

A pre-code security review (4 clusters / 12 items, verified against v3.14.0 code) ruled the i-3a + i-3b + fix-J design **BLOCKED**. The design must be revised to clear 3 blockers before implementation; the conditions below become hard implementation/test requirements; the last group is runtime-only (stage38 + third-party `claude` binary).

#### Blockers (design corrections required)

B1. S6 fix-J actor-id resolution is BROKEN as designed: resolveActorUserID (loop_mcp_user.go:71-85) keys per-user cred lookup on channelType=='bitrix24' (line 80), but BridgeContext (claude_cli_mcp.go:85-94) has NO channel-type field — OptChannel/X-Channel carries the instance name (e.g. 'my-telegram-bot'), not the platform ChannelType discriminator. Threading ONLY X-Sender-ID (as fix-J proposes) means resolveActorUserID runs with empty/wrong channelType: the bitrix24 branch can never fire, and for LINE WORKS DM-after-contact-merge users (peerKind=='direct', userID rewritten to tenant_user UUID at gateway_consumer_normal.go:155) it returns the REWRITTEN UUID and MISSES the lineworks:<uid> cred row entirely. The native loop is protected by a separate CredentialUserID via resolveCredentialUserID (loop_context.go:46-51) that the bridge does not replicate. FIX: add ChannelType to the SIGNED BridgeContext + thread OptChannelType -> X-Channel-Type -> Sign/VerifyBridgeContext -> middleware -> ResolveExternalBridgeTools so resolveActorUserID gets all 4 keyed args; decide whether the bridge must replicate resolveCredentialUserID for DM-merged users OR restrict the LINE WORKS odoo-prod rollout to group chats only (asserted by test). VERIFIED against code.
B2. S5/S11 i-3b enforcement-bypass still present and load-bearing: the direct-inject block at claude_cli_mcp.go:116-126 (AgentMCPLookup) injects per-agent external DB servers straight into the CLI --mcp-config, so the `claude` subprocess connects to them DIRECTLY (claude_cli_session.go:51-52), bypassing the bridge entirely — grant recheck, per-user cred resolution, IsToolAllowed AND the whole S2/S4/S5 isolation analysis are MOOT for those servers (they carry only static server-level Authorization shared across all users). The bridge-isolation sign-off is VACUOUS until i-3b STEP A deletes this block AND removes the MCPServerLookup/AgentMCPLookup plumbing (gateway_providers.go:209-241/:471, http/providers.go) so no caller can re-attach it. Must merge ATOMICALLY with i-3a + fix-J. A regression test must assert the written mcp-config's mcpServers keys are EXACTLY {goclaw-bridge} U keys(static d.Servers) and NEVER a per-agent DB server (currently ZERO coverage of writeMCPConfigInternal output). VERIFIED against code.
B3. S7 401-purge self-heal chain is UNIMPLEMENTED in the design AND the checklist citation is factually wrong: §10.7 claims 'DeleteUserCredentials + connected=false flip at bridge_tool.go:251-283', but bridge_tool.go:257 ONLY flips t.connected.Store(false) and returns a retry hint — it does NOT call DeleteUserCredentials. The only purge is loop_mcp_user.go:172 on the AcquireUser-FAIL path. The design's self-heal requires the lifted ResolveExternalBridgeTools to replicate the isUnauthorized401 -> mcpStore.DeleteUserCredentials(ctx, srv.ID, actorUserID) branch (loop_mcp_user.go:154-173) on its per-request AcquireUser failure — that branch is NOT in the design. Without it, an expired odoo-prod token becomes a PERMANENT failure (no re-onboard until 15m pool idle-evict). FIX: (1) lift the isUnauthorized401 purge branch into ResolveExternalBridgeTools; (2) correct the §10.7 citation; (3) note the purge must key on the resolved ACTOR userID (couples to the S6 fix). VERIFIED against code.

#### Conditions (must-do before merge, tied to checklist items)

C1. S1 cred source-of-truth: do NOT claim full equivalence to manager.resolveServerCredentials in the §10.7 proof — the lifted getUserMCPTools body (loop_mcp_user.go:139-148) OMITS the contextCreds tier (manager.go:264-281). Either (a) prove the contextCreds tier is provably unreachable on the bridge (assert ChannelContextScope never in bridge ctx) AND no target server (odoo-prod) uses context creds, with a regression test, or (b) lift resolveServerCredentials itself. Add a unit test asserting the resolved headers map for (odoo-prod, user) is byte-identical between bridge helper and native loop.
C2. S6b X-Sender-ID forge resistance: VerifyBridgeContext (claude_cli_mcp.go:296-307) has backward-compat fallback tiers that succeed WITHOUT the extra fields. Add a senderVerified flag (mirroring tenantVerified at claude_cli_mcp.go:284) so an attacker who strips/forges X-Sender-ID while keeping a valid OLD-format signature cannot have a forged sender treated as trusted. seedExternalTools MUST fail-closed (surface NO per-user odoo tools) when senderVerified==false. Adversarial test: valid old-format sig + injected X-Sender-ID must NOT resolve creds under the injected id.
C3. S5 fix-J HMAC coupling (J-before-I): land fix-J (senderID + channelType added to SignBridgeContext extra + matching VerifyBridgeContext tier returning senderVerified) BEFORE i-3b removal. Gate i-3b (AgentMCPLookup removal) behind this or a flag, else per-user odoo-prod silently vanishes/mis-resolves for group/merged-contact users.
C4. S5 trust-boundary coding discipline: seedExternalTools MUST derive agentID/userID/tenantID/senderID EXCLUSIVELY via store.*FromContext(ctx), NEVER from r.Header (reading r.Header.Get('X-Agent-ID') would re-trust UNVERIFIED headers and defeat the boundary). Add a test: valid token + raw forged X-Agent-ID/X-Tenant-ID headers + invalid/absent X-Bridge-Sig must surface zero external tools and seedExternalTools must never see forged values.
C5. S4 tenant-missing fail-closed: in ResolveExternalBridgeTools/seedExternalTools, on a ListAccessible error (tenant-less ctx from legacy-HMAC tiers, scope.go:20-25 fail-closed) return ZERO external tools and log — NEVER fall back to ListServers or any non-scoped enumeration. Add a test asserting a tenant-less (legacy-HMAC) bridge request surfaces zero external tools.
C6. S2 sessionToolsStore cleanup (R2): implement request-end cleanup keyed on THIS request's own unique sessionID (or a short TTL sweep) — the one-shot `claude --print` path never sends DELETE (streamable_http.go:694-695 only fires on terminate), so every session leaves a permanent entry (unbounded growth / DoS). Add the leak/cleanup assertion to the concurrency test.
C7. S2 credential-confidentiality test: the adversarial concurrency test MUST assert not just distinct tool NAMES but that each resolved BridgeTool carries the correct per-user api_key/connection (two fake users with different creds, assert each tools/call hits its own captured clientPtr). The grant recheck protects authorization but NOT credential confidentiality — unique-session-ID isolation is the SOLE barrier; treat S2 as P0.
C8. S3 global-fallthrough closure: add the STEP-10 regression test asserting external per-agent tools are NEVER in process-global s.tools (only the per-request session map), so mcp-go handleToolCall's global fallthrough (server.go:1453-1458) can never resolve another agent's external tool by name-guess. Assert each per-request ServerTool handler closure binds THIS request's bt (captured serverID).
C9. S8 pool refCount discipline: the lifted ResolveExternalBridgeTools MUST preserve Release-BEFORE-build ordering (Release right after AcquireUser-success, before convertBridgeToMCPTool/NewToolWithRawSchema), Release on EVERY post-success exit (skip/continue/grant-deny branches), and insert no new fallible step between Acquire-success and the non-deferred Release. Add a concurrency test asserting refCount returns to 0 and no MaxUserConns (default 30) exhaustion under repeated seed.
C10. S9 bridge-disabled invariant: wire the new NewBridgeServer(mcpStore,pool,grantChecker) into the SAME token-gated construction site (server.go:219, inside `if s.cfg.Gateway.Token != ""`); seedExternalTools must attach only via NewBridgeServer construction. Add a test asserting GET/POST /mcp/bridge returns 403 with no tools/list output when token is empty, to lock the invariant against future refactors.
C11. S10 schema-marshal safety: the new convertBridgeToMCPTool (STEP 6) must replicate bridge_server.go:86-90's marshal-error fallback (empty {"type":"object"}) rather than panic/nil, and set ONLY RawInputSchema (never both InputSchema and RawInputSchema) to avoid the errToolSchemaConflict path. The new ServerTool.Handler closure must call bt.Execute and must NOT pass inbound req.Params.Name upstream (keep bridge_tool.go:229 contract).

#### Runtime verifications (cannot be satisfied by static review)

V1. S12 stage38 two-user odoo-prod isolation (CANNOT be replaced by static review, contingent on S6 fix): two distinct LINE WORKS users in the SAME group each send a message triggering an odoo-prod tool call; via psql/SSH confirm TWO distinct UserPoolKey entries (pool.go:105 = tenantID/serverName/user:<userID>) and TWO distinct Authorization bearer tokens, and confirm user B's call NEVER reuses user A's poolEntry. If resolveActorUserID returns the GROUP id (S6 unfixed) both users collapse to the same key and AcquireUser returns the SAME poolEntry — one user served on the other's odoo Authorization (the catastrophic outcome). Do NOT sign off S12 on design grounds; it is contingent on S6 being fixed AND this test passing.
V2. S12 grant-recheck-at-execute keys on wrong id (runtime gap): BridgeTool.Execute reads store.UserIDFromContext (bridge_tool.go:190) = group-scoped X-User-ID, NOT the resolved actor, so a per-user grant revocation in mcp_user_grants keyed by lineworks:<uid> (ListAccessible LEFT JOIN mug.user_id, mcp_servers_access.go:189-194) is checked against the wrong id at execute time. Decide+verify whether the resolved actor id is injected as the ctx UserID for the bridge tool execution path (or grantChecker consults the actor id) so per-user grant revocation is honored at execute time.
V3. S2 CLI session-header round-trip (third-party `claude` binary behavior, not goclaw code): capture bridge HTTP traffic from a real `claude --print --mcp-config` run and assert the tools/list + tools/call POSTs carry the Mcp-Session-Id returned by initialize. SAFE failure mode confirmed (missing ID -> StatelessGeneratingSessionIdManager.Validate('') -> HTTP 404, no clobber), but external tools silently stop working if the CLI omits the header. If the CLI does NOT echo it, derive a unique id from the HMAC-signed per-session context (e.g. X-Session-Key) instead of relying on the client round-trip. Add a two-concurrent-fake-CLI integration test asserting distinct external tool sets.
V4. S7 401 log-leakage check: needs-runtime-verify that the lifted helper's slog.Warn(...'error', err.Error()) (loop_mcp_user.go:174) never embeds the Authorization header in the transport's 401 error string for the streamable-http odoo-prod path (current diagnostics log only has_* booleans + expiry; confirm the streamable-http transport 401 error string carries no bearer token).

#### Net

The biggest correction is **B1**: fix-J must thread **ChannelType** (not just SenderID) into the signed BridgeContext, and the per-user odoo-prod resolution has a real gap for LINE WORKS **DM-after-contact-merge** users (the native loop's `resolveCredentialUserID` is not replicated). Decision required: replicate `resolveCredentialUserID` on the bridge, OR restrict the LINE WORKS odoo-prod rollout to **group chats only** (test-asserted) for v1. **B2** makes the whole isolation analysis contingent on i-3b + i-3a + fix-J merging **atomically**. **B3** requires lifting the 401-purge self-heal into `ResolveExternalBridgeTools`. After these design revisions + a re-review, i-3a is clear to implement.


### 10.10 Design revision + re-review — VERDICT: approved-with-conditions (B1 mechanism corrected)

The section-10.9 blockers were revised and re-reviewed against v3.14.0 code:

- **B2 - CLEARED.** Delete the entire 10-symbol i-3b surface (inject loop `claude_cli_mcp.go:115-126`, `MCPServerLookup` type + nil-guard clause `:106`, `buildMCPServerLookup` `gateway_providers.go:207-242/:470-472`, `http/providers.go:39/:67-71/:241-242`, `gateway_http_handlers.go:95`) and merge i-3a + i-3b + fix-J **atomically**; add a regression test asserting the written mcpServers key set is EXACTLY the goclaw-bridge entry plus the static d.Servers keys, never a per-agent DB server.
- **B3 - CLEARED.** Lift the isUnauthorized401 -> DeleteUserCredentials purge (`loop_mcp_user.go:171`, keyed on the resolved actor id) into `ResolveExternalBridgeTools`. Section-10.7 citation corrected: `bridge_tool.go:254` is a comment, `:257-258` is `t.connected.Store(false)` + a warn - it does NOT call DeleteUserCredentials.
- **B1 - plumbing CLEARED, resolution mechanism CORRECTED.** The signed-context plumbing (ChannelType + SenderID as TRAILING signed extras, senderVerified third return on VerifyBridgeContext, middleware verify-then-inject from ctx, S4/S5 fail-closed) is correct. **But the locked "replicate resolveCredentialUserID" mechanism is itself WRONG** and is replaced:
  - The per-user MCP cred lookup keys on **resolveActorUserID** (`loop_mcp_user.go:71-85`), NOT resolveCredentialUserID - disconnected paths. resolveCredentialUserID (`user_identity_resolver.go:43-88`) does FORWARD (external->tenant UUID) resolution feeding CredentialUserID for SecureCLI/browser - the wrong direction for the MCP actor.
  - resolveActorUserID special-cases ONLY channelType=='bitrix24'; lineworks/other channels fall through to return userID, so a verbatim port returns the rewritten UUID for a LINE WORKS DM and misses lineworks:<uid>.
  - **CORRECTED FIX:** the autobind cred key (`autobind.go:198`) and the message SenderID (`channels/lineworks/handlers.go:128/:210`) are BOTH lineworks:<uid>, so the actor IS resolvable via SenderID - by **extending the resolveActorUserID channel special-case to include lineworks** (and other DM-merge provisioning channels) as a behavioral change. **This fixes both the bridge AND a latent native-loop bug** (today LINE WORKS DM-merged users miss per-user creds in native mode too). Drop all design text claiming resolveCredentialUserID resolves the MCP actor.

**Residual conditions (fold into implementation):**

1. B1-BLOCKING: The design must replace 'verbatim port of resolveActorUserID (loop_mcp_user.go:71-85)' with an explicit BEHAVIORAL change that adds 'lineworks' (and any other DM-merge channel provisioning per-user MCP creds) to the SenderID-preferring branch: `if channelType in {bitrix24, lineworks} && senderID != '' { return senderID }`. A verbatim port returns the rewritten UUID for a lineworks DM (peerKind!='group' → branch 2 → return userID) and therefore CANNOT resolve lineworks:<uid>. This is the actual fix; without it B1 is not cleared. Note this also fixes a latent bug already present in the native loop (LINE WORKS DM-merged per-user MCP creds are broken in v3.14.0 today, not just on the bridge).
2. B1-BLOCKING: Remove/rewrite all design text claiming resolveCredentialUserID is the mechanism that resolves the MCP actor. resolveCredentialUserID feeds CredentialUserID (consumed by SecureCLI/browser_cookie_store only) via FORWARD resolution (external→tenant UUID, user_identity_resolver.go:53/:69) — the OPPOSITE direction from the lineworks:<uid> cred key. The MCP per-user path keys exclusively on resolveActorUserID's output (loop_pipeline_callbacks.go:212→getUserMCPTools→GetUserCredentials, loop_mcp_user.go:121). Replicating resolveCredentialUserID is harmless but irrelevant to the per-user MCP actor; the locked decision's premise conflates the two resolvers.
3. B1: Confirm the bridge's per-actor BridgeTool sub-context injection (§10.2.6 store.WithUserID(ctx, actorID)) does not collide with the middleware's unconditional store.WithUserID(ctx, userID) at server.go:312 — the resolved actor must WIN at execute time so GetUserCredentials/AcquireUser/IsAllowed all key on actorID, not the header X-User-ID.
4. B2-minor: Add a test assertion that removing the `&& d.AgentMCPLookup == nil` clause from the WriteMCPConfig nil-guard (claude_cli_mcp.go:106) does not change the early-return behavior for empty-Servers+empty-Gateway configs.
5. B3: B3's actor-keyed purge is correct only after B1's resolution fix lands; until then the lifted purge keys on the rewritten UUID and purges the wrong/no row for lineworks DM-merged users (the exact cohort B3 targets).
6. S2/S8: sessionToolsStore and Release-before-build are NEW bridge-side constructs (sessionToolsStore does not exist in v3.14.0 — grep empty; native uses Release-AFTER-build at loop_mcp_user.go:151→build→:181 by design). The design correctly flags these as new but must specify the eviction/staleness contract and the documented acceptance of pool.evictIdle never setting connected.Store(false) (pool.go) as an explicit, reviewed risk, not an incidental note.

**Net:** B2 + B3 cleared; B1 is clearable with the small, well-scoped resolveActorUserID lineworks-branch extension (replacing the rejected resolveCredentialUserID replication). Once that correction lands, i-3a + i-3b + fix-J are ready to implement as ONE atomic change. Note S2 sessionToolsStore + S8 Release-before-build are NEW bridge-side constructs (do not exist in v3.14.0; native uses Release-after-build) - design them fresh, do not claim parity.

## 11. Fix I — Consolidated Implementation Spec (AUTHORITATIVE — supersedes §10.0–§10.10)

> Supersedes the docs/26 §10.0–§10.10 design evolution (BLOCKED → revised → approved-with-conditions). One coherent spec for the **single atomic change** `i-3a + i-3b + fix-J`. Grounded in v3.14.0 (`esmith/main` HEAD `40429cd7`). All file:line citations re-pinned against actual code. No code is written here.
>
> **LOCKED corrections from the §10.10 re-review OVERRIDE any stale text in earlier sections.** In particular: the per-user MCP actor resolves via **`resolveActorUserID`** (NOT `resolveCredentialUserID`); B1 is a **behavioral lineworks-branch extension**; the 401-purge citation is **`loop_mcp_user.go:171`** (NOT `bridge_tool.go:257`).

---

#### 1. Overview & Atomic-Change Scope

##### 1.1 Goal
Force-route every per-agent external MCP server through the `goclaw-bridge` HTTP server so that **every external tool call** traverses `BridgeTool.Execute → grantChecker.IsAllowed` + `IsToolAllowed` + per-user credential resolution. Today external servers are direct-injected into the CLI `--mcp-config` (`claude_cli_mcp.go:115-126`), so the `claude` subprocess connects to them **directly** (`claude_cli_session.go:51-52`), bypassing all enforcement and carrying only static server-level `Authorization` shared across all users.

##### 1.2 Three coupled sub-changes — must ship ATOMICALLY
| Sub-change | What it does | Why it cannot ship alone |
|---|---|---|
| **i-3a** | Build bridge dynamic-proxy substrate: `ResolveExternalBridgeTools` lifted into `internal/mcp`, per-request `seedExternalTools` via `WithSessionIdManager(&StatelessGeneratingSessionIdManager{})` + `SetSessionTools`, per-request cleanup | Without i-3b deletion, DB servers still route around the bridge → isolation is **vacuous** (invariant is a property of the FINAL `mcpServers` key set) |
| **i-3b** | Delete the 10-symbol direct-inject surface so external tools can ONLY arrive via the bridge | Deleting alone removes DB tools with no bridge replacement → external tools vanish |
| **fix-J** | Thread **ChannelType + SenderID** as signed BridgeContext extras + `senderVerified`; extend `resolveActorUserID` to include `lineworks` | Without it, per-user odoo-prod silently **mis-resolves** (returns rewritten UUID, misses `lineworks:<uid>`) for LINE WORKS DM-merged users once i-3b deletes the direct-inject |

The isolation invariant is a property of the **final** `mcpServers` key set written by `writeMCPConfigInternal` (`claude_cli_mcp.go:105-184`). Shipping any subset leaves a hole. **One PR, one atomic commit set.**

##### 1.3 Out-of-scope (explicitly deferred, documented as parity gaps)
- Context-scoped creds (`ChannelContextScope`) tier — never injected on the bridge; documented gap (C1).
- search-mode / >threshold tool-deferral parity — `getUserMCPTools` does not enter search mode.
- K respawn-amnesia (§4.1) and F cache-token follow-ups — independent.

---

#### 2. B1 — Actor Resolution on the Bridge

##### 2.1 Root cause (precisely located, v3.14.0)
- `BridgeContext` has 8 fields, **NO `ChannelType` / `SenderID`** (`claude_cli_mcp.go:86-95`). The `Channel` field is the **instance name** (`OptChannel`, e.g. `my-telegram-bot`), NOT the platform discriminator.
- `resolveActorUserID` (`loop_mcp_user.go:71-85`) special-cases **ONLY** `channelType=='bitrix24' && senderID!=''` (line 75 — CONFIRMED still bitrix24-only). For a LINE WORKS DM-after-contact-merge user (`peerKind=='direct'`, `userID` rewritten to a tenant_user UUID at `gateway_consumer_normal.go:157`), it falls through to `peerKind != "group" || senderID == "" → return userID` (branch 2, lines 81-82) and returns the **rewritten UUID**, never `lineworks:<uid>`. The per-user Odoo cred row is keyed `lineworks:<uid>` (autobind `autobind.go:198` `userKey=senderPrefix+lwUserID`), so it is **missed**.
- The per-user MCP cred path keys EXCLUSIVELY on `resolveActorUserID`'s output: `loop_pipeline_callbacks.go → getUserMCPTools → GetUserCredentials` (`loop_mcp_user.go:122`).

##### 2.2 LOCKED CORRECTION — the fix is `resolveActorUserID`, NOT `resolveCredentialUserID`
`resolveCredentialUserID` (`user_identity_resolver.go:43-88`) does **FORWARD** resolution (external → tenant_user UUID) feeding `RunContext.CredentialUserID`, consumed only by SecureCLI / browser_cookie_store — the **WRONG direction** for the MCP actor key. **Drop all design text claiming `resolveCredentialUserID` resolves the MCP actor or that the bridge must replicate it.** Replicating it is harmless but irrelevant.

**The actual fix (BEHAVIORAL, not a verbatim port):** extend `resolveActorUserID`'s channel special-case to include `lineworks` (and any other DM-merge provisioning channel that keys per-user MCP creds by SenderID):

```
if (channelType == "bitrix24" || channelType == "lineworks") && senderID != "" {
    return senderID
}
```

This is a behavioral change that fixes **BOTH** the bridge AND **a latent native-loop bug** — LINE WORKS DM-merged per-user MCP creds are broken in v3.14.0 today (native mode too), because a verbatim `resolveActorUserID` returns the rewritten UUID for a lineworks DM (`peerKind!='group'` → branch 2). The autobind cred key (`autobind.go:198`) and message SenderID (`channels/lineworks/handlers.go:128/:210`) are both `lineworks:<uid>`, so the actor IS resolvable via SenderID once the branch is extended.

##### 2.3 Thread ChannelType + SenderID as TRAILING signed extras
`SignBridgeContext` (`claude_cli_mcp.go:267-276`) is strictly positional: fixed `agentID|userID|channel|chatID|peerKind|workspace|tenantID`, then `|`-joined `extra ...string`. Today extras are `localKey, sessionKey` (positions 0,1). Append ChannelType + SenderID **AFTER** them (positions 2,3) so existing signatures still match a fallback tier. **Prepending would invalidate every in-flight signature.** Edit chain (each grounded):

1. **New Opt consts** — `OptChannelType="channel_type"`, `OptSenderID="sender_id"` in `internal/providers/claude_cli.go` (alongside `OptUserID`).
2. **Producer enrichment** — `internal/agent/loop_pipeline_callbacks.go:303-313`: add `chatReq.Options[providers.OptChannelType]=req.ChannelType` and `[providers.OptSenderID]=req.SenderID`. Both available: `loop_types.go:627` (ChannelType), `:634` (SenderID).
3. **Struct + builder** — add `ChannelType string`, `SenderID string` to `BridgeContext` (`claude_cli_mcp.go:86-95`) and to `bridgeContextFromOpts` (`claude_cli_session.go:171-182`) via `extractStringOpt(opts, OptChannelType/OptSenderID)`. `WriteMCPConfig` (`:101-103`) + `writeMCPConfigInternal` (`:105`) signatures gain the two params.
4. **Header emission** — `writeMCPConfigInternal` (`claude_cli_mcp.go:128-173`): emit `X-Channel-Type` and `X-Sender-ID` under the SAME CRLF guard (`!strings.ContainsAny(v,"\r\n\x00")`) as the existing 9 headers, then pass `channelType, senderID` as the 3rd/4th extras to `SignBridgeContext(... localKey, sessionKey, channelType, senderID)` at `:163`.

##### 2.4 `senderVerified` third return (S6b forge resistance)
`VerifyBridgeContext` (`claude_cli_mcp.go:284-306`) today returns `(ok, tenantVerified)` with 4 short-circuit tiers (all-extras :286-289, no-extra :291-294, no-tenant :295-299, oldest :300-304). Change to return **`(ok, tenantVerified, senderVerified)`**:
- The full tier covering `localKey|sessionKey|channelType|senderID` (the new L1) is the **ONLY** tier returning `senderVerified=true`.
- A new intermediate tier matching `localKey`+`sessionKey` but NOT the channelType/senderID extras (the J-rollout transition window + pre-channelType sessions) returns `senderVerified=false`.
- ALL existing fallback tiers (no-extra, no-tenant, oldest) return `senderVerified=false` — mirroring how L3/L4 already return `tenantVerified=false`.

**Forge attack blocked:** an attacker holding a valid OLD-format signature who injects/strips `X-Sender-ID` lands on a fallback tier → `senderVerified=false` → bridge resolves ZERO per-user creds under the injected sender. Mirrors the `tenantVerified` precedent (`server.go:315-318`).

##### 2.5 Middleware: verify-then-inject from ctx (S4 + S5 + S6b)
In `bridgeContextMiddleware` (`internal/gateway/server.go:256-348`):
- Read `X-Channel-Type` + `X-Sender-ID` alongside existing header reads (`:259-266`).
- Pass them as matching extras to `VerifyBridgeContext(... localKey, sessionKey, channelType, senderID)` at `:280`; capture `senderVerified`.
- Inject `tools.WithToolChannelType(ctx, channelType)` (`internal/tools/context_keys.go:59`) and `store.WithSenderID(ctx, senderID)` (`internal/store/context.go:211`) **ONLY when `senderVerified==true`** — gated identically to the `tenantVerified` tenant injection (`server.go:315-318`).
- **S4 (tenant fail-closed):** when `tenantVerified==false || tenantID==''`, `ResolveExternalBridgeTools` surfaces ZERO per-user external tools (tenant required for `ListAccessible`/pool keys). On `ListAccessible` error (tenant-less legacy-HMAC ctx, `scope.go` fail-closed) return ZERO + log — NEVER fall back to `ListServers` or any non-scoped enumeration.
- **S5 (FromContext-not-headers):** `seedExternalTools`/`ResolveExternalBridgeTools` derive agentID/userID/tenantID/senderID/channelType **EXCLUSIVELY** via `store.*FromContext` / `tools.*FromCtx` — NEVER `r.Header.Get`. The middleware is the sole trust boundary.

##### 2.6 Lift `resolveActorUserID` into a shared importable package
`ResolveExternalBridgeTools` lives in `internal/mcp` and CANNOT import `internal/agent` (`getUserMCPTools` is a `*Loop` method, `loop_mcp_user.go:91`). Move `resolveActorUserID` (with the lineworks-branch extension from §2.2) into a shared, importable package (`internal/mcp` itself or a small `internal/identityresolve`) as a free function `ResolveActorUserID(userID, senderID, peerKind, channelType string) string`. The native `*Loop` callers become thin wrappers so the two paths cannot drift (single source of truth). `ResolveExternalBridgeTools` receives `agentID, tenantID` PLUS the 4 actor-resolution inputs `userID, senderID, peerKind, channelType` from verified ctx, computes the actor id, then runs the lifted per-server loop.

##### 2.7 V2 grant-check + creds must key on the RESOLVED actor (condition 3)
`BridgeTool.Execute` rechecks via `grantChecker.IsAllowed(ctx, agentID, userID, serverID, toolName)` where `userID = store.UserIDFromContext(ctx)` (`bridge_tool.go:188-190`); `mcp_user_grants` are keyed by actor via `ListAccessible` LEFT JOIN on `mug.user_id` (`grant_checker.go`). On the bridge the ctx UserID is the rewritten/group-scoped `X-User-ID` — the WRONG id at execute time.

**Fix + collision resolution (condition 3):** `ResolveExternalBridgeTools` injects the RESOLVED actor id as the ctx `UserID` for the resolved sub-context that the per-actor BridgeTools capture, via `store.WithUserID(ctx, actorID)`. The middleware unconditionally sets `store.WithUserID(ctx, userID)` at `server.go:311` (the `if userID != ""` block, CONFIRMED). The per-actor `store.WithUserID(ctx, actorID)` is applied **AFTER** (downstream of) the middleware in the `seedExternalTools`/`ResolveExternalBridgeTools` call path, so **last-write-wins yields `actorID`** at `GetUserCredentials`, `AcquireUser`, AND `IsAllowed`. The implementer MUST confirm the per-actor `WithUserID` is layered on the request ctx **after** the middleware has run (which it is: middleware wraps `next.ServeHTTP`, and `seedExternalTools` runs inside the handler via `WithHTTPContextFunc`). Ordering is correct; assert it in the per-actor isolation test.

---

#### 3. B2 — Atomic Merge: i-3a + Delete i-3b + fix-J

##### 3.1 i-3b deletion surface — ALL 10 symbols removed in one change
| # | Symbol | Location |
|---|---|---|
| 1 | `AgentMCPLookup` direct-inject loop (`servers[srv.Name]=entry`) | `claude_cli_mcp.go:115-126` |
| 2 | `MCPServerLookup` func type | `claude_cli_mcp.go:30-32` |
| 3 | `AgentMCPLookup MCPServerLookup` field on `MCPConfigData` | `claude_cli_mcp.go:40` |
| 4 | `&& d.AgentMCPLookup == nil` clause in nil-guard | `claude_cli_mcp.go:106` |
| 5 | `buildMCPServerLookup` factory | `cmd/gateway_providers.go:207-242` |
| 6 | startup wiring `mcpData.AgentMCPLookup = buildMCPServerLookup(mcpStore)` | `cmd/gateway_providers.go:471` |
| 7 | runtime wiring `mcpData.AgentMCPLookup = h.mcpLookup` | `internal/http/providers.go:242` |
| 8 | `mcpLookup` field on `ProvidersHandler` | `internal/http/providers.go:39` |
| 9 | `SetMCPServerLookup` setter | `internal/http/providers.go:67-71` |
| 10 | `providersH.SetMCPServerLookup(buildMCPServerLookup(stores.MCP))` wiring | `cmd/gateway_http_handlers.go:95` |

Keep `mcpServerEntryToConfig` ONLY if still referenced by static `d.Servers`. The collision guard at `:118` only protects name collisions, NOT authorization — deletion removes the whole branch.

##### 3.2 Why atomic (grounded in write order)
`writeMCPConfigInternal` builds `servers` as: (1) `maps.Copy` static `d.Servers` (`:112-113`), (2) **[i-3b]** inject `AgentMCPLookup` DB servers (`:115-126`), (3) add `goclaw-bridge` entry (`:128-173`), then marshal `{"mcpServers": servers}` (`:180`). Isolation is a property of the FINAL key set: i-3a alone leaves step (2) routing DB servers around the bridge; deleting i-3b alone removes DB tools with no bridge replacement. Both must land with fix-J.

##### 3.3 B2 key-set regression test (genuinely NEW)
The existing test file `internal/providers/claude_cli_mcp_test.go:9-166` covers ONLY HMAC sign/verify (ZERO assertion on the written key set). New tests:
- **Positive:** construct `MCPConfigData{Servers: {"static-a":…, "static-b":…}, GatewayAddr:"x", GatewayToken:"t"}`, call `WriteMCPConfig`, read+unmarshal the file, assert `mcpServers` keys == **EXACTLY** `{"goclaw-bridge","static-a","static-b"}` — after step (2) removal the only keys are shallow-copied `d.Servers` + the unconditional `servers["goclaw-bridge"]` (`:173`).
- **Negative:** a config that would previously inject a DB server name must now NOT contain it (proves the bypass is closed).
- **Nil-guard (condition 4):** assert removing `&& d.AgentMCPLookup == nil` from the `:106` early-return does NOT change behavior for empty-`Servers`+empty-`GatewayAddr` configs — still returns `""` early.

---

#### 4. B3 — 401-Purge Lift (Self-Heal)

##### 4.1 Citation correction (LOCKED)
The original §10.7 claim that `bridge_tool.go:257` calls `DeleteUserCredentials` is **FALSE**. `bridge_tool.go:257-258` is `t.connected.Store(false)` + a retry-hint warn; the string `DeleteUserCredentials` appears only in a COMMENT at `:254`. **The ONLY actual `DeleteUserCredentials` call in the per-user MCP path is `loop_mcp_user.go:171`** (CONFIRMED — inside the `AcquireUser` error → `isUnauthorized401(err)` branch, keyed on the `userID` param).

##### 4.2 Lift the purge, re-keyed on resolved actor
The purge (`loop_mcp_user.go:154-173`): inside the `AcquireUser` error handler, `if isUnauthorized401(err)` → bitrix diagnostics slog (`:162-170`) → `_ = l.mcpStore.DeleteUserCredentials(ctx, srv.ID, userID)` (`:171`) → purged-warn (`:172`).

**Lift into `ResolveExternalBridgeTools`:** duplicate the `isUnauthorized401 → DeleteUserCredentials` branch into the bridge's per-server `AcquireUser` error handler, keyed on the **resolved actor id** (`lineworks:<uid>` / SenderID), NOT the raw header userID. Because B1 makes `GetUserCredentials`, `AcquireUser`, AND `IsAllowed` all key on the resolved actor, the purge target is consistent end-to-end. The native loop retains its own purge; both call `mcpStore.DeleteUserCredentials(ctx, srv.ID, actorID)`.

##### 4.3 Ordering dependency (condition 5)
B3's actor-keyed purge is correct **only after B1's resolution fix lands**; until then the lifted purge keys on the rewritten UUID and purges the wrong/no row for exactly the lineworks DM-merged cohort B3 targets. Since this is one atomic change, B1 and B3 land together — implementer must order the edits so the lineworks-branch extension (§2.2) is in place before the lifted purge is keyed on `ResolveActorUserID`'s output.

---

#### 5. The 6 Residual Conditions as Concrete Requirements

| # | Condition | Concrete design/test requirement |
|---|---|---|
| **1** | B1 lineworks-branch is BEHAVIORAL, not verbatim port | `ResolveActorUserID` MUST read `if (channelType=="bitrix24" \|\| channelType=="lineworks") && senderID!="" { return senderID }`. A verbatim port returns the rewritten UUID for a lineworks DM (branch 2). **Test:** `ResolveActorUserID("uuid-x","lineworks:42","direct","lineworks")` returns `"lineworks:42"` (not `"uuid-x"`); same inputs with `channelType="telegram"` returns `"uuid-x"` (unchanged). Document the latent native-loop bug this also fixes. |
| **2** | Drop `resolveCredentialUserID`-as-MCP-actor text | No design artifact, comment, or helper may claim `resolveCredentialUserID` resolves the MCP actor or that the bridge replicates it. The MCP per-user path keys exclusively on `ResolveActorUserID`. (Doc/text requirement — verify by grep that no new code/comment references `resolveCredentialUserID` in the bridge resolution path.) |
| **3** | Per-actor `store.WithUserID(actorID)` vs middleware `store.WithUserID(userID)` collision | The per-actor sub-ctx `store.WithUserID(ctx, actorID)` (§2.7) is applied DOWNSTREAM of the middleware's `store.WithUserID(ctx, userID)` (`server.go:311`), so **last-write-wins = actorID**. Confirm the per-actor injection runs inside `seedExternalTools` (after middleware). **Test:** assert `GetUserCredentials`/`AcquireUser`/`IsAllowed` all see `actorID` (`lineworks:<uid>`), not the header `X-User-ID`. |
| **4** | Nil-guard clause removal preserves empty-Servers early return | Removing `&& d.AgentMCPLookup == nil` from `claude_cli_mcp.go:106` must NOT change the early `return ""` for `len(d.Servers)==0 && d.GatewayAddr==""`. **Test:** `MCPConfigData{}` (no Servers, no GatewayAddr) → `WriteMCPConfig` returns `""`. |
| **5** | B3 purge correct only after B1 lands | Order edits so the lineworks extension is in place before the lifted purge keys on `ResolveActorUserID` output. (Implementation-order requirement, §4.3.) |
| **6** | S2 `sessionToolsStore` is NEW (no v3.14.0 parity); S8 Release-before-build MIRRORS native ordering | `sessionToolsStore`/`SessionTools` does NOT exist in v3.14.0 (grep empty) — design fresh, NO parity claim. **S2 eviction/staleness contract:** per-request cleanup keyed on THIS request's unique sessionID (from `WithSessionIdManager(&StatelessGeneratingSessionIdManager{})`, `Generate()=idPrefix+uuid`); the one-shot `claude --print` path never sends DELETE (`streamable_http.go:694-695` fires only on terminate) → add explicit request-end `sessionToolsStore.delete` or short TTL sweep. **S8 Release-BEFORE-build:** bridge uses Release-immediately-after-`AcquireUser`-success, before `convertBridgeToMCPTool`/`NewToolWithRawSchema`, because BridgeTools hold the live `*atomic.Pointer[Client]` (not a snapshot). Native ALSO releases BEFORE build (`loop_mcp_user.go:181` release → `:204` build), so the bridge's Release-before-build MIRRORS native; only `sessionToolsStore` is the genuinely new (no-parity) construct. **Documented accepted risk:** `pool.evictIdle` calls `cancel()`+`Close()` but NEVER `connected.Store(false)`, so a tool captured during the refCount==0 window can read `Connected()==true` against a CLOSED client; staleness detected only on next failed `CallTool` (`bridge_tool.go:257`). Accept the same eventual-consistency staleness the native loop tolerates; re-acquire self-heals. This is an explicit reviewed risk, not an incidental note. |

##### 5.1 Additional folded conditions (from §10.9 C1–C11 / S-items)
- **C1 (cred source-of-truth):** the lifted `ResolveExternalBridgeTools` replicates the inline merge ORDER (server APIKey → `headers["Authorization"]` `loop_mcp_user.go:139-141` → user APIKey override `:144-146` → `maps.Copy` user Headers/Env `:147-148`) and the `hasUserCreds → AcquireUser` decision EXACTLY. It does NOT claim full equivalence to `manager.resolveServerCredentials` (which adds a contextCreds tier unreachable on the bridge). **Test:** resolved headers map for `(odoo-prod, user)` is byte-identical between bridge helper and native loop; document the contextCreds gap.
- **C2/S6b:** see §2.4 senderVerified + §6 adversarial test.
- **C4 (trust boundary):** `seedExternalTools` derives identity EXCLUSIVELY via `store.*FromContext`, never `r.Header`. **Test:** valid token + forged `X-Agent-ID`/`X-Tenant-ID` headers + invalid/absent `X-Bridge-Sig` → zero external tools.
- **C7/S2 (cred confidentiality, P0):** adversarial concurrency test asserts not just distinct tool NAMES but that each resolved BridgeTool carries the correct per-user api_key/connection (two fake users, different creds, each `tools/call` hits its own captured `clientPtr`). Unique-session-ID isolation is the SOLE barrier.
- **C8/S3 (global fallthrough):** external per-agent tools are NEVER in process-global `s.tools` (only the per-request session map), so `handleToolCall`'s global fallthrough can never name-guess another agent's external tool.
- **C9/S8 (refCount):** Release matched on EVERY post-success exit (skip/continue/grant-deny/timeout/panic); no fallible step between Acquire-success and Release. Test: refCount returns to 0, no MaxUserConns exhaustion under repeated seed.
- **C10/S9 (bridge-disabled):** wire `NewBridgeServer(…, mcpStore, pool, grantChecker)` into the SAME token-gated site (`server.go:217-231`, inside `if Gateway.Token != ""`). Test: empty token → `/mcp/bridge` 403, no tools/list, written config contains NO DB servers (only static `d.Servers`; `goclaw-bridge` absent).
- **C11/S10 (schema-marshal):** `convertBridgeToMCPTool` replicates `bridge_server.go:83-91`'s `{"type":"object"}` fallback on marshal failure; set ONLY `RawInputSchema`; handler closure calls `bt.Execute`, never passes inbound `req.Params.Name` upstream.

##### 5.2 Runtime verifications (cannot be satisfied by static review — stage38 + real `claude` binary)
- **V1/S12:** two LINE WORKS users in the SAME group each trigger an odoo-prod tool call; psql/SSH confirm TWO distinct `UserPoolKey` entries + TWO distinct Authorization bearer tokens; user B's call NEVER reuses user A's poolEntry. Contingent on B1 fix.
- **V3/S2:** capture bridge HTTP traffic from a real `claude --print --mcp-config` run; assert tools/list + tools/call carry the `Mcp-Session-Id` from initialize. Safe failure mode (missing ID → `StatelessGeneratingSessionIdManager.Validate("")` → 404, no clobber).
- **V4/S7:** confirm the lifted helper's `slog.Warn(...'error', err.Error())` never embeds Authorization in the streamable-http 401 error string.

---

#### 6. HMAC / Forge Test Extensions (B1 / S6b)
Extend `internal/providers/claude_cli_mcp_test.go` (`:9-166`) and `internal/gateway/bridge_context_test.go`:
- (a) Sign WITH `channelType`/`senderID` extras differs from WITHOUT.
- (b) `VerifyBridgeContext` returns `senderVerified=true` ONLY at the full tier.
- (c) ALL fallback tiers return `senderVerified=false`.
- (d) Adversarial: valid OLD-format sig + injected `X-Sender-ID` → `ok=true, senderVerified=false`, creds NOT resolved under the injected sender.
- (e) Middleware injects `WithToolChannelType`/`WithSenderID` ONLY when `senderVerified`.
- (f) Backward-compat: pre-channelType session (localKey+sessionKey only) still verifies `ok=true` (graceful degradation; per-user tools withheld until config rewrite — flag-day-free, same as pre-tenantID/pre-localKey).

#### 7. In-Flight Session Safety
Per-session config at `~/.goclaw/mcp-configs/<safe-session-key>/mcp-config.json` (`claude_cli_mcp.go:81-83`, atomic write). A session whose config predates the new fields carries an OLD `X-Bridge-Sig` → matches a fallback tier (`ok=true`) → keeps working with `senderVerified=false` (per-user Odoo tools withheld until config rewrite). Same mechanism that already protects pre-localKey/pre-tenantID sessions. This is what lets the change ship without a flag day.

#### 11.S Implementation step order

1. Step 0 (pre-flight): Move resolveActorUserID into a shared importable package (internal/mcp or internal/identityresolve) as free func ResolveActorUserID; make native *Loop callers thin wrappers. This is prerequisite for both B1 and the lifted helper (loop_mcp_user.go:71-85).
2. Step 1 (B1 behavioral fix): In the shared ResolveActorUserID, extend the channel special-case from `channelType=="bitrix24"` to `channelType=="bitrix24" || channelType=="lineworks"` (loop_mcp_user.go:75). This must precede the B3 purge keying.
3. Step 2 (fix-J consts): Add OptChannelType="channel_type" and OptSenderID="sender_id" in internal/providers/claude_cli.go.
4. Step 3 (fix-J producer): In internal/agent/loop_pipeline_callbacks.go:303-313 add chatReq.Options[OptChannelType]=req.ChannelType and [OptSenderID]=req.SenderID (loop_types.go:627/:634).
5. Step 4 (fix-J struct+builder): Add ChannelType/SenderID to BridgeContext (claude_cli_mcp.go:86-95) and bridgeContextFromOpts (claude_cli_session.go:171-182); extend WriteMCPConfig (:101-103) + writeMCPConfigInternal (:105) signatures.
6. Step 5 (fix-J header+sign): In writeMCPConfigInternal emit X-Channel-Type/X-Sender-ID under the CRLF guard; pass channelType,senderID as 3rd/4th extras to SignBridgeContext at claude_cli_mcp.go:163.
7. Step 6 (fix-J verify): Change VerifyBridgeContext (claude_cli_mcp.go:284-306) to return (ok, tenantVerified, senderVerified); add the new full L1 tier (only senderVerified=true) + intermediate localKey/sessionKey-only tier (senderVerified=false); all other tiers senderVerified=false.
8. Step 7 (fix-J middleware): In bridgeContextMiddleware (server.go:256-348) read X-Channel-Type/X-Sender-ID, pass to VerifyBridgeContext, capture senderVerified; inject tools.WithToolChannelType + store.WithSenderID ONLY when senderVerified==true (gate like tenantVerified at :315-318).
9. Step 8 (i-3a lift): Lift getUserMCPTools per-server body (loop_mcp_user.go:115-220) into internal/mcp free func ResolveExternalBridgeTools(ctx, store, pool, gc, tenantID, agentID, userID, senderID, peerKind, channelType); compute actorID via ResolveActorUserID; replicate inline cred merge order (:139-148); refactor Loop.getUserMCPTools to call it.
10. Step 9 (i-3a actor-keyed ctx): Inside ResolveExternalBridgeTools inject store.WithUserID(ctx, actorID) for the per-actor sub-ctx that BridgeTools capture (downstream of middleware's WithUserID at server.go:312 -> last-write-wins=actorID), so GetUserCredentials/AcquireUser/IsAllowed all key on actorID.
11. Step 10 (B3 purge lift): Add the isUnauthorized401 -> mcpStore.DeleteUserCredentials(ctx, srv.ID, actorID) branch (port of loop_mcp_user.go:154-173) into ResolveExternalBridgeTools' AcquireUser error handler, keyed on resolved actorID.
12. Step 11 (i-3a S8 Release-before-build): Release immediately after AcquireUser-success, before convertBridgeToMCPTool; Release on every post-success exit path (skip/continue/grant-deny/timeout/panic).
13. Step 12 (i-3a bridge server): Change NewBridgeServer signature (bridge_server.go) to add (mcpStore, pool, grantChecker); keep builtin BridgeToolNames AddTool loop unchanged.
14. Step 13 (i-3a seeding): Construct bridge with WithSessionIdManager(&StatelessGeneratingSessionIdManager{}) (NOT WithStateLess(true)) + WithHTTPContextFunc(seedExternalTools); seedExternalTools derives identity via store.*FromContext (never r.Header), calls ResolveExternalBridgeTools, SetSessionTools on the ephemeral session; add request-end sessionToolsStore.delete cleanup (S2).
15. Step 14 (i-3a convert+schema): convertBridgeToMCPTool with bridge_server.go:83-91 marshal-error fallback {"type":"object"}; set ONLY RawInputSchema; handler closure calls bt.Execute, never forwards inbound req.Params.Name.
16. Step 15 (i-3a wiring): Wire NewBridgeServer(...,mcpStore,pool,grantChecker) into the token-gated construction site (server.go:217-231, inside if Gateway.Token != "").
17. Step 16 (i-3b deletion): Delete all 10 symbols ATOMICALLY: claude_cli_mcp.go:115-126 inject loop, :30-32 MCPServerLookup type, :40 field, :106 nil-guard clause; gateway_providers.go:207-242 buildMCPServerLookup + :471; http/providers.go:39 field, :67-71 setter, :242; gateway_http_handlers.go:95. Keep mcpServerEntryToConfig only if static d.Servers still uses it.
18. Step 17 (tests): Add B2 key-set regression test + nil-guard test (claude_cli_mcp_test.go); HMAC/senderVerified + adversarial forge tests (claude_cli_mcp_test.go / bridge_context_test.go); ResolveExternalBridgeTools unit tests (allow/deny, hasUserCreds->AcquireUser, 401 purge keyed on actorID, skip-on-cold-server); concurrency cred-confidentiality test; bridge-disabled invariant test; cred byte-identical test.
19. Step 18 (single atomic commit set + PR): one PR carrying i-3a+i-3b+fix-J; document S2/S8 new-construct + evictIdle staleness as explicit reviewed risks; defer V1/V3/V4 to stage38 runtime verification.

#### 11.T Test matrix

- B1 / condition-1 — ResolveActorUserID("uuid-x","lineworks:42","direct","lineworks") == "lineworks:42" (NOT rewritten UUID); same with channelType="telegram" == "uuid-x" (unchanged); bitrix24 branch still works (loop_mcp_user.go:71-85)
- B1 / S6b condition — VerifyBridgeContext returns senderVerified=true ONLY at the new full L1 tier; intermediate localKey/sessionKey-only tier and all legacy fallbacks return senderVerified=false (claude_cli_mcp.go:284-306)
- B1 / S6b adversarial (C2) — valid OLD-format sig + injected X-Sender-ID -> ok=true, senderVerified=false, creds NOT resolved under injected sender
- B1 / sign roundtrip — SignBridgeContext WITH channelType/senderID extras differs from WITHOUT; backward-compat pre-channelType session still ok=true (flag-day-free)
- B1 / middleware (e) — WithToolChannelType + WithSenderID injected ONLY when senderVerified==true; not injected on fallback tiers
- B1 / condition-3 collision — per-actor store.WithUserID(actorID) wins over middleware store.WithUserID(userID) (server.go:312); GetUserCredentials/AcquireUser/IsAllowed all key on actorID=lineworks:<uid>
- B1 / S4 fail-closed — tenantVerified==false || tenantID=='' || ListAccessible error -> ZERO per-user external tools, no ListServers fallback
- B1 / C4 trust-boundary — valid token + forged X-Agent-ID/X-Tenant-ID headers + absent/invalid X-Bridge-Sig -> zero external tools; seedExternalTools never reads r.Header
- B2 / key-set positive — WriteMCPConfig with Servers={static-a,static-b}+GatewayAddr -> mcpServers keys EXACTLY {goclaw-bridge,static-a,static-b}
- B2 / key-set negative — config that previously injected a DB server name now contains NO per-agent DB server (bypass closed)
- B2 / condition-4 nil-guard — MCPConfigData{} (no Servers, no GatewayAddr) -> WriteMCPConfig returns "" after removing && d.AgentMCPLookup==nil clause (claude_cli_mcp.go:106)
- B3 / 401-purge — ResolveExternalBridgeTools AcquireUser 401 -> DeleteUserCredentials(ctx, srv.ID, actorID) keyed on resolved actor (NOT header userID); native loop purge unchanged (loop_mcp_user.go:171)
- B3 / condition-5 ordering — purge keys on lineworks:<uid> only because B1 lineworks branch landed; pre-B1 would purge wrong/no row
- C1 / cred merge — resolved headers map for (odoo-prod, user) byte-identical between ResolveExternalBridgeTools and native getUserMCPTools (server APIKey -> user APIKey override -> maps.Copy Headers/Env, loop_mcp_user.go:139-148)
- C7/S2 / cred-confidentiality (P0) — two concurrent fake users with DIFFERENT creds resolving for distinct actors (lineworks:A vs lineworks:B) -> each BridgeTool carries its own api_key/clientPtr; never observe each other's Authorization; stale (IsConnected()==false) entries evicted not served
- C6/S2 / sessionToolsStore cleanup — one-shot claude --print path leaves NO permanent sessionTools entry (explicit request-end delete or TTL); unbounded-growth assertion
- C8/S3 / global-fallthrough — external per-agent tools NEVER in process-global s.tools (only per-request session map); each ServerTool handler closure binds THIS request's bt (captured serverID)
- C9/S8 / refCount discipline — Release-before-build; refCount returns to 0 on every exit (success/skip/grant-deny/timeout/panic); no MaxUserConns exhaustion under repeated seed
- C10/S9 / bridge-disabled — GatewayToken=='' -> /mcp/bridge 403, no tools/list, written config has NO DB servers (only static d.Servers, goclaw-bridge absent)
- C11/S10 / schema-marshal — malformed external-tool schema -> convertBridgeToMCPTool falls back to {"type":"object"} (RawInputSchema only); whole resolve does not fail
- RUNTIME V1/S12 — stage38: two LINE WORKS users same group -> two distinct UserPoolKey + two distinct Authorization bearer tokens via psql/SSH; user B never reuses user A poolEntry (contingent on B1)
- RUNTIME V3/S2 — real claude --print --mcp-config: tools/list+tools/call carry Mcp-Session-Id from initialize; missing-ID safe (404, no clobber); two-concurrent-fake-CLI distinct external sets
- RUNTIME V4/S7 — lifted helper slog.Warn never embeds Authorization in streamable-http 401 error string


## 12. Follow-up F2 — sub-agent (Agent/Task) escape + WebSearch latency

> Source: production incident 2026-06-18. A LINE WORKS DM "請問近三天的天氣" took **104s** to answer. Prepared for implementation; behavior NOT changed yet (deferred per user).

### 12.1 Root cause (latency AND control-escape)

`buildArgs` (`internal/providers/claude_cli_session.go:82` summoner + `:86` chat-with-bridge) sets `--disallowedTools Bash,Edit,Read,Write,Glob,Grep,WebFetch,WebSearch,TodoRead,TodoWrite,NotebookRead,NotebookEdit`. This disables WebSearch for the **main** agent, but the **`Agent` / `Task` sub-agent tool is NOT in the list**. So when the user asked for weather, Claude (Opus 4.8) spawned an internal sub-agent to do the lookup, and the sub-agent **does NOT inherit `--disallowedTools`** — it ran WebSearch freely.

Evidence (session `.jsonl` trace, dba6b849):
- `ToolSearch {"query":"+web"}` (~7s — bundled CLI tools are deferred)
- `Agent {"description":"查三峽區三天天氣","prompt":"...請使用 WebSearch..."}` → spawns sub-agent
- sub-agent runs ~55s (WebSearch + reasoning) → main agent composes answer
- `v3.run.completed duration_ms=104704`

Two problems:
1. **Latency**: the sub-agent indirection turns a few-second WebSearch into ~55-100s.
2. **Control-escape** (concrete instance of docs/25 §12-D / docs/26 D gap): the sub-agent bypasses `--disallowedTools`, the goclaw MCP bridge, AND the PreToolUse security hooks. It can run WebSearch today — and, by the same escape, potentially Bash/Read/Write that the main agent has blocked. **This is a security gap, not just a perf nit.**

NOT the bottleneck (ruled out): MCP pool warmup (e-smith-hub odoo-prod connected in <1s); "odoo-prod connected twice" was two DIFFERENT agents (e-smith-hub allow_size=6 + another agent allow_size=4), not a double-connect.

### 12.2 Fix (ready to implement)

Add `Agent` and `Task` to BOTH `--disallowedTools` strings (`claude_cli_session.go:82` and `:86`). Closes the sub-agent escape (latency + security in one change). The bundled CLI version (2.1.x) names the tool `Agent`; include `Task` for forward/back compat.

### 12.3 Open decision (DEFERRED 2026-06-18 — "先不動")

Once the sub-agent escape is closed, the main agent still cannot WebSearch (it is in the disallowed list), so web queries (weather etc.) would be declined or knowledge-guessed. Decide:

- **(A, recommended)** Remove `WebFetch,WebSearch` from the chat-bridge disallowed list (`:86` only, keep `:82` summoner locked) → the main agent web-searches **directly** (fast, one round-trip) and keeps the capability. Web is read-only / low-risk.
- **(B)** Keep WebSearch disabled → bot has no web access (fastest, most controlled, loses capability).

The fs/exec tools (`Bash,Edit,Read,Write,Glob,Grep`) stay disabled in BOTH cases — they MUST route through the bridge.

### 12.4 Test

- Assert the written CLI args contain `Agent` and `Task` in `--disallowedTools` (both sites).
- A real DM weather query (Option A) completes with NO `Agent`/`Task` tool_use in the session `.jsonl` and in materially less wall-clock time; (Option B) the agent answers without web and does not spawn a sub-agent.
