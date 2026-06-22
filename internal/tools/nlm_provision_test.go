package tools

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// fakeDocLibrary records the order of Doc operations and returns deterministic
// ids. Implements nlmdoc.DocLibrary.
type fakeDocLibrary struct {
	mu          sync.Mutex
	ops         []string
	ensureCalls int32
	createCalls int32
	docCounter  int32
}

func (f *fakeDocLibrary) EnsureFolder(ctx context.Context, path []string) (string, error) {
	atomic.AddInt32(&f.ensureCalls, 1)
	f.mu.Lock()
	f.ops = append(f.ops, "ensure:"+fmt.Sprint(path))
	f.mu.Unlock()
	return "folder-1", nil
}

func (f *fakeDocLibrary) CreateDoc(ctx context.Context, folderID, title, initialText string) (string, error) {
	n := atomic.AddInt32(&f.createCalls, 1)
	f.mu.Lock()
	f.ops = append(f.ops, "createdoc:"+title)
	f.mu.Unlock()
	return fmt.Sprintf("doc-%d", n), nil
}

func (f *fakeDocLibrary) AppendText(ctx context.Context, docID, text string) error { return nil }

func (f *fakeDocLibrary) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.ops))
	copy(out, f.ops)
	return out
}

// fakeNotebookRunner records notebook ops and returns deterministic ids.
type fakeNotebookRunner struct {
	mu          sync.Mutex
	ops         []string
	createCalls int32
	addCalls    int32
	deleteCalls int32
	nbCounter   int32
	addErr      error
}

func (r *fakeNotebookRunner) CreateNotebook(ctx context.Context, title string) (string, error) {
	n := atomic.AddInt32(&r.createCalls, 1)
	r.mu.Lock()
	r.ops = append(r.ops, "nbcreate:"+title)
	r.mu.Unlock()
	return fmt.Sprintf("nb-%d", n), nil
}

func (r *fakeNotebookRunner) AddDriveSource(ctx context.Context, notebookID, driveDocID string) error {
	atomic.AddInt32(&r.addCalls, 1)
	r.mu.Lock()
	r.ops = append(r.ops, "sourceadd:"+notebookID+"/"+driveDocID)
	r.mu.Unlock()
	return r.addErr
}

func (r *fakeNotebookRunner) DeleteNotebook(ctx context.Context, notebookID string) error {
	atomic.AddInt32(&r.deleteCalls, 1)
	r.mu.Lock()
	r.ops = append(r.ops, "nbdelete:"+notebookID)
	r.mu.Unlock()
	return nil
}

func newProvisioner(st store.NotebookPointerStore, docs *fakeDocLibrary, nb *fakeNotebookRunner) *NotebookProvisioner {
	return NewNotebookProvisioner(st, docs, nb, "goclaw-memory-test")
}

func TestGetOrCreate_MissCreatesInOrder(t *testing.T) {
	st := newFakePointerStore()
	docs := &fakeDocLibrary{}
	nb := &fakeNotebookRunner{}
	p := newProvisioner(st, docs, nb)

	ctx := store.WithTenantSlug(dmCtx(), "esmith")
	nbID, docID, err := p.GetOrCreateScopeNotebook(ctx, testTenant(), store.ScopeKindUser, "user-777", "王小明")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if nbID != "nb-1" || docID != "doc-1" {
		t.Fatalf("got nb=%q doc=%q, want nb-1/doc-1", nbID, docID)
	}

	// Verify creation ORDER: folder → doc → nbcreate → sourceadd.
	docOps := docs.order()
	if len(docOps) != 2 || docOps[0][:6] != "ensure" || docOps[1][:9] != "createdoc" {
		t.Fatalf("doc ops order = %v, want [ensure..., createdoc...]", docOps)
	}
	nb.mu.Lock()
	nbOps := append([]string(nil), nb.ops...)
	nb.mu.Unlock()
	if len(nbOps) != 2 || nbOps[0][:8] != "nbcreate" || nbOps[1][:9] != "sourceadd" {
		t.Fatalf("nb ops order = %v, want [nbcreate, sourceadd]", nbOps)
	}

	// Pointer persisted.
	got, ok, _ := st.Get(ctx, testTenant(), store.ScopeKindUser, "user-777")
	if !ok {
		t.Fatal("pointer not persisted")
	}
	if got.NotebookID != "nb-1" || got.DriveDocID != "doc-1" || got.DisplayName != "王小明" {
		t.Fatalf("pointer = %+v", got)
	}
	if st.inserted != 1 {
		t.Fatalf("inserted = %d, want 1", st.inserted)
	}
}

