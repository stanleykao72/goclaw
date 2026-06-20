package agycli

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Sentinel markers the caller may wrap the prompt with so that agy brackets its
// final answer. When present, extraction is deterministic (ConfidenceHigh) and no
// heuristics are needed. See contracts §Part2 Stage 4.1.
const (
	sentinelBegin = "<<<AGY_ANSWER_BEGIN>>>"
	sentinelEnd   = "<<<AGY_ANSWER_END>>>"
)

// ---------------------------------------------------------------------------
// Stage 3 line-level denylist — VERSION-PINNED.
//
// These patterns describe whole-line agy TUI chrome (spinners, panel borders,
// tool-status rows, progress meters) that must be dropped during salvage. agy's
// human TUI strings drift across releases, so this block is maintained the same
// way internal/providers/claude_cli_deny_patterns.go keeps ShellDenyPatterns:
// one pinned constant region with a version comment, exercised by golden tests
// in parse_test.go so regressions surface when agy is upgraded.
//
// Tested against agy v1.0.9.
//
// CONSERVATISM CONTRACT (contracts §Part2 Stage 3): a line is only ever dropped
// when it matches one of these patterns AND carries little-to-no alphabetic
// answer content (see isNoiseLine). A line is never dropped merely for being
// short, and no line is dropped while inside a fenced code block.
// ---------------------------------------------------------------------------

// agyUnconditionalNoise matches whole lines (already ANSI-stripped and trimmed)
// that are unambiguously agy TUI chrome and carry no answer content. These are
// dropped regardless of their alphabetic ratio because the entire line is the
// noise marker — there is no prose riding along.
var agyUnconditionalNoise = []*regexp.Regexp{
	// Pure spinner-glyph lines: Braille frames or ASCII |/-\ spinner, optionally
	// trailed by a short status word (e.g. "⠋ Thinking", "/ Working", "- Loading…").
	regexp.MustCompile(`^[\x{2800}-\x{28FF}|/\-\\]+(\s+\p{L}{1,15}(…|\.\.\.)?)?$`),

	// Bare status words agy prints while a tool runs (whole line is the status).
	regexp.MustCompile(`^(Running|Executing|Calling|Thinking|Working|Loading|Waiting)(…|\.\.\.)?$`),

	// Labelled tool announcements: "Tool: ...", "Tool call: ...", "[tool] ...".
	regexp.MustCompile(`^(Tool|Tool call|\[tool\]):`),

	// Elapsed-time-only rows, e.g. "(1.2s)" or "(340ms)".
	regexp.MustCompile(`^\(\d+(\.\d+)?\s*(ms|s|m)\)$`),

	// Progress meters with an explicit bar: "[===> ]", "[ ===> .... ] 60%".
	// NOTE: a bare percentage line ("80%") is deliberately NOT listed here — it
	// can be a real terse answer. Bare percent is treated as noise only inside an
	// already-noisy region (see barePercentRE / lastNonNoiseBlock).
	regexp.MustCompile(`^\[\s*=*>*\.*\s*\]\s*\d*%?$`),
}

// barePercentRE matches a standalone percentage line ("42%", "80%"). Such a line
// is a progress meter ONLY when it sits next to OTHER progress meters; on its own
// it is a plausible answer ("80%") and is preserved. lastNonNoiseBlock applies
// this contextually (neighbor check) rather than unconditionally.
var barePercentRE = regexp.MustCompile(`^\d+%$`)

// progressBarRE matches an explicit progress-bar meter ("[===>   ] 60%"), the
// neighbor signal that turns an adjacent bare percent into noise.
var progressBarRE = regexp.MustCompile(`^\[\s*=*>*\.*\s*\]\s*\d*%?$`)

// isProgressMeter reports whether a trimmed line is a progress-bar meter or a bare
// percentage — the two shapes that cluster in a real progress region.
func isProgressMeter(trimmed string) bool {
	return progressBarRE.MatchString(trimmed) || barePercentRE.MatchString(trimmed)
}

