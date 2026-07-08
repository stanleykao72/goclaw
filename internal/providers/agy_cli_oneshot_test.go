package providers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/providers/agycli"
)

// fakePrintRunner fakes agycli.RunPrint. The first call for a fresh
// conversation (opts.Conversation == "") mints conv-<n>; every call is
// recorded with its opts and env.
type fakePrintRunner struct {
	mu       sync.Mutex
	calls    []agycli.PrintOptions
	envs     [][]string
	answers  []string // popped per call; last one repeats when exhausted
	timedOut bool     // when true, every result reports TimedOut
	nextConv int
}

func (f *fakePrintRunner) run(_ context.Context, _ string, opts agycli.PrintOptions, extraEnv []string) (agycli.PrintResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, opts)
	f.envs = append(f.envs, extraEnv)
	answer := "ok"
	if len(f.answers) > 0 {
		answer = f.answers[0]
		if len(f.answers) > 1 {
			f.answers = f.answers[1:]
		}
	}
	res := agycli.PrintResult{Stdout: answer + "\n", ExitCode: 0, TimedOut: f.timedOut}
	if opts.Conversation == "" {
		f.nextConv++
		res.ConversationID = fmt.Sprintf("conv-%d", f.nextConv)
	}
	return res, nil
}

func (f *fakePrintRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakePrintRunner) call(i int) agycli.PrintOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

func (f *fakePrintRunner) env(i int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.envs[i]
}

