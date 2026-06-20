package agycli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// appDirEnv is the env var that overrides agy's app-data root. It exists so
// tests can point the path helpers (and the log reader) at a throwaway
// t.TempDir() instead of the real ~/.gemini/antigravity-cli location, which
// must never be touched by tests.
const appDirEnv = "AGY_APP_DIR"

// AgyAppDir returns agy's app-data root.
//
// Default ~/.gemini/antigravity-cli; override via env AGY_APP_DIR (used by
// tests). If the home dir cannot be resolved and no override is set, it falls
// back to a relative path so the caller still gets a non-empty value to stat
// against.
//
// Note: the conversation store dir is NOT derived from this helper —
// ConversationsDir (in session.go) has its own AGY_CONVERSATIONS_DIR override
// so it can be redirected independently of the rest of the tree.
func AgyAppDir() string {
	if override := os.Getenv(appDirEnv); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Best-effort fallback; a stat against this will simply find nothing.
		return filepath.Join(".gemini", "antigravity-cli")
	}
	return filepath.Join(home, ".gemini", "antigravity-cli")
}

// SettingsPath (settings.json) is intentionally NOT defined here: it already
// lives in health.go, derived from AgyHome() (env AGY_HOME) so the auth/health
// probe can redirect it. Likewise ConversationsDir lives in session.go (env
// AGY_CONVERSATIONS_DIR). The remaining tree helpers below hang off AgyAppDir().

// LogDir returns the directory holding agy's cli-*.log files under AgyAppDir().
func LogDir() string {
	return filepath.Join(AgyAppDir(), "log")
}

// HistoryPath returns the path to agy's history.jsonl under AgyAppDir().
func HistoryPath() string {
	return filepath.Join(AgyAppDir(), "history.jsonl")
}

// PluginsDir returns the directory holding agy's plugins under AgyAppDir().
func PluginsDir() string {
	return filepath.Join(AgyAppDir(), "plugins")
}

// ReadLatestLog returns the tail (last maxLines lines) of the newest
// LogDir()/cli-*.log file, intended for error diagnosis (per Hermes, the first
// stop on any agy failure is the cli log).
//
// It globs LogDir()/cli-*.log, selects the file with the newest modification
// time (ties broken by lexicographic path so the result is deterministic), and
// returns its path plus the last maxLines lines joined by "\n".
//
// Contract:
//   - maxLines <= 0 returns the whole file (no truncation).
//   - A missing log dir or no matching files returns ("", "", nil) — an absent
//     log is a normal first-run state, not an error.
//   - The selected path is always returned alongside any read error, so callers
//     can surface which file failed.
func ReadLatestLog(maxLines int) (path string, tail string, err error) {
	matches, err := filepath.Glob(filepath.Join(LogDir(), "cli-*.log"))
	if err != nil {
		// The only error filepath.Glob returns is ErrBadPattern; our pattern is
		// a fixed literal so this is effectively unreachable, but propagate it
		// rather than swallow.
		return "", "", fmt.Errorf("agycli: glob cli logs: %w", err)
	}
	if len(matches) == 0 {
		// No logs yet (missing dir or empty) is a normal state, not an error.
		return "", "", nil
	}

	// Pick the newest by mtime. Sort the matches first so that, when two files
	// share the same mtime (filesystem timestamp coarsening), the winner is the
	// lexicographically smallest path rather than glob/readdir order.
	sort.Strings(matches)
	var (
		newest    string
		newestSet bool
		newestMod int64 // unix nanoseconds
	)
	for _, m := range matches {
		fi, statErr := os.Stat(m)
		if statErr != nil {
			// A file that vanished or can't be stat-ed between glob and stat is
			// skipped; it simply can't win.
			continue
		}
		if fi.IsDir() {
			continue
		}
		mod := fi.ModTime().UnixNano()
		if !newestSet || mod > newestMod {
			newest, newestSet, newestMod = m, true, mod
		}
	}
	if !newestSet {
		// Every match disappeared / was unstattable — treat as no log.
		return "", "", nil
	}

	data, err := os.ReadFile(newest)
	if err != nil {
		return newest, "", fmt.Errorf("agycli: read %s: %w", newest, err)
	}

	return newest, tailLines(string(data), maxLines), nil
}

// tailLines returns the last n lines of s joined by "\n". A single trailing
// newline in s is not counted as an extra empty line, so "a\nb\n" tailed by 1
// yields "b". n <= 0 returns s unchanged.
func tailLines(s string, n int) string {
	if n <= 0 {
		return s
	}

	// Strip a single trailing newline so it isn't read as a final empty line.
	trimmed := strings.TrimSuffix(s, "\n")

	lines := strings.Split(trimmed, "\n")
	if len(lines) <= n {
		// Fewer lines than requested: return the trailing-newline-stripped
		// content as-is.
		return trimmed
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
