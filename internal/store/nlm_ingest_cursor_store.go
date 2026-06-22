package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// IngestCursor is the per-scope high-water for the NotebookLM ingest worker
// (sub-phase 2.3). One row per (TenantID, ChannelName, HistoryKey) = one row per
// scope/Doc. The cursor is a composite (LastCreatedAt, LastID): the window read
// is rows WHERE (created_at, id) > (LastCreatedAt, LastID), making it
// collision-safe even when AppendBatch stamps one created_at across a batch.
//
// The cursor is the SINGLE serialization point per Doc — advancing it is the
// worker's only durable state, and it never deletes the buffered rows it reads.
type IngestCursor struct {
	TenantID      uuid.UUID `db:"tenant_id"`
	ChannelName   string    `db:"channel_name"`
	HistoryKey    string    `db:"history_key"`
	LastCreatedAt time.Time `db:"last_created_at"`
	LastID        uuid.UUID `db:"last_id"`
	UpdatedAt     time.Time `db:"updated_at"`
}

// IngestCursorStore persists the NotebookLM ingest worker's per-scope high-water.
// The tenant is carried in the IngestCursor (and matched in the WHERE/UPSERT) so
// a cross-tenant drain advances each tenant's cursor independently. Modeled on
// NotebookPointerStore (explicit, db-less injectable for tests).
type IngestCursorStore interface {
	// Get returns the cursor for (tenant, channelName, historyKey). On a miss it
	// returns the zero cursor (LastCreatedAt = epoch, LastID = nil) with ok=false
	// so the first drain reads the whole history for that scope.
	Get(ctx context.Context, tenant uuid.UUID, channelName, historyKey string) (IngestCursor, bool, error)

	// Upsert advances (or creates) the cursor for its (tenant, channel, key). The
	// caller passes the max (created_at, id) of the window it successfully drained.
	Upsert(ctx context.Context, c IngestCursor) error
}
