package tools

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// vaultTestCtx builds a context with the identity signals the vault backend reads.
func vaultTestCtx(backend, channel, peerKind, chatID, userID string) context.Context {
	ctx := context.Background()
	ctx = store.WithAgentID(ctx, uuid.New())
	if backend != "" {
		ctx = store.WithMemoryBackend(ctx, backend)
	}
	ctx = WithToolChannelType(ctx, channel)
	ctx = WithToolPeerKind(ctx, peerKind)
	ctx = WithToolChatID(ctx, chatID)
	ctx = store.WithUserID(ctx, userID)
	return ctx
}

func TestMemoryVaultSubdir(t *testing.T) {
	t.Run("group uses chatID", func(t *testing.T) {
		ctx := vaultTestCtx("vault", "lineworks", "group", "C12345", "u-999")
		got := MemoryVaultSubdir(ctx)
		want := filepath.Join("lineworks", "group-C12345")
		if got != want {
			t.Fatalf("group subdir = %q, want %q", got, want)
		}
	})
	t.Run("direct uses userID", func(t *testing.T) {
		ctx := vaultTestCtx("vault", "lineworks", "direct", "", "u-999")
		got := MemoryVaultSubdir(ctx)
		want := filepath.Join("lineworks", "user-u-999")
		if got != want {
			t.Fatalf("direct subdir = %q, want %q", got, want)
		}
	})
	t.Run("same sender different conversation different scope", func(t *testing.T) {
		grp := MemoryVaultSubdir(vaultTestCtx("vault", "lineworks", "group", "C1", "sameuser"))
		dm := MemoryVaultSubdir(vaultTestCtx("vault", "lineworks", "direct", "", "sameuser"))
		if grp == dm {
			t.Fatalf("group and direct scopes must differ, both = %q", grp)
		}
	})
	t.Run("crafted ids are sanitized (no traversal)", func(t *testing.T) {
		ctx := vaultTestCtx("vault", "lineworks", "group", "../../etc/passwd", "")
		got := MemoryVaultSubdir(ctx)
		if strings.Contains(got, "..") {
			t.Fatalf("subdir must not contain '..': %q", got)
		}
		// The chat segment must collapse separators to underscores.
		if strings.Count(got, string(filepath.Separator)) != 1 {
			t.Fatalf("subdir should have exactly one separator (channel/scope): %q", got)
		}
	})
}

func TestMemoryVaultSubdirForMatchesWriteScope(t *testing.T) {
	// The auto-inject scope (computed from request fields via MemoryVaultSubdirFor)
	// MUST equal the scope a write produces (MemoryVaultSubdir from tool ctx).
	// Group case is the regression: writes use group-<chatID>, recall must too.
	ctx := vaultTestCtx("vault", "lineworks", "group", "Cgroup123", "u-1")
	writeScope := MemoryVaultSubdir(ctx)
	injectScope := MemoryVaultSubdirFor("lineworks", "group", "Cgroup123", "u-1")
	if writeScope != injectScope {
		t.Fatalf("group write scope %q != auto-inject scope %q (round-trip broken)", writeScope, injectScope)
	}
	if injectScope != filepath.Join("lineworks", "group-Cgroup123") {
		t.Fatalf("group scope = %q, want lineworks/group-Cgroup123", injectScope)
	}
}

func TestMemoryVaultGroupRoundTripViaScope(t *testing.T) {
	dir := t.TempDir()
	mi := NewMemoryInterceptor(newMockMemoryStore(), "/workspace", dir)
	ctx := vaultTestCtx("vault", "lineworks", "group", "Crt", "writer-7")
	content := "## 群組索引\n- 全體共享的正體中文記憶"
	if _, err := mi.WriteFile(ctx, "LONGTERM.md", content, false); err != nil {
		t.Fatalf("group write: %v", err)
	}
	// Auto-inject reads via the request-derived scope (no tool ctx keys).
	scope := MemoryVaultSubdirFor("lineworks", "group", "Crt", "writer-7")
	idx, _, err := ReadMemoryVaultIndexForScope(dir, scope, 200, 8192)
	if err != nil {
		t.Fatalf("index read: %v", err)
	}
	if idx != content {
		t.Fatalf("auto-inject index mismatch:\n got=%q\nwant=%q", idx, content)
	}
}

func TestMemoryVaultSubdirDirectEmptyIdentity(t *testing.T) {
	got := MemoryVaultSubdirFor("lineworks", "direct", "", "")
	if got != filepath.Join("lineworks", "user-unknown") {
		t.Fatalf("empty direct identity = %q, want lineworks/user-unknown", got)
	}
}

