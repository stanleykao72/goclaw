package providers

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseGrokJSONResponse covers the single-turn `--output-format json`
// fixture from spec §2.1 plus its mapping edge cases.
func TestParseGrokJSONResponse(t *testing.T) {
	const fixture = `{ "text":"PONG", "stopReason":"EndTurn", "sessionId":"019f-abc",
	  "thought":"thinking hard",
	  "usage":{"input_tokens":13393,"cache_read_input_tokens":1024,
	           "output_tokens":35,"reasoning_tokens":29,"total_tokens":14452} }`

	cases := []struct {
		name             string
		input            string
		wantErr          bool
		wantContent      string
		wantThinking     string
		wantFinishReason string
		wantUsage        *Usage
	}{
		{
			name:             "spec §2.1 fixture",
			input:            fixture,
			wantContent:      "PONG",
			wantThinking:     "thinking hard",
			wantFinishReason: "stop",
			wantUsage: &Usage{
				PromptTokens:     13393,
				CompletionTokens: 35,
				TotalTokens:      14452,
				CacheReadTokens:  1024,
				ThinkingTokens:   29,
			},
		},
		{
			name:             "non-EndTurn stopReason maps to error",
			input:            `{"text":"partial","stopReason":"MaxTokens"}`,
			wantContent:      "partial",
			wantFinishReason: "error",
		},
		{
			name:             "no usage object",
			input:            `{"text":"hi","stopReason":"EndTurn"}`,
			wantContent:      "hi",
			wantFinishReason: "stop",
		},
		{
			name:             "line-delimited: last object wins",
			input:            "{\"text\":\"first\",\"stopReason\":\"EndTurn\"}\n{\"text\":\"second\",\"stopReason\":\"EndTurn\"}",
			wantContent:      "second",
			wantFinishReason: "stop",
		},
		{
			name:    "empty input errors",
			input:   "   \n  ",
			wantErr: true,
		},
		{
			name:    "garbage errors",
			input:   "not json at all",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := parseGrokJSONResponse([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got resp=%+v", resp)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.Content != tc.wantContent {
				t.Errorf("Content = %q, want %q", resp.Content, tc.wantContent)
			}
			if resp.Thinking != tc.wantThinking {
				t.Errorf("Thinking = %q, want %q", resp.Thinking, tc.wantThinking)
			}
			if resp.FinishReason != tc.wantFinishReason {
				t.Errorf("FinishReason = %q, want %q", resp.FinishReason, tc.wantFinishReason)
			}
			if tc.wantUsage != nil {
				if resp.Usage == nil {
					t.Fatalf("Usage = nil, want %+v", tc.wantUsage)
				}
				if *resp.Usage != *tc.wantUsage {
					t.Errorf("Usage = %+v, want %+v", *resp.Usage, *tc.wantUsage)
				}
			}
		})
	}
}

// TestGrokStreamEventParsing covers the three streaming-json event types from
// spec §2.2 and their mapping to StreamChunk / ChatResponse fields.
func TestGrokStreamEventParsing(t *testing.T) {
	lines := []string{
		`{"type":"thought","data":"let me think"}`,
		`{"type":"text","data":"Hello "}`,
		`{"type":"text","data":"world"}`,
		`{"type":"end","stopReason":"EndTurn","sessionId":"019f-xyz","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":13,"cache_read_input_tokens":4,"reasoning_tokens":1}}`,
	}

	var content, thinking strings.Builder
	var final ChatResponse
	for _, line := range lines {
		var ev grokStreamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		switch ev.Type {
		case "thought":
			thinking.WriteString(ev.Data)
		case "text":
			content.WriteString(ev.Data)
		case "end":
			final.FinishReason = grokFinishReason(ev.StopReason)
			if ev.Usage != nil {
				final.Usage = mapGrokUsage(ev.Usage)
			}
			if ev.SessionId != "019f-xyz" {
				t.Errorf("end.SessionId = %q, want 019f-xyz", ev.SessionId)
			}
		}
	}

	if got := content.String(); got != "Hello world" {
		t.Errorf("accumulated content = %q, want %q", got, "Hello world")
	}
	if got := thinking.String(); got != "let me think" {
		t.Errorf("accumulated thinking = %q, want %q", got, "let me think")
	}
	if final.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", final.FinishReason)
	}
	want := &Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 13, CacheReadTokens: 4, ThinkingTokens: 1}
	if final.Usage == nil || *final.Usage != *want {
		t.Errorf("Usage = %+v, want %+v", final.Usage, want)
	}
}

