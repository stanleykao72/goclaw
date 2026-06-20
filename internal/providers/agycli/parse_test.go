package agycli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readFixture loads a raw PTY transcript (with real ESC bytes) from testdata.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// TestParseAgyOutput_Goldens drives the parser against captured raw transcripts.
// Each fixture contains genuine escape bytes (see testdata/*.bin). Assertions
// cover both the salvaged Content and the Confidence signal, plus Truncated for
// the cut-off case.
func TestParseAgyOutput_Goldens(t *testing.T) {
	cases := []struct {
		name           string
		fixture        string
		wantContains   []string // substrings that MUST appear in Content
		wantExcludes   []string // substrings that MUST NOT appear in Content
		wantConfidence Confidence
		wantTruncated  bool
	}{
		{
			name:    "clean",
			fixture: "clean.bin",
			wantContains: []string{
				"The capital of France is Paris.",
				"It has been the capital since the 10th century.",
			},
			wantExcludes:   []string{"\x1b", "\r"},
			wantConfidence: ConfidenceMedium,
		},
		{
			name:    "full-ansi",
			fixture: "full-ansi.bin",
			wantContains: []string{
				"Answer:",
				"The library is at the docs site.", // OSC-8 visible label preserved
				"Use the ParseAgyOutput function",  // color stripped zero-width, not space
			},
			wantExcludes: []string{
				"\x1b",                     // no escape bytes survive
				"]8;;",                     // OSC-8 wrapper gone
				"https://",                 // OSC-8 URI dropped, only label kept
				"ParseAgyOutput  function", // no DOUBLE space from zero-width strip; single space is correct
			},
			wantConfidence: ConfidenceMedium,
		},
		{
			name:    "spinner-heavy",
			fixture: "spinner-heavy.bin",
			wantContains: []string{
				"42 is the answer to life, the universe, and everything.",
			},
			wantExcludes: []string{
				"Thinking", "Working", "⠋", "⠙", "|", "\x1b", "\r",
			},
			wantConfidence: ConfidenceMedium,
		},
		{
			name:    "tool-panel-before-answer",
			fixture: "tool-panel-before-answer.bin",
			wantContains: []string{
				"The function reads the file and returns its contents as a string.",
				"It returns an error if the path does not exist.",
			},
			wantExcludes: []string{
				"read_file", "Running", "45%", "60%",
				"╭", "╮", "╰", "╯", "⏺", "✓", "●",
				"\x1b",
			},
			wantConfidence: ConfidenceMedium,
		},
		{
			name:    "truncated",
			fixture: "truncated.bin",
			wantContains: []string{
				"The migration plan has three phases. First we",
			},
			wantExcludes:   []string{"Thinking", "\x1b", "\r"},
			wantConfidence: ConfidenceMedium,
			wantTruncated:  true,
		},
		{
			name:    "answer-contains-table-chars",
			fixture: "answer-contains-table-chars.bin",
			wantContains: []string{
				"Here is the comparison table:",
				"| Name | Value |", // table pipes inside fence preserved
				"|------|-------|",
				"| a    | 1     |",
				"```",
				`The spinner uses the characters | / - \ in sequence.`, // prose with spinner-like chars kept
			},
			wantExcludes:   []string{"tool done", "⏺", "\x1b"},
			wantConfidence: ConfidenceMedium,
		},
		{
			name:    "sentinel-wrapped",
			fixture: "sentinel-wrapped.bin",
			wantContains: []string{
				"PONG",
			},
			wantExcludes: []string{
				"AGY_ANSWER_BEGIN", "AGY_ANSWER_END",
				"Thinking", "done", "agy>", "\x1b", "\r",
			},
			wantConfidence: ConfidenceHigh,
		},
		{
			name:    "multi-paragraph-answer",
			fixture: "multi-paragraph-answer.bin",
			wantContains: []string{
				"Here is the analysis.",
				"First, a bug on line 10.",
				"In summary, fix both.",
			},
			wantExcludes: []string{
				"read_file", "Running", "45%", "60%",
				"╭", "╮", "╰", "╯", "⏺", "✓", "\x1b",
			},
			wantConfidence: ConfidenceMedium,
		},
		{
			name:    "unicode-box-table",
			fixture: "unicode-box-table.bin",
			wantContains: []string{
				"│ Name │ Value │",
				"│ a    │ 1     │",
				"│ b    │ 2     │",
			},
			wantExcludes:   []string{"tool done", "⏺", "\x1b"},
			wantConfidence: ConfidenceMedium,
		},
		{
			name:    "markdown-pipe-table",
			fixture: "markdown-pipe-table.bin",
			wantContains: []string{
				"| Name | Value |",
				"|------|-------|",
				"| a    | 1     |",
				"| b    | 2     |",
			},
			wantExcludes:   []string{"ran tool", "✓", "\x1b"},
			wantConfidence: ConfidenceMedium,
		},
		{
			name:    "embedded-dcs",
			fixture: "embedded-dcs.bin",
			wantContains: []string{
				"beforeafter",
			},
			wantExcludes:   []string{"stuff", "1;2;3", "\x1b"},
			wantConfidence: ConfidenceMedium,
		},
		{
			name:    "unterminated-dcs",
			fixture: "unterminated-dcs.bin",
			wantContains: []string{
				"answer: -ray vision", // ESC+X dropped, rest of answer survives
			},
			wantExcludes:   []string{"\x1b"},
			wantConfidence: ConfidenceMedium,
		},
		{
			name:    "cursor-up-repaint",
			fixture: "cursor-up-repaint.bin",
			wantContains: []string{
				"The result is 42.", // final answer survives repaint frames
			},
			wantExcludes:   []string{"\x1b", "\r"},
			wantConfidence: ConfidenceMedium,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := readFixture(t, tc.fixture)
			got := ParseAgyOutput(raw)

			for _, sub := range tc.wantContains {
				if !strings.Contains(got.Content, sub) {
					t.Errorf("Content missing %q\n--- full Content ---\n%s", sub, got.Content)
				}
			}
			for _, sub := range tc.wantExcludes {
				if strings.Contains(got.Content, sub) {
					t.Errorf("Content should not contain %q\n--- full Content ---\n%s", sub, got.Content)
				}
			}
			if got.Confidence != tc.wantConfidence {
				t.Errorf("Confidence = %q, want %q", got.Confidence, tc.wantConfidence)
			}
			if got.Truncated != tc.wantTruncated {
				t.Errorf("Truncated = %v, want %v", got.Truncated, tc.wantTruncated)
			}
		})
	}
}

