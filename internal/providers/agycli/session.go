package agycli

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// conversationsEnv is the env var that overrides the conversation store dir.
// It exists primarily so tests can point the snapshot/detect helpers at a
// throwaway t.TempDir() instead of the real ~/.gemini location.
const conversationsEnv = "AGY_CONVERSATIONS_DIR"

// uuidRe matches a canonical 8-4-4-4-12 lowercase/uppercase hex UUID, anchored
// so it only accepts a bare UUID (the basename of "<uuid>.db" after stripping
// the extension). Junk files (e.g. "notes.txt", "backup.db", ".DS_Store") fail
// this and are filtered out of every snapshot.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ConversationsDir returns agy's conversation store dir.
//
// Default ~/.gemini/antigravity-cli/conversations; override via env
// AGY_CONVERSATIONS_DIR (used by tests). If the home dir cannot be resolved and
// no override is set, it falls back to a relative path so the caller still gets
// a non-empty value to stat against.
func ConversationsDir() string {
	if override := os.Getenv(conversationsEnv); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Best-effort fallback; a stat against this will simply find nothing.
		return filepath.Join(".gemini", "antigravity-cli", "conversations")
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
}

// SnapshotConversations returns the set of conversation UUIDs (the "<uuid>"
// portion of "<uuid>.db" basenames) currently on disk in dir.
//
// This is OBSERVATION-ONLY plumbing: paired with DetectNewConversation it tells
// you which conversation db agy minted during a single run, for logging /
// debugging. It is NOT a resume mechanism. Phase 0 verified that "agy --print"
// is stateless and does not replay prior context via --conversation/-c, so a
// captured id cannot be fed back to continue a conversation.
//
// Only files whose basename is "<valid-uuid>.db" are included; anything else
// (other extensions, non-UUID names, sub-directories) is silently filtered so
// the diff in DetectNewConversation only ever sees real conversation ids. A
// missing directory is treated as an empty snapshot (no error), since agy may
// not have created the store yet on a first run.
func SnapshotConversations(dir string) (map[string]struct{}, error) {
	set := make(map[string]struct{})

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Store not created yet => empty snapshot, not an error.
			return set, nil
		}
		return nil, err
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".db") {
			continue
		}
		id := strings.TrimSuffix(name, ".db")
		if !uuidRe.MatchString(id) {
			continue
		}
		set[id] = struct{}{}
	}

	return set, nil
}

// DetectNewConversation diffs two snapshots taken around an agy invocation.
//
// This is OBSERVATION-ONLY: it identifies which conversation db agy produced for
// a run (useful for logging / debugging), NOT a resume primitive. Phase 0
// verified "agy --print" is stateless — the returned id cannot be replayed via
// --conversation/-c to continue prior context. See ObserveNewConversation for
// the observation-only alias.
//
// It computes the set difference (after - before) and resolves it to a single
// conversation id with the following contract:
//
//   - No new id appeared: returns ("", false).
//   - Exactly one new id appeared: returns (id, true).
//   - Multiple new ids appeared (concurrent agy runs, or a leftover write
//     landing in the window): it selects a deterministic winner and signals
//     whether that winner is trustworthy via ok.
//
// Selection among multiple candidates is by newest file mtime (stat-ing
// "<beforeDir>/<id>.db"), with ties broken by lexicographic id compare so the
// result never depends on map iteration order. Equal mtimes are common in
// practice (filesystem timestamp coarsening, plus agy creating the .db before
// writing it asynchronously), so a tie is treated as genuine ambiguity: when
// two or more candidates share the maximum mtime, the deterministic winner is
// still returned but ok=false, letting the caller fall back instead of trusting
// an essentially-arbitrary pick.
//
// Candidates whose .db cannot be stat-ed are ranked below any stat-able one. If
// every candidate is unstattable, the lexicographically smallest id is returned
// with ok=false (ambiguous: no mtime evidence to distinguish them).
func DetectNewConversation(beforeDir string, before, after map[string]struct{}) (id string, ok bool) {
	// Collect ids present in after but not in before.
	var newIDs []string
	for a := range after {
		if _, existed := before[a]; !existed {
			newIDs = append(newIDs, a)
		}
	}

	switch len(newIDs) {
	case 0:
		return "", false
	case 1:
		return newIDs[0], true
	}

	// Sort for deterministic iteration so any fallback / tiebreak is stable
	// regardless of map ordering.
	sort.Strings(newIDs)

	// Multiple new ids: pick the newest by mtime, ties broken by id order.
	// Track the best stat-able candidate and how many share its max mtime.
	var (
		best          string
		bestSet       bool
		bestMod       int64 // unix nanoseconds of best
		maxMtimeCount int   // how many stat-able candidates share bestMod
	)
	for _, nid := range newIDs {
		fi, err := os.Stat(filepath.Join(beforeDir, nid+".db"))
		if err != nil {
			// Unstattable candidate: never displaces a stat-able one.
			continue
		}
		mod := fi.ModTime().UnixNano()
		switch {
		case !bestSet || mod > bestMod:
			// Strictly newer (or first seen) => new sole leader.
			best, bestSet, bestMod, maxMtimeCount = nid, true, mod, 1
		case mod == bestMod:
			// Tie on max mtime. newIDs is sorted, so the first id seen at this
			// mtime is already the lexicographically smallest and stays best.
			maxMtimeCount++
		}
	}

	if !bestSet {
		// All candidates unstattable. Return the deterministic (smallest) id but
		// flag ambiguity — there's no evidence to pick among multiple.
		return newIDs[0], false
	}

	// Ambiguous when 2+ stat-able candidates share the maximum mtime.
	if maxMtimeCount >= 2 {
		return best, false
	}
	return best, true
}

// ObserveNewConversation is DetectNewConversation under an observation-only name; agy --print
// creates a fresh conversation per run, so this only tells you which db a run produced.
func ObserveNewConversation(beforeDir string, before, after map[string]struct{}) (id string, ok bool) {
	return DetectNewConversation(beforeDir, before, after)
}
