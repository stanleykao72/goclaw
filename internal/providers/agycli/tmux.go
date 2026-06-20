package agycli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrTmuxNotFound is returned by RunWithTmux (and the helpers) when the tmux
// binary is absent from PATH. Callers can use errors.Is to detect this and fall
// back to RunWithPTY.
var ErrTmuxNotFound = errors.New("agycli: tmux binary not found in PATH")

// lookupTmux resolves the tmux binary path. It is a package var so tests can
// override it to simulate tmux being absent without touching the real PATH.
var lookupTmux = func() (string, error) { return exec.LookPath("tmux") }

// tmuxPathCache memoizes the resolved tmux path (and any lookup error) so we do
// not hit exec.LookPath on every helper call. Reset implicitly when lookupTmux
// is swapped in a test by also clearing the cache (see resetTmuxPathCache).
var (
	tmuxPathOnce sync.Once
	tmuxPathVal  string
	tmuxPathErr  error
)

// tmuxPath returns the cached path to the tmux binary, or ErrTmuxNotFound if it
// cannot be found on PATH. The lookup happens at most once per process (or per
// resetTmuxPathCache call in tests).
func tmuxPath() (string, error) {
	tmuxPathOnce.Do(func() {
		p, err := lookupTmux()
		if err != nil {
			tmuxPathErr = ErrTmuxNotFound
			return
		}
		tmuxPathVal = p
	})
	return tmuxPathVal, tmuxPathErr
}

// resetTmuxPathCache clears the memoized tmux path so a subsequent tmuxPath call
// re-runs lookupTmux. Tests that swap lookupTmux must call this before and after
// to avoid leaking a cached value into other tests.
func resetTmuxPathCache() {
	tmuxPathOnce = sync.Once{}
	tmuxPathVal = ""
	tmuxPathErr = nil
}

// tmuxAvailable reports whether the tmux binary is on PATH. Use this to gate
// tmux-only behavior and to t.Skip tests on machines without tmux.
func tmuxAvailable() bool {
	_, err := tmuxPath()
	return err == nil
}

// tmuxCmd builds an *exec.Cmd for `tmux <args...>`, returning ErrTmuxNotFound if
// tmux is unavailable. All helpers funnel through this so the not-found path is
// handled in exactly one place.
func tmuxCmd(args ...string) (*exec.Cmd, error) {
	bin, err := tmuxPath()
	if err != nil {
		return nil, err
	}
	return exec.Command(bin, args...), nil
}

