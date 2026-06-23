package agycli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// session_interactive.go implements the PERSISTENT interactive agy session — the
// only path that gives true multi-turn memory.
//
// Phase 0 (agy v1.0.10) established that "agy --print" CANNOT resume: every call
// is a fresh conversation and --conversation/-c do not replay prior context. So
// the only way to keep memory across turns is to hold ONE live interactive agy
// process open and feed it multiple prompts. That is exactly what Session does:
// it launches agy (NO --print) inside a tmux session and drives it with
// send-keys / capture-pane.
//
// Turn detection (Phase 0 Q3, verified against real agy v1.0.10):
//   - READY / awaiting input  <=>  pane contains "? for shortcuts"
//     AND does NOT contain "esc to cancel".
//   - BUSY / streaming         <=>  pane contains "esc to cancel".
//   - The bare ">" input glyph appears in BOTH states — never use it.
//   - Require 2 consecutive idle reads (stability) to dodge a sub-second idle
//     blip that happens between tool steps.
//   - First-run TRUST GATE: a fresh/untrusted workspace shows
//     "Do you trust the contents of this project?" BEFORE the TUI. It has NEITHER
//     marker. Detect ("trust" / "Do you trust") and send Enter once to accept,
//     then await ready.

// Turn-detection anchors. These are substrings of the rendered pane footer.
const (
	// readyMarker is present in the footer ONLY when agy is idle / awaiting input.
	readyMarker = "? for shortcuts"
	// busyMarker is present in the footer ONLY while agy is streaming / running a
	// tool. It is mutually exclusive with the ready state per Phase 0 Q3.
	busyMarker = "esc to cancel"
)

// trustMarkers are substrings that identify the first-run "Do you trust the
// contents of this project?" gate. The gate carries neither readyMarker nor
// busyMarker, so without this detection it would look like a perpetual
// not-ready screen.
var trustMarkers = []string{"Do you trust", "trust the contents", "trust"}

// Session defaults.
const (
	defaultPaneWidth    = 200
	defaultPaneHeight   = 50
	defaultReadyTimeout = 60 * time.Second
	// sessionPollInterval is how often we re-capture the pane while waiting for a
	// turn to finish. ~600ms per Phase 3 guidance: responsive but not hammering.
	sessionPollInterval = 600 * time.Millisecond
)

// geminiInstructionsFile is the per-workspace context file agy (Gemini CLI
// lineage) auto-reads as system/context instructions. agy FOLLOWS its contents
// and does NOT echo them back, unlike text folded into the --print prompt. We
// write the system prompt here before launch so it acts as a true system prompt.
const geminiInstructionsFile = "GEMINI.md"

// agyChannelDirective is always appended to GEMINI.md. agy is agentic and will
// otherwise render INTERACTIVE choice menus (arrow-key selectors) that a
// text-only messaging channel (e.g. LINE WORKS) cannot operate — the turn then
// hangs waiting for a key press that never comes. This directive tells agy the
// channel is text-only so it answers in plain text instead. Verified on real agy
// v1.0.10: with this directive agy stopped emitting the menu and replied in text.
const agyChannelDirective = "\n\n# Channel constraints (text-only)\n" +
	"You are operating on a TEXT-ONLY messaging channel. The user can only read " +
	"and type text — they CANNOT use arrow keys, menus, or interactive selectors. " +
	"NEVER present interactive choice menus, numbered selectors, or multiple-choice " +
	"prompts. If you need clarification, ask in plain text in ONE short sentence, or " +
	"make a reasonable assumption and answer directly. ALWAYS produce a final text " +
	"answer in your reply.\n\n" +
	"# No narration\n" +
	"Do NOT narrate your plan or your tool usage. Do NOT write sentences like " +
	"\"I will use the X tool\", \"I will read the schema of …\", \"I will list the " +
	"schemas …\", or \"I will now …\". Use tools silently and reply ONLY with the " +
	"final answer to the user's request — nothing else."

