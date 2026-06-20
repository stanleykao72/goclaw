package agycli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// drainGrace is how long we let the reader keep copying after ctx fires, before
// we escalate to killing the process group. agy may print its final answer in
// the last moments before a timeout/cancel, and we want to capture those bytes.
const drainGrace = 250 * time.Millisecond

// readerCloseFallback bounds how long we wait for the reader goroutine after a
// kill. SIGKILL closes the slave so io.Copy normally EOFs on its own; we only
// force-close the master as a last resort if the reader is still stuck.
const readerCloseFallback = 2 * time.Second

// neutralizingBaseEnv is merged over os.Environ() (and then under opts.ExtraEnv)
// so agy renders quiet, fixed-width, color-free, non-paged output that the
// salvage parser can handle deterministically. TERM=dumb minimizes cursor/ANSI
// repaint, NO_COLOR/CI suppress decoration, PAGER=cat avoids an interactive
// pager that would block on the pty, and COLUMNS=200 keeps lines from wrapping.
var neutralizingBaseEnv = []string{
	"TERM=dumb",
	"NO_COLOR=1",
	"CI=1",
	"PAGER=cat",
	"COLUMNS=200",
}

// RunWithPTY runs `binary` under a pseudo-terminal so agy (which silently drops
// stdout on a non-TTY, issue #76) believes it is attached to a terminal.
// It merges a noise-neutralizing base env (TERM=dumb,NO_COLOR=1,CI=1,PAGER=cat,
// COLUMNS=200) over os.Environ(), then opts.ExtraEnv over that. Reads the pty
// master until EOF / ctx cancel / opts.Timeout, then kills the process group and
// drains. Never blocks indefinitely. Returns the raw (unparsed) bytes.
func RunWithPTY(ctx context.Context, binary string, opts RunOptions) (RunResult, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultRunTimeout
	}

	// Bound the whole run by the timeout. We derive a child context so a parent
	// cancellation also tears the process down.
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()

	// Note: we intentionally do NOT use exec.CommandContext here. Killing a
	// process running under a pty needs to target the whole process group (agy
	// spawns child tools), so we manage teardown ourselves below.
	cmd := exec.Command(binary, opts.Args...)
	cmd.Dir = opts.Workdir
	cmd.Env = buildEnv(opts.ExtraEnv)

	// Put the child in its own process group so we can signal the entire tree
	// (agy + any tools it spawns) with a single kill on the negative pgid.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true

	// Bound os/exec's own wait so cmd.Wait() cannot block forever if a copying
	// goroutine or the process itself lingers after the fds close. We separately
	// rely on killProcessGroup issuing SIGKILL, which guarantees child exit;
	// WaitDelay is the belt-and-suspenders cap inside os/exec (Go 1.20+).
	cmd.WaitDelay = readerCloseFallback

	// pty.Start allocates a pty, wires the child's stdin/stdout/stderr to the
	// slave side, and starts the process. A start failure (e.g. binary not
	// found) is returned here as an error.
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return RunResult{ExitCode: -1, Duration: time.Since(start)}, err
	}
	// Best-effort close of the master fd; closing it also unblocks any reader.
	defer func() { _ = ptmx.Close() }()

	// Drain the master in a goroutine so the read never blocks the timeout/cancel
	// path. On the slave's final close the master read returns EOF (on Linux it
	// may surface as EIO, which we treat as a clean end-of-stream).
	var buf bytes.Buffer
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		_, _ = io.Copy(&buf, ptmx)
	}()

	timedOut := false

	// Wait for either the read to finish (process exited and pty drained) or the
	// context to fire (timeout/cancel). In the latter case we tear the group down.
	select {
	case <-readDone:
		// Process produced EOF on its own; fall through to Wait below.
	case <-runCtx.Done():
		timedOut = true

		// Give the reader a short grace to copy any bytes the child wrote in
		// its final moments (e.g. agy printing its answer right before the
		// deadline) before we forcibly kill the group.
		select {
		case <-readDone:
			// Child finished on its own within the grace window; no kill needed
			// (the reader has already drained), fall through to Wait.
		case <-time.After(drainGrace):
			killProcessGroup(cmd)
			// SIGKILL closes the slave, so io.Copy should EOF on its own and the
			// reader will return without us touching the master. Only force-close
			// the master as a last resort if the reader is still stuck. Closing
			// it BEFORE the reader drains would discard bytes the child wrote but
			// the reader had not yet copied (the P1 data-loss bug).
			select {
			case <-readDone:
			case <-time.After(readerCloseFallback):
				_ = ptmx.Close()
				<-readDone
			}
		}
	}

	// Reap the direct child with a bound so a lingering process can't hang us
	// forever. WaitDelay (set in pty.Start path via cmd) also caps os/exec's own
	// wait once the fds are closed.
	exitCode := waitBounded(cmd)
	if timedOut {
		// A killed process has no meaningful exit status for our purposes.
		exitCode = -1
	}

	// Reap any grandchildren regardless of exit reason. agy spawns tool
	// subprocesses; on a CLEAN exit those can outlive the direct child (e.g. a
	// backgrounded `sleep &`). Now that the reader has drained and the direct
	// child is reaped, signal the whole process group to clean up stragglers.
	// On the timeout branch the group was already killed above; a second signal
	// is harmless (it targets an already-dead group).
	killProcessGroup(cmd)

	return RunResult{
		Output:   buf.Bytes(),
		ExitCode: exitCode,
		TimedOut: timedOut,
		Duration: time.Since(start),
	}, nil
}