// TestParseAgyOutput_SentinelExact verifies sentinel content is extracted exactly.
func TestParseAgyOutput_SentinelExact(t *testing.T) {
	raw := []byte("noise\n" + sentinelBegin + "\nexactly this\n" + sentinelEnd + "\nmore noise\n")
	got := ParseAgyOutput(raw)
	if got.Confidence != ConfidenceHigh {
		t.Fatalf("Confidence = %q, want high", got.Confidence)
	}
	if got.Content != "exactly this" {
		t.Errorf("Content = %q, want %q", got.Content, "exactly this")
	}
}

// TestParseAgyOutput_SentinelTruncated: opener present, closer missing => salvage
// the remainder and still report high confidence.
func TestParseAgyOutput_SentinelTruncated(t *testing.T) {
	raw := []byte("noise\n" + sentinelBegin + "\npartial answer that was cut")
	got := ParseAgyOutput(raw)
	if got.Confidence != ConfidenceHigh {
		t.Fatalf("Confidence = %q, want high", got.Confidence)
	}
	if !strings.Contains(got.Content, "partial answer that was cut") {
		t.Errorf("Content = %q, want it to contain the partial answer", got.Content)
	}
	if !got.Truncated {
		t.Errorf("Truncated = false, want true (no terminal newline, mid-sentence)")
	}
}

