package agycli

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestE2E_AgyPongSmoke is an end-to-end smoke test that drives the REAL agy
// binary through the full plumbing stack: RunWithPTY (issue #76 workaround) ->
// ParseAgyOutput (text salvage) -> snapshot-diff conversation capture.
//
// It is gated behind AGY_E2E: when that env var is unset the test skips, so the
// default `go test ./...` (and CI without agy auth) never spawns agy. Run it
// explicitly with:
//
//	AGY_E2E=1 go test -run TestE2E -v ./internal/providers/agycli/...
//
// The binary is resolved from $AGY_BIN, else from PATH ("agy"). agy is agentic
// and slow even for a trivial prompt, so a generous timeout is used; the call is
// still bounded and will never hang the suite.
//
// IMPORTANT: agy persists its conversation to the REAL store at
// ~/.gemini/antigravity-cli/conversations regardless of AGY_CONVERSATIONS_DIR
// (that env var is a test-only hook for the snapshot helpers, NOT honored by
// agy). So this test snapshots the REAL store via ConversationsDir()'s default
// and does NOT override it — otherwise the diff would watch an empty temp dir
// while agy wrote elsewhere.
func TestE2E_AgyPongSmoke(t *testing.T) {
	if os.Getenv("AGY_E2E") == "" {
		t.Skip("AGY_E2E unset: skipping real-agy end-to-end smoke test")
	}

	// Resolve the agy binary (override via AGY_BIN; otherwise look it up on PATH).
	binary := os.Getenv("AGY_BIN")
	if binary == "" {
		found, err := exec.LookPath("agy")
		if err != nil {
			t.Skipf("agy binary not found on PATH and AGY_BIN unset: %v", err)
		}
		binary = found
	}

	// Fresh workspace: agy needs an active workspace, and starting clean keeps the
	// snapshot diff unambiguous.
	workspace := t.TempDir()

	// Snapshot the REAL conversation store (no env override): agy writes its new
	// <uuid>.db there no matter what, so the diff must look there too.
	convDir := ConversationsDir()
	t.Logf("watching real conversation store: %s", convDir)

	before, err := SnapshotConversations(convDir)
	if err != nil {
		t.Fatalf("snapshot before: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := RunWithPTY(ctx, binary, RunOptions{
		Workdir: workspace,
		// NOTE on argv order: agy's `-p`/`--print` is a STRING-valued flag
		// (Go flag pkg) that consumes the NEXT token as its value. So
		// `-p --dangerously-skip-permissions "<prompt>"` sets --print to the
		// literal string "--dangerously-skip-permissions" and leaves the real
		// prompt as a dangling positional — agy then answers about the flag (or
		// times out with empty output). The prompt MUST be the value of -p, with
		// --dangerously-skip-permissions placed after it.
		Args: []string{
			"-p", "Reply with exactly one word: PONG",
			"--dangerously-skip-permissions",
		},
		Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("RunWithPTY(agy) returned error: %v", err)
	}
	t.Logf("agy run: exit=%d timedOut=%v duration=%s rawBytes=%d",
		res.ExitCode, res.TimedOut, res.Duration, len(res.Output))
	if res.TimedOut {
		t.Fatalf("agy run timed out (output so far: %q)", string(res.Output))
	}
	if len(res.Output) == 0 {
		t.Fatalf("RunWithPTY returned empty output (PTY round-trip failed / issue #76 not worked around)")
	}

	parsed := ParseAgyOutput(res.Output)
	if strings.TrimSpace(parsed.Content) == "" {
		t.Fatalf("ParseAgyOutput produced empty content from %d raw bytes\n--- raw ---\n%s",
			len(res.Output), string(res.Output))
	}
	t.Logf("agy answer (confidence=%s, truncated=%v): %q",
		parsed.Confidence, parsed.Truncated, parsed.Content)

	// Snapshot-diff: agy mints its own conversation id and persists it as
	// <uuid>.db in the real store. We should detect exactly that new file.
	after, err := SnapshotConversations(convDir)
	if err != nil {
		t.Fatalf("snapshot after: %v", err)
	}
	if id, ok := DetectNewConversation(convDir, before, after); ok {
		t.Logf("captured new agy conversation id: %s", id)
	} else {
		t.Errorf("no new conversation .db detected in %s (before=%d after=%d)",
			convDir, len(before), len(after))
	}
}
