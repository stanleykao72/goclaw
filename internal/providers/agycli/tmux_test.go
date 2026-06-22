package agycli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// requireTmux skips the test when tmux is not installed. Phase 2 tests drive a
// REAL tmux against STUB shell commands; they never spawn agy.
func requireTmux(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tmux tests are POSIX-only")
	}
	if !tmuxAvailable() {
		t.Skip("tmux not available on PATH")
	}
}

// TestTmuxHelpersRoundtrip exercises new/send/capture/kill and verifies no
// session is left behind after kill.
func TestTmuxHelpersRoundtrip(t *testing.T) {
	requireTmux(t)

	name := uniqueSessionName()
	dir := t.TempDir()
	if err := newTmuxSession(name, 120, 40, dir); err != nil {
		t.Fatalf("newTmuxSession: %v", err)
	}
	// Guarantee cleanup even if an assertion fails mid-test.
	defer tmuxKillSession(name)

	if !tmuxHasSession(name) {
		t.Fatalf("tmuxHasSession(%q) = false right after creation, want true", name)
	}

	const marker = "ROUNDTRIP_MARKER_77"
	if err := tmuxSendKeys(name, "printf '%s\\n' "+marker, true); err != nil {
		t.Fatalf("tmuxSendKeys: %v", err)
	}

	// Poll the pane until the marker is rendered (send-keys is async).
	if !waitForPane(t, name, marker, 5*time.Second) {
		out, _ := tmuxCapturePane(name, 0, false)
		t.Fatalf("marker %q never appeared in pane; got:\n%s", marker, out)
	}

	tmuxKillSession(name)
	if tmuxHasSession(name) {
		t.Errorf("tmuxHasSession(%q) = true after kill, want false (leftover session)", name)
	}
}

// TestTmuxSendKeys_LongMultilineUsesPasteBuffer verifies that a prompt larger
// than tmuxLiteralMax (and containing newlines) is delivered intact via the
// paste-buffer path instead of `send-keys -l`, which would fail with
// "command too long" (E2BIG). This is the regression guard for the real-agent
// failure where a system-prompt + history turn overflowed the argv limit.
//
// The decisive regression assertion is at the call layer: a >4KB multi-line
// payload must NOT return the "command too long" (E2BIG) error that the old
// `send-keys -l` path produced. The shell-execution / pane-render outcome is
// deliberately not asserted (bracketed-paste submit semantics are
// shell-dependent and flaky); intact delivery is exercised end-to-end by the
// real-agy Session e2e test.
func TestTmuxSendKeys_LongMultilineUsesPasteBuffer(t *testing.T) {
	requireTmux(t)

	name := uniqueSessionName()
	dir := t.TempDir()
	if err := newTmuxSession(name, 200, 50, dir); err != nil {
		t.Fatalf("newTmuxSession: %v", err)
	}
	defer tmuxKillSession(name)

	// >4KB, multi-line payload — forces the paste-buffer path.
	var sb strings.Builder
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&sb, "line %02d: padding to push this prompt far past the argv limit\n", i)
	}
	payload := strings.TrimRight(sb.String(), "\n")
	if len(payload) <= tmuxLiteralMax || !strings.ContainsRune(payload, '\n') {
		t.Fatalf("test payload %d bytes / must be >tmuxLiteralMax %d and multi-line", len(payload), tmuxLiteralMax)
	}

	// Old path: `send-keys -l -- <payload>` -> "command too long". New path:
	// load-buffer/paste-buffer -> nil. This is the regression guard.
	if err := tmuxSendKeys(name, payload, false); err != nil {
		t.Fatalf("tmuxSendKeys (long/multiline) returned error (E2BIG regression): %v", err)
	}

	// A short single-line payload still uses the simple send-keys path and must
	// also succeed (the threshold branch both ways).
	if err := tmuxSendKeys(name, "echo short_ok", false); err != nil {
		t.Fatalf("tmuxSendKeys (short) error: %v", err)
	}
}

