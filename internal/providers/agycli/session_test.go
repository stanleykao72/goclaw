package agycli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// touchDB creates an empty "<name>.db" file in dir and (when mtime is non-zero)
// stamps it with a controlled modification time so newest-by-mtime tie-breaking
// can be exercised deterministically.
func touchDB(t *testing.T, dir, name string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(dir, name+".db")
	if err := os.WriteFile(p, []byte{}, 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}
}

// writeFile creates an arbitrary (non-.db / non-uuid) file used to prove the
// snapshot filter rejects junk.
func writeFile(t *testing.T, dir, name string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("junk"), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// Sample canonical UUIDs reused across the tests.
const (
	uuidA = "11111111-1111-1111-1111-111111111111"
	uuidB = "22222222-2222-2222-2222-222222222222"
	uuidC = "33333333-3333-3333-3333-333333333333"
	uuidD = "aAbBcCdD-1234-5678-9abc-DEF012345678" // mixed case, still valid
)

func TestConversationsDir_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(conversationsEnv, dir)
	if got := ConversationsDir(); got != dir {
		t.Fatalf("ConversationsDir() = %q, want override %q", got, dir)
	}
}

func TestConversationsDir_DefaultPath(t *testing.T) {
	// Empty override => default under the home dir, ending in the agy path.
	t.Setenv(conversationsEnv, "")
	got := ConversationsDir()
	wantSuffix := filepath.Join(".gemini", "antigravity-cli", "conversations")
	if filepath.Base(got) != "conversations" ||
		got[len(got)-len(wantSuffix):] != wantSuffix {
		t.Fatalf("ConversationsDir() = %q, want suffix %q", got, wantSuffix)
	}
}

func TestSnapshotConversations_FiltersNonUUIDAndNonDB(t *testing.T) {
	dir := t.TempDir()

	// Valid conversation files.
	touchDB(t, dir, uuidA, time.Time{})
	touchDB(t, dir, uuidB, time.Time{})
	touchDB(t, dir, uuidD, time.Time{}) // mixed-case UUID still valid

	// Junk that must be filtered out.
	writeFile(t, dir, "notes.txt")     // wrong extension
	writeFile(t, dir, "backup.db")     // .db but not a UUID
	writeFile(t, dir, "not-a-uuid.db") // .db but malformed UUID
	writeFile(t, dir, ".DS_Store")     // hidden junk
	writeFile(t, dir, uuidA+".db.bak") // uuid prefix but wrong extension
	if err := os.Mkdir(filepath.Join(dir, uuidC+".db"), 0o755); err != nil {
		t.Fatalf("mkdir dir entry: %v", err) // a directory named like a db must be skipped
	}

	snap, err := SnapshotConversations(dir)
	if err != nil {
		t.Fatalf("SnapshotConversations: %v", err)
	}

	want := map[string]struct{}{uuidA: {}, uuidB: {}, uuidD: {}}
	if len(snap) != len(want) {
		t.Fatalf("snapshot size = %d (%v), want %d (%v)", len(snap), snap, len(want), want)
	}
	for id := range want {
		if _, ok := snap[id]; !ok {
			t.Errorf("snapshot missing expected id %q", id)
		}
	}
}

func TestSnapshotConversations_MissingDirIsEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	snap, err := SnapshotConversations(dir)
	if err != nil {
		t.Fatalf("SnapshotConversations on missing dir: unexpected err %v", err)
	}
	if len(snap) != 0 {
		t.Fatalf("snapshot of missing dir = %v, want empty", snap)
	}
}

func TestDetectNewConversation_SingleNew(t *testing.T) {
	dir := t.TempDir()
	touchDB(t, dir, uuidA, time.Time{})
	touchDB(t, dir, uuidB, time.Time{})

	before := map[string]struct{}{uuidA: {}}
	after := map[string]struct{}{uuidA: {}, uuidB: {}}

	id, ok := DetectNewConversation(dir, before, after)
	if !ok {
		t.Fatalf("DetectNewConversation: ok = false, want true")
	}
	if id != uuidB {
		t.Fatalf("DetectNewConversation: id = %q, want %q", id, uuidB)
	}
}

func TestDetectNewConversation_MultipleNewPicksNewestMtime(t *testing.T) {
	dir := t.TempDir()

	base := time.Now().Add(-time.Hour)
	// uuidC is the newest; uuidB middle; uuidA oldest of the new set.
	touchDB(t, dir, uuidA, base.Add(1*time.Minute))
	touchDB(t, dir, uuidB, base.Add(2*time.Minute))
	touchDB(t, dir, uuidC, base.Add(3*time.Minute))

	before := map[string]struct{}{}
	after := map[string]struct{}{uuidA: {}, uuidB: {}, uuidC: {}}

	id, ok := DetectNewConversation(dir, before, after)
	if !ok {
		t.Fatalf("DetectNewConversation: ok = false, want true")
	}
	if id != uuidC {
		t.Fatalf("DetectNewConversation: id = %q, want newest %q", id, uuidC)
	}
}