// TestBuildGrokArgs verifies the argv builder against spec §2.4 plus the
// system-prompt (--rules) and session first-vs-later branch (probe A/B).
func TestBuildGrokArgs(t *testing.T) {
	sid := deriveSessionUUID("chat-42")
	sidStr := sid.String()

	cases := []struct {
		name         string
		model        string
		prompt       string
		systemPrompt string
		permMode     string
		outputFormat string
		workDir      string
		exists       bool
		wantContains [][]string // ordered adjacent flag+value pairs that must appear
		wantAbsent   []string
	}{
		{
			name:         "first turn json with model + rules + cwd",
			model:        "grok-4.5",
			prompt:       "ping",
			systemPrompt: "be terse",
			permMode:     "bypassPermissions",
			outputFormat: "json",
			workDir:      "/tmp/ws",
			exists:       false,
			wantContains: [][]string{
				{"-p", "ping"},
				{"--permission-mode", "bypassPermissions"},
				{"--output-format", "json"},
				{"--model", "grok-4.5"},
				{"--rules", "be terse"},
				{"--cwd", "/tmp/ws"},
				{"--session-id", sidStr},
			},
			wantAbsent: []string{"--resume"},
		},
		{
			name:         "later turn resumes, streaming, no model, no rules",
			prompt:       "again",
			outputFormat: "streaming-json",
			workDir:      "/tmp/ws",
			exists:       true,
			wantContains: [][]string{
				{"-p", "again"},
				{"--output-format", "streaming-json"},
				{"--resume", sidStr},
			},
			wantAbsent: []string{"--model", "--rules", "--session-id"},
		},
		{
			name:         "empty permMode defaults to bypassPermissions",
			prompt:       "x",
			outputFormat: "json",
			exists:       false,
			wantContains: [][]string{
				{"--permission-mode", "bypassPermissions"},
			},
			wantAbsent: []string{"--cwd"}, // empty workDir omits --cwd
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := buildGrokArgs(tc.model, tc.prompt, tc.systemPrompt, tc.permMode, tc.outputFormat, tc.workDir, sid, tc.exists)
			joined := strings.Join(args, "\x00")
			for _, pair := range tc.wantContains {
				needle := strings.Join(pair, "\x00")
				if !strings.Contains(joined, needle) {
					t.Errorf("args missing adjacent %v\n got: %v", pair, args)
				}
			}
			for _, absent := range tc.wantAbsent {
				if containsArg(args, absent) {
					t.Errorf("args should not contain %q\n got: %v", absent, args)
				}
			}
			// -p must be first with the prompt immediately after (§2.4).
			if len(args) < 2 || args[0] != "-p" || args[1] != tc.prompt {
				t.Errorf("argv must start with -p <prompt>, got %v", args)
			}
			// --session-id and --resume are mutually exclusive per invocation.
			if containsArg(args, "--session-id") && containsArg(args, "--resume") {
				t.Errorf("--session-id and --resume must be mutually exclusive, got %v", args)
			}
		})
	}
}

