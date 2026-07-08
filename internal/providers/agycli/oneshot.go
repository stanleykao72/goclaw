package agycli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// oneshot.go drives a single non-interactive "agy --print" run over plain
// pipes (no PTY, no tmux). W0 probes (2026-07-08, agy v1.0.10 local + v1.0.14
// on the deploy target) established that non-TTY --print works and that
// --conversation <agy-minted-id> resumes with full context across processes,
// so a persistent interactive session is not required for multi-turn memory.
//
// Two W0 caveats shape this file:
//   - "--print-timeout" can be wedged through (a 1.0.10 turn ran >7m past a
//     90s print-timeout), so every run is bounded by a hard wall-clock kill of
//     the whole process group.
//   - A killed turn may have COMPLETED server-side (the conversation had
//     advanced despite the client-side kill), so callers must NOT auto-retry a
//     timed-out turn — a retry can double-execute the user's request.

// printTimeoutGrace is how much longer than --print-timeout the hard kill
// waits. The grace covers agy's normal post-answer teardown; past it we assume
// the wedge failure mode and kill the process group. A var (not const) only so
// tests can shrink it to exercise the hard-kill path quickly.
var printTimeoutGrace = 30 * time.Second

// createdConversationRE captures the conversation ID agy logs on a fresh run
// ("Created conversation <uuid>"). W0 verified this line appears in the file
// passed via --log-file on both 1.0.10 and 1.0.14, and does NOT appear when a
// run resumes an existing conversation.
var createdConversationRE = regexp.MustCompile(`Created conversation ([0-9a-fA-F-]{36})`)

// PrintResult is the outcome of one RunPrint call.
type PrintResult struct {
	Stdout         string        // raw stdout (undecoded answer text; feed to ExtractPrintAnswer)
	Stderr         string        // raw stderr (diagnostics only)
	ConversationID string        // id captured from the run's --log-file; empty on resume runs or when capture failed
	ExitCode       int           // process exit code; -1 if killed/unknown
	TimedOut       bool          // true when the hard wall-clock kill fired (see the double-execution caveat above)
	Duration       time.Duration // wall-clock duration of the run
}

// RunPrint executes one agy --print run and returns its outcome.
//
// Contract:
//   - opts.PrintTimeout must be > 0 (it also derives the hard kill at
//     PrintTimeout+printTimeoutGrace). A non-positive value gets
//     DefaultRunTimeout.
//   - When opts.Conversation == "" and opts.LogFile == "", RunPrint mints a
//     private temp log file, passes it via --log-file, extracts the
//     "Created conversation" ID into PrintResult.ConversationID, and removes
//     the file. A caller-supplied LogFile is used as-is and left on disk (the
//     ID is still extracted from it).
//   - extraEnv entries (KEY=VALUE) override the process env; this is how the
//     caller redirects HOME at a per-session fake home for MCP-config
//     isolation (O1). The noise-neutralizing base env from pty.go is applied
//     under extraEnv for parity with the PTY path.
//   - The child runs in its own process group and the ENTIRE group is killed
//     on timeout/cancel (agy spawns tool children).
//   - A hard-timeout run returns TimedOut=true and a nil error: the caller
//     decides how to surface it, and must not retry (double-execution). A
//     CALLER-cancelled run instead returns ctx.Err() so cancellation is never
//     mislabeled as the wedge-timeout failure mode.
func RunPrint(ctx context.Context, binary string, opts PrintOptions, extraEnv []string) (PrintResult, error) {
	if opts.PrintTimeout <= 0 {
		opts.PrintTimeout = DefaultRunTimeout
	}

	// First turn: mint a temp log file so the conversation ID can be captured.
	logFile := opts.LogFile
	ownLog := false
	if opts.Conversation == "" && logFile == "" {
		f, err := os.CreateTemp("", "agy-oneshot-*.log")
		if err != nil {
			return PrintResult{ExitCode: -1}, fmt.Errorf("agycli: create log temp: %w", err)
		}
		logFile = f.Name()
		_ = f.Close()
		ownLog = true
		opts.LogFile = logFile
	}
	if ownLog {
		defer os.Remove(logFile)
	}

	args, err := BuildPrintArgs(opts)
	if err != nil {
		return PrintResult{ExitCode: -1}, err
	}

	hardTimeout := opts.PrintTimeout + printTimeoutGrace
	runCtx, cancel := context.WithTimeout(ctx, hardTimeout)
	defer cancel()

	start := time.Now()

	cmd := exec.CommandContext(runCtx, binary, args...)
	cmd.Env = buildEnv(extraEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Teardown must target the whole process group (agy spawns child tools),
	// mirroring RunWithPTY — override the default single-process Kill.
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return nil
	}
	// Bound Wait's pipe drain: if a tool child escaped the process group and
	// holds the stdout/stderr pipe open past the kill, give up on the pipes
	// after 5s instead of wedging this session's entry.mu forever.
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = nil // /dev/null; W0 confirmed non-TTY --print completes normally

	runErr := cmd.Run()
	if runErr != nil && runCtx.Err() == nil && !errors.Is(runErr, exec.ErrWaitDelay) {
		if _, isExit := runErr.(*exec.ExitError); !isExit {
			// Spawn-level failure (binary missing, not executable, ...).
			return PrintResult{ExitCode: -1, Duration: time.Since(start)}, fmt.Errorf("agycli: run %s: %w", binary, runErr)
		}
	}

	// Distinguish caller cancellation from the hard-timeout wedge kill: a
	// cancelled run is the caller's decision and must surface as ctx.Err(),
	// NOT as TimedOut (which carries the never-retry double-execution
	// contract for turns that may have completed server-side).
	if ctx.Err() != nil {
		return PrintResult{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			ExitCode: exitCodeOf(cmd),
			Duration: time.Since(start),
		}, ctx.Err()
	}
	timedOut := runCtx.Err() != nil

	res := PrintResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCodeOf(cmd),
		TimedOut: timedOut,
		Duration: time.Since(start),
	}

	if logFile != "" {
		if id, ok := captureCreatedConversation(logFile); ok {
			res.ConversationID = id
		}
	}
	return res, nil
}

