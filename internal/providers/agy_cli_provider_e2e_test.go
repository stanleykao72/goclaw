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