// TestParseAgyOutput_EmptyFallback: all-noise input falls back to ConfidenceLow
// (never panics, never loses everything entirely).
func TestParseAgyOutput_AllNoiseFallback(t *testing.T) {
	// Pure spinner repaint that collapses to a single trailing spinner frame.
	raw := []byte("\r\x1b[2K⠋\r\x1b[2K⠙\r\x1b[2K⠹")
	got := ParseAgyOutput(raw)
	// The last surviving frame is a lone spinner glyph -> noise -> empty block ->
	// Stage-5 fallback returns ANSI-stripped text at low confidence.
	if got.Confidence != ConfidenceLow {
		t.Errorf("Confidence = %q, want low", got.Confidence)
	}
}

// TestParseAgyOutput_TrulyEmpty: empty input never panics and is not truncated.
func TestParseAgyOutput_TrulyEmpty(t *testing.T) {
	got := ParseAgyOutput(nil)
	if got.Content != "" {
		t.Errorf("Content = %q, want empty", got.Content)
	}
	if got.Confidence != ConfidenceLow {
		t.Errorf("Confidence = %q, want low", got.Confidence)
	}
	if got.Truncated {
		t.Errorf("Truncated = true, want false for empty input")
	}
}

// TestParseAgyOutput_MultiParagraphExact pins the EXACT content of a 3-paragraph
// answer after a tool panel: every paragraph (and its interior blank lines) must
// survive — the old lastNonNoiseBlock kept only the final paragraph.
func TestParseAgyOutput_MultiParagraphExact(t *testing.T) {
	raw := readFixture(t, "multi-paragraph-answer.bin")
	got := ParseAgyOutput(raw)
	want := "Here is the analysis.\n\nFirst, a bug on line 10.\n\nIn summary, fix both."
	if got.Content != want {
		t.Errorf("Content =\n%q\nwant\n%q", got.Content, want)
	}
	if got.Confidence != ConfidenceMedium {
		t.Errorf("Confidence = %q, want medium", got.Confidence)
	}
}

// TestParseAgyOutput_InlineMultiParagraph repros the spec example directly (no
// fixture file) with EXACT-content equality.
func TestParseAgyOutput_InlineMultiParagraph(t *testing.T) {
	raw := []byte("Here is the analysis.\n\nFirst, a bug on line 10.\n\nIn summary, fix both.\n")
	got := ParseAgyOutput(raw)
	want := "Here is the analysis.\n\nFirst, a bug on line 10.\n\nIn summary, fix both."
	if got.Content != want {
		t.Errorf("Content =\n%q\nwant\n%q", got.Content, want)
	}
}

// TestParseAgyOutput_UnicodeBoxTableExact pins every row of an unfenced unicode
// box table — the whole >=2-row run must survive, not just the last row.
func TestParseAgyOutput_UnicodeBoxTableExact(t *testing.T) {
	raw := readFixture(t, "unicode-box-table.bin")
	got := ParseAgyOutput(raw)
	for _, row := range []string{
		"┌──────┬───────┐",
		"│ Name │ Value │",
		"├──────┼───────┤",
		"│ a    │ 1     │",
		"│ b    │ 2     │",
		"└──────┴───────┘",
	} {
		if !strings.Contains(got.Content, row) {
			t.Errorf("missing table row %q\n--- Content ---\n%s", row, got.Content)
		}
	}
}

// TestParseAgyOutput_PipeTableExact pins every row of an unfenced markdown pipe
// table.
func TestParseAgyOutput_PipeTableExact(t *testing.T) {
	raw := readFixture(t, "markdown-pipe-table.bin")
	got := ParseAgyOutput(raw)
	want := "Results below:\n| Name | Value |\n|------|-------|\n| a    | 1     |\n| b    | 2     |"
	if got.Content != want {
		t.Errorf("Content =\n%q\nwant\n%q", got.Content, want)
	}
}