func TestTruncateForBudgetCJKRuneBoundary(t *testing.T) {
	// 30 Chinese chars (3 bytes each = 90 bytes). Cap at 50 bytes lands mid-rune.
	content := strings.Repeat("記", 30) + "\n"
	out, trunc, _ := truncateForBudget(content, 0, 50)
	if !trunc {
		t.Fatal("expected truncation")
	}
	if !utf8.ValidString(out) {
		t.Fatalf("truncated CJK output must be valid UTF-8: %q", out)
	}
}

func TestIsVaultLayoutPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"LONGTERM.md", true},
		{"topics/people.md", true},
		{"journal/2026-06-05.md", true},
		{"archive/old.md", true},
		{"MEMORY.md", false}, // native db path, not the vault layout extension
		{"notes.md", false},
		{"config.yaml", false},
	}
	for _, c := range cases {
		if got := isVaultLayoutPath(c.path, ""); got != c.want {
			t.Errorf("isVaultLayoutPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestIsMemoryPathForBackend(t *testing.T) {
	// db backend: only the native memory paths, never the vault layout.
	if isMemoryPathForBackend("db", "LONGTERM.md", "") {
		t.Error("db backend must NOT treat LONGTERM.md as memory (would change db behavior)")
	}
	if !isMemoryPathForBackend("db", "MEMORY.md", "") {
		t.Error("db backend must treat MEMORY.md as memory")
	}
	// vault backend: native paths AND the vault layout.
	if !isMemoryPathForBackend("vault", "LONGTERM.md", "") {
		t.Error("vault backend must treat LONGTERM.md as memory")
	}
	if !isMemoryPathForBackend("vault", "MEMORY.md", "") {
		t.Error("vault backend must still treat MEMORY.md as memory")
	}
	if isMemoryPathForBackend("vault", "random.txt", "") {
		t.Error("vault backend must not treat a non-memory path as memory")
	}
}

func TestWouldRouteVault(t *testing.T) {
	dir := t.TempDir()
	mi := NewMemoryInterceptor(newMockMemoryStore(), "/workspace", dir)
	vaultCtx := vaultTestCtx("vault", "lineworks", "group", "C1", "")
	dbCtx := vaultTestCtx("db", "lineworks", "group", "C1", "")

	if !mi.WouldRouteVault(vaultCtx, "LONGTERM.md") {
		t.Error("vault + memory path + vaultDir set must route to vault (exempt)")
	}
	if mi.WouldRouteVault(vaultCtx, "src/main.go") {
		t.Error("vault + non-memory path must NOT route to vault")
	}
	if mi.WouldRouteVault(dbCtx, "LONGTERM.md") {
		t.Error("db backend must never route to vault")
	}
	// vaultDir unset → must NOT exempt (write would fall through to db/host + ACL).
	miNoVault := NewMemoryInterceptor(newMockMemoryStore(), "/workspace", "")
	if miNoVault.WouldRouteVault(vaultCtx, "LONGTERM.md") {
		t.Error("vault mode but empty vaultDir must NOT route to vault (ACL must still apply)")
	}
	// nil interceptor → false (no panic).
	var nilMi *MemoryInterceptor
	if nilMi.WouldRouteVault(vaultCtx, "LONGTERM.md") {
		t.Error("nil interceptor must not route to vault")
	}
}

func TestMemoryVaultRoundTripChinese(t *testing.T) {
	ms := newMockMemoryStore()
	dir := t.TempDir()
	mi := NewMemoryInterceptor(ms, "/workspace", dir)
	ctx := vaultTestCtx("vault", "lineworks", "group", "Cabc", "")

	content := "## 偏好\n- 使用者偏好正體中文回覆\n- 名稱：高玉明"
	res, err := mi.WriteFile(ctx, "LONGTERM.md", content, false)
	if err != nil || !res.Handled {
		t.Fatalf("vault write: handled=%v err=%v", res.Handled, err)
	}
	// Must NOT have touched the DB.
	if len(ms.docs) != 0 {
		t.Fatalf("vault write must not call PutDocument; docs=%v", ms.docs)
	}
	// File must exist on disk under the scope.
	want := filepath.Join(dir, "lineworks", "group-Cabc", "LONGTERM.md")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected vault file at %s: %v", want, err)
	}
	// Read back identical (incl. Chinese).
	got, handled, err := mi.ReadFile(ctx, "LONGTERM.md")
	if err != nil || !handled {
		t.Fatalf("vault read: handled=%v err=%v", handled, err)
	}
	if got != content {
		t.Fatalf("round-trip mismatch:\n got=%q\nwant=%q", got, content)
	}
}

func TestMemoryVaultAppendAndOverwrite(t *testing.T) {
	ms := newMockMemoryStore()
	dir := t.TempDir()
	mi := NewMemoryInterceptor(ms, "/workspace", dir)
	ctx := vaultTestCtx("vault", "lineworks", "direct", "", "u1")

	if _, err := mi.WriteFile(ctx, "journal/2026-06-05.md", "first", false); err != nil {
		t.Fatalf("write1: %v", err)
	}
	if _, err := mi.WriteFile(ctx, "journal/2026-06-05.md", "second", true); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, _, _ := mi.ReadFile(ctx, "journal/2026-06-05.md")
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Fatalf("append must preserve both: %q", got)
	}
	// Non-append overwrite with different content returns PreviousContent.
	res, err := mi.WriteFile(ctx, "journal/2026-06-05.md", "replaced", false)
	if err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if res.PreviousContent == "" {
		t.Fatal("overwrite of differing content must report PreviousContent")
	}
}