func newOneShotProvider(t *testing.T, fr *fakePrintRunner, opts ...AgyCLIOption) *AgyCLIProvider {
	t.Helper()
	base := []AgyCLIOption{
		WithAgyCLIOneShot(true),
		WithAgyCLIWorkDir(t.TempDir()),
		withAgyCLIRunPrint(fr.run),
		withAgyCLISessionFactory(func(context.Context, agycli.SessionOptions) (agySession, error) {
			t.Fatal("interactive session factory must not be used in one-shot mode")
			return nil, nil
		}),
	}
	p := NewAgyCLIProvider("agy", append(base, opts...)...)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func oneShotReq(sessionKey, system, user string) ChatRequest {
	msgs := []Message{}
	if system != "" {
		msgs = append(msgs, Message{Role: "system", Content: system})
	}
	msgs = append(msgs, Message{Role: "user", Content: user})
	return ChatRequest{
		Messages: msgs,
		Options:  map[string]any{OptSessionKey: sessionKey},
	}
}

func TestAgyCLI_OneShot_SeedThenTurn(t *testing.T) {
	fr := &fakePrintRunner{answers: []string{"OK", "hello there"}}
	p := newOneShotProvider(t, fr)

	resp, err := p.Chat(context.Background(), oneShotReq("s1", "Reply in pirate voice.", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	if fr.count() != 2 {
		t.Fatalf("runPrint calls = %d, want 2 (seed + turn)", fr.count())
	}
	seed := fr.call(0)
	if seed.Conversation != "" {
		t.Fatal("seed turn must start a fresh conversation")
	}
	if !strings.Contains(seed.Prompt, "SYSTEM INSTRUCTIONS") ||
		!strings.Contains(seed.Prompt, "Reply in pirate voice.") ||
		!strings.Contains(seed.Prompt, "TEXT-ONLY messaging channel") {
		t.Fatalf("seed prompt missing framing/system prompt/channel directive:\n%s", seed.Prompt)
	}
	turn := fr.call(1)
	if turn.Conversation != "conv-1" {
		t.Fatalf("user turn conversation = %q, want conv-1 (seed's)", turn.Conversation)
	}
	if turn.Prompt != "hi" {
		t.Fatalf("user turn prompt = %q — the system prompt must not be folded into turns", turn.Prompt)
	}
	// The seed answer is discarded; the response carries the user turn's answer.
	if resp.Content != "hello there" {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("finish reason = %q", resp.FinishReason)
	}
}

func TestAgyCLI_OneShot_SessionReuseThreadsConversation(t *testing.T) {
	fr := &fakePrintRunner{}
	p := newOneShotProvider(t, fr)

	ctx := context.Background()
	if _, err := p.Chat(ctx, oneShotReq("s1", "sys", "one")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(ctx, oneShotReq("s1", "sys", "two")); err != nil {
		t.Fatal(err)
	}
	// seed + turn1 + turn2 — NO second seed on reuse.
	if fr.count() != 3 {
		t.Fatalf("runPrint calls = %d, want 3", fr.count())
	}
	if got := fr.call(2); got.Conversation != "conv-1" || got.Prompt != "two" {
		t.Fatalf("turn2 = %+v, want conv-1/two", got)
	}
}

func TestAgyCLI_OneShot_DistinctKeysGetDistinctConversations(t *testing.T) {
	fr := &fakePrintRunner{}
	p := newOneShotProvider(t, fr)

	ctx := context.Background()
	if _, err := p.Chat(ctx, oneShotReq("a", "", "hi")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(ctx, oneShotReq("b", "", "hi")); err != nil {
		t.Fatal(err)
	}
	convs := map[string]bool{}
	for i := 0; i < fr.count(); i++ {
		if c := fr.call(i).Conversation; c != "" {
			convs[c] = true
		}
	}
	if len(convs) != 2 {
		t.Fatalf("distinct conversations = %v, want 2", convs)
	}
}

func TestAgyCLI_OneShot_TimedOutTurnMapsToErrorAndNoRetry(t *testing.T) {
	fr := &fakePrintRunner{}
	p := newOneShotProvider(t, fr)
	ctx := context.Background()
	if _, err := p.Chat(ctx, oneShotReq("s1", "", "warmup")); err != nil {
		t.Fatal(err)
	}
	before := fr.count()

	fr.mu.Lock()
	fr.timedOut = true
	fr.mu.Unlock()
	resp, err := p.Chat(ctx, oneShotReq("s1", "", "slow one"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "error" {
		t.Fatalf("finish reason = %q, want error", resp.FinishReason)
	}
	// Exactly ONE additional run: a timed-out turn must never be retried
	// (it may have completed server-side — double-execution hazard).
	if fr.count() != before+1 {
		t.Fatalf("runPrint calls = %d, want %d (no retry)", fr.count(), before+1)
	}
}

func TestAgyCLI_OneShot_SeedTimeoutFailsCreateAndCleansUp(t *testing.T) {
	fr := &fakePrintRunner{timedOut: true}
	bl := &fakeBridgeListeners{}
	realGem := newFakeRealGemini(t)
	p := newOneShotProvider(t, fr,
		WithAgyCLIBridge(bl, "tok"),
		withAgyCLIRealGeminiDir(func() (string, error) { return realGem, nil }),
	)

	_, err := p.Chat(context.Background(), bridgeChatReq("s1", "hi"))
	if err == nil {
		t.Fatal("expected seed-timeout error")
	}
	if bl.closes() != 1 {
		t.Fatalf("bridge closer calls = %d, want 1 (cleanup on failed create)", bl.closes())
	}
	p.mu.Lock()
	n := len(p.sessions)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("failed create must not register a session, got %d", n)
	}
}

// newFakeRealGemini mirrors the agycli fakehome test fixture without importing
// its test package: auth-ish files + a config dir with one operator server.
func newFakeRealGemini(t *testing.T) string {
	t.Helper()
	real := filepath.Join(t.TempDir(), "real-gemini")
	for _, d := range []string{"antigravity-cli", "config"} {
		if err := os.MkdirAll(filepath.Join(real, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(real, "oauth_creds.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := `{"mcpServers":{"operator-odoo":{"command":"/bin/odoo-mcp"}}}`
	if err := os.WriteFile(filepath.Join(real, "config", "mcp_config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return real
}

func TestAgyCLI_OneShot_BridgeBuildsIsolatedFakeHome(t *testing.T) {
	fr := &fakePrintRunner{}
	bl := &fakeBridgeListeners{url: "http://127.0.0.1:40001/mcp/bridge"}
	realGem := newFakeRealGemini(t)
	p := newOneShotProvider(t, fr,
		WithAgyCLIBridge(bl, "tok-abc"),
		withAgyCLIRealGeminiDir(func() (string, error) { return realGem, nil }),
	)

	if _, err := p.Chat(context.Background(), bridgeChatReq("s1", "hi")); err != nil {
		t.Fatal(err)
	}

	// Every run of this session (seed + turn) carries the SAME fake-home HOME.
	if fr.count() != 2 {
		t.Fatalf("runPrint calls = %d", fr.count())
	}
	env0, env1 := fr.env(0), fr.env(1)
	if len(env0) != 1 || !strings.HasPrefix(env0[0], "HOME=") {
		t.Fatalf("seed env = %v, want a single HOME override", env0)
	}
	if len(env1) != 1 || env1[0] != env0[0] {
		t.Fatalf("turn env %v differs from seed env %v", env1, env0)
	}
	fakeHome := strings.TrimPrefix(env0[0], "HOME=")

	// The isolated config holds the bridge entry merged over the operator's.
	cfgPath := filepath.Join(fakeHome, ".gemini", "config", "mcp_config.json")
	assertBridgeConfigOnDisk(t, cfgPath, "http://127.0.0.1:40001/mcp/bridge", "Bearer tok-abc")

	// The REAL global config never learns about the bridge (O1: no global write).
	raw, err := os.ReadFile(filepath.Join(realGem, "config", "mcp_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "goclaw-bridge") {
		t.Fatal("bridge entry leaked into the real global config")
	}

	// Close removes the fake home and runs the bridge closer.
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fakeHome); !os.IsNotExist(err) {
		t.Fatalf("fake home %s not removed on close (err=%v)", fakeHome, err)
	}
	if bl.closes() != 1 {
		t.Fatalf("bridge closer calls = %d", bl.closes())
	}
}

func TestAgyCLI_OneShot_ReapRemovesFakeHome(t *testing.T) {
	fr := &fakePrintRunner{}
	bl := &fakeBridgeListeners{}
	realGem := newFakeRealGemini(t)
	p := newOneShotProvider(t, fr,
		WithAgyCLIBridge(bl, "tok"),
		withAgyCLIRealGeminiDir(func() (string, error) { return realGem, nil }),
		WithAgyCLIIdleTTL(time.Minute),
	)

	if _, err := p.Chat(context.Background(), bridgeChatReq("s1", "hi")); err != nil {
		t.Fatal(err)
	}
	fakeHome := strings.TrimPrefix(fr.env(0)[0], "HOME=")

	p.reapIdle(timeFarFuture())
	if _, err := os.Stat(fakeHome); !os.IsNotExist(err) {
		t.Fatalf("fake home not removed on reap (err=%v)", err)
	}
	if bl.closes() != 1 {
		t.Fatalf("bridge closer calls = %d", bl.closes())
	}
}

func TestAgyCLI_OneShot_EphemeralSeedsAndCleansUp(t *testing.T) {
	fr := &fakePrintRunner{answers: []string{"OK", "answer"}}
	bl := &fakeBridgeListeners{}
	realGem := newFakeRealGemini(t)
	p := newOneShotProvider(t, fr,
		WithAgyCLIBridge(bl, "tok"),
		withAgyCLIRealGeminiDir(func() (string, error) { return realGem, nil }),
	)

	resp, err := p.Chat(context.Background(), bridgeChatReq("", "one-off"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "answer" {
		t.Fatalf("content = %q", resp.Content)
	}
	if fr.count() != 2 {
		t.Fatalf("runPrint calls = %d, want 2 (seed + turn)", fr.count())
	}
	if bl.closes() != 1 {
		t.Fatalf("ephemeral bridge closer calls = %d", bl.closes())
	}
	fakeHome := strings.TrimPrefix(fr.env(0)[0], "HOME=")
	if _, err := os.Stat(fakeHome); !os.IsNotExist(err) {
		t.Fatalf("ephemeral fake home not removed (err=%v)", err)
	}
	p.mu.Lock()
	n := len(p.sessions)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("ephemeral call must not register a session, got %d", n)
	}
}

func TestAgyCLI_OneShot_NoBridgeNoFakeHome(t *testing.T) {
	fr := &fakePrintRunner{}
	p := newOneShotProvider(t, fr)
	if _, err := p.Chat(context.Background(), oneShotReq("s1", "", "hi")); err != nil {
		t.Fatal(err)
	}
	if env := fr.env(0); len(env) != 0 {
		t.Fatalf("no-bridge runs must not override env, got %v", env)
	}
}

func TestAgyCLI_OneShot_ConcurrentSessionsIsolated(t *testing.T) {
	fr := &fakePrintRunner{}
	bl := &fakeBridgeListeners{}
	realGem := newFakeRealGemini(t)
	p := newOneShotProvider(t, fr,
		WithAgyCLIBridge(bl, "tok"),
		withAgyCLIRealGeminiDir(func() (string, error) { return realGem, nil }),
	)

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("s%d", i%4) // 4 sessions, 2 turns each
			if _, err := p.Chat(context.Background(), bridgeChatReq(key, "msg")); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// Each distinct session key gets its own HOME; turns of the same key share it.
	homesByConv := map[string]map[string]bool{}
	for i := 0; i < fr.count(); i++ {
		opts, env := fr.call(i), fr.env(i)
		if len(env) != 1 {
			t.Fatalf("call %d env = %v", i, env)
		}
		key := opts.Conversation // "" for seeds
		if homesByConv[key] == nil {
			homesByConv[key] = map[string]bool{}
		}
		homesByConv[key][env[0]] = true
	}
	for conv, homes := range homesByConv {
		if conv == "" {
			continue // seeds: one home each, distinct by construction
		}
		if len(homes) != 1 {
			t.Fatalf("conversation %s used %d homes, want 1: %v", conv, len(homes), homes)
		}
	}
}