// barePercentIsNoise decides, contextually, whether the bare-percent line at
// lines[idx] is a progress meter (noise) rather than a terse answer. It is noise
// only when an adjacent non-blank line is itself a progress meter — i.e. it sits
// inside a run of progress chrome. A lone "80%" (no progress-meter neighbor) is a
// real answer and kept.
func barePercentIsNoise(lines []string, idx int) bool {
	if !barePercentRE.MatchString(strings.TrimSpace(lines[idx])) {
		return false
	}
	// Look at the nearest non-blank neighbor on each side.
	for j := idx - 1; j >= 0; j-- {
		t := strings.TrimSpace(lines[j])
		if t == "" {
			continue
		}
		if isProgressMeter(t) {
			return true
		}
		break
	}
	for j := idx + 1; j < len(lines); j++ {
		t := strings.TrimSpace(lines[j])
		if t == "" {
			continue
		}
		if isProgressMeter(t) {
			return true
		}
		break
	}
	return false
}

// agyToolStatusPrefix matches lines that BEGIN with an agy tool-status glyph.
// Such a row is chrome ("✓ read_file (0.3s)", "⏺ tool done") but the same glyph
// could legitimately open an answer bullet that is a full sentence. To honor the
// conservatism contract (never drop real prose), these are dropped only when the
// line is short — see isNoiseLine's word-count guard.
var agyToolStatusPrefix = regexp.MustCompile(`^[✓✗⏺●⎿▶▸»]\s`)

// toolStatusMaxWords is the word ceiling below which a glyph-prefixed line is
// treated as a tool-status row rather than an answer bullet. Tool rows are terse
// ("✓ read_file (0.3s)" = 3 words); a real bulleted answer sentence is longer.
const toolStatusMaxWords = 6

// boxDrawingDominant reports whether a trimmed line is composed mostly of
// box-drawing / panel glyphs and common bullets with no real answer prose. It is
// kept as a routine (not a regexp) so the "dominant" ratio is explicit and easy
// to tune. Box-drawing lives in the Unicode block U+2500..U+257F; we also treat a
// few common bullet/ellipsis glyphs and pipe/space as structural.
func boxDrawingDominant(line string) bool {
	var structural, alpha, total int
	for _, r := range line {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		switch {
		case r >= 0x2500 && r <= 0x257F: // box drawing
			structural++
		case r == '•' || r == '·' || r == '…' || r == '|' || r == '─' || r == '═':
			structural++
		case unicode.IsLetter(r):
			alpha++
		}
	}
	if total == 0 {
		return false
	}
	// Dominant only when borders overwhelm the line and there is essentially no
	// alphabetic content riding along (e.g. a titled panel border "├─ result ─┤"
	// keeps its word and is preserved).
	return structural*4 >= total*3 && alpha == 0
}

// isBoxDrawing reports whether r is in the Unicode Box Drawing block.
func isBoxDrawing(r rune) bool { return r >= 0x2500 && r <= 0x257F }

// looksLikeTableRow reports whether a trimmed line is plausibly a row of an
// unfenced table. It accepts three shapes:
//
//   - a Markdown pipe row ("| a | 1 |", "|---|---|") — >=2 ASCII/box verticals;
//   - a Unicode box-drawing cell row ("│ a │ 1 │") — >=2 box verticals;
//   - a Unicode box-drawing rule/border row ("┌──┬──┐", "├──┼──┤", "└──┴──┘") —
//     a line composed almost entirely of box-drawing glyphs with no prose.
//
// A single such line is ambiguous (a stray border), so callers only honor table
// rows in contiguous runs of >=2 (see markTableRuns) — that run is what
// distinguishes a real table from chrome.
func looksLikeTableRow(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	// Count vertical cell separators (ASCII pipe + box-drawing verticals), the
	// alphanumeric cell content, box-drawing glyphs, and total non-space runes.
	var seps, alnum, box, total int
	for _, r := range t {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		switch {
		case r == '|' || r == '│' || r == '┃' || r == '║':
			seps++
			if isBoxDrawing(r) {
				box++
			}
		case isBoxDrawing(r):
			box++
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			alnum++
		}
	}
	// Cell row: at least two vertical separators framing cells.
	if seps >= 2 {
		return true
	}
	// Border / rule row: overwhelmingly box-drawing with no prose riding along
	// (e.g. "┌──────┬───────┐", "├──────┼───────┤", "└──────┴───────┘").
	if alnum == 0 && box*5 >= total*4 && box >= 3 {
		return true
	}
	return false
}

