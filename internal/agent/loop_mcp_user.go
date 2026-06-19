package agent

import (
	"context"
	"log/slog"

	mcpbridge "github.com/nextlevelbuilder/goclaw/internal/mcp"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// resolveActorUserID picks the user identifier used for per-user resource
// lookups (MCP credentials, RBAC grants, audit attribution) given the routing
// fields carried on a pipeline.RunInput / agent.RunRequest.
//
// Provisioner contract: per-user MCP credentials are keyed by the real
// external user id (= SenderID for Bitrix24, Telegram, etc.). The agent
// loop must look them up with the same key the provisioner used to store
// them, otherwise rows are missed and MCP tools silently disappear.
//
// The gateway consumer (cmd/gateway_consumer_normal.go) rewrites UserID in
// two scenarios where the original value would break per-actor lookups:
//
//  1. Group chats: UserID → "group:<channel>:<chatID>" composite (or
//     "guild:<guildID>:user:<senderID>" for Discord) so multiple users in
//     the same group share conversation memory and session state.
//  2. DM with merged contact: UserID → tenant_user UUID after sender has
//     been merged via ContactCollector.ResolveTenantUserID. Enables
//     per-user features cross-channel for the same human, but breaks
//     credential lookups keyed by external user id.
//
// Both rewrites are correct for *memory and tenant-user resolution*, but
// wrong for resources scoped per-actor:
//
//   - MCP credentials are minted per-user by channel provisioners and stored
//     with user_id = SenderID. Looking them up by the rewritten UserID always
//     misses the row.
//   - RBAC grants and audit attribution must reflect the real actor, not
//     the rewritten container — otherwise every action in a group or after
//     contact-merge looks identical to the policy engine.
//
// For channel provisioners that always key by SenderID regardless of
// DM/group/merge state (Bitrix24, LINE WORKS), we MUST always prefer SenderID.
// Without the channelType discriminator, DMs with merged contacts hit the
// "return userID" branch and silently lose MCP creds.
//
// Other channels (Telegram, Slack, Discord, Zalo) currently do not
// provision per-user MCP credentials, so for them the helper retains the
// previous group-rewrite recovery semantics. When those channels later
// add per-user MCP integrations they can register their type here.
//
// Synthetic ticker / notification senders carry empty SenderID. They do
// not own per-user credentials, so the function falls back to UserID and
// the lookup returns nil safely either way.
//
// The implementation lives in internal/mcp (mcpbridge.ResolveActorUserID) so
// the native agent loop and the bridge force-route path share one definition
// and cannot drift (docs/26 §11 §2.6, §11.S step 0). This is a thin wrapper.
func resolveActorUserID(userID, senderID, peerKind, channelType string) string {
	return mcpbridge.ResolveActorUserID(userID, senderID, peerKind, channelType)
}

// getUserMCPTools returns per-user MCP tools for servers requiring user credentials.
// Tools are cached per-user in mcpUserTools sync.Map and registered in the shared
// tool registry so ExecuteWithContext can resolve them. On first call for a user,
// connections are established via pool.AcquireUser() and BridgeTools created.
func (l *Loop) getUserMCPTools(ctx context.Context, userID string) []tools.Tool {
	if len(l.mcpUserCredSrvs) == 0 || l.mcpPool == nil || l.mcpStore == nil || userID == "" {
		if userID == "" && len(l.mcpUserCredSrvs) > 0 {
			slog.Debug("mcp.user_tools_skipped", "reason", "empty_user_id", "servers", len(l.mcpUserCredSrvs))
		}
		return nil
	}

	if cached, ok := l.mcpUserTools.Load(userID); ok {
		cachedTools := cached.([]tools.Tool)
		// Check if any cached tool's connection was evicted by pool.
		// If so, clear cache and re-acquire connections.
		allConnected := true
		for _, t := range cachedTools {
			if bt, ok := t.(interface{ IsConnected() bool }); ok && !bt.IsConnected() {
				allConnected = false
				break
			}
		}
		if allConnected {
			return cachedTools
		}
		l.mcpUserTools.Delete(userID)
		slog.Debug("mcp.user_tools_stale", "user", userID, "reason", "pool_evicted")
	}

	var userTools []tools.Tool
	for _, info := range l.mcpUserCredSrvs {
		srv := info.Server

		// Check if user has credentials for this server
		uc, err := l.mcpStore.GetUserCredentials(ctx, srv.ID, userID)
		if err != nil || uc == nil || (uc.APIKey == "" && len(uc.Headers) == 0 && len(uc.Env) == 0) {
			continue
		}

		// Resolve the per-user BridgeTools via the shared helper (docs/26
		// §11 §2.6, §11.S step 8). The cred merge, AcquireUser, release-before-
		// build, 401-purge self-heal and grant filtering live in
		// mcpbridge.BuildUserCredServerTools so the native loop and the bridge
		// force-route path share ONE definition and cannot drift.
		userTools = append(userTools,
			mcpbridge.BuildUserCredServerTools(ctx, l.mcpStore, l.mcpPool, l.mcpGrantChecker, l.tenantID, info, userID, uc)...)
	}

	if len(userTools) > 0 {
		l.mcpUserTools.Store(userID, userTools)
		// Update "mcp" tool group so policy expansion via alsoAllow includes
		// per-user tools. MergeToolGroup is additive — safe for concurrent users.
		var names []string
		for _, t := range userTools {
			names = append(names, t.Name())
		}
		l.registry.MergeToolGroup("mcp", names)
		slog.Info("mcp.user_tools_loaded", "user", userID, "tools", len(userTools))
	}
	return userTools
}

// executeToolForActor resolves a tool by name with per-user isolation.
//
// For per-user MCP tools (cached in mcpUserTools by actorUserID), we MUST
// resolve from the user's own slice so the BridgeTool used carries that
// user's MCP api_key + pool connection. Resolving via the shared registry
// alone leaks the first user's BridgeTool to every subsequent user.
//
// Fallback to shared registry for non-MCP tools (memory, web, exec, etc.)
// and for cases where actorUserID has no per-user tools (synthetic events,
// non-Bitrix channels without per-user provisioning).
func (l *Loop) executeToolForActor(
	ctx context.Context,
	name string,
	args map[string]any,
	channel, chatID, peerKind, sessionKey, actorUserID string,
) *tools.Result {
	if actorUserID != "" {
		if cached, ok := l.mcpUserTools.Load(actorUserID); ok {
			for _, t := range cached.([]tools.Tool) {
				if t.Name() != name {
					continue
				}
				// Apply ContextualTool / PeerKindAware setters if supported.
				if ct, ok := t.(tools.ContextualTool); ok {
					ct.SetContext(channel, chatID)
				}
				if pa, ok := t.(tools.PeerKindAware); ok {
					pa.SetPeerKind(peerKind)
				}
				return t.Execute(ctx, args)
			}
		}
	}
	return l.tools.ExecuteWithContext(ctx, name, args, channel, chatID, peerKind, sessionKey, nil)
}