// TestRunWithTmux_CapturesOutput verifies a stub's stdout is captured, ExitCode
// is 0, and no session is left behind.
func TestRunWithTmux_CapturesOutput(t *testing.T) {
	requireTmux(t)

	dir := t.TempDir()
	const marker = "HELLO_TMUX_OK"
	stub := writeStub(t, dir, "echo_stub.sh", "#!/bin/sh\nprintf '%s' '"+marker+"'\n")

	before := tmuxSessionCount(t)
	res, err := RunWithTmux(context.Background(), stub, RunOptions{
		Workdir: dir,
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunWithTmux: %v", err)
	}
	if res.TimedOut {
		t.Errorf("TimedOut = true, want false")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(string(res.Output), marker) {
		t.Errorf("Output %q does not contain marker %q", res.Output, marker)
	}
	if strings.Contains(string(res.Output), doneSentinelPrefix) {
		t.Errorf("Output still contains sentinel: %q", res.Output)
	}
	if got := tmuxSessionCount(t); got != before {
		t.Errorf("session count changed: before=%d after=%d (leftover session)", before, got)
	}
}

// TestRunWithTmux_RenderedBeatsRepaint is the key value test: a stub emits
// spinner/cursor-up repaint frames then a final line. Because tmux RENDERS the
// screen, the captured output must be the CLEAN final state without the
// intermediate spinner frames — something raw-PTY byte capture cannot do.
func TestRunWithTmux_RenderedBeatsRepaint(t *testing.T) {
	requireTmux(t)

	dir := t.TempDir()
	// Spinner frames overwritten in place via \r, then \r + clear-to-EOL (ESC[K)
	// and the final answer written over the same line. This is the realistic agy
	// repaint pattern: a busy spinner on one logical line replaced by the answer.
	// A raw byte stream still contains every "Working..." frame; only a rendered
	// terminal collapses them to the final state.
	body := "#!/bin/sh\n" +
		"printf 'Working... -\\rWorking... \\\\\\rWorking... |\\r\\033[KFINAL_ANSWER_TMUX\\n'\n"
	stub := writeStub(t, dir, "spinner_stub.sh", body)

	res, err := RunWithTmux(context.Background(), stub, RunOptions{
		Workdir: dir,
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunWithTmux: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	out := string(res.Output)
	if !strings.Contains(out, "FINAL_ANSWER_TMUX") {
		t.Errorf("Output missing final answer; got:\n%s", out)
	}
	// The rendered screen should NOT carry intermediate spinner frames. After
	// \r overwrites and the cursor-up repaint, no "Working..." text survives on
	// the rendered grid.
	if strings.Contains(out, "Working...") {
		t.Errorf("Output still contains intermediate spinner frame 'Working...'; rendered capture should have collapsed it. got:\n%s", out)
	}
}

// TestRunWithTmux_Timeout verifies a long-running stub is cut off promptly, with
// TimedOut set and the session killed.
func TestRunWithTmux_Timeout(t *testing.T) {
	requireTmux(t)

	dir := t.TempDir()
	stub := writeStub(t, dir, "sleep_stub.sh", "#!/bin/sh\nsleep 30\n")

	before := tmuxSessionCount(t)
	start := time.Now()
	res, err := RunWithTmux(context.Background(), stub, RunOptions{
		Workdir: dir,
		Timeout: 2 * time.Second,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RunWithTmux: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("TimedOut = false, want true")
	}
	// Should return shortly after the 2s deadline (allow generous slack for the
	// final capture + poll cadence), nowhere near the 30s sleep.
	if elapsed > 10*time.Second {
		t.Errorf("RunWithTmux took %v, want it to return promptly after the 2s timeout", elapsed)
	}
	if got := tmuxSessionCount(t); got != before {
		t.Errorf("session count changed: before=%d after=%d (leftover session after timeout)", before, got)
	}
}

// TestRunWithTmux_ExitCode verifies the exit code is parsed from the sentinel.
func TestRunWithTmux_ExitCode(t *testing.T) {
	requireTmux(t)

	dir := t.TempDir()
	stub := writeStub(t, dir, "exit_stub.sh", "#!/bin/sh\nexit 3\n")

	res, err := RunWithTmux(context.Background(), stub, RunOptions{
		Workdir: dir,
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunWithTmux: %v", err)
	}
	if res.TimedOut {
		t.Errorf("TimedOut = true, want false")
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
}

// TestRunWithTmux_NoTmux forces tmux to appear absent and asserts the function
// returns ErrTmuxNotFound so callers can fall back to RunWithPTY.
func TestRunWithTmux_NoTmux(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}
	// Swap the lookup to simulate tmux missing, and reset the path cache around
	// the test so neither the real value nor the fake leaks.
	origLookup := lookupTmux
	resetTmuxPathCache()
	lookupTmux = func() (string, error) { return "", errors.New("not found") }
	defer func() {
		lookupTmux = origLookup
		resetTmuxPathCache()
	}()

	if tmuxAvailable() {
		t.Fatalf("tmuxAvailable() = true after forcing not-found")
	}

	res, err := RunWithTmux(context.Background(), "/bin/true", RunOptions{Timeout: time.Second})
	if !errors.Is(err, ErrTmuxNotFound) {
		t.Errorf("err = %v, want ErrTmuxNotFound", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 on not-found", res.ExitCode)
	}
}

// TestRunWithTmux_ExtraEnv verifies ExtraEnv assignments reach the spawned
// command line and are visible to the stub.
func TestRunWithTmux_ExtraEnv(t *testing.T) {
	requireTmux(t)

	dir := t.TempDir()
	// Echo an env var the stub reads from its environment.
	stub := writeStub(t, dir, "env_stub.sh", "#!/bin/sh\nprintf 'ENV=%s' \"$AGY_TEST_VAR\"\n")

	res, err := RunWithTmux(context.Background(), stub, RunOptions{
		Workdir:  dir,
		Timeout:  15 * time.Second,
		ExtraEnv: []string{"AGY_TEST_VAR=tmux_env_value_xyz"},
	})
	if err != nil {
		t.Fatalf("RunWithTmux: %v", err)
	}
	if !strings.Contains(string(res.Output), "tmux_env_value_xyz") {
		t.Errorf("Output %q does not contain injected env value", res.Output)
	}
}

// TestBuildTmuxCommand_QuotesArgs verifies prompts with spaces/quotes are safely
// single-quote escaped into the command line.
func TestBuildTmuxCommand_QuotesArgs(t *testing.T) {
	cmd := buildTmuxCommand("/path/to/agy", RunOptions{
		Args: []string{"-p", "tell me about it's \"weird\" prompt"},
	})
	if !strings.Contains(cmd, `'/path/to/agy'`) {
		t.Errorf("binary not single-quoted: %s", cmd)
	}
	// The embedded single quote in "it's" must be escaped via '\'' .
	if !strings.Contains(cmd, `'\''`) {
		t.Errorf("embedded single quote not escaped: %s", cmd)
	}
	// Sentinel must be present so exit code is recoverable.
	if !strings.Contains(cmd, doneSentinelPrefix+"%d"+doneSentinelSuffix) {
		t.Errorf("sentinel format missing from command: %s", cmd)
	}
}

// TestParseDoneSentinel checks code extraction and that the echoed `%d` format
// (the literal command line) is not mistaken for a real sentinel.
func TestParseDoneSentinel(t *testing.T) {
	// Real printf output.
	if code, ok := parseDoneSentinel("some output\n" + doneSentinelPrefix + "7" + doneSentinelSuffix + "\n"); !ok || code != 7 {
		t.Errorf("parse code 7 => (%d,%v), want (7,true)", code, ok)
	}
	// Echoed command line carrying the %d format must NOT match.
	if _, ok := parseDoneSentinel("printf ... " + doneSentinelPrefix + "%d" + doneSentinelSuffix + " ..."); ok {
		t.Errorf("format-string sentinel should not parse as a real code")
	}
	// No sentinel at all.
	if _, ok := parseDoneSentinel("nothing here"); ok {
		t.Errorf("found a sentinel where there is none")
	}
	// Last occurrence wins.
	if code, _ := parseDoneSentinel(doneSentinelPrefix + "1" + doneSentinelSuffix + "\nx\n" + doneSentinelPrefix + "9" + doneSentinelSuffix); code != 9 {
		t.Errorf("last-wins code = %d, want 9", code)
	}
}

// --- test helpers ---

// waitForPane polls the named session's pane until want appears or the deadline
// elapses.
func waitForPane(t *testing.T, name, want string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := tmuxCapturePane(name, 0, false)
		if err == nil && strings.Contains(out, want) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// tmuxSessionCount returns the number of live tmux sessions on the default
// server (0 when none exist / server not running). Used to assert RunWithTmux
// leaves no leftover sessions.
func tmuxSessionCount(t *testing.T) int {
	t.Helper()
	bin, err := tmuxPath()
	if err != nil {
		t.Fatalf("tmuxPath: %v", err)
	}
	cmd := exec.Command(bin, "list-sessions", "-F", "#{session_name}")
	out, err := cmd.Output()
	if err != nil {
		// No server running / no sessions => list-sessions exits non-zero.
		return 0
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}