func TestDetectNewConversation_EqualMtimeMultipleNewIsAmbiguous(t *testing.T) {
	dir := t.TempDir()

	// All three new ids share the exact same mtime — the realistic case where
	// filesystem coarsening / async writes collapse the timestamps. The pick
	// must be deterministic (lexicographically smallest) AND flagged ambiguous.
	same := time.Now().Add(-time.Hour).Truncate(time.Second)
	touchDB(t, dir, uuidC, same)
	touchDB(t, dir, uuidA, same)
	touchDB(t, dir, uuidB, same)

	before := map[string]struct{}{}
	after := map[string]struct{}{uuidA: {}, uuidB: {}, uuidC: {}}

	// Run several times: result must be stable (not map-order dependent).
	for i := 0; i < 5; i++ {
		id, ok := DetectNewConversation(dir, before, after)
		if ok {
			t.Fatalf("iter %d: ok = true, want false for tied mtimes (ambiguous)", i)
		}
		if id != uuidA {
			t.Fatalf("iter %d: id = %q, want deterministic smallest %q", i, id, uuidA)
		}
	}
}

func TestDetectNewConversation_TieOnMaxMtimeAmongStattable(t *testing.T) {
	dir := t.TempDir()

	// uuidA is strictly older; uuidB and uuidC share the newest mtime. The two
	// tied-newest ids make the selection ambiguous even though one candidate is
	// clearly older.
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	touchDB(t, dir, uuidA, base.Add(1*time.Minute))
	touchDB(t, dir, uuidB, base.Add(5*time.Minute))
	touchDB(t, dir, uuidC, base.Add(5*time.Minute))

	before := map[string]struct{}{}
	after := map[string]struct{}{uuidA: {}, uuidB: {}, uuidC: {}}

	id, ok := DetectNewConversation(dir, before, after)
	if ok {
		t.Fatalf("ok = true, want false when 2 ids share the max mtime")
	}
	// Deterministic winner among the tied max = lexicographically smallest (B < C).
	if id != uuidB {
		t.Fatalf("id = %q, want tied-max smallest %q", id, uuidB)
	}
}

func TestDetectNewConversation_AllUnstattableIsDeterministicAndAmbiguous(t *testing.T) {
	dir := t.TempDir()

	// No .db files exist on disk for the new ids, so every stat fails. The
	// fallback must be deterministic (smallest id) and flagged ambiguous.
	before := map[string]struct{}{}
	after := map[string]struct{}{uuidC: {}, uuidA: {}, uuidB: {}}

	for i := 0; i < 5; i++ {
		id, ok := DetectNewConversation(dir, before, after)
		if ok {
			t.Fatalf("iter %d: ok = true, want false when all candidates unstattable", i)
		}
		if id != uuidA {
			t.Fatalf("iter %d: id = %q, want deterministic smallest %q", i, id, uuidA)
		}
	}
}

func TestDetectNewConversation_OneStattableAmongUnstattableWins(t *testing.T) {
	dir := t.TempDir()

	// Only uuidB has a real .db file; the other two cannot be stat-ed. The lone
	// stat-able candidate is the unambiguous winner (ok=true), regardless of id
	// ordering relative to the unstattable ones.
	touchDB(t, dir, uuidB, time.Now().Add(-time.Minute))

	before := map[string]struct{}{}
	after := map[string]struct{}{uuidA: {}, uuidB: {}, uuidC: {}}

	id, ok := DetectNewConversation(dir, before, after)
	if !ok {
		t.Fatalf("ok = false, want true for the single stat-able candidate")
	}
	if id != uuidB {
		t.Fatalf("id = %q, want the only stat-able id %q", id, uuidB)
	}
}

// TestObserveNewConversation_MatchesDetect proves the observation-only alias is
// a behavior-preserving wrapper over DetectNewConversation across the full
// contract: no-new, single-new, and the ambiguous multi-new (tied mtime) case.
func TestObserveNewConversation_MatchesDetect(t *testing.T) {
	dir := t.TempDir()

	same := time.Now().Add(-time.Hour).Truncate(time.Second)
	touchDB(t, dir, uuidA, same)
	touchDB(t, dir, uuidB, same)
	touchDB(t, dir, uuidC, same)

	cases := []struct {
		name          string
		before, after map[string]struct{}
	}{
		{
			name:   "none",
			before: map[string]struct{}{uuidA: {}},
			after:  map[string]struct{}{uuidA: {}},
		},
		{
			name:   "single",
			before: map[string]struct{}{uuidA: {}},
			after:  map[string]struct{}{uuidA: {}, uuidB: {}},
		},
		{
			name:   "ambiguous-tied-mtime",
			before: map[string]struct{}{},
			after:  map[string]struct{}{uuidA: {}, uuidB: {}, uuidC: {}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantID, wantOK := DetectNewConversation(dir, tc.before, tc.after)
			gotID, gotOK := ObserveNewConversation(dir, tc.before, tc.after)
			if gotID != wantID || gotOK != wantOK {
				t.Fatalf("ObserveNewConversation = (%q, %v), want DetectNewConversation = (%q, %v)",
					gotID, gotOK, wantID, wantOK)
			}
		})
	}
}

func TestDetectNewConversation_None(t *testing.T) {
	dir := t.TempDir()
	touchDB(t, dir, uuidA, time.Time{})

	before := map[string]struct{}{uuidA: {}}
	after := map[string]struct{}{uuidA: {}}

	id, ok := DetectNewConversation(dir, before, after)
	if ok {
		t.Fatalf("DetectNewConversation: ok = true, want false")
	}
	if id != "" {
		t.Fatalf("DetectNewConversation: id = %q, want empty", id)
	}
}