func TestGetOrCreate_HitReturnsExistingNoCreate(t *testing.T) {
	st := newFakePointerStore()
	tenant := testTenant()
	st.rows[ptrKey(tenant, store.ScopeKindShared, "")] = store.NotebookPointer{
		TenantID: tenant, ScopeKind: store.ScopeKindShared, NotebookID: "nb-existing", DriveDocID: "doc-existing",
	}
	docs := &fakeDocLibrary{}
	nb := &fakeNotebookRunner{}
	p := newProvisioner(st, docs, nb)

	nbID, docID, err := p.GetOrCreateScopeNotebook(dmCtx(), tenant, store.ScopeKindShared, "", "")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if nbID != "nb-existing" || docID != "doc-existing" {
		t.Fatalf("got nb=%q doc=%q, want existing", nbID, docID)
	}
	// Nothing created.
	if docs.ensureCalls != 0 || docs.createCalls != 0 {
		t.Fatalf("doc creation happened on a hit: ensure=%d create=%d", docs.ensureCalls, docs.createCalls)
	}
	if nb.createCalls != 0 {
		t.Fatalf("notebook created on a hit: %d", nb.createCalls)
	}
}

func TestGetOrCreate_AddSourceFailureCleansUp(t *testing.T) {
	st := newFakePointerStore()
	docs := &fakeDocLibrary{}
	nb := &fakeNotebookRunner{addErr: fmt.Errorf("nlm down")}
	p := newProvisioner(st, docs, nb)

	_, _, err := p.GetOrCreateScopeNotebook(dmCtx(), testTenant(), store.ScopeKindUser, "user-777", "")
	if err == nil {
		t.Fatal("expected error when source add fails")
	}
	// Our orphan notebook must be deleted.
	if nb.deleteCalls != 1 {
		t.Fatalf("delete calls = %d, want 1 (cleanup orphan)", nb.deleteCalls)
	}
	// No pointer persisted.
	if _, ok, _ := st.Get(context.Background(), testTenant(), store.ScopeKindUser, "user-777"); ok {
		t.Fatal("pointer must not persist when source add fails")
	}
}

func TestGetOrCreate_RaceOnlyOneWins(t *testing.T) {
	st := newFakePointerStore()
	docs := &fakeDocLibrary{}
	nb := &fakeNotebookRunner{}
	p := newProvisioner(st, docs, nb)
	ctx := store.WithTenantSlug(dmCtx(), "esmith")

	const goroutines = 8
	var wg sync.WaitGroup
	results := make([]string, goroutines)
	errs := make([]error, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			nbID, _, err := p.GetOrCreateScopeNotebook(ctx, testTenant(), store.ScopeKindGroup, "chat-race", "群")
			results[idx] = nbID
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d errored: %v", i, err)
		}
	}
	// All callers must agree on the SAME winning notebook id.
	winner := results[0]
	for i, r := range results {
		if r != winner {
			t.Fatalf("goroutine %d got %q, winner is %q — race produced divergent results", i, r, winner)
		}
	}
	// Exactly one pointer row materialised regardless of concurrency.
	if st.inserted != 1 {
		t.Fatalf("inserted = %d, want exactly 1", st.inserted)
	}
	// Losers must have cleaned up their orphan notebooks. With N racers, at
	// most one notebook survives; deletes == (notebooks created - 1).
	created := int(nb.createCalls)
	if int(nb.deleteCalls) != created-1 {
		t.Fatalf("deletes = %d, created = %d, want deletes == created-1 (orphan cleanup)", nb.deleteCalls, created)
	}
}