// SessionOptions configures a persistent interactive agy session.
type SessionOptions struct {
	Workdir         string        // cwd for the agy process (agy needs an active workspace)
	Model           string        // optional; passed as --model at launch when set
	SystemPrompt    string        // written to <Workdir>/GEMINI.md before launch so agy reads it as system/context instructions; NOT sent as a turn
	SkipPermissions bool          // --dangerously-skip-permissions at launch
	Sandbox         bool          // --sandbox at launch
	PaneWidth       int           // tmux pane width; default defaultPaneWidth (200)
	PaneHeight      int           // tmux pane height; default defaultPaneHeight (50)
	ReadyTimeout    time.Duration // wait for the initial prompt-ready; default 60s
	TurnTimeout     time.Duration // per-turn wait; default DefaultRunTimeout (5m)
}

// TurnResult is the outcome of one prompt/slash turn.
type TurnResult struct {
	Answer     string     // extracted + cleaned answer text for THIS turn
	RawCapture string     // full rendered pane capture (debug aid)
	Confidence Confidence // high = cleanly bounded between echo and footer; medium otherwise
	TimedOut   bool       // true when the turn did not reach ready before TurnTimeout/ctx
}

// Session is a persistent interactive agy process driven over tmux. It is safe
// for sequential use; SendPrompt/SendSlash are serialized by an internal mutex so
// overlapping turns cannot interleave send-keys/capture.
type Session struct {
	name   string // tmux session name
	binary string // resolved agy binary path
	opts   SessionOptions

	mu     sync.Mutex
	closed bool
}

// NewSession launches a persistent interactive agy process inside a fresh tmux
// session, accepts the first-run trust gate if present, waits for the initial
// prompt-ready, and returns a ready-to-drive Session. On any failure the tmux
// session is torn down before returning the error.
func NewSession(ctx context.Context, binary string, opts SessionOptions) (*Session, error) {
	if binary == "" {
		return nil, errors.New("agycli: NewSession: empty binary path")
	}
	if !tmuxAvailable() {
		return nil, ErrTmuxNotFound
	}

	opts = applySessionDefaults(opts)

	// Sync <Workdir>/GEMINI.md BEFORE launch so agy reads the system prompt as
	// system/context instructions (it follows them and does NOT echo them, the
	// way folding the system prompt into the --print/turn prompt would). The
	// workdir is stable per session_key and reused across restarts, so we must
	// ALSO clear a stale GEMINI.md when this session has no system prompt —
	// otherwise a previous run's instructions would silently survive. A failure
	// here is logged but non-fatal: the session can still run without it.
	if opts.Workdir != "" {
		syncGeminiInstructions(opts.Workdir, opts.SystemPrompt)
	}

	name := uniqueSessionName()
	if err := newTmuxSession(name, opts.PaneWidth, opts.PaneHeight, opts.Workdir); err != nil {
		return nil, err
	}

	s := &Session{name: name, binary: binary, opts: opts}

	// Launch interactive agy (NO --print) in the pane. agy with no prompt opens
	// the TUI directly.
	cmdLine := buildInteractiveLaunch(binary, opts)
	if err := tmuxSendKeys(name, cmdLine, true); err != nil {
		s.Close()
		return nil, fmt.Errorf("agycli: launch agy: %w", err)
	}

	// Handle the trust gate (if any) and then await the initial ready footer.
	if err := s.awaitReady(ctx, opts.ReadyTimeout); err != nil {
		// Capture a final screen into the error for diagnosis before teardown.
		final, _ := tmuxCapturePane(name, 0, false)
		s.Close()
		return nil, fmt.Errorf("agycli: agy never became ready: %w\n--- last pane ---\n%s", err, final)
	}

	return s, nil
}

// applySessionDefaults fills zero-valued options with their documented defaults.
func applySessionDefaults(o SessionOptions) SessionOptions {
	if o.PaneWidth <= 0 {
		o.PaneWidth = defaultPaneWidth
	}
	if o.PaneHeight <= 0 {
		o.PaneHeight = defaultPaneHeight
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = defaultReadyTimeout
	}
	if o.TurnTimeout <= 0 {
		o.TurnTimeout = DefaultRunTimeout
	}
	return o
}

