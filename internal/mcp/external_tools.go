package mcp

import (
	"context"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// ResolveActorUserID picks the user identifier used for per-user resource
// lookups (MCP credentials, RBAC grants, audit attribution) given the routing
// fields carried on a pipeline.RunInput / agent.RunRequest.
//
// LOCKED per docs/26 §11 §2.2. Channel-specific provisioners (bitrix24,
// lineworks) key per-user MCP credentials by SenderID, because BOTH the group
// rewrite AND the DM merged-contact rewrite override UserID — SenderID is the
// only stable lookup key. The lineworks branch ALSO fixes a latent native-loop
// bug present in v3.14.0: before it, a LINE WORKS DM-after-contact-merge user
// (peerKind=="direct", userID rewritten to a tenant_user UUID) hit the
// "return userID" branch and silently lost per-user MCP creds keyed
// lineworks:<uid>.
//
// Branch ORDER is load-bearing: the channel-specific early returns MUST precede
// the peerKind!="group" fallback, or lineworks/bitrix DMs fall through to the
// rewritten UUID.
//
// This is the single source of truth shared by the native agent loop and the
// bridge force-route path (docs/26 §11 §2.6, §11.S step 0).
func ResolveActorUserID(userID, senderID, peerKind, channelType string) string {
	// Channel-specific provisioners always key MCP credentials by SenderID
	// (raw channel user id). Group rewrite AND DM merged-contact rewrite both
	// override UserID — SenderID is the only stable lookup key.
	if (channelType == "bitrix24" || channelType == "lineworks") && senderID != "" {
		return senderID
	}
	// Other channels: original group-rewrite recovery only. DMs without
	// channel-specific handling retain UserID semantics (assumed to equal
	// SenderID where it matters).
	if peerKind != "group" || senderID == "" {
		return userID
	}
	return senderID
}

// credHasNonEmpty reports whether m[key] is present and non-blank.
func credHasNonEmpty(m map[string]string, key string) bool {
	if m == nil {
		return false
	}
	return strings.TrimSpace(m[key]) != ""
}

// credsPresent reports whether a user-credentials record carries any usable
// material (APIKey, custom headers, or env). Mirrors the inline check at the
// former loop_mcp_user.go:122 and resolveServerCredentials hasUserCreds logic.
func credsPresent(uc *store.MCPUserCredentials) bool {
	return uc != nil && (uc.APIKey != "" || len(uc.Headers) > 0 || len(uc.Env) > 0)
}

// BuildUserCredServerTools resolves the per-user BridgeTools for ONE
// user-credential MCP server, given an already-fetched non-empty credentials
// record. Lifted VERBATIM from the former Loop.getUserMCPTools per-server body
// (docs/26 §11 §2.6, §11.S step 8 — single source of truth) so the native agent
// loop and the bridge force-route path cannot drift.
//
// Credential merge order (docs/26 §11 C1, byte-identical to the native inline
// merge at loop_mcp_user.go:139-148): server APIKey → user APIKey override →
// maps.Copy user Headers/Env (user overrides server defaults). The bridge
// intentionally OMITS the contextCreds tier that manager.resolveServerCredentials
// applies — ChannelContextScope is never injected on the bridge ctx, so it is
// unreachable there (documented parity gap C1).
//
// The pool connection is Released IMMEDIATELY after a successful acquire, BEFORE
// BridgeTools are built (docs/26 §11 S8 / R14): BridgeTools hold the client
// pointer directly and detect eviction via the connected atomic.Bool; releasing
// up front lets pool idle eviction work (refCount=0 + lastUsed TTL). Do NOT
// reorder to release-after-build.
//
// On a 401 from AcquireUser the user credentials are purged so re-onboarding can
// re-mint them (docs/26 §11 B3, §4.2). The purge is keyed on the userID passed
// in — callers MUST pass the RESOLVED actor id (ResolveActorUserID output), not
// a raw header user id.
//
// Returns nil (with refCount balanced) on acquire failure.
func BuildUserCredServerTools(ctx context.Context, st store.MCPServerStore, pool *Pool, gc GrantChecker, tenantID uuid.UUID, info store.MCPAccessInfo, userID string, uc *store.MCPUserCredentials) []tools.Tool {
	srv := info.Server

	// Resolve connection params: server defaults merged with user overrides.
	args := ParseJSONBytesToStringSlice(srv.Args)
	env := ParseJSONBytesToStringMap(srv.Env)
	if env == nil {
		env = make(map[string]string)
	}
	headers := ParseJSONBytesToStringMap(srv.Headers)
	if headers == nil {
		headers = make(map[string]string)
	}

	// Inject server-level API key into headers if present.
	if srv.APIKey != "" && headers["Authorization"] == "" {
		headers["Authorization"] = "Bearer " + srv.APIKey
	}

	// Merge user credentials (user overrides server defaults).
	if uc.APIKey != "" {
		headers["Authorization"] = "Bearer " + uc.APIKey
	}
	maps.Copy(headers, uc.Headers)
	maps.Copy(env, uc.Env)

	// Acquire user-keyed pool connection.
	entry, err := pool.AcquireUser(ctx, tenantID, srv.Name, userID,
		srv.Transport, srv.Command, args, env, srv.URL, headers, srv.TimeoutSec)
	if err != nil {
		if isUnauthorizedErr(err) {
			expiresAt := strings.TrimSpace(uc.Env["BITRIX_EXPIRES_AT"])
			expired := false
			if expiresAt != "" {
				if t, parseErr := time.Parse(time.RFC3339, expiresAt); parseErr == nil {
					expired = time.Now().UTC().After(t)
				}
			}
			slog.Warn("mcp.user_401_diagnostics",
				"server", srv.Name,
				"user", userID,
				"has_bitrix_domain", credHasNonEmpty(uc.Env, "BITRIX_DOMAIN"),
				"has_access_token", credHasNonEmpty(uc.Env, "BITRIX_ACCESS_TOKEN"),
				"has_refresh_token", credHasNonEmpty(uc.Env, "BITRIX_REFRESH_TOKEN"),
				"bitrix_expires_at", expiresAt,
				"bitrix_expired", expired,
			)
			_ = st.DeleteUserCredentials(ctx, srv.ID, userID)
			slog.Warn("mcp.user_credentials_purged", "server", srv.Name, "user", userID, "reason", "unauthorized_401")
		}
		slog.Warn("mcp.user_pool_acquire_failed", "server", srv.Name, "user", userID, "error", err)
		return nil
	}

	// Release immediately — BridgeTools hold the client pointer directly.
	pool.ReleaseUser(UserPoolKey(tenantID, srv.Name, userID))

	return buildBridgeToolsFromEntry(entry, srv, info, gc, userID, "user_cred")
}

// resolveSharedServerTools resolves BridgeTools for ONE shared-credential
// (non per-user) external MCP server on the bridge force-route path (docs/26
// §11 §2.6, the !hasUserCreds branch of the "hasUserCreds → AcquireUser
// decision"). Uses the shared pool connection (pool.Acquire) with server-level
// credentials only.
//
// Per docs/26 §11 C1 the bridge intentionally OMITS the contextCreds tier;
// header env-var expansion (resolveEnvVars) mirrors the native shared path in
// resolveServerCredentials. The connection is Released immediately after
// acquire (same rationale as BuildUserCredServerTools).
func resolveSharedServerTools(ctx context.Context, pool *Pool, gc GrantChecker, tenantID uuid.UUID, info store.MCPAccessInfo) []tools.Tool {
	srv := info.Server

	args := ParseJSONBytesToStringSlice(srv.Args)
	env := ParseJSONBytesToStringMap(srv.Env)
	headers, err := resolveEnvVars(ParseJSONBytesToStringMap(srv.Headers))
	if err != nil {
		slog.Warn("security.mcp.env_var_rejected", "server", srv.Name, "err", err)
		return nil
	}
	if headers == nil {
		headers = make(map[string]string)
	}
	if srv.APIKey != "" && headers["Authorization"] == "" {
		headers["Authorization"] = "Bearer " + srv.APIKey
	}

	entry, err := pool.Acquire(ctx, tenantID, srv.Name,
		srv.Transport, srv.Command, args, env, srv.URL, headers, srv.TimeoutSec)
	if err != nil {
		slog.Warn("mcp.shared_pool_acquire_failed", "server", srv.Name, "error", err)
		return nil
	}

	// Release immediately — BridgeTools hold the client pointer directly.
	pool.Release(poolKey(tenantID, srv.Name))

	return buildBridgeToolsFromEntry(entry, srv, info, gc, "", "shared_bridge")
}

// buildBridgeToolsFromEntry converts a pool entry's discovered MCP tools into
// grant-filtered BridgeTools. Lifted from the former getUserMCPTools build loop
// (loop_mcp_user.go:199-220). Tools failing the agent's allow/deny grant
// (info.ToolAllow / info.ToolDeny) are filtered out up front so the LLM never
// sees a tool it cannot call.
func buildBridgeToolsFromEntry(entry *poolEntry, srv store.MCPServerData, info store.MCPAccessInfo, gc GrantChecker, userID, path string) []tools.Tool {
	hints := ParseToolHints(srv.Settings)
	var out []tools.Tool
	var filteredOut []string
	for _, mcpTool := range entry.MCPTools() {
		if !IsToolAllowed(mcpTool.Name, info.ToolAllow, info.ToolDeny) {
			filteredOut = append(filteredOut, mcpTool.Name)
			continue
		}
		bt := NewBridgeTool(srv.Name, mcpTool, entry.ClientPtr(), srv.ToolPrefix, srv.TimeoutSec, entry.Connected(), srv.ID, gc).
			WithHints(hints.Global, hints.HintFor(mcpTool.Name)).
			WithForceReconnect(entry.RequestForceReconnect())
		out = append(out, bt)
	}
	if len(filteredOut) > 0 {
		slog.Info("mcp.tools.filtered_at_register",
			"server", srv.Name,
			"server_id", srv.ID,
			"user", userID,
			"path", path,
			"filtered_count", len(filteredOut),
			"filtered_tools", filteredOut,
			"allow_size", len(info.ToolAllow),
			"deny_size", len(info.ToolDeny),
		)
	}
	return out
}

// ResolveExternalBridgeTools is the bridge force-route entry point (docs/26
// §11 §2.6, i-3a). It enumerates every external MCP server accessible to the
// agent for the RESOLVED actor and returns the per-actor BridgeTools the bridge
// must surface, plus the resolved actor id so the caller can key the
// execute-time ctx UserID on the actor (docs/26 §11 §2.7 / condition 3).
//
// Trust boundary (docs/26 §11 §2.5 S5 / condition C4): the caller MUST derive
// agentID/tenantID/userID/senderID/peerKind/channelType EXCLUSIVELY from the
// HMAC-verified ctx (store.*FromContext / tools.*FromCtx), NEVER from raw
// request headers. This function trusts its arguments.
//
// Fail-closed (docs/26 §11 §2.5 S4 / condition C5): on any ListAccessible error
// (e.g. a tenant-less legacy-HMAC ctx whose scope lookup fails closed) return
// ZERO tools and the error — NEVER fall back to a non-scoped enumeration.
//
// Per-server dispatch matches the native "hasUserCreds → AcquireUser decision"
// (docs/26 §11 C1): a server with per-user creds for the actor goes through the
// user-keyed pool (BuildUserCredServerTools); a server requiring per-user creds
// for which the actor has none is skipped; everything else uses the shared pool
// (resolveSharedServerTools).
//
// NOTE (i-3a substrate, docs/26 §13 wave 2): this resolver is built and
// unit-tested DORMANT. The live force-route wiring (seedExternalTools +
// NewBridgeServer deps + i-3b direct-inject deletion) lands atomically in the
// wave-3 flip, gated by the mandatory §11 §5.2 security review.
func ResolveExternalBridgeTools(ctx context.Context, st store.MCPServerStore, pool *Pool, gc GrantChecker, tenantID uuid.UUID, agentID uuid.UUID, userID, senderID, peerKind, channelType string) ([]tools.Tool, string, error) {
	actorID := ResolveActorUserID(userID, senderID, peerKind, channelType)

	if st == nil || pool == nil {
		return nil, actorID, nil
	}

	accessible, err := st.ListAccessible(ctx, agentID, actorID)
	if err != nil {
		// Fail-closed: a tenant-less / legacy-HMAC ctx yields a scope error.
		// Never fall back to ListServers or any non-scoped enumeration.
		slog.Warn("mcp.bridge.list_accessible_failed", "agent", agentID, "actor", actorID, "error", err)
		return nil, actorID, err
	}

	var out []tools.Tool
	for _, info := range accessible {
		srv := info.Server
		if !srv.Enabled {
			continue
		}

		uc, _ := st.GetUserCredentials(ctx, srv.ID, actorID)
		hasUserCreds := credsPresent(uc)

		if requireUserCreds(srv.Settings) && !hasUserCreds {
			// Per-user server with no creds for this actor: skip (matches
			// native getUserMCPTools / resolveServerCredentials skip).
			continue
		}

		if hasUserCreds {
			out = append(out, BuildUserCredServerTools(ctx, st, pool, gc, tenantID, info, actorID, uc)...)
			continue
		}
		out = append(out, resolveSharedServerTools(ctx, pool, gc, tenantID, info)...)
	}
	return out, actorID, nil
}
