package agycli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// UNIT: turn-state classification (Phase 0 Q3) — no agy, synthetic captures.
// ---------------------------------------------------------------------------

func TestClassifyState(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want paneState
	}{
		{
			name: "ready idle footer",
			pane: "Here is the answer.\n\n> \n  Gemini 3.x Flash (Medium)   ? for shortcuts",
			want: stateReady,
		},
		{
			name: "busy streaming footer",
			pane: "Thinking...\n> \n  Gemini 3.x Flash (Medium)   esc to cancel",
			want: stateBusy,
		},
		{
			name: "busy wins when both present (transient frame)",
			pane: "partial\n? for shortcuts ... esc to cancel",
			want: stateBusy,
		},
		{
			name: "neither marker (trust gate / booting)",
			pane: "Do you trust the contents of this project?\n  Yes   No",
			want: stateUnknown,
		},
		{
			name: "interactive menu => awaiting input (wins over busy)",
			pane: "Which city?\n  1. Taipei\n  2. Tokyo\n\n  ↑/↓ Navigate · enter Select · esc Skip\n  esc to cancel",
			want: stateAwaitingInput,
		},
		{
			name: "menu alternate wording (Confirm) => awaiting input",
			pane: "Pick one:\n  ↑/↓ Navigate   enter Confirm   esc to cancel",
			want: stateAwaitingInput,
		},
		{
			name: "prose with Select/Navigate but no arrow => not awaiting (ready)",
			pane: "Navigate to settings and Select your option.\n> \n  Gemini 3.x Flash (Medium)   ? for shortcuts",
			want: stateReady,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean := string(StripANSI([]byte(tc.pane)))
			if got := classifyState(clean); got != tc.want {
				t.Errorf("classifyState = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsTrustGate(t *testing.T) {
	trust := "  Antigravity CLI\n\n  Do you trust the contents of this project?\n\n  > Yes\n    No\n"
	if !isTrustGate(string(StripANSI([]byte(trust)))) {
		t.Errorf("isTrustGate(trust screen) = false, want true")
	}
	// A normal ready answer that happens to contain the word 'trust' must NOT be
	// classified as the gate (it has the ready marker).
	ready := "You can trust this result.\n> \n  Gemini 3.x Flash (Medium)   ? for shortcuts"
	if isTrustGate(string(StripANSI([]byte(ready)))) {
		t.Errorf("isTrustGate(ready answer mentioning trust) = true, want false")
	}
	// A busy screen is never the gate.
	busy := "trust me, working...\n  esc to cancel"
	if isTrustGate(string(StripANSI([]byte(busy)))) {
		t.Errorf("isTrustGate(busy) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// UNIT: answer extraction from a synthetic multi-turn pane dump.
// ---------------------------------------------------------------------------

// twoTurnPane simulates the rendered scrollback after two turns. The SECOND
// turn's answer is multi-line and must be the one extracted (the LAST echoed
// prompt wins), with the echo line and footer stripped and internal blank lines
// preserved.
const twoTurnPane = `╭──────────────────────────────────────────────╮
│ > Remember this codeword: PURPLE-RHINO-42      │
╰──────────────────────────────────────────────╯
OK

  Gemini 3.x Flash (Medium)   ? for shortcuts
╭──────────────────────────────────────────────╮
│ > What codeword did I tell you earlier?        │
╰──────────────────────────────────────────────╯
The codeword you told me earlier is:

PURPLE-RHINO-42

Let me know if you need anything else.

>
  Gemini 3.x Flash (Medium)   ? for shortcuts`

func TestExtractTurnAnswer_SecondTurnMultiline(t *testing.T) {
	answer, conf := extractTurnAnswer(twoTurnPane, "What codeword did I tell you earlier?", "")

	if !strings.Contains(answer, "PURPLE-RHINO-42") {
		t.Errorf("answer missing codeword.\nanswer=%q", answer)
	}
	// Multi-line must survive: both the opening sentence and the trailing one.
	if !strings.Contains(answer, "The codeword you told me earlier is:") {
		t.Errorf("answer missing opening line.\nanswer=%q", answer)
	}
	if !strings.Contains(answer, "Let me know if you need anything else.") {
		t.Errorf("answer missing trailing line (truncated at blank?).\nanswer=%q", answer)
	}
	// Must NOT contain the first turn's "OK" answer or the echoed prompt.
	if strings.Contains(answer, "Remember this codeword") {
		t.Errorf("answer leaked turn-1 echo.\nanswer=%q", answer)
	}
	if strings.Contains(answer, "? for shortcuts") {
		t.Errorf("answer leaked footer.\nanswer=%q", answer)
	}
	if strings.HasPrefix(strings.TrimSpace(answer), "OK") {
		t.Errorf("answer leaked turn-1 answer 'OK'.\nanswer=%q", answer)
	}
	if conf != ConfidenceHigh {
		t.Errorf("Confidence = %v, want high (cleanly bounded)", conf)
	}
}

func TestExtractTurnAnswer_FirstTurn(t *testing.T) {
	answer, _ := extractTurnAnswer(twoTurnPane, "Remember this codeword: PURPLE-RHINO-42", "")
	// Matching the FIRST prompt — but lastEchoIndex finds the LAST line that
	// contains that exact text, which is the turn-1 echo (turn-2 echo doesn't
	// contain it), so the bounded answer should be "OK".
	if strings.TrimSpace(answer) != "OK" {
		t.Errorf("turn-1 answer = %q, want %q", answer, "OK")
	}
}

func TestCleanAnswerLines_PreservesInternalBlanks(t *testing.T) {
	lines := []string{
		"╭────────╮",
		"Para one.",
		"",
		"Para two.",
		"> ",
		"  Gemini 3.x Flash (Medium)   ? for shortcuts",
	}
	got := cleanAnswerLines(lines)
	want := "Para one.\n\nPara two."
	if got != want {
		t.Errorf("cleanAnswerLines = %q, want %q", got, want)
	}
}

func TestDedent_StripsGutterPreservesRelative(t *testing.T) {
	// agy renders a 2-space gutter; nested content keeps its extra indent.
	in := []string{"  Top line", "    Nested item", "", "  Back to top"}
	got := dedent(in)
	want := []string{"Top line", "  Nested item", "", "Back to top"}
	if len(got) != len(want) {
		t.Fatalf("dedent length = %d, want %d (%q)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dedent[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// A line at column 0 means no common gutter => unchanged.
	flush := []string{"no indent", "  some"}
	if g := dedent(flush); g[0] != "no indent" || g[1] != "  some" {
		t.Errorf("dedent should be a no-op when a line is flush-left: %q", g)
	}
}

func TestCleanAnswerLines_DedentsAgyGutter(t *testing.T) {
	// Mirrors the real agy rendering: 2-space gutter on the answer line.
	lines := []string{"  PURPLE-RHINO-42  ", "  Gemini 3.x Flash (Medium)   ? for shortcuts"}
	got := cleanAnswerLines(lines)
	if got != "PURPLE-RHINO-42" {
		t.Errorf("cleanAnswerLines = %q, want %q (gutter not stripped?)", got, "PURPLE-RHINO-42")
	}
}

func TestApplySessionDefaults(t *testing.T) {
	o := applySessionDefaults(SessionOptions{})
	if o.PaneWidth != defaultPaneWidth || o.PaneHeight != defaultPaneHeight {
		t.Errorf("pane defaults not applied: %dx%d", o.PaneWidth, o.PaneHeight)
	}
	if o.ReadyTimeout != defaultReadyTimeout {
		t.Errorf("ReadyTimeout default not applied: %v", o.ReadyTimeout)
	}
	if o.TurnTimeout != DefaultRunTimeout {
		t.Errorf("TurnTimeout default not applied: %v", o.TurnTimeout)
	}
}

func TestBuildInteractiveLaunch(t *testing.T) {
	cmd := buildInteractiveLaunch("/path/to/agy", SessionOptions{
		SkipPermissions: true,
		Sandbox:         true,
		Model:           "gemini-3-pro",
	})
	if strings.Contains(cmd, "--print") || strings.Contains(cmd, " -p ") {
		t.Errorf("interactive launch must NOT contain --print: %s", cmd)
	}
	for _, want := range []string{"'/path/to/agy'", "--dangerously-skip-permissions", "--sandbox", "--model 'gemini-3-pro'"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("launch missing %q: %s", want, cmd)
		}
	}
}

// ---------------------------------------------------------------------------
// UNIT: Phase 2 P3 fixes in tmux.go (nonce sentinels + env-key validation).
// ---------------------------------------------------------------------------

func TestSentinelsWithNonce(t *testing.T) {
	s := sentinelsWithNonce("deadbeef")
	if !strings.Contains(s.start, "deadbeef") || !strings.Contains(s.donePrefix, "deadbeef") {
		t.Errorf("nonce not embedded: start=%q donePrefix=%q", s.start, s.donePrefix)
	}
	// Empty nonce degrades to the default constants.
	d := sentinelsWithNonce("")
	if d.start != startSentinel || d.donePrefix != doneSentinelPrefix {
		t.Errorf("empty nonce did not degrade to defaults: %+v", d)
	}
}

func TestNonceFromSessionName(t *testing.T) {
	if got := nonceFromSessionName("agycli-0011aabb"); got != "0011aabb" {
		t.Errorf("nonceFromSessionName = %q, want %q", got, "0011aabb")
	}
	// Non-alphanumerics dropped; prefix-less name handled.
	if got := nonceFromSessionName("foo-bar.baz"); got != "foobarbaz" {
		t.Errorf("nonceFromSessionName(no prefix) = %q, want %q", got, "foobarbaz")
	}
}

func TestParseDoneSentinelSent_NonceIsolation(t *testing.T) {
	a := sentinelsWithNonce("aaaa")
	b := sentinelsWithNonce("bbbb")
	// A DONE marker minted with nonce a must NOT be parsed by b's matcher.
	screen := "output\n" + a.donePrefix + "5" + a.doneSuffix + "\n"
	if _, ok := parseDoneSentinelSent(screen, b); ok {
		t.Errorf("nonce-b matcher matched nonce-a sentinel — collision not prevented")
	}
	if code, ok := parseDoneSentinelSent(screen, a); !ok || code != 5 {
		t.Errorf("nonce-a matcher failed on its own sentinel: code=%d ok=%v", code, ok)
	}
}

func TestBuildTmuxCommand_RejectsBadEnvKey(t *testing.T) {
	cmd := buildTmuxCommand("/bin/echo", RunOptions{
		ExtraEnv: []string{
			"GOOD_KEY=value1",
			"bad key=value2",       // space in key => invalid, must be skipped
			"1BAD=value3",          // leading digit => invalid, must be skipped
			"INJECT;rm -rf=value4", // shell metachars => invalid, must be skipped
			"OK2=value5",           // valid
			"noequalsign",          // no '=' => skipped
		},
	})
	if !strings.Contains(cmd, "GOOD_KEY=") || !strings.Contains(cmd, "OK2=") {
		t.Errorf("valid env keys missing from command: %s", cmd)
	}
	for _, bad := range []string{"bad key=", "1BAD=", "INJECT;rm -rf=", "noequalsign="} {
		if strings.Contains(cmd, bad) {
			t.Errorf("invalid env entry %q leaked into command: %s", bad, cmd)
		}
	}
	// The dangerous metachars must not appear as an unquoted assignment.
	if strings.Contains(cmd, "INJECT;rm") {
		t.Errorf("shell-injection env key leaked: %s", cmd)
	}
}

func TestValidEnvKey(t *testing.T) {
	good := []string{"A", "_x", "FOO_BAR", "x1", "_"}
	bad := []string{"", "1a", "a-b", "a b", "a=b", "a;b", "FOO BAR"}
	for _, k := range good {
		if !validEnvKey(k) {
			t.Errorf("validEnvKey(%q) = false, want true", k)
		}
	}
	for _, k := range bad {
		if validEnvKey(k) {
			t.Errorf("validEnvKey(%q) = true, want false", k)
		}
	}
}

// ---------------------------------------------------------------------------
// UNIT: GEMINI.md system-prompt delivery (no agy/tmux needed).
// ---------------------------------------------------------------------------

// The system prompt must be written to <workdir>/GEMINI.md so agy reads it as
// system/context instructions (followed, not echoed) — NOT folded into a turn.
func TestWriteGeminiInstructions_WritesFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ws") // non-existent subdir => MkdirAll path
	const prompt = "You are TB. Answer in one sentence and sign — TB."

	syncGeminiInstructions(dir, prompt)

	path := filepath.Join(dir, geminiInstructionsFile)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("GEMINI.md not written: %v", err)
	}
	// GEMINI.md = system prompt + the always-appended channel directive.
	want := prompt + agyChannelDirective
	if string(got) != want {
		t.Errorf("GEMINI.md content = %q, want %q", got, want)
	}
	if !strings.Contains(string(got), prompt) || !strings.Contains(string(got), "TEXT-ONLY") {
		t.Errorf("GEMINI.md must contain both the system prompt and the channel directive; got %q", got)
	}
	if base := filepath.Base(path); base != "GEMINI.md" {
		t.Errorf("instructions filename = %q, want GEMINI.md", base)
	}
}

func TestWriteGeminiInstructions_EmptyPromptStillWritesDirective(t *testing.T) {
	dir := t.TempDir()
	syncGeminiInstructions(dir, "")
	got, err := os.ReadFile(filepath.Join(dir, geminiInstructionsFile))
	if err != nil {
		t.Fatalf("GEMINI.md not written for empty prompt: %v", err)
	}
	if !strings.Contains(string(got), "TEXT-ONLY") {
		t.Errorf("empty-prompt GEMINI.md must still contain the channel directive; got %q", got)
	}
	if strings.HasPrefix(string(got), "\n") {
		t.Errorf("empty-prompt GEMINI.md must not start with a blank line; got %q", got)
	}
}

func TestWriteGeminiInstructions_OverwritesStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, geminiInstructionsFile)
	if err := os.WriteFile(path, []byte("STALE PRIOR INSTRUCTIONS"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncGeminiInstructions(dir, "") // empty prompt: must replace stale with directive-only
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), "STALE") {
		t.Errorf("stale GEMINI.md content survived; got %q", got)
	}
	if !strings.Contains(string(got), "TEXT-ONLY") {
		t.Errorf("expected directive after overwrite; got %q", got)
	}
}

// A write failure (e.g. workdir is a regular file) must be swallowed, not panic.
func TestWriteGeminiInstructions_FailureIsNonFatal(t *testing.T) {
	// Make a regular file and try to use it as the workdir: MkdirAll fails.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Must not panic; the function logs a warning and returns.
	syncGeminiInstructions(f, "irrelevant")
}

// ---------------------------------------------------------------------------
// REAL-AGY e2e: the headline proof of multi-turn memory. Gated by AGY_E2E.
// ---------------------------------------------------------------------------

// resolveAgyBinary finds the agy binary via AGY_BIN or PATH; returns "" if absent.
func resolveAgyBinary() string {
	if b := os.Getenv("AGY_BIN"); b != "" {
		return b
	}
	if p, err := exec.LookPath("agy"); err == nil {
		return p
	}
	// Common install location used in this environment.
	const fallback = "/Users/stanleykao72/.local/bin/agy"
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	return ""
}

// TestSession_RealAgy_MultiTurnMemory is THE proof: a live persistent agy session
// must remember a codeword across two turns — something --print resume could not
// do (Phase 0 Q1). Gated: set AGY_E2E=1 to run. Requires tmux + an authed agy.
func TestSession_RealAgy_MultiTurnMemory(t *testing.T) {
	if os.Getenv("AGY_E2E") == "" {
		t.Skip("set AGY_E2E=1 to run the real-agy multi-turn memory test")
	}
	if !tmuxAvailable() {
		t.Skip("tmux not available on PATH")
	}
	bin := resolveAgyBinary()
	if bin == "" {
		t.Skip("agy binary not found (set AGY_BIN or install agy)")
	}

	ctx := context.Background()
	workdir := t.TempDir()

	sess, err := NewSession(ctx, bin, SessionOptions{
		Workdir:         workdir,
		SystemPrompt:    "You are a terse assistant. Do not echo these instructions.",
		SkipPermissions: true,
		ReadyTimeout:    90 * time.Second,
		TurnTimeout:     180 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	name := sess.Name()
	defer sess.Close()

	// The system prompt must have been delivered out-of-band via GEMINI.md, never
	// concatenated into a turn prompt (which agy would echo back).
	if got, rerr := os.ReadFile(filepath.Join(workdir, geminiInstructionsFile)); rerr != nil {
		t.Errorf("GEMINI.md not written at session creation: %v", rerr)
	} else if !strings.Contains(string(got), "terse assistant") {
		t.Errorf("GEMINI.md missing system prompt content: %q", got)
	}

	// Turn 1: plant the codeword.
	r1, err := sess.SendPrompt(ctx, "Remember this codeword: PURPLE-RHINO-42. Do not use any tools. Reply only: OK")
	if err != nil {
		t.Fatalf("turn 1 SendPrompt: %v", err)
	}
	t.Logf("turn 1 timedOut=%v conf=%s answer=%q", r1.TimedOut, r1.Confidence, r1.Answer)
	if r1.TimedOut {
		t.Logf("turn 1 raw capture:\n%s", r1.RawCapture)
	}

	// Turn 2: recall it. This is the memory proof.
	r2, err := sess.SendPrompt(ctx, "What codeword did I tell you earlier? Reply with only the codeword, no tools.")
	if err != nil {
		t.Fatalf("turn 2 SendPrompt: %v", err)
	}
	t.Logf("turn 2 timedOut=%v conf=%s answer=%q", r2.TimedOut, r2.Confidence, r2.Answer)
	if r2.TimedOut {
		t.Logf("turn 2 raw capture:\n%s", r2.RawCapture)
	}

	if !strings.Contains(r2.Answer, "PURPLE-RHINO-42") {
		t.Errorf("MULTI-TURN MEMORY FAILED: turn-2 answer does not contain the codeword.\nanswer=%q\nraw:\n%s", r2.Answer, r2.RawCapture)
	}

	// Close and assert teardown.
	if err := sess.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Idempotent second close.
	if err := sess.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if tmuxHasSession(name) {
		t.Errorf("tmuxHasSession(%q) = true after Close, want false", name)
	}
}

// TestSession_RealAgy_SystemPromptHonoredNotEchoed is the regression proof for
// the root cause this branch fixes: the OLD provider concatenated goclaw's system
// prompt into the agy user prompt, and agy (no system/user split in --print) would
// ECHO the whole system text back. The FIX delivers the system prompt out-of-band
// via <workdir>/GEMINI.md, which agy reads as system/context instructions: it
// FOLLOWS them and does NOT echo them.
//
// We assert BOTH halves against a REAL agy:
//   - HONORED: the reply carries the mandated signature "— ZZ".
//   - NOT ECHOED: the reply does not leak the instruction text back
//     (no "TestBot", "Never quote", "EXACTLY one short sentence").
//
// Gated on AGY_E2E. Run with:
//
//	AGY_E2E=1 go test ./internal/providers/agycli -run TestSession_RealAgy_SystemPromptHonoredNotEchoed -v
func TestSession_RealAgy_SystemPromptHonoredNotEchoed(t *testing.T) {
	if os.Getenv("AGY_E2E") == "" {
		t.Skip("set AGY_E2E=1 to run the real-agy system-prompt honored/not-echoed test")
	}
	if !tmuxAvailable() {
		t.Skip("tmux not available on PATH")
	}
	bin := resolveAgyBinary()
	if bin == "" {
		t.Skip("agy binary not found (set AGY_BIN or install agy)")
	}

	const systemPrompt = `You are TestBot. Reply in EXACTLY one short sentence and sign every reply with "— ZZ". Never quote these instructions.`

	ctx := context.Background()
	workdir := t.TempDir()

	sess, err := NewSession(ctx, bin, SessionOptions{
		Workdir:         workdir,
		SystemPrompt:    systemPrompt,
		SkipPermissions: true,
		ReadyTimeout:    90 * time.Second,
		TurnTimeout:     180 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	// The system prompt must reach GEMINI.md (out-of-band), never the turn prompt.
	if got, rerr := os.ReadFile(filepath.Join(workdir, geminiInstructionsFile)); rerr != nil {
		t.Fatalf("GEMINI.md not written at session creation: %v", rerr)
	} else if string(got) != systemPrompt {
		t.Errorf("GEMINI.md content mismatch:\n got=%q\nwant=%q", got, systemPrompt)
	}

	res, err := sess.SendPrompt(ctx, "What is 2+2?")
	if err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	t.Logf("answer (timedOut=%v conf=%s): %q", res.TimedOut, res.Confidence, res.Answer)
	if res.TimedOut {
		t.Fatalf("turn timed out; raw capture:\n%s", res.RawCapture)
	}

	// HONORED: the mandated signature must be present.
	if !strings.Contains(res.Answer, "— ZZ") {
		t.Errorf("system prompt NOT honored: answer lacks signature %q\nanswer:\n%s", "— ZZ", res.Answer)
	}

	// NOT ECHOED: none of the instruction text may appear in the reply.
	for _, leak := range []string{"TestBot", "Never quote", "EXACTLY one short sentence"} {
		if strings.Contains(res.Answer, leak) {
			t.Errorf("system prompt ECHOED back: answer contains %q (regression of the concat bug)\nanswer:\n%s", leak, res.Answer)
		}
	}
}