// buildEnv layers the env as: os.Environ() < neutralizingBaseEnv < opts.ExtraEnv.
// Later entries with the same KEY override earlier ones; we de-dup by key so the
// child sees a single, well-defined value per variable.
func buildEnv(extra []string) []string {
	merged := map[string]string{}
	var order []string
	put := func(kv string) {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if _, seen := merged[key]; !seen {
			order = append(order, key)
		}
		merged[key] = kv
	}
	for _, kv := range os.Environ() {
		put(kv)
	}
	for _, kv := range neutralizingBaseEnv {
		put(kv)
	}
	for _, kv := range extra {
		put(kv)
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, merged[key])
	}
	return out
}

// killProcessGroup sends SIGKILL to the child's whole process group (agy plus
// any tools it spawned). Setsid above makes the child a group leader, so its PID
// is also the PGID; signalling the negative PID hits the entire group.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pgid, err := syscall.Getpgid(pid); err == nil {
		// Negative pgid targets the whole group.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	// Fallback: kill just the leader if we couldn't resolve the group.
	_ = cmd.Process.Kill()
}

// waitBounded reaps the process and returns its exit code, but never blocks
// longer than a short grace period. If the process is already gone it returns
// promptly; if it lingers past the grace window we give up waiting (the group
// kill has already been issued) and report an unknown exit code.
//
// The inner Wait goroutine could in principle outlive this function if
// cmd.Wait() never returns. In practice that does not happen: killProcessGroup
// issues SIGKILL (uninterceptable, guarantees child exit) and cmd.WaitDelay
// caps os/exec's own wait after the fds close, so cmd.Wait() is guaranteed to
// return and the goroutine to finish. The buffered channel ensures the
// goroutine's send never blocks even if we have already returned via timeout.
func waitBounded(cmd *exec.Cmd) int {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return exitCodeFromWaitErr(err)
	case <-time.After(5 * time.Second):
		// Lingering despite the kill; don't hang the caller.
		return -1
	}
}

// exitCodeFromWaitErr extracts a numeric exit code from cmd.Wait()'s error.
// nil error => 0. An *exec.ExitError carries the real code; anything else
// (including signal-terminated processes) maps to -1 (unknown/killed).
func exitCodeFromWaitErr(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
	}
	return -1
}
