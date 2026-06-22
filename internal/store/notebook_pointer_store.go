package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Memory scope kinds for the NotebookLM 4-tier system. These are the only
// valid values for NotebookPointer.ScopeKind.
const (
	ScopeKindShared = "shared"
	ScopeKindUser   = "user"
	ScopeKindAgent  = "agent"
	ScopeKindGroup  = "group"
)

// NotebookPointer maps a memory scope to its NotebookLM notebook and the
// backing Drive Doc. Persisted in nlm_notebooks; the natural key is
// (TenantID, ScopeKind, ScopeID).
//
// ScopeID is ” for the shared scope, and the lineworks userId / agentKey /
// chatId for user / agent / group scopes respectively. It is ALWAYS derived
// from the verified injected identity — never from LLM args or message body.
type NotebookPointer struct {
	TenantID    uuid.UUID `db:"tenant_id"`
	ScopeKind   string    `db:"scope_kind"`
	ScopeID     string    `db:"scope_id"`
	NotebookID  string    `db:"notebook_id"`
	DriveDocID  string    `db:"drive_doc_id"`
	DisplayName string    `db:"display_name"`
	CreatedAt   time.Time `db:"created_at"`
}

// NotebookPointerStore persists the scope→notebook pointer table. The tenant is
// an explicit argument (the provisioner resolves it from the verified injected
// identity before calling) — never read from LLM input.
type NotebookPointerStore interface {
	// Get returns the pointer for (tenant, scopeKind, scopeID). The second
	// return is false (with nil error) when no pointer exists — callers use
	// this to drive lazy-create (miss) vs reuse (hit).
	Get(ctx context.Context, tenant uuid.UUID, scopeKind, scopeID string) (*NotebookPointer, bool, error)

	// Create inserts a pointer race-safely: INSERT ... ON CONFLICT
	// (tenant_id, scope_kind, scope_id) DO NOTHING. On conflict (a concurrent
	// caller won) it returns the EXISTING row, so the caller always ends with
	// a valid pointer and can detect "someone else created it" by comparing
	// notebook ids. Returns the winning row (ours or theirs).
	Create(ctx context.Context, p NotebookPointer) (*NotebookPointer, error)

	// List returns all pointers for the tenant (used by recall's
	// resolveNotebookSet to filter to existing scopes without creating any).
	List(ctx context.Context, tenant uuid.UUID) ([]NotebookPointer, error)
}
