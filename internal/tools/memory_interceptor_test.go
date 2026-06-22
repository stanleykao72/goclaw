package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// mockMemoryStore is a minimal in-memory implementation of store.MemoryStore
// for unit testing the MemoryInterceptor.
type mockMemoryStore struct {
	docs map[string]string // key: "agentID|userID|path"
}

func newMockMemoryStore() *mockMemoryStore {
	return &mockMemoryStore{docs: make(map[string]string)}
}

func docKey(agentID, userID, path string) string {
	return agentID + "|" + userID + "|" + path
}

func (m *mockMemoryStore) GetDocument(_ context.Context, agentID, userID, path string) (string, error) {
	if v, ok := m.docs[docKey(agentID, userID, path)]; ok {
		return v, nil
	}
	return "", fmt.Errorf("not found")
}

func (m *mockMemoryStore) PutDocument(_ context.Context, agentID, userID, path, content string) error {
	m.docs[docKey(agentID, userID, path)] = content
	return nil
}

func (m *mockMemoryStore) DeleteDocument(_ context.Context, agentID, userID, path string) error {
	delete(m.docs, docKey(agentID, userID, path))
	return nil
}

func (m *mockMemoryStore) ListDocuments(_ context.Context, agentID, userID string) ([]store.DocumentInfo, error) {
	var out []store.DocumentInfo
	prefix := agentID + "|" + userID + "|"
	for k := range m.docs {
		if after, ok := strings.CutPrefix(k, prefix); ok {
			path := after
			out = append(out, store.DocumentInfo{Path: path})
		}
	}
	return out, nil
}

// Unused interface methods — satisfy store.MemoryStore.
func (m *mockMemoryStore) ListAllDocumentsGlobal(_ context.Context) ([]store.DocumentInfo, error) {
	return nil, nil
}
func (m *mockMemoryStore) ListAllDocuments(_ context.Context, _ string) ([]store.DocumentInfo, error) {
	return nil, nil
}
func (m *mockMemoryStore) GetDocumentDetail(_ context.Context, _, _, _ string) (*store.DocumentDetail, error) {
	return nil, nil
}
func (m *mockMemoryStore) ListChunks(_ context.Context, _, _, _ string) ([]store.ChunkInfo, error) {
	return nil, nil
}
func (m *mockMemoryStore) Search(_ context.Context, _ string, _, _ string, _ store.MemorySearchOptions) ([]store.MemorySearchResult, error) {
	return nil, nil
}
func (m *mockMemoryStore) IndexDocument(_ context.Context, _, _, _ string) error { return nil }
func (m *mockMemoryStore) IndexAll(_ context.Context, _, _ string) error         { return nil }
func (m *mockMemoryStore) SetEmbeddingProvider(_ store.EmbeddingProvider)        {}
func (m *mockMemoryStore) Close() error                                          { return nil }

// --- Test helpers ---

func memCtx(agentID uuid.UUID, userID, leaderID string) context.Context {
	ctx := context.Background()
	ctx = store.WithAgentID(ctx, agentID)
	ctx = store.WithUserID(ctx, userID)
	if leaderID != "" {
		ctx = WithLeaderAgentID(ctx, leaderID)
	}
	return ctx
}

// --- ReadFile tests ---

func TestReadFile_NoLeader_OwnMemory(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	agentID := uuid.New()

	ms.docs[docKey(agentID.String(), "user1", "MEMORY.md")] = "my notes"

	ctx := memCtx(agentID, "user1", "")
	content, handled, err := mi.ReadFile(ctx, "MEMORY.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if content != "my notes" {
		t.Errorf("expected 'my notes', got %q", content)
	}
}

func TestReadFile_LeaderFallback(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	memberID := uuid.New()
	leaderID := uuid.New()

	// Leader has memory, member does not.
	ms.docs[docKey(leaderID.String(), "user1", "MEMORY.md")] = "leader notes"

	ctx := memCtx(memberID, "user1", leaderID.String())
	content, handled, err := mi.ReadFile(ctx, "MEMORY.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if content != "leader notes" {
		t.Errorf("expected 'leader notes', got %q", content)
	}
}

func TestReadFile_LeaderFallback_SharedScope(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	memberID := uuid.New()
	leaderID := uuid.New()

	// Leader has shared (global) memory only.
	ms.docs[docKey(leaderID.String(), "", "MEMORY.md")] = "leader shared"

	ctx := memCtx(memberID, "user1", leaderID.String())
	content, handled, err := mi.ReadFile(ctx, "MEMORY.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if content != "leader shared" {
		t.Errorf("expected 'leader shared', got %q", content)
	}
}