// writeGeminiInstructions writes the system prompt to <workdir>/GEMINI.md so agy
// reads it as system/context instructions on launch. The workdir is created if
// needed (0700) and the file is written 0644 — it is agent instructions that agy
// must be able to read. Any failure is logged as a warning and swallowed: a
// missing context file degrades the session but must not abort it.
func syncGeminiInstructions(workdir, systemPrompt string) {
	// GEMINI.md ALWAYS carries at least the channel directive (text-only / no
	// interactive menus). With a system prompt we write systemPrompt + directive;
	// without one we write the directive alone (leading newlines stripped). Since
	// we always overwrite, a stale GEMINI.md from a previous run on this (stable,
	// reused) workdir is replaced — no separate removal needed.
	content := systemPrompt + agyChannelDirective
	if systemPrompt == "" {
		content = strings.TrimLeft(agyChannelDirective, "\n")
	}
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		slog.Warn("agycli: failed to create workdir for GEMINI.md", "dir", workdir, "error", err)
		return
	}
	path := filepath.Join(workdir, geminiInstructionsFile)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		slog.Warn("agycli: failed to write GEMINI.md", "path", path, "error", err)
	}
}

// buildInteractiveLaunch assembles the shell line that starts interactive agy in
// the pane. It single-quotes the binary and each flag value so a model name with
// odd characters survives. No --print is ever passed (that would defeat the
// persistent session). Launch flags: --model, --dangerously-skip-permissions,
// --sandbox per opts.
func buildInteractiveLaunch(binary string, opts SessionOptions) string {
	var sb strings.Builder
	sb.WriteString(shellSingleQuote(binary))
	if opts.SkipPermissions {
		sb.WriteString(" --dangerously-skip-permissions")
	}
	if opts.Sandbox {
		sb.WriteString(" --sandbox")
	}
	if opts.Model != "" {
		sb.WriteString(" --model ")
		sb.WriteString(shellSingleQuote(opts.Model))
	}
	return sb.String()
}

// SendPrompt types text into the live agy process, waits for the turn to
// complete, and returns this turn's extracted answer.
func (s *Session) SendPrompt(ctx context.Context, text string) (TurnResult, error) {
	return s.sendAndAwait(ctx, text)
}

// SendSlash sends a slash command (e.g. "/clear", "/model gemini-3-pro"). Some
// slash commands don't produce a normal answer block; SendSlash returns whatever
// the pane shows and does not hang waiting for an answer (it still waits for the
// footer to return to ready, bounded by TurnTimeout).
func (s *Session) SendSlash(ctx context.Context, cmd string) (TurnResult, error) {
	return s.sendAndAwait(ctx, cmd)
}

// sendAndAwait is the shared core of SendPrompt/SendSlash: snapshot, type+enter,
// await turn, extract.
func (s *Session) sendAndAwait(ctx context.Context, text string) (TurnResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return TurnResult{}, errors.New("agycli: session is closed")
	}

	// Snapshot before sending. Currently passed to extractTurnAnswer for future
	// scrollback-diff disambiguation; extraction today relies on the last-echo +
	// footer bounding, so this is a debug/forward-compat hook (see extractTurnAnswer).
	before, _ := tmuxCapturePane(s.name, 0, false)

	if err := tmuxSendKeys(s.name, text, true); err != nil {
		return TurnResult{}, fmt.Errorf("agycli: send prompt: %w", err)
	}

	timedOut := s.awaitTurn(ctx, s.opts.TurnTimeout) != nil

	raw, capErr := tmuxCapturePane(s.name, 0, false)
	if capErr != nil {
		return TurnResult{TimedOut: timedOut}, fmt.Errorf("agycli: capture after turn: %w", capErr)
	}

	answer, conf := extractTurnAnswer(raw, text, before)
	return TurnResult{
		Answer:     answer,
		RawCapture: raw,
		Confidence: conf,
		TimedOut:   timedOut,
	}, nil
}

// Capture returns the full rendered pane (full scrollback, no escapes).
func (s *Session) Capture() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("agycli: session is closed")
	}
	return tmuxCapturePane(s.name, 0, false)
}