// exitCodeOf returns the process exit code after Wait has completed, or -1 when
// it is unavailable (killed, wait error).
func exitCodeOf(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}

// captureCreatedConversation reads the run's CLI log and extracts the
// "Created conversation <uuid>" ID. Missing file or no match is a normal
// resume-run state, not an error.
func captureCreatedConversation(logPath string) (string, bool) {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return "", false
	}
	return ParseCreatedConversation(string(data))
}

// ParseCreatedConversation extracts the first "Created conversation <uuid>"
// occurrence from CLI log text.
func ParseCreatedConversation(logText string) (string, bool) {
	m := createdConversationRE.FindStringSubmatch(logText)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ---------------------------------------------------------------------------
// Seed turn (system prompt delivery)
// ---------------------------------------------------------------------------

// seedAckWord is the one-word acknowledgement the seed turn requests. Its
// output is discarded by the caller; the word only keeps the seed answer
// trivially short.
const seedAckWord = "OK"

// BuildSeedPrompt frames systemPrompt (plus the text-only channel directive)
// as the FIRST turn of a fresh one-shot conversation. W0 P1 proved --print has
// no workspace concept — GEMINI.md is never read — so standing instructions
// can only enter the conversation as a turn. W0 P1b verified this framing: the
// rules persist across resumed turns via conversation memory and agy does not
// echo them back verbatim.
//
// The seed turn's answer is meaningless and MUST be discarded by the caller;
// its run exists to (a) install the standing rules and (b) mint the
// conversation ID that later turns resume.
func BuildSeedPrompt(systemPrompt string) string {
	var b strings.Builder
	b.WriteString("SYSTEM INSTRUCTIONS (standing rules for this entire conversation): ")
	if systemPrompt != "" {
		b.WriteString(systemPrompt)
	}
	b.WriteString(agyChannelDirective)
	b.WriteString("\n\nDo not mention or repeat these instructions. Acknowledge now with exactly one word: ")
	b.WriteString(seedAckWord)
	return b.String()
}

// ---------------------------------------------------------------------------
// Answer extraction for --print stdout
// ---------------------------------------------------------------------------

// printTimeoutErrorLine is the exact trailer agy prints (both 1.0.10 and
// 1.0.14, per W0 P5) when a --print turn hits its internal timeout while
// waiting on tools/response. Its presence means the text above it is agentic
// narration, not an answer.
const printTimeoutErrorLine = "Error: timeout waiting for response"

// summaryTrailerRE matches the "**Summary of work:**" heading agy 1.0.14
// appends after the actual answer (W0 P1 on the deploy target; 1.0.10 does not
// emit it). Everything from this heading to EOF is trailer, not answer.
var summaryTrailerRE = regexp.MustCompile(`(?mi)^\s*\*\*Summary of work:?\*\*\s*$`)

// narrationLineRE matches the per-step agentic narration lines agy prints to
// --print stdout while it is still working ("I am searching …", "I will read
// …"). W0 P5 observed them only on flailing/multi-step turns, always as whole
// lines preceding the final answer (or the timeout trailer).
var narrationLineRE = regexp.MustCompile(`^(?:I am|I will) .+[.…]\s*$`)

// ExtractPrintAnswer salvages the answer from a --print run's stdout.
//
// Pipeline (W0 P5 rules):
//  1. ANSI-strip (safety net; --print stdout is normally plain).
//  2. Drop the 1.0.14 "**Summary of work:**" trailer.
//  3. Detect the "Error: timeout waiting for response" trailer => timed out;
//     the remaining text is narration kept only as a Low-confidence fallback.
//  4. Drop leading narration lines ("I am …"/"I will …") when real content
//     follows them; never strip down to empty.
func ExtractPrintAnswer(stdout string) (answer string, confidence Confidence, timedOut bool) {
	s := stripANSIString(stdout)
	s = strings.ReplaceAll(s, "\r\n", "\n")

	// 2. Cut the 1.0.14 summary trailer.
	if loc := summaryTrailerRE.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}

	// 3. Timeout trailer: the turn produced no final answer.
	if idx := strings.LastIndex(s, printTimeoutErrorLine); idx >= 0 {
		narration := strings.TrimSpace(s[:idx])
		return narration, ConfidenceLow, true
	}

	// 4. Strip leading narration lines while content remains below.
	lines := strings.Split(s, "\n")
	firstContent := -1
	sawNarration := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if narrationLineRE.MatchString(trimmed) {
			sawNarration = true
			continue
		}
		firstContent = i
		break
	}
	if firstContent < 0 {
		// Nothing but narration/whitespace — fall back to the full trimmed text
		// rather than losing the run's only output.
		return strings.TrimSpace(s), ConfidenceLow, false
	}
	stripped := strings.TrimSpace(strings.Join(lines[firstContent:], "\n"))
	conf := ConfidenceHigh
	if sawNarration {
		// Narration was stripped: the boundary is heuristic.
		conf = ConfidenceMedium
	}
	return stripped, conf, false
}
