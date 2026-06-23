//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/store/pg"
)

func TestNotebookPointerStore_GetMissCreateHit(t *testing.T) {
	db := testDB(t)
	tenantID, _ := seedTenantAgent(t, db)
	s := pg.NewPGNotebookPointerStore(db)
	ctx := context.Background()

	t.Cleanup(func() {
		db.Exec("DELETE FROM nlm_notebooks WHERE tenant_id = $1", tenantID)
	})

	// Miss: no pointer yet.
	if _, ok, err := s.Get(ctx, tenantID, store.ScopeKindUser, "user-1"); err != nil {
		t.Fatalf("Get miss: %v", err)
	} else if ok {
		t.Fatal("expected miss before any create")
	}

	// Create → returns our row.
	winner, err := s.Create(ctx, store.NotebookPointer{
		TenantID: tenantID, ScopeKind: store.ScopeKindUser, ScopeID: "user-1",
		NotebookID: "nb-1", DriveDocID: "doc-1", DisplayName: "王小明",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if winner.NotebookID != "nb-1" || winner.DriveDocID != "doc-1" || winner.DisplayName != "王小明" {
		t.Fatalf("winner = %+v", winner)
	}
	if winner.CreatedAt.IsZero() {
		t.Fatal("created_at not populated by DB default")
	}

	// Hit: Get now returns the row.
	got, ok, err := s.Get(ctx, tenantID, store.ScopeKindUser, "user-1")
	if err != nil || !ok {
		t.Fatalf("Get hit: ok=%v err=%v", ok, err)
	}
	if got.NotebookID != "nb-1" {
		t.Fatalf("got = %+v", got)
	}
}

func TestNotebookPointerStore_OnConflictReturnsExisting(t *testing.T) {
	db := testDB(t)
	tenantID, _ := seedTenantAgent(t, db)
	s := pg.NewPGNotebookPointerStore(db)
	ctx := context.Background()
	t.Cleanup(func() {
		db.Exec("DELETE FROM nlm_notebooks WHERE tenant_id = $1", tenantID)
	})

	first, err := s.Create(ctx, store.NotebookPointer{
		TenantID: tenantID, ScopeKind: store.ScopeKindShared, ScopeID: "",
		NotebookID: "nb-winner", DriveDocID: "doc-winner",
	})
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Second Create for the SAME (tenant, kind, id) but different ids → must
	// NOT overwrite; returns the EXISTING winner row.
	second, err := s.Create(ctx, store.NotebookPointer{
		TenantID: tenantID, ScopeKind: store.ScopeKindShared, ScopeID: "",
		NotebookID: "nb-loser", DriveDocID: "doc-loser",
	})
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if second.NotebookID != "nb-winner" {
		t.Fatalf("ON CONFLICT did not return existing: got %q, want nb-winner", second.NotebookID)
	}
	if second.NotebookID != first.NotebookID {
		t.Fatalf("second %q != first %q", second.NotebookID, first.NotebookID)
	}

	// DB still has exactly one row for this scope.
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM nlm_notebooks WHERE tenant_id=$1 AND scope_kind=$2 AND scope_id=$3`,
		tenantID, store.ScopeKindShared, "").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1", count)
	}
}

func TestNotebookPointerStore_ConcurrentCreateRaceSafe(t *testing.T) {
	db := testDB(t)
	tenantID, _ := seedTenantAgent(t, db)
	s := pg.NewPGNotebookPointerStore(db)
	ctx := context.Background()
	t.Cleanup(func() {
		db.Exec("DELETE FROM nlm_notebooks WHERE tenant_id = $1", tenantID)
	})

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	winners := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			w, err := s.Create(ctx, store.NotebookPointer{
				TenantID: tenantID, ScopeKind: store.ScopeKindGroup, ScopeID: "chat-1",
				NotebookID: "nb-" + string(rune('A'+idx)), DriveDocID: "doc",
			})
			if err != nil {
				errs[idx] = err
				return
			}
			winners[idx] = w.NotebookID
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	// All callers must converge on the same winning notebook id.
	want := winners[0]
	for i, w := range winners {
		if w != want {
			t.Fatalf("goroutine %d got %q, want all == %q", i, w, want)
		}
	}
	// Exactly one row in the DB.
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM nlm_notebooks WHERE tenant_id=$1 AND scope_kind=$2 AND scope_id=$3`,
		tenantID, store.ScopeKindGroup, "chat-1").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want exactly 1", count)
	}
}

func TestNotebookPointerStore_ListAndTenantIsolation(t *testing.T) {
	db := testDB(t)
	tenantA, _ := seedTenantAgent(t, db)
	tenantB, _ := seedTenantAgent(t, db)
	s := pg.NewPGNotebookPointerStore(db)
	ctx := context.Background()
	t.Cleanup(func() {
		db.Exec("DELETE FROM nlm_notebooks WHERE tenant_id IN ($1,$2)", tenantA, tenantB)
	})

	if _, err := s.Create(ctx, store.NotebookPointer{TenantID: tenantA, ScopeKind: store.ScopeKindShared, NotebookID: "a-shared", DriveDocID: "d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, store.NotebookPointer{TenantID: tenantA, ScopeKind: store.ScopeKindUser, ScopeID: "u1", NotebookID: "a-user", DriveDocID: "d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, store.NotebookPointer{TenantID: tenantB, ScopeKind: store.ScopeKindShared, NotebookID: "b-shared", DriveDocID: "d"}); err != nil {
		t.Fatal(err)
	}

	listA, err := s.List(ctx, tenantA)
	if err != nil {
		t.Fatalf("List A: %v", err)
	}
	if len(listA) != 2 {
		t.Fatalf("tenant A list = %d, want 2 (no cross-tenant leak)", len(listA))
	}
	for _, p := range listA {
		if p.TenantID != tenantA {
			t.Fatalf("tenant A list leaked tenant %s", p.TenantID)
		}
	}

	// Cross-tenant Get must NOT see the other tenant's pointer.
	if _, ok, _ := s.Get(ctx, tenantB, store.ScopeKindUser, "u1"); ok {
		t.Fatal("tenant B saw tenant A's user pointer — isolation breach")
	}
}