// markTableRuns scans lines and returns a parallel boolean slice that is true for
// every line belonging to a contiguous run of >=2 table-row lines. Such runs are
// real tables (not chrome) and must be kept whole even when individual rows would
// otherwise look box-drawing-dominant. Blank lines break a run.
func markTableRuns(lines []string) []bool {
	keep := make([]bool, len(lines))
	i := 0
	for i < len(lines) {
		if !looksLikeTableRow(lines[i]) {
			i++
			continue
		}
		j := i
		for j < len(lines) && looksLikeTableRow(lines[j]) {
			j++
		}
		if j-i >= 2 { // a run of >=2 rows is a table; keep the whole run
			for k := i; k < j; k++ {
				keep[k] = true
			}
		}
		i = j
	}
	return keep
}

// wordCount counts whitespace-separated tokens that contain at least one letter
// or digit (so a leading glyph like "✓" is not counted as a word).
func wordCount(line string) int {
	n := 0
	for _, f := range strings.Fields(line) {
		for _, r := range f {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				n++
				break
			}
		}
	}
	return n
}

// isNoiseLine reports whether a (raw, untrimmed) line should be dropped in
// Stage 3. It is deliberately conservative: it never drops a line merely for
// being short, never drops a prose line that happens to mention spinner-like
// characters, and the caller guarantees it is never invoked while inside a code
// fence.
func isNoiseLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false // blank-line collapsing is Stage 5's job, not a drop here
	}

	// Box-drawing-dominant panel borders.
	if boxDrawingDominant(trimmed) {
		return true
	}

	// Unconditional chrome: the whole line is a spinner/status/progress marker.
	for _, re := range agyUnconditionalNoise {
		if re.MatchString(trimmed) {
			return true
		}
	}

	// Glyph-prefixed tool-status rows: drop only when the line is terse enough to
	// be chrome rather than a genuine answer bullet ("● First, plan the rollout,
	// then stage it, then ship." stays; "⏺ tool done" goes).
	if agyToolStatusPrefix.MatchString(trimmed) && wordCount(trimmed) <= toolStatusMaxWords {
		return true
	}

	return false
}

// ---------------------------------------------------------------------------
// ParseAgyOutput / StripANSI
// ---------------------------------------------------------------------------

// ParseAgyOutput salvages the most plausible final-answer text from raw agy PTY
// output. Never panics. Pipeline: CR-overwrite collapse -> ANSI/OSC/control strip
// -> spinner/box/tool-UI line denylist -> sentinel-or-last-block answer location
// -> cleanup. Empty result falls back to the ANSI-stripped full text (ConfidenceLow).
func ParseAgyOutput(raw []byte) ParseResult {
	// Stage 1: normalize line endings and collapse carriage-return overwrites.
	collapsed := collapseCarriageReturns(raw)

	// Stage 2: strip ANSI / OSC / 2-byte-ESC / lone control sequences.
	stripped := stripANSIString(collapsed)

	// Detect truncation on the cleaned text before later stages discard tails.
	truncated := looksTruncated(stripped)

	// Stage 4.1: sentinel-delimited answer wins outright (ConfidenceHigh) — but
	// only when the bracketed body actually carries content. An empty/whitespace
	// sentinel body is not a high-confidence answer; fall through to the last-block
	// heuristic / Low fallback instead of returning High-empty.
	if content, ok := extractSentinel(stripped); ok {
		if cleaned := cleanup(content); strings.TrimSpace(cleaned) != "" {
			return ParseResult{
				Content:    cleaned,
				Confidence: ConfidenceHigh,
				Truncated:  truncated,
			}
		}
	}

	// Stages 3 + 4.2: drop noise lines, then take the last contiguous non-noise
	// block of prose (ConfidenceMedium).
	if block := lastNonNoiseBlock(stripped); block != "" {
		return ParseResult{
			Content:    cleanup(block),
			Confidence: ConfidenceMedium,
			Truncated:  truncated,
		}
	}

	// Stage 5 fallback: never lose the answer entirely — return the ANSI-stripped
	// full text with ConfidenceLow.
	return ParseResult{
		Content:    cleanup(stripped),
		Confidence: ConfidenceLow,
		Truncated:  truncated,
	}
}

// StripANSI removes CSI/OSC/2-byte ESC sequences and lone control chars (keeps
// \n,\t). It is rune/UTF-8 safe and replaces escape sequences with nothing
// (zero-width), so "foo\x1b[0mbar" becomes "foobar", never "foo bar".
func StripANSI(raw []byte) []byte {
	return []byte(stripANSIString(string(raw)))
}

// ---------------------------------------------------------------------------
// Stage 1 — carriage-return overwrite collapse
// ---------------------------------------------------------------------------

