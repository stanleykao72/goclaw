package agycli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLog creates LogDir()/<name> with the given content and (when mtime is
// non-zero) stamps a controlled modification time so newest-by-mtime selection
// can be exercised deterministically. The log dir is created on demand.
func writeLog(t *testing.T, logDir, name, content string, mtime time.Time) string {
	t.Helper()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", logDir, err)
	}
	p := filepath.Join(logDir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}
	return p
}

func TestAgyAppDir_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(appDirEnv, dir)
	if got := AgyAppDir(); got != dir {
		t.Fatalf("AgyAppDir() = %q, want override %q", got, dir)
	}
}

func TestAgyAppDir_DefaultPath(t *testing.T) {
	t.Setenv(appDirEnv, "")
	got := AgyAppDir()
	wantSuffix := filepath.Join(".gemini", "antigravity-cli")
	if filepath.Base(got) != "antigravity-cli" ||
		got[len(got)-len(wantSuffix):] != wantSuffix {
		t.Fatalf("AgyAppDir() = %q, want suffix %q", got, wantSuffix)
	}
}

func TestPathHelpers_UnderAppDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(appDirEnv, dir)

	// SettingsPath is owned by health.go (AGY_HOME override) and ConversationsDir
	// by session.go (AGY_CONVERSATIONS_DIR), so they are not asserted here.
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"LogDir", LogDir(), filepath.Join(dir, "log")},
		{"HistoryPath", HistoryPath(), filepath.Join(dir, "history.jsonl")},
		{"PluginsDir", PluginsDir(), filepath.Join(dir, "plugins")},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// ConversationsDir has its own AGY_CONVERSATIONS_DIR override and is NOT derived
// from AGY_APP_DIR; guard that contract so a future refactor doesn't quietly
// couple them.
func TestConversationsDir_IndependentOfAppDir(t *testing.T) {
	appDir := t.TempDir()
	t.Setenv(appDirEnv, appDir)
	t.Setenv(conversationsEnv, "") // force the default home-derived path

	got := ConversationsDir()
	if got == filepath.Join(appDir, "conversations") {
		t.Fatalf("ConversationsDir() = %q, must not be derived from AGY_APP_DIR", got)
	}
}

func TestReadLatestLog_PicksNewestAndTails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(appDirEnv, dir)
	logDir := LogDir()

	base := time.Now().Add(-time.Hour)
	writeLog(t, logDir, "cli-001.log", "old-1\nold-2\nold-3\n", base.Add(1*time.Minute))
	newest := writeLog(t, logDir, "cli-002.log",
		"l1\nl2\nl3\nl4\nl5\n", base.Add(5*time.Minute))

	path, tail, err := ReadLatestLog(2)
	if err != nil {
		t.Fatalf("ReadLatestLog: %v", err)
	}
	if path != newest {
		t.Fatalf("path = %q, want newest %q", path, newest)
	}
	if want := "l4\nl5"; tail != want {
		t.Fatalf("tail = %q, want %q", tail, want)
	}
}

func TestReadLatestLog_FewerLinesThanRequested(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(appDirEnv, dir)

	writeLog(t, LogDir(), "cli-1.log", "only-one-line\n", time.Time{})

	_, tail, err := ReadLatestLog(10)
	if err != nil {
		t.Fatalf("ReadLatestLog: %v", err)
	}
	if want := "only-one-line"; tail != want {
		t.Fatalf("tail = %q, want %q", tail, want)
	}
}

func TestReadLatestLog_NoTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(appDirEnv, dir)

	writeLog(t, LogDir(), "cli-1.log", "a\nb\nc", time.Time{})

	_, tail, err := ReadLatestLog(2)
	if err != nil {
		t.Fatalf("ReadLatestLog: %v", err)
	}
	if want := "b\nc"; tail != want {
		t.Fatalf("tail = %q, want %q", tail, want)
	}
}

func TestReadLatestLog_MaxLinesNonPositiveReturnsWholeFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(appDirEnv, dir)

	content := "x1\nx2\nx3\n"
	writeLog(t, LogDir(), "cli-1.log", content, time.Time{})

	for _, n := range []int{0, -5} {
		_, tail, err := ReadLatestLog(n)
		if err != nil {
			t.Fatalf("ReadLatestLog(%d): %v", n, err)
		}
		if tail != content {
			t.Fatalf("ReadLatestLog(%d) tail = %q, want whole file %q", n, tail, content)
		}
	}
}

func TestReadLatestLog_MissingDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(appDirEnv, dir) // no log/ subdir created

	path, tail, err := ReadLatestLog(5)
	if err != nil {
		t.Fatalf("ReadLatestLog on missing log dir: unexpected err %v", err)
	}
	if path != "" || tail != "" {
		t.Fatalf("ReadLatestLog on missing log dir = (%q, %q), want empty", path, tail)
	}
}

func TestReadLatestLog_EmptyLogDirNoMatches(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(appDirEnv, dir)

	logDir := LogDir()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", logDir, err)
	}
	// A non-matching file must be ignored (only cli-*.log counts).
	if err := os.WriteFile(filepath.Join(logDir, "other.txt"), []byte("nope"), 0o600); err != nil {
		t.Fatalf("write other.txt: %v", err)
	}

	path, tail, err := ReadLatestLog(5)
	if err != nil {
		t.Fatalf("ReadLatestLog: %v", err)
	}
	if path != "" || tail != "" {
		t.Fatalf("ReadLatestLog with no matching logs = (%q, %q), want empty", path, tail)
	}
}

func TestReadLatestLog_TieOnMtimePicksLexicographicallySmallest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(appDirEnv, dir)
	logDir := LogDir()

	// Two files share the exact same mtime (filesystem coarsening / batched
	// writes). The winner must be deterministic: lexicographically smallest path.
	same := time.Now().Add(-time.Hour).Truncate(time.Second)
	writeLog(t, logDir, "cli-bbb.log", "from-bbb\n", same)
	writeLog(t, logDir, "cli-aaa.log", "from-aaa\n", same)

	wantPath := filepath.Join(logDir, "cli-aaa.log")
	for i := 0; i < 5; i++ {
		path, tail, err := ReadLatestLog(1)
		if err != nil {
			t.Fatalf("iter %d: ReadLatestLog: %v", i, err)
		}
		if path != wantPath {
			t.Fatalf("iter %d: path = %q, want deterministic smallest %q", i, path, wantPath)
		}
		if tail != "from-aaa" {
			t.Fatalf("iter %d: tail = %q, want %q", i, tail, "from-aaa")
		}
	}
}