// TestGrokSessionUUIDReuse verifies the provider derives a stable session UUID
// from the session key (shared with claude_cli) so turn-1 --session-id and
// later --resume target the same session (probe B).
func TestGrokSessionUUIDReuse(t *testing.T) {
	a := deriveSessionUUID("group:99")
	b := deriveSessionUUID("group:99")
	if a != b {
		t.Fatalf("deriveSessionUUID not stable: %s != %s", a, b)
	}
	if a.String() == "" || a.Version() != 5 {
		t.Fatalf("expected a valid RFC-4122 v5 UUID, got %s (v%d)", a, a.Version())
	}
	if deriveSessionUUID("group:100") == a {
		t.Fatal("distinct session keys must derive distinct UUIDs")
	}

	// First turn (no on-disk session) → --session-id; later turn → --resume.
	first := buildGrokArgs("grok-4.5", "p", "", "bypassPermissions", "json", "/tmp/x", a, false)
	later := buildGrokArgs("grok-4.5", "p", "", "bypassPermissions", "json", "/tmp/x", a, true)
	if !containsArgValue(first, "--session-id", a.String()) {
		t.Errorf("first turn should pass --session-id %s, got %v", a, first)
	}
	if !containsArgValue(later, "--resume", a.String()) {
		t.Errorf("later turn should pass --resume %s, got %v", a, later)
	}
}

// TestGrokEncodeCWD verifies the per-cwd session dir percent-encoding used by
// grokSessionExists ('/' → %2F, unreserved chars preserved).
func TestGrokEncodeCWD(t *testing.T) {
	cases := map[string]string{
		"/tmp/ws":         "%2Ftmp%2Fws",
		"/a-b_c.d~e":      "%2Fa-b_c.d~e",
		"/space here":     "%2Fspace%20here",
	}
	for in, want := range cases {
		if got := grokEncodeCWD(in); got != want {
			t.Errorf("grokEncodeCWD(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGrokProviderBasics sanity-checks the provider constructor + capabilities.
func TestGrokProviderBasics(t *testing.T) {
	p := NewGrokCLIProvider("", WithGrokCLIName("grok-test"), WithGrokCLIModel("grok-4.5"))
	if p.Name() != "grok-test" {
		t.Errorf("Name = %q, want grok-test", p.Name())
	}
	if p.DefaultModel() != "grok-4.5" {
		t.Errorf("DefaultModel = %q, want grok-4.5", p.DefaultModel())
	}
	if p.cliPath != "grok" {
		t.Errorf("cliPath default = %q, want grok", p.cliPath)
	}
	caps := p.Capabilities()
	if !caps.Streaming || !caps.ToolCalling || !caps.StreamWithTools || !caps.Thinking {
		t.Errorf("expected streaming/toolcalling/streamwithtools/thinking all true, got %+v", caps)
	}
	if caps.Vision {
		t.Error("Vision must be false for this pass")
	}
	if caps.TokenizerID != "o200k_base" {
		t.Errorf("TokenizerID = %q, want o200k_base", caps.TokenizerID)
	}
	if caps.MaxContextWindow != grokCLIMaxContextWindow {
		t.Errorf("MaxContextWindow = %d, want %d", caps.MaxContextWindow, grokCLIMaxContextWindow)
	}
	if err := p.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}

// containsArg reports whether flag appears as a standalone argv element.
func containsArg(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// containsArgValue reports whether flag appears immediately followed by value.
func containsArgValue(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestGrokLoginLine(t *testing.T) {
	cases := []struct {
		name        string
		out         string
		wantLogged  bool
		wantAccount string
	}{
		{"logged-in", "You are logged in with grok.com.\n\nDefault model: grok-4.5\n", true, "grok.com"},
		{"logged-in-no-period", "You are logged in with grok.com\n", true, "grok.com"},
		{"signed-out", "Not logged in. Run `grok login`.\n", false, ""},
		{"empty", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := grokLoginLine.FindStringSubmatch(tc.out)
			gotLogged := m != nil
			if gotLogged != tc.wantLogged {
				t.Fatalf("loggedIn = %v, want %v (match=%v)", gotLogged, tc.wantLogged, m)
			}
			if gotLogged && m[1] != tc.wantAccount {
				t.Fatalf("account = %q, want %q", m[1], tc.wantAccount)
			}
		})
	}
}