// Close terminates the tmux session (which reaps the agy process running as the
// pane's shell child). It is idempotent and safe to call twice.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	tmuxKillSession(s.name)
	return nil
}

// Name returns the tmux session name (useful for tests asserting teardown).
func (s *Session) Name() string { return s.name }

// awaitReady polls the pane until it reaches the ready state, auto-accepting the
// trust gate once if it appears. Bounded by ctx and timeout. It enforces the
// 2-consecutive-idle stability rule.
//
// requireLeaveReady is false here: at boot the pane goes unknown/booting -> ready,
// it is never already-ready before we start, so there is no pre-turn ready frame
// to guard against.
func (s *Session) awaitReady(ctx context.Context, timeout time.Duration) error {
	return s.pollUntilReady(ctx, timeout, true, false)
}

// awaitTurn polls the pane until the footer returns to the ready state after a
// prompt was sent. It still guards against (and auto-accepts) a trust gate in
// case one surfaces mid-session, and enforces the 2-consecutive-idle rule.
// Returns a non-nil error on timeout/cancel (the caller maps it to TimedOut).
//
// requireLeaveReady is TRUE here: immediately after send-keys the pane is still
// showing the PREVIOUS turn's ready footer (agy has not yet rendered
// "esc to cancel"). Without a guard, two consecutive captures of that stale ready
// footer would be misread as "this turn already finished" — a false positive that
// silently extracts the previous turn's answer and breaks multi-turn memory. The
// guard makes the loop first observe a non-ready (busy / re-rendering) frame
// before any ready streak can count.
func (s *Session) awaitTurn(ctx context.Context, timeout time.Duration) error {
	return s.pollUntilReady(ctx, timeout, true, true)
}

// pollUntilReady is the shared polling loop. It waits for two consecutive idle
// (ready) captures. While waiting it auto-accepts the trust gate at most once
// (when acceptTrust is true). A neither-marker screen that is NOT a trust gate is
// treated as still-initializing and keeps the loop going until the deadline.
//
// When requireLeaveReady is true the loop will NOT accept any ready streak until
// it has first seen at least one non-ready (busy or unknown) frame — this defeats
// the post-send race where the pre-turn ready footer is still on screen.
func (s *Session) pollUntilReady(ctx context.Context, timeout time.Duration, acceptTrust, requireLeaveReady bool) error {
	deadline := time.Now().Add(timeout)
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	trustAccepted := false
	idleStreak := 0
	// leftReady tracks whether we have observed a non-ready frame yet. When
	// requireLeaveReady is false we treat the turn as already-left (true) so the
	// boot path is unaffected.
	leftReady := !requireLeaveReady
	var lastCapture string

	ticker := time.NewTicker(sessionPollInterval)
	defer ticker.Stop()

	check := func() (done bool) {
		raw, err := tmuxCapturePane(s.name, 0, false)
		if err != nil {
			idleStreak = 0
			return false
		}
		clean := string(StripANSI([]byte(raw)))

		// Trust gate: accept once, then keep polling for the TUI. A trust gate is
		// itself a non-ready frame, so seeing it satisfies the leave-ready guard.
		if acceptTrust && !trustAccepted && isTrustGate(clean) {
			_ = tmuxSendKeys(s.name, "", true) // Enter to accept (default Yes)
			trustAccepted = true
			idleStreak = 0
			leftReady = true
			return false
		}

		switch classifyState(clean) {
		case stateReady, stateAwaitingInput:
			// stateAwaitingInput (interactive menu) is terminal too: agy is blocked
			// waiting for a key the text channel can't send, so the turn is "done"
			// — extract the visible question/options as the answer rather than hang.
			// Post-send guard: ignore ready frames until the turn has actually
			// started (a non-ready frame seen). This is the stale pre-turn footer.
			if !leftReady {
				idleStreak = 0
				lastCapture = clean
				return false
			}
			// Stability: require 2 consecutive idle reads. To dodge a transient
			// blip we also accept byte-stability across the two reads.
			if idleStreak == 0 {
				idleStreak = 1
				lastCapture = clean
				return false
			}
			// Second consecutive ready read => stable.
			_ = lastCapture
			idleStreak = 2
			return true
		default: // stateBusy or stateUnknown
			idleStreak = 0
			leftReady = true
			lastCapture = clean
			return false
		}
	}

	// Immediate first check (don't wait a full tick before the first capture).
	if check() {
		return nil
	}
	for {
		select {
		case <-pollCtx.Done():
			return pollCtx.Err()
		case <-ticker.C:
			if check() {
				return nil
			}
		}
	}
}

