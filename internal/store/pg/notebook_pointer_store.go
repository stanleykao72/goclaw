package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// PGNotebookPointerStore implements store.NotebookPointerStore backed by
// Postgres (nlm_notebooks table).
type PGNotebookPointerStore struct {
	db *sql.DB
}

// NewPGNotebookPointerStore creates a new PGNotebookPointerStore.
func NewPGNotebookPointerStore(db *sql.DB) *PGNotebookPointerStore {
	return &PGNotebookPointerStore{db: db}
}

// notebookPointerColumns is the SELECT projection, ordered to match the
// db-tagged fields scanned via sqlx.
const notebookPointerColumns = `tenant_id, scope_kind, scope_id, notebook_id, drive_doc_id, display_name, created_at`

// Get returns the pointer for (tenant, scopeKind, scopeID), or (nil, false, nil)
// when none exists. The tenant filter is the explicit argument (a verified UUID
// resolved by the provisioner), giving cross-tenant isolation directly in the
// WHERE clause.
func (s *PGNotebookPointerStore) Get(ctx context.Context, tenant uuid.UUID, scopeKind, scopeID string) (*store.NotebookPointer, bool, error) {
	var p store.NotebookPointer
	err := pkgSqlxDB.GetContext(ctx, &p,
		`SELECT `+notebookPointerColumns+`
		   FROM nlm_notebooks
		  WHERE tenant_id = $1 AND scope_kind = $2 AND scope_id = $3`,
		tenant, scopeKind, scopeID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get notebook pointer: %w", err)
	}
	return &p, true, nil
}

// Create inserts a pointer race-safely. RACE MODEL:
//
//  1. INSERT ... ON CONFLICT (tenant_id, scope_kind, scope_id) DO NOTHING.
//  2. If rows were affected, WE won → re-SELECT and return our row.
//  3. If RowsAffected == 0, a concurrent caller already inserted this scope →
//     re-SELECT returns THEIR row.
//
// A bare INSERT would error (and lose the winner's row) on the second racer;
// DO-NOTHING + re-SELECT guarantees every caller ends with the single winning
// pointer. The UNIQUE(tenant_id, scope_kind, scope_id) constraint is the
// arbiter — exactly one INSERT materialises a row.
func (s *PGNotebookPointerStore) Create(ctx context.Context, p store.NotebookPointer) (*store.NotebookPointer, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO nlm_notebooks
		    (tenant_id, scope_kind, scope_id, notebook_id, drive_doc_id, display_name)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (tenant_id, scope_kind, scope_id) DO NOTHING`,
		p.TenantID, p.ScopeKind, p.ScopeID, p.NotebookID, p.DriveDocID, p.DisplayName,
	)
	if err != nil {
		return nil, fmt.Errorf("insert notebook pointer: %w", err)
	}

	// Whether we won (affected==1) or a racer won (affected==0), the row now
	// exists — re-SELECT returns the authoritative winning pointer.
	existing, ok, err := s.Get(ctx, p.TenantID, p.ScopeKind, p.ScopeID)
	if err != nil {
		return nil, err
	}
	if !ok {
		// Should be unreachable: we just inserted-or-conflicted on this key.
		return nil, fmt.Errorf("notebook pointer vanished after upsert: %s/%s/%s",
			p.TenantID, p.ScopeKind, p.ScopeID)
	}
	_ = res
	return existing, nil
}

// List returns all pointers for the tenant.
func (s *PGNotebookPointerStore) List(ctx context.Context, tenant uuid.UUID) ([]store.NotebookPointer, error) {
	var out []store.NotebookPointer
	err := pkgSqlxDB.SelectContext(ctx, &out,
		`SELECT `+notebookPointerColumns+`
		   FROM nlm_notebooks
		  WHERE tenant_id = $1
		  ORDER BY scope_kind, scope_id`,
		tenant,
	)
	if err != nil {
		return nil, fmt.Errorf("list notebook pointers: %w", err)
	}
	return out, nil
}
