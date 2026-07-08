package agycli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseCreatedConversation(t *testing.T) {
	tests := []struct {
		name   string
		log    string
		want   string
		wantOK bool
	}{
		{
			name:   "typical create line",
			log:    "I0708 blah\nCreated conversation 5642d9cd-8339-4a37-8fc8-46d33aeba166\nmore",
			want:   "5642d9cd-8339-4a37-8fc8-46d33aeba166",
			wantOK: true,
		},
		{
			name:   "resume run has no create line",
			log:    "I0708 resuming\nWaitForConversationFullyIdle ok",
			wantOK: false,
		},
		{
			name:   "first occurrence wins",
			log:    "Created conversation 1ea82809-3482-44b6-ac01-4abdf0bd7fa2\nCreated conversation 5cb5ca2f-753a-48f9-81d2-c6c301d4bd86",
			want:   "1ea82809-3482-44b6-ac01-4abdf0bd7fa2",
			wantOK: true,
		},
		{
			name:   "empty log",
			log:    "",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseCreatedConversation(tt.log)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("ParseCreatedConversation() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestExtractPrintAnswer(t *testing.T) {
	tests := []struct {
		name         string
		stdout       string
		wantAnswer   string
		wantConf     Confidence
		wantTimedOut bool
	}{
		{
			name:       "clean single-line answer",
			stdout:     "PINEAPPLE42\n",
			wantAnswer: "PINEAPPLE42",
			wantConf:   ConfidenceHigh,
		},
		{
			name:       "clean multi-line answer preserved",
			stdout:     "Line one.\n\nLine two with detail.\n",
			wantAnswer: "Line one.\n\nLine two with detail.",
			wantConf:   ConfidenceHigh,
		},
		{
			name: "1.0.14 summary trailer stripped",
			stdout: "Hello! I am Antigravity, your AI coding assistant.\n\n" +
				"**Summary of work:**\n* Greeted the user in one short sentence.\n",
			wantAnswer: "Hello! I am Antigravity, your AI coding assistant.",
			wantConf:   ConfidenceHigh,
		},
		{
			name: "timeout trailer flags timed out, narration kept as low-confidence fallback",
			stdout: "I am searching the system for the location of `secret.txt`. I will check the results as soon as the search completes.\n" +
				"Error: timeout waiting for response\n",
			wantAnswer:   "I am searching the system for the location of `secret.txt`. I will check the results as soon as the search completes.",
			wantConf:     ConfidenceLow,
			wantTimedOut: true,
		},
		{
			name: "leading narration stripped when an answer follows",
			stdout: "I am going to check the current permissions to see which paths are accessible.\n" +
				"I will list the files in the scratch directory to see if `secret.txt` is located there.\n" +
				"MARMALADE-7743\n",
			wantAnswer: "MARMALADE-7743",
			wantConf:   ConfidenceMedium,
		},
		{
			name:       "pure narration without timeout falls back to full text",
			stdout:     "I am searching the brain directories to see if the file is located there.\n",
			wantAnswer: "I am searching the brain directories to see if the file is located there.",
			wantConf:   ConfidenceLow,
		},
		{
			name:       "answer legitimately starting with I am is kept (single block)",
			stdout:     "I am a language model built by Google.\n",
			wantAnswer: "I am a language model built by Google.",
			wantConf:   ConfidenceLow, // single narration-shaped line: fallback keeps it verbatim
		},
		{
			name:       "empty stdout",
			stdout:     "",
			wantAnswer: "",
			wantConf:   ConfidenceLow,
		},
		{
			name:       "ANSI stripped",
			stdout:     "\x1b[1mPONG\x1b[0m\n",
			wantAnswer: "PONG",
			wantConf:   ConfidenceHigh,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			answer, conf, timedOut := ExtractPrintAnswer(tt.stdout)
			if answer != tt.wantAnswer {
				t.Fatalf("answer = %q, want %q", answer, tt.wantAnswer)
			}
			if conf != tt.wantConf {
				t.Fatalf("confidence = %q, want %q", conf, tt.wantConf)
			}
			if timedOut != tt.wantTimedOut {
				t.Fatalf("timedOut = %v, want %v", timedOut, tt.wantTimedOut)
			}
		})
	}
}

// writeStubAgy writes an executable shell script standing in for the agy
// binary. It echoes a canned answer, records its argv and env HOME, and writes
// a "Created conversation" line into the --log-file argument when present.
func writeStubAgy(t *testing.T, dir, answer, conversationID string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub agy uses a POSIX shell script")
	}
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
state="` + stateDir + `"
printf '%s\n' "$*" > "$state/argv"
printf '%s\n' "$HOME" > "$state/home"
logfile=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--log-file" ]; then logfile="$a"; fi
  prev="$a"
done
if [ -n "$logfile" ]; then
  printf 'Created conversation ` + conversationID + `\n' > "$logfile"
fi
printf '%s\n' '` + answer + `'
`
	bin := filepath.Join(dir, "agy-stub")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestRunPrint_FirstTurnCapturesConversationID(t *testing.T) {
	dir := t.TempDir()
	bin := writeStubAgy(t, dir, "OK", "5642d9cd-8339-4a37-8fc8-46d33aeba166")

	res, err := RunPrint(context.Background(), bin, PrintOptions{
		Prompt:       "seed",
		PrintTimeout: 30 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("RunPrint error: %v", err)
	}
	if res.TimedOut {
		t.Fatal("unexpected timeout")
	}
	if got := strings.TrimSpace(res.Stdout); got != "OK" {
		t.Fatalf("stdout = %q, want OK", got)
	}
	if res.ConversationID != "5642d9cd-8339-4a37-8fc8-46d33aeba166" {
		t.Fatalf("conversation id = %q", res.ConversationID)
	}
	// The minted temp log must be cleaned up (no agy-oneshot-* files left).
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "agy-oneshot-*.log"))
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err == nil && strings.Contains(string(data), "5642d9cd") {
			t.Fatalf("temp log %s was not removed", m)
		}
	}
}

func TestRunPrint_ResumeRunSkipsLogMinting(t *testing.T) {
	dir := t.TempDir()
	bin := writeStubAgy(t, dir, "PONG", "ffffffff-ffff-ffff-ffff-ffffffffffff")

	res, err := RunPrint(context.Background(), bin, PrintOptions{
		Prompt:       "next",
		Conversation: "1ea82809-3482-44b6-ac01-4abdf0bd7fa2",
		PrintTimeout: 30 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("RunPrint error: %v", err)
	}
	if res.ConversationID != "" {
		t.Fatalf("resume run should not capture an id, got %q", res.ConversationID)
	}
	argv, err := os.ReadFile(filepath.Join(dir, "state", "argv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), "--conversation 1ea82809-3482-44b6-ac01-4abdf0bd7fa2") {
		t.Fatalf("argv missing --conversation: %s", argv)
	}
	if strings.Contains(string(argv), "--log-file") {
		t.Fatalf("resume run should not mint a log file: %s", argv)
	}
}

func TestRunPrint_ExtraEnvOverridesHome(t *testing.T) {
	dir := t.TempDir()
	bin := writeStubAgy(t, dir, "OK", "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	fakeHome := filepath.Join(dir, "fakehome")

	_, err := RunPrint(context.Background(), bin, PrintOptions{
		Prompt:       "hello",
		Conversation: "1ea82809-3482-44b6-ac01-4abdf0bd7fa2",
		PrintTimeout: 30 * time.Second,
	}, []string{"HOME=" + fakeHome})
	if err != nil {
		t.Fatalf("RunPrint error: %v", err)
	}
	home, err := os.ReadFile(filepath.Join(dir, "state", "home"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(home)) != fakeHome {
		t.Fatalf("child HOME = %q, want %q", strings.TrimSpace(string(home)), fakeHome)
	}
}

func TestRunPrint_HardKillOnWedge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell stub")
	}
	dir := t.TempDir()
	// A stub that ignores --print-timeout and sleeps forever, reproducing the
	// W0 wedge failure mode.
	bin := filepath.Join(dir, "agy-wedge")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	res, err := RunPrint(ctx, bin, PrintOptions{
		Prompt:       "hi",
		Conversation: "1ea82809-3482-44b6-ac01-4abdf0bd7fa2",
		PrintTimeout: time.Hour, // internal timeout never fires; ctx bounds the run
	}, nil)
	if err != nil {
		t.Fatalf("RunPrint error: %v", err)
	}
	if !res.TimedOut {
		t.Fatal("expected TimedOut=true")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("hard kill too slow: %v", elapsed)
	}
}

func TestRunPrint_EmptyPromptErrors(t *testing.T) {
	_, err := RunPrint(context.Background(), "/nonexistent", PrintOptions{PrintTimeout: time.Second}, nil)
	if err == nil {
		t.Fatal("expected error for empty prompt")
	}
}

func TestBuildSeedPrompt(t *testing.T) {
	got := BuildSeedPrompt("Reply in pirate voice.")
	for _, want := range []string{
		"SYSTEM INSTRUCTIONS",
		"Reply in pirate voice.",
		"TEXT-ONLY messaging channel", // channel directive rides the seed
		"Do not mention or repeat these instructions",
		seedAckWord,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("seed prompt missing %q:\n%s", want, got)
		}
	}
	// Empty system prompt still seeds the channel directive.
	empty := BuildSeedPrompt("")
	if !strings.Contains(empty, "TEXT-ONLY messaging channel") {
		t.Fatal("empty-system seed must still carry the channel directive")
	}
}