func TestMemoryVaultContainment(t *testing.T) {
	dir := t.TempDir()
	scope := filepath.Join("lineworks", "group-C1")
	if _, err := resolveMemoryVaultPath(dir, scope, "../../escape.md"); err == nil {
		t.Fatal("expected containment error for traversal path")
	}
	if _, err := resolveMemoryVaultPath(dir, scope, "topics/ok.md"); err != nil {
		t.Fatalf("legit nested path must resolve: %v", err)
	}
}

func TestMemoryVaultColdStart(t *testing.T) {
	ms := newMockMemoryStore()
	dir := t.TempDir() // empty — nothing written yet
	mi := NewMemoryInterceptor(ms, "/workspace", dir)
	ctx := vaultTestCtx("vault", "lineworks", "group", "Cnone", "")

	got, handled, err := mi.ReadFile(ctx, "LONGTERM.md")
	if err != nil {
		t.Fatalf("cold-start read must not error: %v", err)
	}
	if !handled || got != "" {
		t.Fatalf("cold-start read = (%q, handled=%v), want empty handled", got, handled)
	}
	listing, handled, err := mi.ListFiles(ctx, "memory")
	if err != nil || !handled || listing != "" {
		t.Fatalf("cold-start list = (%q, handled=%v, err=%v), want empty handled", listing, handled, err)
	}
}

func TestTruncateForBudget(t *testing.T) {
	five := "l1\nl2\nl3\nl4\nl5\n"
	out, trunc, _ := truncateForBudget(five, 3, 0)
	if !trunc {
		t.Fatal("expected truncation at 3 lines")
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("truncated output must carry marker: %q", out)
	}
	if strings.Contains(out, "l5") {
		t.Fatalf("line 5 should be dropped: %q", out)
	}
	out2, trunc2, _ := truncateForBudget("short\n", 100, 1000)
	if trunc2 || out2 != "short\n" {
		t.Fatalf("no truncation expected: out=%q trunc=%v", out2, trunc2)
	}
}

func TestAutoInjectReadsMEMORYmdFallback(t *testing.T) {
	scope := filepath.Join("lineworks", "group-Cfallback")

	t.Run("MEMORY.md-only scope recalls", func(t *testing.T) {
		dir := t.TempDir()
		content := "## 偏好\n- 使用者偏好正體中文回覆"
		writeScopeFile(t, dir, scope, "MEMORY.md", content)
		idx, _, err := ReadMemoryVaultIndexForScope(dir, scope, 200, 8192)
		if err != nil {
			t.Fatalf("index read: %v", err)
		}
		if idx != content {
			t.Fatalf("MEMORY.md fallback must be injected:\n got=%q\nwant=%q", idx, content)
		}
	})

	t.Run("LONGTERM.md preferred when both exist", func(t *testing.T) {
		dir := t.TempDir()
		longterm := "## 策展索引\n- curated 正體中文"
		memory := "## 原生\n- native MEMORY content"
		writeScopeFile(t, dir, scope, "LONGTERM.md", longterm)
		writeScopeFile(t, dir, scope, "MEMORY.md", memory)
		idx, _, err := ReadMemoryVaultIndexForScope(dir, scope, 200, 8192)
		if err != nil {
			t.Fatalf("index read: %v", err)
		}
		if idx != longterm {
			t.Fatalf("LONGTERM.md must win precedence:\n got=%q\nwant=%q", idx, longterm)
		}
	})

	t.Run("cold-start when neither exists", func(t *testing.T) {
		dir := t.TempDir()
		idx, trunc, err := ReadMemoryVaultIndexForScope(dir, scope, 200, 8192)
		if err != nil {
			t.Fatalf("cold-start must not error: %v", err)
		}
		if idx != "" || trunc {
			t.Fatalf("cold-start = (%q, trunc=%v), want empty/false", idx, trunc)
		}
	})

	t.Run("empty index file is treated as cold-start", func(t *testing.T) {
		dir := t.TempDir()
		writeScopeFile(t, dir, scope, "LONGTERM.md", "")
		writeScopeFile(t, dir, scope, "MEMORY.md", "native after empty curated")
		idx, _, err := ReadMemoryVaultIndexForScope(dir, scope, 200, 8192)
		if err != nil {
			t.Fatalf("index read: %v", err)
		}
		if idx != "native after empty curated" {
			t.Fatalf("empty LONGTERM.md must fall through to MEMORY.md: got=%q", idx)
		}
	})
}