// newTmuxSession starts a detached tmux session named `name` with a pane of the
// given width/height, rooted at workdir. It also raises history-limit so long
// agy output is not truncated by the default scrollback. The session is created
// with the neutralizing env injected via tmux's per-session env (-e KEY=VAL,
// supported since tmux 3.2) so the spawned shell inherits NO_COLOR/CI etc.
//
// Note: TERM is intentionally NOT forced to dumb here — tmux is a real terminal
// emulator and needs a real $TERM (it sets one for its panes), which is exactly
// what lets capture-pane return a rendered screen instead of raw repaint bytes.
func newTmuxSession(name string, w, h int, workdir string) error {
	if w <= 0 {
		w = 200
	}
	if h <= 0 {
		h = 50
	}
	// Raise scrollback BEFORE the pane is born, atomically in the same server
	// invocation, so the captured pane actually gets the large history. A pane's
	// history-limit is fixed at creation time: `set-option -t <session>
	// history-limit` issued AFTER new-session does NOT enlarge the existing pane's
	// scrollback (it stays at the server default, ~2000 lines), silently
	// truncating long agy output before capture-pane -S - can read it. Chaining
	// `set-option -g history-limit N \; new-session ...` in one invocation makes
	// the new pane inherit N. Doing it in one invocation also means there is no
	// half-created session to leak if a later step fails.
	args := []string{
		"set-option", "-g", "history-limit", strconv.Itoa(tmuxHistoryLimit),
		";", "new-session", "-d", "-s", name, "-x", strconv.Itoa(w), "-y", strconv.Itoa(h),
	}
	if workdir != "" {
		args = append(args, "-c", workdir)
	}
	// Inject neutralizing env vars into the session (3.2+ -e). We pass only the
	// color/CI hints; TERM is left to tmux. Unknown KEY=VAL are harmless.
	for _, kv := range tmuxSessionEnv() {
		args = append(args, "-e", kv)
	}
	cmd, err := tmuxCmd(args...)
	if err != nil {
		return err
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session %q: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// tmuxHistoryLimit is the per-pane scrollback (in lines) requested for capture
// sessions. It must be set as the global option at-or-before new-session so the
// captured pane inherits it; see newTmuxSession.
const tmuxHistoryLimit = 100000

// tmuxSessionEnv returns the env hints injected into a new tmux session. We keep
// the color/CI neutralizers from the base env but deliberately drop TERM (tmux
// owns the pane's terminal type) and any PAGER override (tmux is fine with the
// default; forcing it is unnecessary here).
func tmuxSessionEnv() []string {
	var out []string
	for _, kv := range neutralizingBaseEnv {
		if strings.HasPrefix(kv, "TERM=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// tmuxSendKeys sends `keys` to the named session as LITERAL text, then (when
// enter is true) submits the line with a separate Enter key press.
//
// The `-l` (literal) flag is essential: without it, tmux send-keys interprets an
// argument that exactly matches a key-name token as that KEY rather than text. So
// a prompt whose entire content is e.g. "Enter", "Escape", "Space", "Tab", or
// "C-c" would be turned into the corresponding key — and "C-c" would SIGINT and
// kill the agy process driving the persistent session. `-l` forces the payload to
// be typed verbatim, closing that injection vector via prompt text. Multi-word
// prompts were already safe (no whole-token match), but single-token prompts were
// not; `-l` makes every prompt safe regardless of content.
//
// Enter must be sent in a SEPARATE invocation (not appended to the literal call)
// because under `-l` the token "Enter" would itself be typed as the five letters
// "Enter" instead of pressing the Return key. The literal `--` still guards keys
// beginning with a dash from being parsed as flags. An empty `keys` with
// enter=true sends only Enter (used to accept the trust gate / submit a blank).
func tmuxSendKeys(name, keys string, enter bool) error {
	if keys != "" {
		args := []string{"send-keys", "-t", name, "-l", "--", keys}
		cmd, err := tmuxCmd(args...)
		if err != nil {
			return err
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("tmux send-keys (literal) %q: %w: %s", name, err, strings.TrimSpace(string(out)))
		}
	}
	if enter {
		cmd, err := tmuxCmd("send-keys", "-t", name, "Enter")
		if err != nil {
			return err
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("tmux send-keys (enter) %q: %w: %s", name, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// tmuxCapturePane returns the rendered contents of the named session's pane.
// scrollbackLines controls how far back into history to capture: a value <= 0
// captures the full history (-S -), otherwise the last scrollbackLines rows
// (-S -<n>). When withEscapes is true (-e), ANSI/color escape sequences are
// preserved in the output; otherwise tmux emits plain text.
//
// -J is always passed so soft-wrapped lines (the shell echo of a long command,
// or a long answer line) are joined back into a single logical line. Without it
// tmux inserts a hard newline at the pane width, which can split our completion
// sentinel (e.g. "__AGY\n_DONE_42__") and defeat substring detection.
func tmuxCapturePane(name string, scrollbackLines int, withEscapes bool) (string, error) {
	args := []string{"capture-pane", "-t", name, "-p", "-J"}
	if withEscapes {
		args = append(args, "-e")
	}
	if scrollbackLines <= 0 {
		args = append(args, "-S", "-")
	} else {
		args = append(args, "-S", "-"+strconv.Itoa(scrollbackLines))
	}
	cmd, err := tmuxCmd(args...)
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane %q: %w", name, err)
	}
	return string(out), nil
}

// tmuxKillSession best-effort terminates the named session. Errors (e.g. the
// session already gone) are intentionally ignored so this is safe to defer on
// every exit path.
func tmuxKillSession(name string) {
	cmd, err := tmuxCmd("kill-session", "-t", name)
	if err != nil {
		return
	}
	_ = cmd.Run()
}

// tmuxHasSession reports whether a session with the given name currently exists.
// It returns false if tmux is unavailable or the session is absent.
func tmuxHasSession(name string) bool {
	cmd, err := tmuxCmd("has-session", "-t", name)
	if err != nil {
		return false
	}
	// has-session exits 0 when the session exists, non-zero otherwise.
	return cmd.Run() == nil
}

// uniqueSessionName builds a collision-resistant session name. It prefers crypto
// randomness ("agycli-<16 hex>") and falls back to pid+nanotime if the RNG fails
// (extremely unlikely), so two concurrent runs never share a session.
func uniqueSessionName() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "agycli-" + hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("agycli-%d-%d", os.Getpid(), time.Now().UnixNano())
}

// tmuxPollInterval is how often RunWithTmux re-captures the pane while waiting
// for the completion sentinel. Fast enough to feel responsive, slow enough to
// not hammer tmux.
const tmuxPollInterval = 500 * time.Millisecond

// RunWithTmux runs `binary` with opts.Args inside a throwaway tmux session and
// returns the RENDERED screen output. Because tmux is a real terminal emulator,
// capture-pane returns the post-render screen — sidestepping the cursor-up
// repaint and spinner residue that the raw-PTY parser in parse.go has to fight.
//
// Mechanics: it creates a unique detached session, then sends a single shell
// line of the form
//
//	cd <workdir> && <binary> <args...> ; printf '\n__AGY_DONE_%d__\n' $?
//
// and polls capture-pane (~every tmuxPollInterval) until the __AGY_DONE_<code>__
// sentinel appears, ctx is cancelled, or opts.Timeout (default DefaultRunTimeout)
// elapses. The session is ALWAYS killed on return (defer), including error and
// timeout paths. The returned RunResult.Output has the typed command line and
// the sentinel stripped and ANSI removed; RunResult.ExitCode is parsed from the
// sentinel; RunResult.TimedOut is set on deadline/cancel.
//
// Returns ErrTmuxNotFound if tmux is absent so the caller can fall back to
// RunWithPTY.
func RunWithTmux(ctx context.Context, binary string, opts RunOptions) (RunResult, error) {
	if !tmuxAvailable() {
		return RunResult{ExitCode: -1}, ErrTmuxNotFound
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultRunTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()

	name := uniqueSessionName()
	// Phase 2 P3: derive a per-run nonce from the session name's random hex so the
	// START/DONE sentinels cannot collide with anything agy itself prints. Reusing
	// the session name keeps it free (no extra RNG call) and traceable.
	sents := sentinelsWithNonce(nonceFromSessionName(name))
	// Default pane size mirrors the Phase 0 verified drive pattern (200x50).
	if err := newTmuxSession(name, 200, 50, opts.Workdir); err != nil {
		return RunResult{ExitCode: -1, Duration: time.Since(start)}, err
	}
	// ALWAYS tear the session down, on every path (success/error/timeout).
	defer tmuxKillSession(name)

	// Inject ExtraEnv as inline assignments on the command line. The session-level
	// -e in newTmuxSession covers the neutralizing base; per-run ExtraEnv is layered
	// here so it wins, mirroring buildEnv's precedence (base < ExtraEnv).
	cmdLine := buildTmuxCommandSent(binary, opts, sents)
	if err := tmuxSendKeys(name, cmdLine, true); err != nil {
		return RunResult{ExitCode: -1, Duration: time.Since(start)}, err
	}

	ticker := time.NewTicker(tmuxPollInterval)
	defer ticker.Stop()

	for {
		raw, capErr := tmuxCapturePane(name, 0, false)
		if capErr == nil {
			if code, ok := parseDoneSentinelSent(raw, sents); ok {
				cleaned := cleanTmuxOutputSent(raw, cmdLine, sents)
				return RunResult{
					Output:   []byte(cleaned),
					ExitCode: code,
					TimedOut: false,
					Duration: time.Since(start),
				}, nil
			}
		}

		select {
		case <-runCtx.Done():
			// Timeout or parent cancel: grab a final screen for diagnostics, then
			// the deferred kill-session tears everything down.
			final, _ := tmuxCapturePane(name, 0, false)
			cleaned := cleanTmuxOutputSent(final, cmdLine, sents)
			return RunResult{
				Output:   []byte(cleaned),
				ExitCode: -1,
				TimedOut: true,
				Duration: time.Since(start),
			}, nil
		case <-ticker.C:
			// Poll again.
		}
	}
}

// buildTmuxCommand assembles the single shell line sent into the tmux pane using
// the default (nonce-free) sentinels. Kept for back-compat with callers/tests
// that do not thread a per-run nonce; RunWithTmux uses buildTmuxCommandSent.
func buildTmuxCommand(binary string, opts RunOptions) string {
	return buildTmuxCommandSent(binary, opts, defaultSentinels())
}

// buildTmuxCommandSent assembles the single shell line sent into the tmux pane. It
// quotes the binary and every arg with single-quote escaping so prompts
// containing spaces/quotes/newlines survive intact, prefixes any VALIDATED
// ExtraEnv as `KEY=VALUE` assignments, brackets the command with the provided
// START/DONE sentinels, and records $? in the DONE sentinel so the exit code can
// be recovered from the rendered screen.
//
// Phase 2 P3 hardening:
//   - sents carries a per-run nonce so the sentinels cannot be forged by the
//     command's own output.
//   - each ExtraEnv KEY is validated against ^[A-Za-z_][A-Za-z0-9_]*$; entries
//     with an invalid key (or no '=') are SKIPPED, never emitted into the shell
//     line (a bad key could otherwise inject arbitrary shell tokens).
//
// Final shape (workdir omitted when empty since the session is already -c'd):
//
//	printf '\n__AGY_START_<nonce>__\n' ; [ENV...] '<binary>' '<args...>' ; printf '\n__AGY_DONE_<nonce>_%d__\n' "$?"
func buildTmuxCommandSent(binary string, opts RunOptions, sents sentinels) string {
	var sb strings.Builder

	// ExtraEnv assignments (KEY=VALUE). The value is single-quote escaped; the
	// KEY is validated so a malformed entry cannot inject shell tokens.
	for _, kv := range opts.ExtraEnv {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue // no '=' => not a KEY=VALUE assignment; skip.
		}
		key, val := kv[:i], kv[i+1:]
		if !validEnvKey(key) {
			continue // reject keys that aren't [A-Za-z_][A-Za-z0-9_]*.
		}
		sb.WriteString(key)
		sb.WriteByte('=')
		sb.WriteString(shellSingleQuote(val))
		sb.WriteByte(' ')
	}

	sb.WriteString(shellSingleQuote(binary))
	for _, a := range opts.Args {
		sb.WriteByte(' ')
		sb.WriteString(shellSingleQuote(a))
	}

	// Wrap the actual command between a START marker (printed BEFORE it runs) and
	// a DONE marker (printed AFTER, carrying $?). The START marker lets cleanup
	// slice out exactly the command's own output, discarding the user's shell
	// prompt and the echoed command line that precede it. `;` (not `&&`) chains so
	// both markers always print regardless of the command's success/failure.
	//
	// Shape: printf START ; <cmd> ; printf DONE_<$?>
	full := "printf '\\n" + sents.start + "\\n' ; " +
		sb.String() +
		" ; printf '\\n" + sents.donePrefix + "%d" + sents.doneSuffix + "\\n' \"$?\""
	return full
}

// envKeyRe matches a valid POSIX-ish env var name. Used to reject ExtraEnv keys
// that could otherwise inject shell tokens when written verbatim before the
// command (Phase 2 P3).
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validEnvKey(k string) bool { return envKeyRe.MatchString(k) }

// shellSingleQuote wraps s in single quotes for safe use in a POSIX shell,
// escaping any embedded single quote via the '\” idiom. This makes spaces,
// double quotes, dollar signs, backticks, and newlines all literal.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Sentinel pieces (default, nonce-free). The command prints startSentinel before
// the real command and doneSentinelPrefix + <code> + doneSentinelSuffix after it.
// The poll loop scans the rendered screen for the DONE marker to detect
// completion and parse the exit code; cleanup slices the answer text out between
// START and DONE. RunWithTmux uses a per-run nonce variant (see sentinels); these
// bare constants remain for back-compat callers/tests.
const (
	startSentinel      = "__AGY_START__"
	doneSentinelPrefix = "__AGY_DONE_"
	doneSentinelSuffix = "__"
)

// sentinels is a concrete set of START/DONE markers for one run. RunWithTmux mints
// a per-run instance via sentinelsWithNonce so the markers cannot collide with
// arbitrary agy output (Phase 2 P3).
type sentinels struct {
	start      string // e.g. "__AGY_START_<nonce>__"
	donePrefix string // e.g. "__AGY_DONE_<nonce>_"
	doneSuffix string // "__"
}

// defaultSentinels returns the nonce-free sentinels backed by the package
// constants. Used by buildTmuxCommand and the back-compat wrappers.
func defaultSentinels() sentinels {
	return sentinels{
		start:      startSentinel,
		donePrefix: doneSentinelPrefix,
		doneSuffix: doneSentinelSuffix,
	}
}

// sentinelsWithNonce embeds nonce into both markers so a run's sentinels are
// unique and unforgeable by the command's own output. An empty nonce degrades to
// defaultSentinels.
func sentinelsWithNonce(nonce string) sentinels {
	if nonce == "" {
		return defaultSentinels()
	}
	return sentinels{
		start:      "__AGY_START_" + nonce + "__",
		donePrefix: "__AGY_DONE_" + nonce + "_",
		doneSuffix: "__",
	}
}

// nonceFromSessionName extracts the random hex suffix from a uniqueSessionName
// ("agycli-<hex>") to reuse as the sentinel nonce. Falls back to the whole name
// (with non-alphanumerics dropped) if the prefix is absent.
func nonceFromSessionName(name string) string {
	const pfx = "agycli-"
	s := name
	if strings.HasPrefix(name, pfx) {
		s = name[len(pfx):]
	}
	// Keep only [A-Za-z0-9] so the nonce is safe inside a printf format/grep.
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// parseDoneSentinel is the back-compat wrapper over parseDoneSentinelSent using
// the default (nonce-free) sentinels.
func parseDoneSentinel(screen string) (int, bool) {
	return parseDoneSentinelSent(screen, defaultSentinels())
}

// parseDoneSentinelSent scans rendered pane text for the DONE marker
// (sents.donePrefix + <code> + sents.doneSuffix) and returns the parsed exit
// code. It only matches a sentinel that is the printf OUTPUT (a standalone
// token), not the echoed command line that contains the printf format string
// `%d` — so it requires the digits between prefix and suffix to parse as an
// integer. The last matching occurrence wins.
func parseDoneSentinelSent(screen string, sents sentinels) (int, bool) {
	code := 0
	found := false
	search := screen
	for {
		i := strings.Index(search, sents.donePrefix)
		if i < 0 {
			break
		}
		rest := search[i+len(sents.donePrefix):]
		j := strings.Index(rest, sents.doneSuffix)
		if j < 0 {
			break
		}
		digits := rest[:j]
		// Advance past this prefix for the next iteration regardless of match.
		search = rest[j+len(sents.doneSuffix):]
		if digits == "" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(digits))
		if err != nil {
			// e.g. the echoed command line's DONE_<nonce>_%d__ — skip it.
			continue
		}
		code = n
		found = true
	}
	return code, found
}

// cleanTmuxOutput turns a raw rendered pane capture into the answer text. It
// strips ANSI first (defense-in-depth; capture without -e is usually plain),
// then slices out exactly the command's own output as the text between the
// START sentinel and the DONE sentinel — discarding the user's shell prompt, the
// echoed command line, and the markers themselves.
//
// Robustness: the echoed command line ALSO contains the literal "__AGY_START__"
// and "__AGY_DONE_%d__" format strings (it is the command we typed). We slice
// from the LAST START marker that precedes the real DONE marker; the printf
// output START always comes after the single echoed command line, so the slice
// captures the real output, not the echo. If sentinels are absent (e.g. a
// timeout before START printed), it falls back to dropping the echoed command
// line and any sentinel-bearing lines.
func cleanTmuxOutput(raw, cmdLine string) string {
	return cleanTmuxOutputSent(raw, cmdLine, defaultSentinels())
}

// cleanTmuxOutputSent is the nonce-aware form of cleanTmuxOutput.
func cleanTmuxOutputSent(raw, cmdLine string, sents sentinels) string {
	text := string(StripANSI([]byte(raw)))

	if done := lastDoneSentinelIndexSent(text, sents); done >= 0 {
		if start := lastIndexBefore(text, sents.start, done); start >= 0 {
			inner := text[start+len(sents.start) : done]
			return strings.TrimSpace(inner)
		}
	}

	// Fallback: no usable sentinel pair. Filter scaffolding line-by-line.
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		trimmed := strings.TrimRight(ln, " \t")
		if cmdLine != "" && strings.Contains(trimmed, cmdLine) {
			continue
		}
		if strings.Contains(trimmed, sents.donePrefix) || strings.Contains(trimmed, sents.start) {
			continue
		}
		out = append(out, trimmed)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// lastDoneSentinelIndexSent returns the byte offset of the start of the LAST real
// DONE sentinel (one whose code parses as an integer) in text, or -1 if none.
// A real sentinel is the printf output; the echoed "...DONE_<nonce>_%d__" format
// string is rejected because "%d" is not an integer.
func lastDoneSentinelIndexSent(text string, sents sentinels) int {
	best := -1
	from := 0
	for {
		i := strings.Index(text[from:], sents.donePrefix)
		if i < 0 {
			break
		}
		abs := from + i
		rest := text[abs+len(sents.donePrefix):]
		j := strings.Index(rest, sents.doneSuffix)
		if j < 0 {
			break
		}
		digits := strings.TrimSpace(rest[:j])
		if digits != "" {
			if _, err := strconv.Atoi(digits); err == nil {
				best = abs
			}
		}
		from = abs + len(sents.donePrefix)
	}
	return best
}

// lastIndexBefore returns the byte offset of the last occurrence of sub in
// text that starts before limit, or -1 if none.
func lastIndexBefore(text, sub string, limit int) int {
	if limit < 0 || limit > len(text) {
		limit = len(text)
	}
	return strings.LastIndex(text[:limit], sub)
}