// collapseCarriageReturns converts CRLF to LF first (so Windows line endings are
// preserved), then treats a bare CR as a logical-line reset: anything written
// after a CR overwrites the current line from the start. PTY spinners repaint by
// emitting \r before each frame, so only the final write of every physical line
// survives — killing the vast majority of spinner/progress noise.
//
// KNOWN LIMITATION (cursor-up repaint): this pass collapses only bare CR
// single-line repaints. Multi-line repaints driven by cursor-movement CSIs —
// ESC[<n>A (cursor up), ESC[<n>F (cursor up to column 1) — are NOT interpreted as
// logical-line rewinds here; those CSIs are simply stripped as zero-width control
// sequences in Stage 2, leaving every painted frame as a separate line. The
// downstream Stage-3 denylist (spinner/progress/box-drawing rows) and the
// last-non-noise-block heuristic absorb the resulting stale frames in practice
// (the final answer block still wins), so a full terminal-grid emulation is
// intentionally out of scope for this salvage parser. If a future agy release
// repaints answer *prose* (not just chrome) via cursor-up, revisit this with a
// proper ESC[<n>A line-rewind pass before Stage 2.
func collapseCarriageReturns(raw []byte) string {
	s := string(raw)
	// CRLF -> LF.
	s = strings.ReplaceAll(s, "\r\n", "\n")

	if !strings.ContainsRune(s, '\r') {
		return s
	}

	var out strings.Builder
	out.Grow(len(s))
	var buf []byte // bytes accumulated for the current logical line
	flush := func() {
		out.Write(buf)
		buf = buf[:0]
	}
	for _, r := range s {
		switch r {
		case '\n':
			flush()
			out.WriteByte('\n')
		case '\r':
			// Bare CR: reset the current logical line (later content wins).
			buf = buf[:0]
		default:
			buf = utf8.AppendRune(buf, r)
		}
	}
	flush()
	return out.String()
}

// ---------------------------------------------------------------------------
// Stage 2 — ANSI / OSC / control stripper (hand-rolled state machine)
// ---------------------------------------------------------------------------

// stripANSIString removes terminal escape sequences and lone control characters
// from s, preserving \n and \t. A hand-rolled scanner is used (rather than a
// regexp) because it is robust against unusual/partial sequences and lets us
// preserve the human-visible text inside OSC-8 hyperlinks while discarding the
// wrapper bytes. Removal is always zero-width.
func stripANSIString(s string) string {
	var out strings.Builder
	out.Grow(len(s))

	i := 0
	n := len(s)
	for i < n {
		c := s[i]
		if c == 0x1b { // ESC
			advance := skipEscapeSequence(s, i, &out)
			if advance > 0 {
				i += advance
				continue
			}
			// Lone/garbled ESC with nothing recognizable following: drop the ESC
			// byte itself and move on.
			i++
			continue
		}

		// Decode one rune so multibyte UTF-8 (Braille, box-drawing, CJK) is never
		// split. ASCII fast-path handles control chars.
		if c < utf8.RuneSelf {
			if c == '\n' || c == '\t' {
				out.WriteByte(c)
			} else if c < 0x20 || c == 0x7f {
				// Lone control char (NUL..US except \n,\t, plus DEL): drop.
			} else {
				out.WriteByte(c)
			}
			i++
			continue
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// Invalid byte; drop it rather than emit a replacement char.
			i++
			continue
		}
		out.WriteString(s[i : i+size])
		i += size
	}
	return out.String()
}