// TestParseAgyOutput_TerseAnswers covers terse one-line answers that the old
// promptEchoRE / percent denylist destroyed. Each input puts a chrome boundary
// (a dropped tool-status row) before the answer — the realistic transcript shape
// — so the terse answer is the last block. EXACT-content equality.
func TestParseAgyOutput_TerseAnswers(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"csharp", "⏺ tool done\nC#\n", "C#"},
		{"fsharp", "✓ ran (0.1s)\nF#\n", "F#"},
		{"arch", "⏺ tool done\nx86_64#\n", "x86_64#"},
		{"version", "⏺ tool done\nv1.2.3#\n", "v1.2.3#"},
		{"make", "⏺ tool done\nmake#\n", "make#"},
		{"jsonfile", "⏺ tool done\nresult.json#\n", "result.json#"},
		{"dasharrow", "⏺ tool done\na-b-c>\n", "a-b-c>"},
		{"percent", "⏺ tool done\n80%\n", "80%"},
		{"pipe-answer", "⏺ tool done\na|b\n", "a|b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseAgyOutput([]byte(tc.raw))
			if got.Content != tc.want {
				t.Errorf("Content = %q, want %q (Confidence=%q)", got.Content, tc.want, got.Confidence)
			}
		})
	}
}

// TestParseAgyOutput_TersePromptShapesNotStripped is a finer-grained guard on the
// prompt-echo stripper alone: a terse answer that merely *looks* prompt-ish must
// never be stripped to empty. Here the answer is the only content (no boundary),
// so the result is the whole thing — what matters is that it is NOT lost.
func TestParseAgyOutput_TersePromptShapesNotStripped(t *testing.T) {
	for _, ans := range []string{"C#", "F#", "x86_64#", "v1.2.3#", "make#", "result.json#", "a-b-c>", "80%"} {
		got := ParseAgyOutput([]byte(ans + "\n"))
		if !strings.Contains(got.Content, ans) {
			t.Errorf("terse answer %q lost; Content=%q", ans, got.Content)
		}
	}
}

// TestParseAgyOutput_BarePercentInNoisyRegion verifies a percent line IS dropped
// when it rides inside a dropped-chrome region (progress meter), even though a
// standalone "80%" is kept (see TestParseAgyOutput_TerseAnswers).
func TestParseAgyOutput_BarePercentInNoisyRegion(t *testing.T) {
	raw := []byte("⏺ tool running\n45%\n[===>   ] 60%\n\nThe real answer.\n")
	got := ParseAgyOutput(raw)
	if got.Content != "The real answer." {
		t.Errorf("Content = %q, want %q", got.Content, "The real answer.")
	}
	if strings.Contains(got.Content, "45%") {
		t.Errorf("Content should not contain progress percent: %q", got.Content)
	}
}

// TestParseAgyOutput_EmptySentinelBody: an empty/whitespace sentinel body must NOT
// return High-empty; it falls through to the last-block / Low heuristic.
func TestParseAgyOutput_EmptySentinelBody(t *testing.T) {
	raw := []byte("preceding real answer\n" + sentinelBegin + "\n   \n" + sentinelEnd + "\n")
	got := ParseAgyOutput(raw)
	if got.Confidence == ConfidenceHigh {
		t.Errorf("empty sentinel body should not be High; got %q content=%q", got.Confidence, got.Content)
	}
	if strings.TrimSpace(got.Content) == "" {
		// The fall-through should still recover the preceding real answer.
		t.Errorf("expected non-empty fallback content, got empty")
	}
}

// TestParseAgyOutput_TrulyEmptySentinelBodyNoContent: empty sentinel body with no
// other content falls through to Low with empty content (never High-empty).
func TestParseAgyOutput_TrulyEmptySentinelBodyNoContent(t *testing.T) {
	raw := []byte(sentinelBegin + "\n\n" + sentinelEnd + "\n")
	got := ParseAgyOutput(raw)
	if got.Confidence == ConfidenceHigh {
		t.Errorf("empty-body-only sentinel must not be High; got %q", got.Confidence)
	}
}

// TestExtractSentinel_LastWellFormedPair: an unclosed first BEGIN followed by a
// well-formed pair must extract the SECOND pair and leak no marker text.
func TestExtractSentinel_LastWellFormedPair(t *testing.T) {
	raw := []byte("junk\n" + sentinelBegin + " stray opener with no end\nmore junk\n" +
		sentinelBegin + "\nthe real answer\n" + sentinelEnd + "\n")
	got := ParseAgyOutput(raw)
	if got.Confidence != ConfidenceHigh {
		t.Fatalf("Confidence = %q, want high", got.Confidence)
	}
	if got.Content != "the real answer" {
		t.Errorf("Content = %q, want %q", got.Content, "the real answer")
	}
	if strings.Contains(got.Content, "AGY_ANSWER_BEGIN") || strings.Contains(got.Content, "AGY_ANSWER_END") {
		t.Errorf("Content leaked a sentinel marker: %q", got.Content)
	}
}

