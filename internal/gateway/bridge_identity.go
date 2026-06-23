package gateway

import (
	"context"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// ResolvedIdentity is the identity-source-agnostic set of fields that the MCP
// bridge injects into the request context. Both the claude path (headers +
// HMAC, via bridgeContextMiddleware) and the agy path (per-session loopback
// port, via BridgeSessionListeners) build one of these and call
// injectBridgeIdentity so the downstream pipeline — per-user MCP grants,
// per-agent memory backend, channel routing — keys off identical ctx values
// regardless of how the identity was authenticated.
//
// TenantVerified / SenderVerified gate the tenant and sender/channelType
// fields: only inject them when the source actually authenticated them. For
// the claude path these come from VerifyBridgeContext (the HMAC must cover
// them). For the agy path the port itself is the credential, so an identity
// minted into the session store is already trusted and sets both to true.
type ResolvedIdentity struct {
	AgentID     uuid.UUID
	UserID      string
	TenantID    string
	Channel     string
	ChatID      string
	PeerKind    string
	Workspace   string
	LocalKey    string
	SessionKey  string
	ChannelType string
	SenderID    string

	TenantVerified bool
	SenderVerified bool
}

// injectBridgeIdentity injects the resolved identity into ctx using the exact
// same field set + gating + nil-guards as the legacy bridgeContextMiddleware
// inline block. It is the single source of truth for bridge ctx injection so
// the claude and agy paths stay bit-for-bit identical.
//
// Behaviour mirrored from the original block:
//   - AgentID is injected only when non-nil; when an agentStore is wired and the
//     row resolves, the agent key, memory backend, and shell deny groups are
//     propagated too (memory backend defaults to "db" for non-vault agents).
//   - UserID is injected when non-empty.
//   - TenantID is injected only when TenantVerified && it parses as a UUID.
//   - SenderID / ChannelType are injected only when SenderVerified (and non-empty).
//   - channel/chatID/peerKind are injected when non-empty.
//   - Workspace is injected only when an agent or user identity is present
//     (prevents unauthenticated path injection).
//   - localKey/sessionKey are injected when non-empty (routing, not security).
func injectBridgeIdentity(ctx context.Context, id ResolvedIdentity, agentStore store.AgentStore) context.Context {
	hasIdentity := id.AgentID != uuid.Nil || id.UserID != ""

	if id.AgentID != uuid.Nil {
		ctx = store.WithAgentID(ctx, id.AgentID)

		// Inject per-agent shell deny group overrides so the exec tool
		// respects the same policy as the normal agent loop.
		if agentStore != nil {
			ag, err := agentStore.GetByIDUnscoped(ctx, id.AgentID)
			if err == nil && ag != nil {
				// Propagate the agent key so bridged session tools (sessions_list/
				// history/send) can resolve identity via ToolAgentKeyFromCtx. The MCP
				// bridge otherwise injects only the agent UUID, leaving the key empty
				// -> session tools fail with "agent context required".
				ctx = tools.WithToolAgentKey(ctx, ag.AgentKey)
				// Propagate the per-agent memory backend ("db" | "vault")
				// so bridge memory tools (write_file/read_file/list_files/
				// memory_search/memory_get on MEMORY.md & the vault layout)
				// route to the SAME backend as the native agent loop. Without
				// this the bridge ctx carries no backend, MemoryBackendFromCtx
				// defaults to "db", and a vault-mode agent's memory writes
				// silently land in Postgres+KG instead of the Obsidian vault
				// file the per-turn auto-injector recalls from — so the saved
				// memory is never recalled. Mirrors resolver.go's RunContext
				// (ag.ParseMemoryBackend()); defaults to "db" so non-vault
				// agents are bit-for-bit unchanged.
				ctx = store.WithMemoryBackend(ctx, ag.ParseMemoryBackend())
				// Propagate the per-agent memory mode ("notebook" | "vault" |
				// "both") so bridge memory tools gate the SAME subsystems as the
				// native agent loop. Mirrors WithMemoryBackend above; defaults to
				// "both" so non-configured agents keep every subsystem active.
				ctx = store.WithMemoryMode(ctx, ag.ParseMemoryMode())
				groups := ag.ParseShellDenyGroups()
				if groups != nil {
					ctx = store.WithShellDenyGroups(ctx, groups)
				}
			}
		}
	}
	if id.UserID != "" {
		ctx = store.WithUserID(ctx, id.UserID)
	}
	// Only inject tenant_id when the source verified it (HMAC level 1 for
	// claude; port-as-credential for agy). Fallback levels (pre-tenantID
	// sessions) must not trust unsigned tenant headers.
	if id.TenantVerified && id.TenantID != "" {
		if tid, err := uuid.Parse(id.TenantID); err == nil {
			ctx = store.WithTenantID(ctx, tid)
		}
	}
	// Inject sender identity + channel type only when the source verified them
	// (full HMAC tier for claude; port-as-credential for agy). Fallback tiers
	// must not trust unsigned X-Sender-ID / X-Channel-Type headers — forge
	// resistance, mirroring TenantVerified.
	if id.SenderVerified {
		if id.SenderID != "" {
			ctx = store.WithSenderID(ctx, id.SenderID)
		}
		if id.ChannelType != "" {
			ctx = tools.WithToolChannelType(ctx, id.ChannelType)
		}
	}

	// Inject channel routing context for tools like message, cron, etc.
	if id.Channel != "" {
		ctx = tools.WithToolChannel(ctx, id.Channel)
	}
	if id.ChatID != "" {
		ctx = tools.WithToolChatID(ctx, id.ChatID)
	}
	if id.PeerKind != "" {
		ctx = tools.WithToolPeerKind(ctx, id.PeerKind)
	}
	// Inject workspace so bridge tools (read_image, read_file, etc.) can resolve
	// paths. Only when agent/user context is present to prevent unauthenticated
	// path injection.
	if id.Workspace != "" && hasIdentity {
		ctx = tools.WithToolWorkspace(ctx, id.Workspace)
	}
	// Routing context (localKey, sessionKey) is injected unconditionally like
	// channel/chatID. These are used for message routing (forum topics), not
	// security-sensitive operations. Without valid agent context, tool execution
	// will fail anyway.
	if id.LocalKey != "" {
		ctx = tools.WithToolLocalKey(ctx, id.LocalKey)
	}
	if id.SessionKey != "" {
		ctx = tools.WithToolSessionKey(ctx, id.SessionKey)
	}

	return ctx
}