// skipEscapeSequence handles the escape sequence beginning at s[i] (s[i]==ESC).
// It writes any preserved visible text (OSC-8 link label) to out and returns the
// number of bytes consumed (including the ESC). It returns 0 when the byte after
// ESC is not part of a recognizable sequence, leaving the caller to drop just the
// ESC byte.
func skipEscapeSequence(s string, i int, out *strings.Builder) int {
	n := len(s)
	if i+1 >= n {
		return 1 // trailing lone ESC
	}
	switch s[i+1] {
	case '[':
		// CSI: ESC [ <params 0x30-0x3F> <intermediates 0x20-0x2F> <final 0x40-0x7E>
		j := i + 2
		for j < n {
			b := s[j]
			if b == 0x1b {
				// A stray ESC inside the CSI means the prefix was malformed (the
				// terminal would abort the sequence here). Consume only the malformed
				// prefix "ESC [ ... " up to (but not including) the inner ESC, so the
				// scanner restarts cleanly at that ESC instead of letting a later '['
				// become a bogus terminator and leak params.
				return j - i
			}
			if b >= 0x40 && b <= 0x7e {
				return j - i + 1 // include final byte
			}
			j++
		}
		return n - i // unterminated CSI: consume to end
	case ']':
		// OSC: ESC ] ... terminated by BEL (0x07) or ST (ESC \).
		return skipOSC(s, i, out)
	case '(', ')', '*', '+', '-', '.', '/':
		// Charset designation: ESC ( B etc. (one more byte).
		if i+2 < n {
			return 3
		}
		return n - i
	case '=', '>', '<', '7', '8', 'c', 'D', 'E', 'H', 'M', 'Z':
		// Two-byte sequences (keypad mode, save/restore cursor, reset, etc.).
		return 2
	case 'P', 'X', '^', '_':
		// DCS/SOS/PM/APC strings: ESC <P/X/^/_> ... ST (ESC \).
		return skipStringTerminated(s, i)
	default:
		// Unknown ESC sequence: drop ESC + the following byte.
		return 2
	}
}

// skipOSC consumes an OSC sequence starting at s[i] (s[i]==ESC, s[i+1]==']').
// OSC-8 hyperlinks have the form:
//
//	ESC ] 8 ; params ; URI  (BEL|ST)  visible text  ESC ] 8 ; ; (BEL|ST)
//
// The wrapper (the two OSC-8 control segments) is removed but the visible link
// text between them is preserved. All other OSC sequences are removed entirely.
func skipOSC(s string, i int, out *strings.Builder) int {
	n := len(s)
	body, end := readOSCBody(s, i)
	if end < 0 {
		return n - i // unterminated OSC: consume to end, nothing preserved
	}
	// Detect OSC-8 link opener: "8;params;URI". An opener carries a non-empty
	// URI; the matching closer is "8;;" (empty params, empty URI).
	if strings.HasPrefix(body, "8;") {
		rest := body[len("8;"):]
		// rest == "params;URI". The closer "8;;" yields rest == ";" (empty).
		if uri := osc8URI(rest); uri == "" {
			// Closer (or label-less): just drop the wrapper.
			return end - i
		}
		// Opener with a URI: drop the wrapper here. The visible label that
		// follows will be emitted as ordinary text by the main loop, and the
		// trailing "8;;" closer will be dropped when we reach it. Nothing to
		// preserve at the opener itself.
		return end - i
	}
	// Non-OSC-8 (title set, etc.): remove entirely.
	return end - i
}

// readOSCBody returns the OSC payload (the bytes between "ESC ]" and its
// terminator) and the index just past the terminator. end is -1 if unterminated.
func readOSCBody(s string, i int) (body string, end int) {
	n := len(s)
	start := i + 2 // past "ESC ]"
	j := start
	for j < n {
		if s[j] == 0x07 { // BEL terminator
			return s[start:j], j + 1
		}
		if s[j] == 0x1b && j+1 < n && s[j+1] == '\\' { // ST terminator (ESC \)
			return s[start:j], j + 2
		}
		j++
	}
	return "", -1
}

// osc8URI extracts the URI from an OSC-8 opener payload of the form
// "params;URI" (the leading "8;" already stripped). Returns "" when no URI is
// present (i.e. a closer "8;;").
func osc8URI(rest string) string {
	idx := strings.IndexByte(rest, ';')
	if idx < 0 {
		return ""
	}
	return rest[idx+1:]
}

// stringTerminatedScanWindow bounds how far skipStringTerminated looks for a
// terminator. A well-formed DCS/SOS/PM/APC string is short; if no ST/BEL appears
// within this window the ESC is almost certainly lone garbage and the bytes after
// it are the real answer, so we must NOT consume to EOF (which would eat the
// answer). 4 KiB comfortably covers any legitimate device string.
const stringTerminatedScanWindow = 4096