// paneState classifies the rendered footer into the three Phase 0 Q3 states.
type paneState int

const (
	stateUnknown       paneState = iota // neither marker (trust gate / still booting)
	stateBusy                           // "esc to cancel" present
	stateReady                          // "? for shortcuts" present AND no busy marker
	stateAwaitingInput                  // interactive choice-menu shown; agy waits for a key the text channel can't send — treat as terminal
)

// awaitSelectHints are the selection/skip cues on the interactive menu footer.
// They are only honored on a line that ALSO carries the up-arrow glyph, so prose
// mentioning these words cannot misfire the menu detection.
var awaitSelectHints = []string{"Select", "Confirm", "Skip"}

// classifyState applies the Phase 0 Q3 rule to already-ANSI-stripped pane text.
// busy wins over ready when both substrings somehow co-occur in a transient
// frame (treat as still-busy to avoid a premature ready).
func classifyState(clean string) paneState {
	// Awaiting-input (interactive menu) is checked FIRST: the menu footer also
	// shows the busy marker ("esc to cancel"), so without this the menu would be
	// misread as busy and the turn would hang forever (the user can't press keys).
	if isAwaitingInput(clean) {
		return stateAwaitingInput
	}
	hasBusy := strings.Contains(clean, busyMarker)
	hasReady := strings.Contains(clean, readyMarker)
	switch {
	case hasBusy:
		return stateBusy
	case hasReady:
		return stateReady
	default:
		return stateUnknown
	}
}

// isAwaitingInput reports whether the pane shows agy's interactive choice menu
// (agy is blocked waiting for an arrow-key selection). The menu footer renders
// the up-arrow glyph and a select/skip cue on ONE line (after tmux -J rejoin),
// e.g. "↑/↓ Navigate · enter Select · esc Skip". Require both ON THE SAME LINE,
// anchored by the "↑" glyph — prose mentioning "Select"/"Navigate" across the
// pane (or a streaming fragment) cannot trigger it.
func isAwaitingInput(clean string) bool {
	for _, line := range strings.Split(clean, "\n") {
		if strings.Contains(line, "↑") && containsAny(line, awaitSelectHints) {
			return true
		}
	}
	return false
}

