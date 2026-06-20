package agycli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeStub writes an executable shell script to dir and returns its path.
// These stubs stand in for the real agy binary so unit tests never spawn it.
func writeStub(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
	return p
}

// TestRunWithPTY_CapturesOutput verifies that stdout produced by a child under a
// pty is captured into RunResult.Output, and that a clean exit yields code 0.
func TestRunWithPTY_CapturesOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty stubs are POSIX shell scripts")
	}
	dir := t.TempDir()
	const marker = "HELLO_FROM_STUB_42"
	stub := writeStub(t, dir, "echo_stub.sh", "#!/bin/sh\nprintf '%s\\n' '"+marker+"'\n")

	res, err := RunWithPTY(context.Background(), stub, RunOptions{
		Workdir: dir,
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunWithPTY returned error: %v", err)
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
	if res.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", res.Duration)
	}
}

// TestRunWithPTY_ExtraEnv verifies ExtraEnv is layered into the child env and
// overrides the neutralizing base env.
func TestRunWithPTY_ExtraEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty stubs are POSIX shell scripts")
	}
	dir := t.TempDir()
	// Print a custom var plus TERM (which ExtraEnv overrides over the base env).
	stub := writeStub(t, dir, "env_stub.sh", "#!/bin/sh\nprintf 'VAR=%s TERM=%s\\n' \"$STUB_VAR\" \"$TERM\"\n")

	res, err := RunWithPTY(context.Background(), stub, RunOptions{
		Workdir:  dir,
		Timeout:  10 * time.Second,
		ExtraEnv: []string{"STUB_VAR=banana", "TERM=xterm-override"},
	})
	if err != nil {
		t.Fatalf("RunWithPTY returned error: %v", err)
	}
	out := string(res.Output)
	if !strings.Contains(out, "VAR=banana") {
		t.Errorf("Output %q missing VAR=banana (ExtraEnv not applied)", out)
	}
	if !strings.Contains(out, "TERM=xterm-override") {
		t.Errorf("Output %q missing TERM=xterm-override (ExtraEnv did not override base env)", out)
	}
}

// TestRunWithPTY_Timeout verifies that a child exceeding the Timeout is killed,
// TimedOut is set, and the call returns promptly (well under the sleep).
func TestRunWithPTY_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /bin/sleep on windows")
	}
	dir := t.TempDir()
	// Sleep far longer than the timeout so the kill path is exercised.
	stub := writeStub(t, dir, "sleep_stub.sh", "#!/bin/sh\nsleep 30\n")

	began := time.Now()
	res, err := RunWithPTY(context.Background(), stub, RunOptions{
		Workdir: dir,
		Timeout: 200 * time.Millisecond,
	})
	elapsed := time.Since(began)
	if err != nil {
		t.Fatalf("RunWithPTY returned error: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("TimedOut = false, want true")
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for killed process", res.ExitCode)
	}
	// Must return well before the 30s sleep (allow generous slack for reap grace).
	if elapsed > 10*time.Second {
		t.Errorf("RunWithPTY took %v, expected prompt return after timeout", elapsed)
	}
}

// TestRunWithPTY_ContextCancel verifies that cancelling the parent context tears
// the process down and marks TimedOut.
func TestRunWithPTY_ContextCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /bin/sleep on windows")
	}
	dir := t.TempDir()
	stub := writeStub(t, dir, "sleep_stub.sh", "#!/bin/sh\nsleep 30\n")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	began := time.Now()
	res, err := RunWithPTY(ctx, stub, RunOptions{
		Workdir: dir,
		Timeout: 30 * time.Second, // long; cancel should fire first
	})
	elapsed := time.Since(began)
	if err != nil {
		t.Fatalf("RunWithPTY returned error: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("TimedOut = false, want true after context cancel")
	}
	if elapsed > 10*time.Second {
		t.Errorf("RunWithPTY took %v, expected prompt return after cancel", elapsed)
	}
}