// skipStringTerminated consumes a DCS/SOS/PM/APC string starting at s[i]
// (s[i]==ESC, s[i+1] in {P,X,^,_}), terminated by ST (ESC \) or BEL. The whole
// sequence is removed. If no terminator is found within a bounded window (or an
// inner lone ESC appears first), the ESC was lone garbled chrome: drop only the
// two intro bytes (ESC + intro) and let the scanner resume on the rest.
func skipStringTerminated(s string, i int) int {
	n := len(s)
	limit := i + 2 + stringTerminatedScanWindow
	if limit > n {
		limit = n
	}
	j := i + 2
	for j < limit {
		if s[j] == 0x07 { // BEL terminator
			return j - i + 1
		}
		if s[j] == 0x1b {
			if j+1 < n && s[j+1] == '\\' { // ST terminator (ESC \)
				return j - i + 2
			}
			// Inner lone ESC before any terminator: treat the prefix as garbled and
			// drop just ESC + intro byte, restarting the scan at this inner ESC.
			return 2
		}
		j++
	}
	// No terminator within the window: lone garbled ESC. Drop only ESC + intro.
	return 2
}

// ---------------------------------------------------------------------------
// Stage 4.1 — sentinel extraction
// ---------------------------------------------------------------------------

// extractSentinel returns the text bracketed by the sentinel markers. It prefers
// the LAST well-formed BEGIN..END pair (so a stray unclosed first BEGIN followed
// by a clean pair does not leak the second BEGIN marker into the result). Only if
// no closed pair exists does it fall back to "everything after the first BEGIN"
// (truncated answer). Any residual sentinel markers are scrubbed from the result.
func extractSentinel(s string) (string, bool) {
	bi := strings.Index(s, sentinelBegin)
	if bi < 0 {
		return "", false
	}

	// Find the last END, then the last BEGIN that precedes it: that is the most
	// recent, well-formed answer block.
	if ei := strings.LastIndex(s, sentinelEnd); ei >= 0 {
		begin := strings.LastIndex(s[:ei], sentinelBegin)
		if begin >= 0 {
			body := s[begin+len(sentinelBegin) : ei]
			return scrubSentinels(body), true
		}
	}

	// No closed pair: salvage everything after the first opener (partial answer).
	return scrubSentinels(s[bi+len(sentinelBegin):]), true
}

// scrubSentinels removes any leftover sentinel marker substrings from text, so a
// malformed/duplicated marker cannot leak into the extracted answer.
func scrubSentinels(s string) string {
	s = strings.ReplaceAll(s, sentinelBegin, "")
	s = strings.ReplaceAll(s, sentinelEnd, "")
	return s
}

// ---------------------------------------------------------------------------
// Stages 3 + 4.2 — noise drop + last non-noise block
// ---------------------------------------------------------------------------

// codeFenceRE matches a Markdown code-fence delimiter line (``` or ~~~, with an
// optional language tag). Lines inside a fence are never treated as noise.
var codeFenceRE = regexp.MustCompile("^\\s*(```|~~~)")

// blockBoundary is a unique in-band sentinel pushed into the kept[] slice where a
// real noise/chrome line was dropped. It marks a hard answer-block boundary so the
// backward scan stops there, while ordinary blank lines (which it cannot collide
// with — it carries non-whitespace bytes) are retained inside the final block.
// Using a sentinel rather than "" is what lets a multi-paragraph answer keep its
// interior blank lines (P1: the old code emitted "" per dropped chrome line and
// also stopped the scan at every blank line, losing all but the last paragraph).
const blockBoundary = "\x00\x00AGY_BLOCK_BOUNDARY\x00\x00"

// lastNonNoiseBlock drops Stage-3 noise lines (honoring code fences and unfenced
// tables) and returns the last answer block — the prose that follows the final
// tool/spinner/chrome panel. The block may itself contain interior blank lines
// (multi-paragraph answers); it is terminated only by a dropped-chrome boundary.
func lastNonNoiseBlock(s string) string {
	lines := strings.Split(s, "\n")
	tableRow := markTableRuns(lines)
	kept := make([]string, 0, len(lines))

	inFence := false
	for idx, line := range lines {
		if codeFenceRE.MatchString(line) {
			inFence = !inFence
			kept = append(kept, line)
			continue
		}
		if inFence {
			kept = append(kept, line) // never drop inside a code fence
			continue
		}
		// Unfenced table rows that are part of a >=2-row run are real content; keep
		// the whole run even when a single row would look box-drawing-dominant.
		if tableRow[idx] {
			kept = append(kept, line)
			continue
		}
		// Bare percentage ("80%") is noise only when it sits among other progress
		// meters; a standalone "80%" answer is preserved.
		if barePercentIsNoise(lines, idx) {
			kept = append(kept, blockBoundary)
			continue
		}
		if isNoiseLine(line) {
			// Drop the chrome line, but push a UNIQUE boundary marker (not "") so the
			// backward scan stops here while plain blank lines stay inside the block.
			kept = append(kept, blockBoundary)
			continue
		}
		// A genuine blank line is neither chrome nor a boundary: keep it verbatim so
		// it can live inside a multi-paragraph final block.
		kept = append(kept, line)
	}

	// Trim trailing blank/boundary lines, then walk back to the last hard boundary.
	end := len(kept)
	for end > 0 && (kept[end-1] == blockBoundary || strings.TrimSpace(kept[end-1]) == "") {
		end--
	}
	if end == 0 {
		return ""
	}
	start := end
	for start > 0 && kept[start-1] != blockBoundary {
		start--
	}
	block := kept[start:end]
	// Drop any leading blank lines inside the recovered block.
	for len(block) > 0 && strings.TrimSpace(block[0]) == "" {
		block = block[1:]
	}
	return strings.Join(block, "\n")
}

