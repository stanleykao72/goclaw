package agycli

import "time"

// Confidence signals how trustworthy the extracted answer is.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"   // sentinel-delimited answer matched
	ConfidenceMedium Confidence = "medium" // clean last-block heuristic
	ConfidenceLow    Confidence = "low"    // fallback: returned ANSI-stripped full text
)

// RunOptions configures a single PTY-driven agy invocation.
type RunOptions struct {
	Workdir string // cwd for the process (agy needs an active workspace)
	// Args is the full argv after the binary. WARNING: agy's -p/--print is a
	// string-valued flag (Go flag pkg) — it consumes the NEXT token as its value,
	// so the prompt MUST be -p's value. Correct:
	// ["-p", prompt, "--dangerously-skip-permissions", "--model", "gemini-3-pro"]
	// or ["--print=" + prompt, "--dangerously-skip-permissions", ...].
	// WRONG: ["-p","--dangerously-skip-permissions", prompt] sets
	// --print="--dangerously-skip-permissions" and drops the real prompt.
	Args     []string
	Timeout  time.Duration // hard wall-clock cap; 0 => DefaultRunTimeout
	ExtraEnv []string      // KEY=VALUE pairs merged over the neutralized base env
}

// RunResult is the raw outcome of a PTY run (no parsing applied).
type RunResult struct {
	Output   []byte // raw bytes read from the pty master (still contains ANSI/control)
	ExitCode int    // process exit code; -1 if unknown/killed
	TimedOut bool   // true if killed due to Timeout/ctx
	Duration time.Duration
}

// ParseResult is the salvaged final answer from raw agy PTY output.
type ParseResult struct {
	Content    string
	Confidence Confidence
	Truncated  bool // true when output appears cut off (timeout/EOF mid-answer)
}

// DefaultRunTimeout mirrors agy --print-timeout default (5m). agy is agentic and slow.
const DefaultRunTimeout = 5 * time.Minute
