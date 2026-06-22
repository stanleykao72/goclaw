package tools

import (
	"context"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// peerKindGroup is the ToolPeerKindFromCtx value indicating a group chat. Only
// in a group does the group scope apply (and chatId become a valid scope_id).
const peerKindGroup = "group"

// ScopeKey identifies a single memory scope (kind + id). Used by ingest/curate
// to name the one scope a write targets. ScopeID is "" for shared.
type ScopeKey struct {
	Kind string
	ID   string
}

// scopeKeyForCtx derives the SINGLE write scope from the verified injected
// identity for a given scope kind. SECURITY: scope_id always comes from ctx
// identity helpers, NEVER from LLM args or message content.
//
//	shared → ("shared", "")
//	user   → ("user", UserIDFromContext)        — the lineworks user id
//	agent  → ("agent", AgentKeyFromContext)      — the agentKey STRING (not the UUID)
//	group  → ("group", ToolChatIDFromCtx)        — ONLY when peerKind == "group"
//
// Returns ok=false when the required identity dimension is absent (e.g. a user
// scope with no user id, or a group scope outside a group chat) so callers
// skip the scope rather than create a mis-keyed pointer.
func scopeKeyForCtx(ctx context.Context, scopeKind string) (ScopeKey, bool) {
	switch scopeKind {
	case store.ScopeKindShared:
		return ScopeKey{Kind: store.ScopeKindShared, ID: ""}, true

	case store.ScopeKindUser:
		uid := store.UserIDFromContext(ctx)
		if uid == "" {
			return ScopeKey{}, false
		}
		return ScopeKey{Kind: store.ScopeKindUser, ID: uid}, true

	case store.ScopeKindAgent:
		// agentKey is the STRING identifier, not the agent UUID.
		key := store.AgentKeyFromContext(ctx)
		if key == "" {
			return ScopeKey{}, false
		}
		return ScopeKey{Kind: store.ScopeKindAgent, ID: key}, true

	case store.ScopeKindGroup:
		if ToolPeerKindFromCtx(ctx) != peerKindGroup {
			return ScopeKey{}, false
		}
		chatID := ToolChatIDFromCtx(ctx)
		if chatID == "" {
			return ScopeKey{}, false
		}
		return ScopeKey{Kind: store.ScopeKindGroup, ID: chatID}, true

	default:
		return ScopeKey{}, false
	}
}

// candidateScopesForCtx returns the ordered scope set recall should query,
// derived purely from verified identity:
//
//	[shared] + [user-<uid>] + [agent-<agentKey>]  (+ [group-<chatId>] if peerKind==group)
//
// Scopes whose identity dimension is absent are omitted. This is the
// pre-existence-filter list (it does NOT consult the pointer table).
func candidateScopesForCtx(ctx context.Context) []ScopeKey {
	order := []string{
		store.ScopeKindShared,
		store.ScopeKindUser,
		store.ScopeKindAgent,
		store.ScopeKindGroup,
	}
	out := make([]ScopeKey, 0, len(order))
	for _, kind := range order {
		if sk, ok := scopeKeyForCtx(ctx, kind); ok {
			out = append(out, sk)
		}
	}
	return out
}

// ResolvedNotebook pairs a scope with its resolved notebook + Drive Doc. Only
// scopes that EXIST in the pointer table are returned (recall never creates).
type ResolvedNotebook struct {
	Scope      ScopeKey
	NotebookID string
	DriveDocID string
}

// resolveNotebookSet returns, for the verified identity in ctx, the ordered set
// of EXISTING notebooks recall should query (Phase 2.4 will pass these to
// `nlm cross query`). Order is [shared, user, agent, group]; absent scopes and
// scopes with no pointer are excluded. NEVER creates a notebook on read.
//
// tenant is the verified tenant resolved by the caller (from ctx identity). The
// store argument is injected so this is unit-testable with a fake.
func resolveNotebookSet(ctx context.Context, ptrStore store.NotebookPointerStore, tenant uuid.UUID) ([]ResolvedNotebook, error) {
	candidates := candidateScopesForCtx(ctx)
	if len(candidates) == 0 {
		return nil, nil
	}

	out := make([]ResolvedNotebook, 0, len(candidates))
	for _, sk := range candidates {
		ptr, ok, err := ptrStore.Get(ctx, tenant, sk.Kind, sk.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // scope has no notebook yet — skip (no create on read)
		}
		out = append(out, ResolvedNotebook{
			Scope:      sk,
			NotebookID: ptr.NotebookID,
			DriveDocID: ptr.DriveDocID,
		})
	}
	return out, nil
}