// TestParseAgyOutput_CursorUpRepaintRecovers verifies the final answer survives a
// cursor-up repaint sequence (known limitation: frames are not collapsed, but the
// last-block heuristic still recovers the answer). Never panics.
func TestParseAgyOutput_CursorUpRepaintRecovers(t *testing.T) {
	raw := readFixture(t, "cursor-up-repaint.bin")
	got := ParseAgyOutput(raw)
	if !strings.Contains(got.Content, "The result is 42.") {
		t.Errorf("Content missing final answer\n--- Content ---\n%s", got.Content)
	}
	if strings.Contains(got.Content, "\x1b") {
		t.Errorf("Content leaked ESC bytes: %q", got.Content)
	}
}

// TestStripANSI_EmbeddedDCSExact: a complete DCS string is removed; surrounding
// text is joined with zero width. EXACT equality.
func TestStripANSI_EmbeddedDCSExact(t *testing.T) {
	in := []byte("before\x1bP1;2;3stuff\x1b\\after")
	got := string(StripANSI(in))
	if got != "beforeafter" {
		t.Errorf("StripANSI = %q, want %q", got, "beforeafter")
	}
}

// TestStripANSI_UnterminatedDCSNoOverConsume: an unterminated DCS must drop only
// ESC + intro byte, never consume the rest of the answer.
func TestStripANSI_UnterminatedDCSNoOverConsume(t *testing.T) {
	in := []byte("answer: \x1bX-ray vision")
	got := string(StripANSI(in))
	if got != "answer: -ray vision" {
		t.Errorf("StripANSI = %q, want %q", got, "answer: -ray vision")
	}
}

// TestStripANSI_InnerESCInCSI: a stray ESC mid-CSI must not let the next '[' act
// as a bogus terminator that leaks params.
func TestStripANSI_InnerESCInCSI(t *testing.T) {
	in := []byte("a\x1b[\x1b[0mvisible")
	got := string(StripANSI(in))
	if got != "avisible" {
		t.Errorf("StripANSI = %q, want %q", got, "avisible")
	}
}