// TestRunWithPTY_BinaryNotFound verifies a missing binary surfaces as an error.
func TestRunWithPTY_BinaryNotFound(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "definitely-not-a-real-binary")

	res, err := RunWithPTY(context.Background(), missing, RunOptions{
		Workdir: dir,
		Timeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatalf("expected error for missing binary, got nil (result: %+v)", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 on start failure", res.ExitCode)
	}
}

// TestRunWithPTY_NonZeroExit verifies a clean non-zero exit code is reported.
func TestRunWithPTY_NonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty stubs are POSIX shell scripts")
	}
	dir := t.TempDir()
	stub := writeStub(t, dir, "exit_stub.sh", "#!/bin/sh\nprintf 'partial\\n'\nexit 3\n")

	res, err := RunWithPTY(context.Background(), stub, RunOptions{
		Workdir: dir,
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunWithPTY returned error: %v", err)
	}
	if res.TimedOut {
		t.Errorf("TimedOut = true, want false")
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(string(res.Output), "partial") {
		t.Errorf("Output %q missing 'partial'", res.Output)
	}
}

// TestRunWithPTY_PartialOutputCapturedOnTimeout is the LOSSMARK regression: a
// child that prints a marker and then sleeps far past the timeout must still
// have its marker captured. The earlier teardown closed the pty master before
// the reader drained, racing away bytes the child had already written (~3/10).
// We run several iterations so a residual race would surface.
func TestRunWithPTY_PartialOutputCapturedOnTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty stubs are POSIX shell scripts")
	}
	dir := t.TempDir()
	const marker = "LOSSMARK"
	// Print the marker (with a newline so the line is flushed), then sleep well
	// past the timeout so the kill path runs while the marker is in flight.
	stub := writeStub(t, dir, "loss_stub.sh", "#!/bin/sh\nprintf '%s\\n' '"+marker+"'\nsleep 30\n")

	const iters = 10
	for i := 0; i < iters; i++ {
		res, err := RunWithPTY(context.Background(), stub, RunOptions{
			Workdir: dir,
			Timeout: 100 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("iter %d: RunWithPTY returned error: %v", i, err)
		}
		if !res.TimedOut {
			t.Errorf("iter %d: TimedOut = false, want true", i)
		}
		if !strings.Contains(string(res.Output), marker) {
			t.Fatalf("iter %d: Output %q lost marker %q (P1 data-loss regression)", i, res.Output, marker)
		}
	}
}

// TestRunWithPTY_GrandchildReapedOnCleanExit verifies that a tool grandchild
// backgrounded by the child is reaped even when the direct child exits cleanly.
// The stub backgrounds a process that, after a delay, touches a sentinel file;
// if the process group is killed on the clean-exit path the sentinel never
// appears. Reproduces the original leak (stub returned 0 while the grandchild
// lived on).
func TestRunWithPTY_GrandchildReapedOnCleanExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty stubs are POSIX shell scripts")
	}
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "grandchild_survived")
	// Background a grandchild that sleeps briefly then writes the sentinel, then
	// the direct child exits 0 immediately. If we fail to kill the group on clean
	// exit, the grandchild survives and creates the sentinel.
	body := "#!/bin/sh\n" +
		"( sleep 1; : > '" + sentinel + "' ) &\n" +
		"printf 'DONE\\n'\n" +
		"exit 0\n"
	stub := writeStub(t, dir, "grandchild_stub.sh", body)

	res, err := RunWithPTY(context.Background(), stub, RunOptions{
		Workdir: dir,
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunWithPTY returned error: %v", err)
	}
	if res.TimedOut {
		t.Errorf("TimedOut = true, want false (clean exit)")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(string(res.Output), "DONE") {
		t.Errorf("Output %q missing DONE", res.Output)
	}

	// Wait past the grandchild's sleep; if the group was reaped, the sentinel
	// must NOT appear.
	time.Sleep(2 * time.Second)
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Errorf("grandchild survived clean exit and wrote %s (process group leak)", sentinel)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error: %v", statErr)
	}
}

// TestBuildEnv_Layering verifies the os < base < extra override ordering and
// that each key appears exactly once.
func TestBuildEnv_Layering(t *testing.T) {
	t.Setenv("AGYCLI_TEST_BASEKEY", "from_os")
	t.Setenv("TERM", "from_os") // base env should override this

	env := buildEnv([]string{"AGYCLI_TEST_BASEKEY=from_extra"})

	got := map[string]string{}
	seen := map[string]int{}
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		got[kv[:i]] = kv[i+1:]
		seen[kv[:i]]++
	}

	if got["TERM"] != "dumb" {
		t.Errorf("TERM = %q, want dumb (base env override of os)", got["TERM"])
	}
	if got["NO_COLOR"] != "1" {
		t.Errorf("NO_COLOR = %q, want 1", got["NO_COLOR"])
	}
	if got["AGYCLI_TEST_BASEKEY"] != "from_extra" {
		t.Errorf("AGYCLI_TEST_BASEKEY = %q, want from_extra (ExtraEnv override)", got["AGYCLI_TEST_BASEKEY"])
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("key %q appears %d times, want exactly 1", k, n)
		}
	}
}