// containsAny reports whether s contains any of the given substrings.
func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// isTrustGate reports whether the (ANSI-stripped) pane shows the first-run trust
// prompt. It requires a trust marker AND the absence of both turn markers, so a
// normal answer that merely mentions the word "trust" is not misdetected.
func isTrustGate(clean string) bool {
	if strings.Contains(clean, busyMarker) || strings.Contains(clean, readyMarker) {
		return false
	}
	lower := strings.ToLower(clean)
	for _, m := range trustMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// extractTurnAnswer pulls THIS turn's answer out of a full-scrollback capture.
//
// Strategy:
//  1. ANSI-strip the whole capture.
//  2. Find the LAST line that looks like the echoed prompt input (begins with the
//     ">" input glyph and contains the prompt text — matched robustly because the
//     text may be soft-wrapped, which tmux -J already rejoins). For slash
//     commands the echoed line is the command itself.
//  3. Take everything AFTER that echoed line, up to (but excluding) the footer /
//     ready line (the line containing readyMarker or the model label).
//  4. Clean: drop input-box border lines, drop the footer, drop the bare ">"
//     prompt line, trim — while PRESERVING internal blank lines so multi-line
//     answers survive.
//
// Confidence is high when the answer is cleanly bounded by both an echo line and
// a footer line; medium otherwise (e.g. echo not found, footer absent).
func extractTurnAnswer(raw, promptText, before string) (string, Confidence) {
	text := string(StripANSI([]byte(raw)))
	lines := strings.Split(text, "\n")

	// Locate the echoed prompt line (last match), and the footer line after it.
	echoIdx := lastEchoIndex(lines, promptText)
	footerIdx := footerIndexAfter(lines, echoIdx)

	conf := ConfidenceMedium
	lo := 0
	hi := len(lines)
	if echoIdx >= 0 {
		lo = echoIdx + 1
	}
	if footerIdx >= 0 {
		hi = footerIdx
	}
	if echoIdx >= 0 && footerIdx >= 0 {
		conf = ConfidenceHigh
	}
	if lo > hi {
		lo, hi = 0, len(lines)
		conf = ConfidenceMedium
	}

	body := lines[lo:hi]
	cleaned := cleanAnswerLines(body)
	if strings.TrimSpace(cleaned) == "" {
		// Nothing between echo and footer (e.g. a slash command that produced no
		// answer block). Fall back to a low-confidence scan of the whole capture
		// minus chrome so callers still see something.
		alt := cleanAnswerLines(lines)
		return strings.TrimSpace(alt), ConfidenceLow
	}
	return cleaned, conf
}

// lastEchoIndex returns the index of the LAST line that is the echoed prompt.
// agy renders the submitted prompt in an input box prefixed by ">". We match a
// line that, after trimming the leading ">" glyph and box padding, contains a
// meaningful chunk of promptText. Because tmux -J rejoins soft-wraps, the whole
// prompt is usually on one logical line. If the prompt is long we fall back to
// matching its first non-trivial token sequence.
func lastEchoIndex(lines []string, promptText string) int {
	needle := strings.TrimSpace(promptText)
	// Use a prefix of the prompt as a robust anchor (first ~40 runes) so wrapping
	// / truncation in the box doesn't defeat the match.
	anchor := needle
	if len([]rune(anchor)) > 40 {
		anchor = string([]rune(anchor)[:40])
	}
	for i := len(lines) - 1; i >= 0; i-- {
		ln := stripPromptGlyph(lines[i])
		if needle != "" && strings.Contains(ln, needle) {
			return i
		}
		if anchor != "" && anchor != needle && strings.Contains(ln, anchor) {
			return i
		}
	}
	return -1
}

// footerIndexAfter returns the index of the first footer/ready line at or after
// `after`+1, or -1 if none. A footer line is one containing the ready marker or
// the model label hint "(Medium)" / "Flash" / "Pro" style — but we anchor on the
// readyMarker primarily since it's the stable Phase 0 Q3 signal.
func footerIndexAfter(lines []string, after int) int {
	start := 0
	if after >= 0 {
		start = after + 1
	}
	for i := start; i < len(lines); i++ {
		if strings.Contains(lines[i], readyMarker) || strings.Contains(lines[i], busyMarker) {
			return i
		}
	}
	return -1
}

// stripPromptGlyph removes a leading ">" input glyph (and any box-drawing /
// padding around it) from a rendered line so the prompt text can be matched.
func stripPromptGlyph(line string) string {
	s := line
	// Drop common left box-drawing characters and the ">" glyph + spaces.
	s = strings.TrimLeft(s, "│|> \t")
	return s
}

// cleanAnswerLines turns a slice of rendered answer lines into clean text. It
// drops input-box borders, footer/marker lines, and a bare ">" prompt line, but
// PRESERVES internal blank lines so multi-line / multi-paragraph answers survive.
// Leading/trailing blank lines are trimmed.
func cleanAnswerLines(lines []string) string {
	out := make([]string, 0, len(lines))
	dropThoughtTitle := false
	for _, ln := range lines {
		trimmed := strings.TrimRight(ln, " \t")
		// A "Thought for ..." summary is followed by an indented thought-title
		// line that carries no glyph (e.g. "Determining the Query's Intent").
		// Drop that single following non-blank line so the title doesn't leak.
		if dropThoughtTitle {
			if strings.TrimSpace(trimmed) == "" {
				continue // skip blanks between the summary and its title
			}
			dropThoughtTitle = false
			continue // drop the title line itself
		}
		if strings.HasPrefix(strings.TrimSpace(trimmed), "Thought for ") {
			dropThoughtTitle = true
			continue
		}
		if isChromeLine(trimmed) {
			continue
		}
		out = append(out, trimmed)
	}
	// Trim leading/trailing blank lines without collapsing internal ones.
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	// Remove the uniform left gutter agy renders in front of answer lines (a 2+
	// space indent) while PRESERVING relative indentation (e.g. nested list items
	// or code blocks). Done by stripping the longest common leading-whitespace
	// prefix shared by every non-blank line.
	out = dedent(out)
	return strings.Join(out, "\n")
}

// dedent strips the longest leading run of space/tab characters common to every
// non-blank line. Blank lines are ignored when computing the prefix and are left
// empty. Relative indentation is preserved.
func dedent(lines []string) []string {
	common := -1 // -1 = unset
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		n := 0
		for n < len(ln) && (ln[n] == ' ' || ln[n] == '\t') {
			n++
		}
		if common == -1 || n < common {
			common = n
		}
		if common == 0 {
			break
		}
	}
	if common <= 0 {
		return lines
	}
	out := make([]string, len(lines))
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			out[i] = ""
			continue
		}
		out[i] = ln[common:]
	}
	return out
}

