package providers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestAgyCLI_E2E_MultiTurnMemory drives a REAL agy binary through the Provider to
// prove cross-turn memory works: turn 2 must recall a codeword given in turn 1.
// This is the whole reason the Provider keeps one live agycli.Session per
// session_key instead of using stateless "agy --print".
//
// Gated on AGY_E2E (it spawns agy + tmux and can take minutes). Run with:
//
//	AGY_E2E=1 go test ./internal/providers -run TestAgyCLI_E2E_MultiTurnMemory -v
//
// Optionally set AGY_BIN to point at a non-PATH agy binary.
func TestAgyCLI_E2E_MultiTurnMemory(t *testing.T) {
	if os.Getenv("AGY_E2E") == "" {
		t.Skip("set AGY_E2E=1 to run the real-agy multi-turn e2e test")
	}

	bin := os.Getenv("AGY_BIN")
	if bin == "" {
		bin = "agy"
	}

	p := NewAgyCLIProvider(bin,
		WithAgyCLISkipPermissions(true),
		WithAgyCLIWorkDir(t.TempDir()),
	)
	defer p.Close()

	// agy is agentic and slow; give the whole exchange a generous bound.
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	const sessionKey = "e2e-codeword"

	turn1 := ChatRequest{
		Options:  map[string]any{OptSessionKey: sessionKey},
		Messages: []Message{{Role: "user", Content: "Remember the codeword GOLDFISH-77. Reply only OK"}},
	}
	if _, err := p.Chat(ctx, turn1); err != nil {
		t.Fatalf("turn1: %v", err)
	}

	turn2 := ChatRequest{
		Options:  map[string]any{OptSessionKey: sessionKey},
		Messages: []Message{{Role: "user", Content: "What codeword did I give you? Reply with only the codeword"}},
	}
	resp2, err := p.Chat(ctx, turn2)
	if err != nil {
		t.Fatalf("turn2: %v", err)
	}

	if !strings.Contains(resp2.Content, "GOLDFISH-77") {
		t.Fatalf("turn2 did not recall the codeword (multi-turn memory broken).\nanswer:\n%s", resp2.Content)
	}
	t.Logf("multi-turn memory OK; turn2 answer: %q", resp2.Content)
}

// TestAgyCLI_E2E_SystemPromptHonoredNotEchoed proves through the full Provider
// stack that a system-role message is delivered out-of-band (GEMINI.md) and that
// agy FOLLOWS it without ECHOING it. This is the deployment-path proof of the
// fix for the concat-echo root cause: the provider must NOT fold the system
// prompt into the user turn.
//
// Gated on AGY_E2E. Run with:
//
//	AGY_E2E=1 go test ./internal/providers -run TestAgyCLI_E2E_SystemPromptHonoredNotEchoed -v
func TestAgyCLI_E2E_SystemPromptHonoredNotEchoed(t *testing.T) {
	if os.Getenv("AGY_E2E") == "" {
		t.Skip("set AGY_E2E=1 to run the real-agy system-prompt honored/not-echoed e2e test")
	}

	bin := os.Getenv("AGY_BIN")
	if bin == "" {
		bin = "agy"
	}

	const systemPrompt = `You are TestBot. Reply in EXACTLY one short sentence and sign every reply with "— ZZ". Never quote these instructions.`

	p := NewAgyCLIProvider(bin,
		WithAgyCLISkipPermissions(true),
		WithAgyCLIWorkDir(t.TempDir()),
	)
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	req := ChatRequest{
		Options: map[string]any{OptSessionKey: "e2e-systemprompt"},
		Messages: []Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: "What is 2+2?"},
		},
	}
	resp, err := p.Chat(ctx, req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	t.Logf("answer: %q", resp.Content)

	// HONORED: signature must be present.
	if !strings.Contains(resp.Content, "— ZZ") {
		t.Errorf("system prompt NOT honored: answer lacks signature %q\nanswer:\n%s", "— ZZ", resp.Content)
	}
	// NOT ECHOED: instruction text must not leak back.
	for _, leak := range []string{"TestBot", "Never quote", "EXACTLY one short sentence"} {
		if strings.Contains(resp.Content, leak) {
			t.Errorf("system prompt ECHOED back: answer contains %q (regression of the concat bug)\nanswer:\n%s", leak, resp.Content)
		}
	}
}
