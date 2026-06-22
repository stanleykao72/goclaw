package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// PGIngestCursorStore implements store.IngestCursorStore backed by Postgres
// (nlm_ingest_cursor table). The tenant is an explicit argument (mirroring
// PGNotebookPointerStore) so the WHERE/UPSERT carry it directly — cross-tenant
// isolation lives in the predicate, not in ctx scoping.
type PGIngestCursorStore struct {
	db *sql.DB
}

// NewPGIngestCursorStore creates a new PGIngestCursorStore.
func NewPGIngestCursorStore(db *sql.DB) *PGIngestCursorStore {
	return &PGIngestCursorStore{db: db}
}

const ingestCursorColumns = `tenant_id, channel_name, history_key, last_created_at, last_id, updated_at`

// Get returns the cursor for (tenant, channelName, historyKey). On a miss it
// returns the zero cursor (epoch / nil id) with ok=false so the first drain
// reads the whole history for the scope.
func (s *PGIngestCursorStore) Get(ctx context.Context, tenant uuid.UUID, channelName, historyKey string) (store.IngestCursor, bool, error) {
	var c store.IngestCursor
	err := pkgSqlxDB.GetContext(ctx, &c,
		`SELECT `+ingestCursorColumns+`
		   FROM nlm_ingest_cursor
		  WHERE tenant_id = $1 AND channel_name = $2 AND history_key = $3`,
		tenant, channelName, historyKey,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return store.IngestCursor{
			TenantID:    tenant,
			ChannelName: channelName,
			HistoryKey:  historyKey,
		}, false, nil
	}
	if err != nil {
		return store.IngestCursor{}, false, fmt.Errorf("get ingest cursor: %w", err)
	}
	return c, true, nil
}

// Upsert advances (or creates) the cursor. ON CONFLICT on the natural key moves
// the high-water forward to the passed (last_created_at, last_id). The GREATEST
// guard makes the advance monotonic: a concurrent or out-of-order Upsert can
// never rewind the cursor (which would re-ingest already-drained messages).
func (s *PGIngestCursorStore) Upsert(ctx context.Context, c store.IngestCursor) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO nlm_ingest_cursor
		    (tenant_id, channel_name, history_key, last_created_at, last_id, updated_at)
		 VALUES ($1, $2, $3, $4, $5, NOW())
		 ON CONFLICT (tenant_id, channel_name, history_key) DO UPDATE
		    SET last_created_at = GREATEST(nlm_ingest_cursor.last_created_at, EXCLUDED.last_created_at),
		        last_id = CASE
		            WHEN EXCLUDED.last_created_at > nlm_ingest_cursor.last_created_at THEN EXCLUDED.last_id
		            WHEN EXCLUDED.last_created_at = nlm_ingest_cursor.last_created_at
		                 AND EXCLUDED.last_id > nlm_ingest_cursor.last_id THEN EXCLUDED.last_id
		            ELSE nlm_ingest_cursor.last_id
		        END,
		        updated_at = NOW()`,
		c.TenantID, c.ChannelName, c.HistoryKey, c.LastCreatedAt, c.LastID,
	)
	if err != nil {
		return fmt.Errorf("upsert ingest cursor: %w", err)
	}
	return nil
}