func TestReadFile_LeaderIsSelf(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	agentID := uuid.New()

	ms.docs[docKey(agentID.String(), "user1", "MEMORY.md")] = "own notes"

	// Leader is the same agent — should read own memory, no fallback.
	ctx := memCtx(agentID, "user1", agentID.String())
	content, handled, err := mi.ReadFile(ctx, "MEMORY.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if content != "own notes" {
		t.Errorf("expected 'own notes', got %q", content)
	}
}

func TestReadFile_NonMemoryPath(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")

	ctx := memCtx(uuid.New(), "user1", "")
	_, handled, err := mi.ReadFile(ctx, "README.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Error("expected handled=false for non-memory path")
	}
}

func TestReadFile_MemberNoMemory_NoLeader_Empty(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")

	ctx := memCtx(uuid.New(), "user1", "")
	content, handled, err := mi.ReadFile(ctx, "MEMORY.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true for memory path")
	}
	if content != "" {
		t.Errorf("expected empty content, got %q", content)
	}
}

// --- memory_mode gate tests ---

func TestInterceptor_NotebookMode_WriteFallsThrough(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	agentID := uuid.New()

	// notebook mode: a memory-path write must NOT be intercepted (Handled=false),
	// so write_file falls through to a normal host write. Nothing is persisted to
	// the memory store.
	ctx := store.WithMemoryMode(memCtx(agentID, "user1", ""), store.MemoryModeNotebook)
	result, err := mi.WriteFile(ctx, "MEMORY.md", "new content", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Handled {
		t.Fatal("notebook mode: expected Handled=false (write falls through)")
	}
	if _, ok := ms.docs[docKey(agentID.String(), "user1", "MEMORY.md")]; ok {
		t.Error("notebook mode: memory store must not be written")
	}
}

func TestInterceptor_NotebookMode_ReadFallsThrough(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	agentID := uuid.New()
	ms.docs[docKey(agentID.String(), "user1", "MEMORY.md")] = "my notes"

	ctx := store.WithMemoryMode(memCtx(agentID, "user1", ""), store.MemoryModeNotebook)
	_, handled, err := mi.ReadFile(ctx, "MEMORY.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Error("notebook mode: expected handled=false (read falls through)")
	}
}

func TestInterceptor_NotebookMode_ListFallsThrough(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	agentID := uuid.New()

	ctx := store.WithMemoryMode(memCtx(agentID, "user1", ""), store.MemoryModeNotebook)
	_, handled, err := mi.ListFiles(ctx, "memory")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Error("notebook mode: expected handled=false (list falls through)")
	}
}

func TestInterceptor_NotebookMode_WouldRouteVaultFalse(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/vault", "/vault") // vault dir configured
	agentID := uuid.New()

	// Even with backend=vault, notebook mode must NOT exempt the ACL — the write
	// is not intercepted into the vault, so the file-writer permission applies.
	ctx := store.WithMemoryBackend(memCtx(agentID, "user1", ""), "vault")
	ctx = store.WithMemoryMode(ctx, store.MemoryModeNotebook)
	if mi.WouldRouteVault(ctx, "MEMORY.md") {
		t.Error("notebook mode: WouldRouteVault must be false")
	}
}