// isChromeLine reports whether a line is TUI chrome (input-box border, footer,
// bare prompt glyph) rather than answer content. Blank lines are NOT chrome (we
// keep them to preserve answer structure).
func isChromeLine(trimmed string) bool {
	t := strings.TrimSpace(trimmed)
	if t == "" {
		return false // keep blanks for multi-line answers
	}
	// Footer / turn markers.
	if strings.Contains(t, readyMarker) || strings.Contains(t, busyMarker) {
		return true
	}
	// agy agentic-progress chrome rendered in the live TUI (not part of the
	// answer): thought lines ("▸ Thought for 1s, 380 tokens"), tool-call status
	// lines ("● WebSearch(...)", "⏺ ...", "✦ ..."), and the "(ctrl+o to expand)"
	// hint. These start with a distinct status glyph; matching the leading glyph
	// (after trimming) avoids touching real answer prose, which never starts with
	// these symbols.
	if isAgenticProgressLine(t) {
		return true
	}
	// Box-drawing only line (input box top/bottom borders).
	if isBoxDrawingOnly(t) {
		return true
	}
	// A bare prompt line: just the ">" glyph (optionally inside a box).
	stripped := strings.TrimSpace(stripPromptGlyph(t))
	if stripped == "" {
		return true
	}
	return false
}

// agenticGlyphs are the leading symbols agy uses for live thought/tool-status
// lines in the interactive TUI. Answer prose never begins with these.
var agenticGlyphs = []string{"▸", "▾", "▿", "●", "⏺", "✦", "◆", "·"}

// isAgenticProgressLine reports whether t is an agy thought/tool-status chrome
// line that should not appear in the extracted answer.
func isAgenticProgressLine(t string) bool {
	// The "(ctrl+o to expand)" / "(ctrl+r to expand)" hint that agy appends to
	// collapsed tool-call lines.
	if strings.Contains(t, "ctrl+o to expand") || strings.Contains(t, "ctrl+r to expand") {
		return true
	}
	for _, g := range agenticGlyphs {
		if strings.HasPrefix(t, g) {
			return true
		}
	}
	// "Thought for <n>s" summary line even if the glyph was stripped/wrapped.
	if strings.HasPrefix(t, "Thought for ") {
		return true
	}
	return false
}

// isBoxDrawingOnly reports whether t consists solely of box-drawing characters,
// dashes, and spaces — i.e. an input-box border row with no text.
func isBoxDrawingOnly(t string) bool {
	for _, r := range t {
		switch {
		case r == ' ' || r == '\t':
		case r == '-' || r == '_' || r == '=':
		case r >= 0x2500 && r <= 0x257F: // Unicode box drawing block
		case r == '+' || r == '|' || r == '>':
		default:
			return false
		}
	}
	return len(strings.TrimSpace(t)) > 0
}