// TestLooksTruncated_CompleteNoNewlineNotTruncated: a complete newline-less answer
// ending on a content letter / CJK rune must NOT be flagged truncated.
func TestLooksTruncated_CompleteNoNewlineNotTruncated(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"cjk complete", "法國的首都是巴黎", false},
		{"latin one word", "Paris", false},
		{"dangling preposition", "The plan is to", true},
		{"dangling article", "We need the", true},
		{"open code fence", "Here:\n```go\nfunc main() {", true},
		{"closed code fence", "Here:\n```go\nfunc main() {}\n```\n", false},
		{"dangling list marker", "Steps:\n- ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksTruncated(tc.in); got != tc.want {
				t.Errorf("looksTruncated(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestLooksLikeTableRow exercises the table-row detector directly.
func TestLooksLikeTableRow(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"| Name | Value |", true},
		{"|------|-------|", true},
		{"│ a │ 1 │", true},
		{"a | b | c", true},                               // 2 pipes + alnum
		{"The spinner uses | / - \\ in sequence.", false}, // single pipe => not a row
		{"a|b", false},                                    // only one pipe
		{"plain prose here", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := looksLikeTableRow(tc.line); got != tc.want {
			t.Errorf("looksLikeTableRow(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// TestStripANSI_ZeroWidth verifies escape removal is zero-width (no space splits).
func TestStripANSI_ZeroWidth(t *testing.T) {
	in := []byte("foo\x1b[0mbar")
	got := string(StripANSI(in))
	if got != "foobar" {
		t.Errorf("StripANSI = %q, want %q", got, "foobar")
	}
}

// TestStripANSI_KeepsTabAndNewline verifies \t and \n survive while other control
// chars are dropped.
func TestStripANSI_KeepsTabAndNewline(t *testing.T) {
	in := []byte("a\tb\nc\x00d\x07e")
	got := string(StripANSI(in))
	if got != "a\tb\ncde" {
		t.Errorf("StripANSI = %q, want %q", got, "a\tb\ncde")
	}
}

// TestStripANSI_OSC8PreservesLabel checks the OSC-8 visible label survives while
// the wrapper and URI are removed.
func TestStripANSI_OSC8PreservesLabel(t *testing.T) {
	in := []byte("see \x1b]8;;https://x.test\x07the link\x1b]8;;\x07 now")
	got := string(StripANSI(in))
	want := "see the link now"
	if got != want {
		t.Errorf("StripANSI = %q, want %q", got, want)
	}
}

// TestStripANSI_UTF8Safe verifies multibyte runes (Braille, CJK) pass through
// intact and are never split.
func TestStripANSI_UTF8Safe(t *testing.T) {
	in := []byte("中文\x1b[31m測試\x1b[0m⠿done")
	got := string(StripANSI(in))
	want := "中文測試⠿done"
	if got != want {
		t.Errorf("StripANSI = %q, want %q", got, want)
	}
}

// TestStripANSI_UnterminatedCSI does not panic and consumes to end.
func TestStripANSI_UnterminatedCSI(t *testing.T) {
	in := []byte("abc\x1b[1;2;3") // no final byte
	got := string(StripANSI(in))
	if got != "abc" {
		t.Errorf("StripANSI = %q, want %q", got, "abc")
	}
}

// TestCollapseCarriageReturns_Overwrite checks bare-CR line reset and CRLF safety.
func TestCollapseCarriageReturns_Overwrite(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"crlf preserved", "a\r\nb\r\n", "a\nb\n"},
		{"bare cr overwrites", "loading...\rdone", "done"},
		{"spinner frames", "\rA\rB\rC", "C"},
		{"cr then newline keeps last write", "x\ry\nz", "y\nz"},
		{"no cr passthrough", "plain text\n", "plain text\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := collapseCarriageReturns([]byte(tc.in))
			if got != tc.want {
				t.Errorf("collapseCarriageReturns(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestIsNoiseLine_Conservatism guards the "never drop short-but-alpha" and "never
// drop prose that merely mentions spinner chars" contracts.
func TestIsNoiseLine_Conservatism(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"pure braille spinner", "⠋⠙⠹", true},
		{"ascii spinner with status", "/ Working", true},
		{"tool status row", "✓ read_file (0.3s)", true},
		{"running marker", "Running…", true},
		// A BARE percentage is NO LONGER unconditional noise: "80%" can be a real
		// terse answer. It is treated as noise only inside an already-dropped region
		// (handled contextually in lastNonNoiseBlock, not in isNoiseLine).
		{"bare percent NOT dropped standalone", "  45%", false},
		{"progress bar", "[===>   ] 60%", true},
		{"box border", "╭──────────╮", true},
		{"short alpha answer NOT dropped", "Yes.", false},
		{"single word NOT dropped", "Paris", false},
		{"prose mentioning spinner chars NOT dropped", `The spinner uses | / - \ in turn.`, false},
		{"table row NOT dropped", "| Name | Value |", false},
		{"titled border with word NOT dropped", "├─ result ─┤ ok and fine here", false},
		{"blank line not noise", "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNoiseLine(tc.line); got != tc.want {
				t.Errorf("isNoiseLine(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// TestLooksTruncated covers the conservative cut-off heuristic.
func TestLooksTruncated(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"complete with newline", "All done.\n", false},
		{"complete sentence no newline", "All done.", false},
		{"mid-sentence no newline", "The plan is to", true},
		{"timeout marker", "...print-timeout reached\n", true},
		{"interrupted marker", "operation interrupted", true},
		{"empty not truncated", "", false},
		{"ends on colon complete", "Here are the steps:", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksTruncated(tc.in); got != tc.want {
				t.Errorf("looksTruncated(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