func TestInterceptor_BothMode_StillIndexes(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	agentID := uuid.New()

	// both mode (default) → memory write is intercepted + persisted, unchanged.
	ctx := store.WithMemoryMode(memCtx(agentID, "user1", ""), store.MemoryModeBoth)
	result, err := mi.WriteFile(ctx, "MEMORY.md", "kept", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Handled {
		t.Fatal("both mode: expected Handled=true (intercepted)")
	}
	if got := ms.docs[docKey(agentID.String(), "user1", "MEMORY.md")]; got != "kept" {
		t.Errorf("both mode: expected persisted 'kept', got %q", got)
	}
}

func TestInterceptor_UnsetMode_DefaultsToBoth(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	agentID := uuid.New()

	// No memory_mode injected → default "both" → interception unchanged.
	ctx := memCtx(agentID, "user1", "")
	result, err := mi.WriteFile(ctx, "MEMORY.md", "kept", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Handled {
		t.Fatal("unset mode: expected Handled=true (defaults to both)")
	}
}

// --- WriteFile tests ---

func TestWriteFile_NoLeader_AllowWrite(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	agentID := uuid.New()

	ctx := memCtx(agentID, "user1", "")
	result, err := mi.WriteFile(ctx, "MEMORY.md", "new content", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Handled {
		t.Fatal("expected handled=true")
	}

	// Verify content was written.
	got, _ := ms.GetDocument(ctx, agentID.String(), "user1", "MEMORY.md")
	if got != "new content" {
		t.Errorf("expected 'new content', got %q", got)
	}
}

func TestWriteFile_LeaderPresent_BlockWrite(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	memberID := uuid.New()
	leaderID := uuid.New()

	ctx := memCtx(memberID, "user1", leaderID.String())
	result, err := mi.WriteFile(ctx, "MEMORY.md", "attempt", false)
	if err == nil {
		t.Fatal("expected error for blocked write")
	}
	if !result.Handled {
		t.Fatal("expected handled=true")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("expected read-only error, got: %v", err)
	}
}

func TestWriteFile_LeaderIsSelf_AllowWrite(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	agentID := uuid.New()

	// Leader is the same agent — should allow write.
	ctx := memCtx(agentID, "user1", agentID.String())
	result, err := mi.WriteFile(ctx, "MEMORY.md", "leader writes", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Handled {
		t.Fatal("expected handled=true")
	}

	got, _ := ms.GetDocument(ctx, agentID.String(), "user1", "MEMORY.md")
	if got != "leader writes" {
		t.Errorf("expected 'leader writes', got %q", got)
	}
}

// --- MemoryGetTool leader fallback tests ---

func TestMemoryGet_LeaderFallback(t *testing.T) {
	ms := newMockMemoryStore()
	tool := NewMemoryGetTool()
	tool.SetMemoryStore(ms)

	memberID := uuid.New()
	leaderID := uuid.New()

	ms.docs[docKey(leaderID.String(), "user1", "MEMORY.md")] = "leader get content"

	ctx := memCtx(memberID, "user1", leaderID.String())
	result := tool.Execute(ctx, map[string]any{"path": "MEMORY.md"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "leader get content") {
		t.Errorf("expected leader content in result, got: %s", result.ForLLM)
	}
}

func TestMemoryGet_BlockedByNoLeader(t *testing.T) {
	ms := newMockMemoryStore()
	tool := NewMemoryGetTool()
	tool.SetMemoryStore(ms)

	memberID := uuid.New()
	// No leader, no own memory → error.
	ctx := memCtx(memberID, "user1", "")
	result := tool.Execute(ctx, map[string]any{"path": "MEMORY.md"})
	if !result.IsError {
		t.Fatal("expected error for missing memory")
	}
}

// --- MemorySearchTool leader fallback tests ---

func TestMemorySearch_LeaderFallback(t *testing.T) {
	ms := newMockMemoryStore()
	tool := NewMemorySearchTool()
	tool.SetMemoryStore(ms)

	memberID := uuid.New()
	leaderID := uuid.New()

	// mockMemoryStore.Search returns nil — just verify no crash and correct agent IDs used.
	ctx := memCtx(memberID, "user1", leaderID.String())
	result := tool.Execute(ctx, map[string]any{"query": "test"})
	// With mock returning nil results for both, should get "No memory results found".
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "No memory results found") {
		t.Errorf("expected no results message, got: %s", result.ForLLM)
	}
}

// --- vault read-tool memory_mode gate tests ---

func TestMemorySearch_NotebookMode_NoOp(t *testing.T) {
	ms := newMockMemoryStore()
	tool := NewMemorySearchTool()
	tool.SetMemoryStore(ms)

	// notebook mode → empty (non-error) result without touching the store.
	ctx := store.WithMemoryMode(memCtx(uuid.New(), "user1", ""), store.MemoryModeNotebook)
	result := tool.Execute(ctx, map[string]any{"query": "anything"})
	if result.IsError {
		t.Fatalf("notebook mode must no-op, not error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "No memory results found") {
		t.Errorf("expected empty-results message, got: %s", result.ForLLM)
	}
}

func TestMemorySearch_NotebookMode_NoStore_StillNoOp(t *testing.T) {
	// Even with NO memory store wired, notebook mode returns the empty result
	// (gate fires before the "memory system not available" error path).
	tool := NewMemorySearchTool()
	ctx := store.WithMemoryMode(memCtx(uuid.New(), "user1", ""), store.MemoryModeNotebook)
	result := tool.Execute(ctx, map[string]any{"query": "anything"})
	if result.IsError {
		t.Fatalf("notebook mode must no-op even without a store: %s", result.ForLLM)
	}
}

func TestMemoryGet_NotebookMode_NoOp(t *testing.T) {
	ms := newMockMemoryStore()
	tool := NewMemoryGetTool()
	tool.SetMemoryStore(ms)
	agentID := uuid.New()
	ms.docs[docKey(agentID.String(), "user1", "MEMORY.md")] = "secret notes"

	ctx := store.WithMemoryMode(memCtx(agentID, "user1", ""), store.MemoryModeNotebook)
	result := tool.Execute(ctx, map[string]any{"path": "MEMORY.md"})
	if result.IsError {
		t.Fatalf("notebook mode must no-op, not error: %s", result.ForLLM)
	}
	if strings.Contains(result.ForLLM, "secret notes") {
		t.Errorf("notebook mode must NOT return vault content, got: %s", result.ForLLM)
	}
}

func TestMemoryExpand_NotebookMode_NoOp(t *testing.T) {
	tool := NewMemoryExpandTool()
	epID := uuid.New()
	ep := &fakeEpisodicStoreRead{byID: map[string]*store.EpisodicSummary{
		epID.String(): {ID: epID, Summary: "deep secret"},
	}}
	tool.SetEpisodicStore(ep)

	ctx := store.WithMemoryMode(context.Background(), store.MemoryModeNotebook)
	result := tool.Execute(ctx, map[string]any{"id": epID.String()})
	if result.IsError {
		t.Fatalf("notebook mode must no-op, not error: %s", result.ForLLM)
	}
	if strings.Contains(result.ForLLM, "deep secret") {
		t.Errorf("notebook mode must NOT return episodic content, got: %s", result.ForLLM)
	}
}

func TestMemorySearch_BothMode_StillSearches(t *testing.T) {
	ms := newMockMemoryStore()
	tool := NewMemorySearchTool()
	tool.SetMemoryStore(ms)

	// both mode (default) → search runs (mock returns nil → "No memory results"),
	// proving the gate did NOT short-circuit before the real search path.
	ctx := store.WithMemoryMode(memCtx(uuid.New(), "user1", ""), store.MemoryModeBoth)
	result := tool.Execute(ctx, map[string]any{"query": "test"})
	if result.IsError {
		t.Fatalf("both mode unexpected error: %s", result.ForLLM)
	}
}

// --- ListFiles tests ---

func TestListFiles_MergeLeaderDocs(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	memberID := uuid.New()
	leaderID := uuid.New()

	// Leader has docs, member has none.
	ms.docs[docKey(leaderID.String(), "user1", "MEMORY.md")] = "leader mem"
	ms.docs[docKey(leaderID.String(), "user1", "memory/notes.md")] = "leader notes"

	ctx := memCtx(memberID, "user1", leaderID.String())
	listing, handled, err := mi.ListFiles(ctx, "memory")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if !strings.Contains(listing, "MEMORY.md") {
		t.Errorf("expected MEMORY.md in listing, got: %s", listing)
	}
	if !strings.Contains(listing, "memory/notes.md") {
		t.Errorf("expected memory/notes.md in listing, got: %s", listing)
	}
}

func TestListFiles_LeaderGlobalScopeFallback(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	memberID := uuid.New()
	leaderID := uuid.New()

	// Leader has only global-scope docs (userID="").
	ms.docs[docKey(leaderID.String(), "", "MEMORY.md")] = "leader global"

	ctx := memCtx(memberID, "user1", leaderID.String())
	listing, handled, err := mi.ListFiles(ctx, "memory")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if !strings.Contains(listing, "MEMORY.md") {
		t.Errorf("expected MEMORY.md from leader's global scope, got: %s", listing)
	}
}

func TestReadFile_LeaderFallback_MemorySubpath(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	memberID := uuid.New()
	leaderID := uuid.New()

	// Leader has a memory subpath file.
	ms.docs[docKey(leaderID.String(), "user1", "memory/notes.md")] = "leader subpath"

	ctx := memCtx(memberID, "user1", leaderID.String())
	content, handled, err := mi.ReadFile(ctx, "memory/notes.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if content != "leader subpath" {
		t.Errorf("expected 'leader subpath', got %q", content)
	}
}

func TestListFiles_LeaderIsSelf_NoDuplication(t *testing.T) {
	ms := newMockMemoryStore()
	mi := NewMemoryInterceptor(ms, "/workspace", "")
	agentID := uuid.New()

	ms.docs[docKey(agentID.String(), "user1", "MEMORY.md")] = "own mem"

	ctx := memCtx(agentID, "user1", agentID.String())
	listing, handled, err := mi.ListFiles(ctx, "memory")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	// Should appear exactly once.
	count := strings.Count(listing, "MEMORY.md")
	if count != 1 {
		t.Errorf("expected MEMORY.md once, got %d times in: %s", count, listing)
	}
}