func TestMemoryVaultMapForScope(t *testing.T) {
	scope := filepath.Join("lineworks", "group-Cmap")

	t.Run("map contains index plus topics/journal listing", func(t *testing.T) {
		dir := t.TempDir()
		writeScopeFile(t, dir, scope, "MEMORY.md", "## 偏好\n- 使用者偏好正體中文")
		writeScopeFile(t, dir, scope, "topics/pref.md", "深入偏好")
		writeScopeFile(t, dir, scope, "journal/2026-06-05.md", "今日筆記")
		got, err := MemoryVaultMapForScope(dir, scope, 200, 8192)
		if err != nil {
			t.Fatalf("map: %v", err)
		}
		// Index content present (not gated on a query substring).
		if !strings.Contains(got, "正體中文") {
			t.Fatalf("map must include index content: %q", got)
		}
		// Files listed so the agent can read_file them.
		if !strings.Contains(got, "## Memory files") {
			t.Fatalf("map must have a Memory files section: %q", got)
		}
		if !strings.Contains(got, "topics/pref.md") || !strings.Contains(got, "journal/2026-06-05.md") {
			t.Fatalf("map must list topics/journal files: %q", got)
		}
	})

	t.Run("cold-start map is empty", func(t *testing.T) {
		dir := t.TempDir()
		got, err := MemoryVaultMapForScope(dir, scope, 200, 8192)
		if err != nil {
			t.Fatalf("cold-start map must not error: %v", err)
		}
		if got != "" {
			t.Fatalf("cold-start map must be empty: %q", got)
		}
	})
}

// writeScopeFile writes a file under vaultDir/scope/rel for tests that need an
// explicit on-disk scope layout (bypassing the tool ctx).
func writeScopeFile(t *testing.T, vaultDir, scope, rel, content string) {
	t.Helper()
	full := filepath.Join(vaultDir, scope, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func TestMemoryVaultConcurrentAppendNoLostUpdate(t *testing.T) {
	ms := newMockMemoryStore()
	dir := t.TempDir()
	mi := NewMemoryInterceptor(ms, "/workspace", dir)
	ctx := vaultTestCtx("vault", "lineworks", "group", "Crace", "")

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := "marker-" + strconv.Itoa(i)
			if _, err := mi.WriteFile(ctx, "journal/2026-06-05.md", marker, true); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got, _, err := mi.ReadFile(ctx, "journal/2026-06-05.md")
	if err != nil {
		t.Fatalf("read after concurrent appends: %v", err)
	}
	for i := 0; i < n; i++ {
		if !strings.Contains(got, "marker-"+strconv.Itoa(i)) {
			t.Fatalf("lost update: marker-%d missing from result", i)
		}
	}
}

func TestDBBackendUnaffectedByVaultWiring(t *testing.T) {
	ms := newMockMemoryStore()
	dir := t.TempDir()
	// vaultDir is set, but the agent is in db mode (no MemoryBackend on ctx).
	mi := NewMemoryInterceptor(ms, "/workspace", dir)
	ctx := context.Background()
	ctx = store.WithAgentID(ctx, uuid.New())

	res, err := mi.WriteFile(ctx, "MEMORY.md", "english note", false)
	if err != nil || !res.Handled {
		t.Fatalf("db write: handled=%v err=%v", res.Handled, err)
	}
	// Must have gone to the DB (PutDocument), not to disk.
	if len(ms.docs) != 1 {
		t.Fatalf("db write must call PutDocument exactly once; docs=%v", ms.docs)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("db write must not create vault files; found %d entries", len(entries))
	}
	// A vault-layout path in db mode is NOT a memory path → not handled.
	if _, handled, _ := mi.ReadFile(ctx, "LONGTERM.md"); handled {
		t.Fatal("db mode must not handle LONGTERM.md as memory")
	}
}