// ---------------------------------------------------------------------------
// Stage 5 — cleanup
// ---------------------------------------------------------------------------

// multiBlankRE collapses 3+ consecutive blank lines into a single blank line.
var multiBlankRE = regexp.MustCompile(`\n[ \t]*\n([ \t]*\n)+`)

// cleanup collapses runs of blank lines, trims trailing whitespace per line,
// strips a trailing prompt-echo line, and TrimSpaces the whole result.
func cleanup(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = multiBlankRE.ReplaceAllString(s, "\n\n")

	// Trim trailing whitespace on each line (PTY padding to COLUMNS width).
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	lines = stripTrailingPromptEcho(lines)
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// promptEchoRE matches a trailing shell-style prompt that agy's PTY re-prints
// after the answer. Two genuine shapes only:
//
//   - a BARE prompt sigil on its own line: ">", "$", "#" (optionally surrounded
//     by whitespace), e.g. "> " or "$".
//   - an identifier prompt followed by a sigil AND the trailing space the PTY
//     echoes: "agy> ", "user$ ", "root# " (trailing space REQUIRED).
//
// The required trailing space is the discriminator that protects terse one-line
// answers, which never carry it: "C#", "F#", "x86_64#", "v1.2.3#", "make#",
// "result.json#", "a-b-c>" are all left untouched. Identifier chars are limited
// to [A-Za-z0-9_-] (no '.') so version/path-shaped tokens are never treated as a
// prompt host name.
var promptEchoRE = regexp.MustCompile(`^\s*(?:[>$#]|[A-Za-z0-9_-]+[>$#] )\s*$`)

// stripTrailingPromptEcho removes trailing empty / prompt-only lines, but never
// empties a non-empty block: if stripping would leave nothing, the original lines
// are returned so a terse answer that merely looks prompt-ish is not lost.
func stripTrailingPromptEcho(lines []string) []string {
	out := append([]string(nil), lines...)
	for len(out) > 0 {
		last := out[len(out)-1]
		if strings.TrimSpace(last) == "" || promptEchoRE.MatchString(last) {
			out = out[:len(out)-1]
			continue
		}
		break
	}
	// Guard: if the block had real content but stripping consumed all of it, keep
	// the original (prefer the unstripped block over an empty result).
	if len(out) == 0 {
		for _, l := range lines {
			if strings.TrimSpace(l) != "" {
				return lines
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Truncation heuristic (conservative)
// ---------------------------------------------------------------------------

// timeoutMarkerRE matches agy's print-timeout / interruption markers. When the
// output ends shortly after one of these with no clean terminator, it is treated
// as a cut-off answer.
var timeoutMarkerRE = regexp.MustCompile(`(?i)(print[- ]?timeout|timed out|timeout reached|\binterrupted\b|operation cancell?ed)`)

// danglingWord matches a trailing connective/function word that strongly signals
// an unfinished sentence ("...the plan is to", "...we need the"). A complete
// newline-less answer ("Paris", "法國的首都是巴黎") ends on a content word and is
// NOT flagged — ending-on-a-letter alone is no longer treated as truncation.
var danglingWord = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "to": {}, "of": {}, "in": {}, "on": {},
	"at": {}, "by": {}, "for": {}, "and": {}, "or": {}, "but": {}, "with": {},
	"is": {}, "are": {}, "was": {}, "were": {}, "be": {}, "as": {}, "that": {},
	"this": {}, "from": {}, "into": {}, "than": {}, "then": {}, "we": {}, "it": {},
}

// veryLongUnterminated is the byte length above which an answer that lacks a
// terminal newline is itself a co-signal of truncation (a long capture is far
// more likely to have been cut off than a short, deliberate one-liner).
const veryLongUnterminated = 2000

// looksTruncated applies a conservative cut-off heuristic. It returns true only
// when there is positive evidence of truncation. Ending on a letter/CJK rune with
// no terminal newline is, on its own, NOT enough (a terse complete answer like
// "Paris" or "法國的首都是巴黎" must not be flagged). A co-signal is required:
//
//   - a timeout / interruption marker near the tail; OR
//   - an unclosed answer sentinel (BEGIN with no matching END); OR
//   - an unclosed code fence (odd number of fence delimiters); OR
//   - a trailing list marker (dangling bullet / numbered item); OR
//   - a dangling connective word ("...is to", "...we need the"); OR
//   - very long unterminated output.
//
// Empty output is never truncated.
func looksTruncated(stripped string) bool {
	if strings.TrimSpace(stripped) == "" {
		return false
	}

	// Unclosed answer sentinel: a BEGIN marker with no matching END after it means
	// the answer was cut off before agy could print the closer — strong evidence of
	// truncation regardless of how the tail rune looks.
	if bi := strings.Index(stripped, sentinelBegin); bi >= 0 {
		if !strings.Contains(stripped[bi+len(sentinelBegin):], sentinelEnd) {
			return true
		}
	}

	// Timeout/interruption marker anywhere in the last ~400 bytes — co-signal on
	// its own, independent of the terminal-newline state.
	tail := stripped
	if len(tail) > 400 {
		tail = tail[len(tail)-400:]
	}
	if timeoutMarkerRE.MatchString(tail) {
		return true
	}

	// Unclosed code fence is a strong truncation signal even with a final newline
	// (the answer was cut off inside a ``` block).
	if countCodeFences(stripped)%2 == 1 {
		return true
	}

	// A normal, complete capture from a PTY ends with a newline; with one present
	// and the fence balanced, we have no positive evidence of a cut-off.
	if strings.HasSuffix(stripped, "\n") {
		return false
	}
	trimmedRight := strings.TrimRight(stripped, " \t")
	if trimmedRight == "" {
		return false
	}
	lastRune, _ := utf8.DecodeLastRuneInString(trimmedRight)
	switch lastRune {
	case '.', '!', '?', ':', ';', ')', ']', '}', '"', '\'', '`', '…', '。', '！', '？':
		return false // sentence/clause terminator => looks complete
	}

	// Co-signal: a dangling list marker on the final line ("- ", "* ", "1. ").
	if lastLineIsDanglingListMarker(trimmedRight) {
		return true
	}

	// Co-signal: very long output with no terminator.
	if len(trimmedRight) > veryLongUnterminated &&
		(unicode.IsLetter(lastRune) || unicode.IsDigit(lastRune)) {
		return true
	}

	// Co-signal: ends on a dangling connective/function word.
	if unicode.IsLetter(lastRune) {
		fields := strings.Fields(trimmedRight)
		if len(fields) > 0 {
			last := strings.ToLower(fields[len(fields)-1])
			if _, ok := danglingWord[last]; ok {
				return true
			}
		}
	}

	// No co-signal: a newline-less answer ending on a content word is treated as
	// complete (conservative — we do not invent truncation).
	return false
}

// countCodeFences counts Markdown code-fence delimiter lines (``` or ~~~).
func countCodeFences(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if codeFenceRE.MatchString(line) {
			n++
		}
	}
	return n
}

// danglingListMarkerRE matches a final line that is just an empty list bullet or
// numbered-item marker ("- ", "* ", "+ ", "1. ") with nothing after it.
var danglingListMarkerRE = regexp.MustCompile(`(?m)^[ \t]*([-*+]|\d+[.)])[ \t]*$`)

// lastLineIsDanglingListMarker reports whether the final line of s is an empty
// list marker — a strong signal the answer was cut off mid-list.
func lastLineIsDanglingListMarker(s string) bool {
	idx := strings.LastIndexByte(s, '\n')
	last := s
	if idx >= 0 {
		last = s[idx+1:]
	}
	return danglingListMarkerRE.MatchString(last)
}
