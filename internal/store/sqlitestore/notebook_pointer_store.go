//go:build sqlite || sqliteonly

package sqlitestore

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// SQLiteNotebookPointerStore is a stub: the goclaw × NotebookLM memory system
// targets the PostgreSQL production backend only. It satisfies the interface so
// `-tags sqlite`/`sqliteonly` builds compile and the Stores field is non-nil,
// but every method returns a clear "unsupported" error rather than silently
// no-oping (which would mask misconfiguration on a SQLite deployment).
type SQLiteNotebookPointerStore struct{}

// NewSQLiteNotebookPointerStore constructs the stub.
func NewSQLiteNotebookPointerStore() *SQLiteNotebookPointerStore {
	return &SQLiteNotebookPointerStore{}
}

var errNotebookPointerSQLiteUnsupported = fmt.Errorf("nlm_notebooks: NotebookLM memory requires the PostgreSQL backend")

func (s *SQLiteNotebookPointerStore) Get(ctx context.Context, tenant uuid.UUID, scopeKind, scopeID string) (*store.NotebookPointer, bool, error) {
	return nil, false, errNotebookPointerSQLiteUnsupported
}

func (s *SQLiteNotebookPointerStore) Create(ctx context.Context, p store.NotebookPointer) (*store.NotebookPointer, error) {
	return nil, errNotebookPointerSQLiteUnsupported
}

func (s *SQLiteNotebookPointerStore) List(ctx context.Context, tenant uuid.UUID) ([]store.NotebookPointer, error) {
	return nil, errNotebookPointerSQLiteUnsupported
}
