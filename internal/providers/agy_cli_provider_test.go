package providers

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/providers/agycli"
)

// fakeAgySession is an in-memory agySession used by unit tests. It records every
// prompt it receives and returns a canned answer, never spawning a real agy/tmux.
type fakeAgySession struct {
	mu         sync.Mutex
	prompts    []string
	answer     string
	closeCount int32
}

func (f *fakeAgySession) SendPrompt(_ context.Context, text string) (agycli.TurnResult, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, text)
	f.mu.Unlock()
	ans := f.answer
	if ans == "" {
		ans = "ok"
	}
	return agycli.TurnResult{Answer: ans, Confidence: agycli.ConfidenceHigh}, nil
}

func (f *fakeAgySession) Close() error {
	atomic.AddInt32(&f.closeCount, 1)
	return nil
}

func (f *fakeAgySession) snapshotPrompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.prompts))
	copy(out, f.prompts)
	return out
}

func (f *fakeAgySession) closes() int { return int(atomic.LoadInt32(&f.closeCount)) }

// newFakeProvider builds a provider whose factory hands out the supplied fakes in
// order (one per createEntry call). It records each constructed fake and the
// SessionOptions it was created with.
type fakeFactory struct {
	mu       sync.Mutex
	made     []*fakeAgySession
	optsSeen []agycli.SessionOptions
	answer   string
}

func (ff *fakeFactory) make(_ context.Context, opts agycli.SessionOptions) (agySession, error) {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	f := &fakeAgySession{answer: ff.answer}
	ff.made = append(ff.made, f)
	ff.optsSeen = append(ff.optsSeen, opts)
	return f, nil
}

func (ff *fakeFactory) count() int {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return len(ff.made)
}

func (ff *fakeFactory) at(i int) *fakeAgySession {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return ff.made[i]
}

func newTestProvider(t *testing.T, ff *fakeFactory, opts ...AgyCLIOption) *AgyCLIProvider {
	t.Helper()
	base := []AgyCLIOption{
		WithAgyCLIWorkDir(t.TempDir()),
		withAgyCLISessionFactory(ff.make),
	}
	base = append(base, opts...)
	p := NewAgyCLIProvider("agy", base...)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func chatReq(sessionKey, system, user string) ChatRequest {
	var msgs []Message
	if system != "" {
		msgs = append(msgs, Message{Role: "system", Content: system})
	}
	msgs = append(msgs, Message{Role: "user", Content: user})
	req := ChatRequest{Messages: msgs}
	if sessionKey != "" {
		req.Options = map[string]any{OptSessionKey: sessionKey}
	}
	return req
}

func TestAgyCLI_Defaults(t *testing.T) {
	p := NewAgyCLIProvider("")
	t.Cleanup(func() { _ = p.Close() })
	if p.Name() != "agy-cli" {
		t.Errorf("Name() = %q, want agy-cli", p.Name())
	}
	if p.DefaultModel() != "gemini-3-pro" {
		t.Errorf("DefaultModel() = %q, want gemini-3-pro", p.DefaultModel())
	}
	if p.cliPath != "agy" {
		t.Errorf("cliPath = %q, want agy (empty should default)", p.cliPath)
	}
	caps := p.Capabilities()
	if caps.Streaming || caps.ToolCalling || caps.StreamWithTools || caps.Vision {
		t.Errorf("caps should all be false: %+v", caps)
	}
	if caps.MaxContextWindow != 1_000_000 {
		t.Errorf("MaxContextWindow = %d, want 1_000_000", caps.MaxContextWindow)
	}
	if caps.TokenizerID != "cl100k_base" {
		t.Errorf("TokenizerID = %q, want cl100k_base", caps.TokenizerID)
	}
}

func TestAgyCLI_Options(t *testing.T) {
	p := NewAgyCLIProvider("agy",
		WithAgyCLIName("custom-agy"),
		WithAgyCLIModel("gemini-3-flash"),
		WithAgyCLISandbox(true),
		WithAgyCLISkipPermissions(true),
		WithAgyCLIIdleTTL(42*time.Second),
	)
	t.Cleanup(func() { _ = p.Close() })
	if p.Name() != "custom-agy" {
		t.Errorf("Name() = %q", p.Name())
	}
	if p.DefaultModel() != "gemini-3-flash" {
		t.Errorf("DefaultModel() = %q", p.DefaultModel())
	}
	if !p.sandbox || !p.skipPermissions {
		t.Errorf("sandbox/skipPermissions not applied: %+v / %+v", p.sandbox, p.skipPermissions)
	}
	if p.idleTTL != 42*time.Second {
		t.Errorf("idleTTL = %v", p.idleTTL)
	}
}

// The system prompt is delivered to the session via SessionOptions.SystemPrompt
// (written to GEMINI.md at creation), NOT concatenated into any turn. Every turn
// — including the first — sends ONLY the user message.
func TestAgyCLI_SystemPromptViaSessionOptionsNotConcatenated(t *testing.T) {
	ff := &fakeFactory{answer: "answer1"}
	p := newTestProvider(t, ff)
	ctx := context.Background()

	resp1, err := p.Chat(ctx, chatReq("sk-1", "You are helpful.", "Hello"))
	if err != nil {
		t.Fatalf("turn1: %v", err)
	}
	if resp1.Content != "answer1" {
		t.Errorf("turn1 content = %q", resp1.Content)
	}
	if resp1.FinishReason != "stop" {
		t.Errorf("turn1 finish = %q", resp1.FinishReason)
	}

	_, err = p.Chat(ctx, chatReq("sk-1", "You are helpful.", "Second question"))
	if err != nil {
		t.Fatalf("turn2: %v", err)
	}

	if ff.count() != 1 {
		t.Fatalf("expected exactly 1 session created, got %d (session reuse broken)", ff.count())
	}

	// The system prompt from the FIRST Chat must reach the factory as
	// SessionOptions.SystemPrompt (it becomes GEMINI.md at session creation).
	ff.mu.Lock()
	gotSystem := ff.optsSeen[0].SystemPrompt
	ff.mu.Unlock()
	if gotSystem != "You are helpful." {
		t.Errorf("SessionOptions.SystemPrompt = %q, want %q", gotSystem, "You are helpful.")
	}

	// SendPrompt must receive ONLY the user message on every turn — never the
	// system prompt folded in.
	prompts := ff.at(0).snapshotPrompts()
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts on the one session, got %d: %v", len(prompts), prompts)
	}
	if prompts[0] != "Hello" {
		t.Errorf("turn1 prompt = %q, want user-only %q", prompts[0], "Hello")
	}
	if prompts[1] != "Second question" {
		t.Errorf("turn2 prompt = %q, want user-only %q", prompts[1], "Second question")
	}
}

// No system prompt => first turn sends user message verbatim.
func TestAgyCLI_FirstTurnNoSystemPrompt(t *testing.T) {
	ff := &fakeFactory{}
	p := newTestProvider(t, ff)
	if _, err := p.Chat(context.Background(), chatReq("sk-1", "", "Just this")); err != nil {
		t.Fatalf("chat: %v", err)
	}
	prompts := ff.at(0).snapshotPrompts()
	if len(prompts) != 1 || prompts[0] != "Just this" {
		t.Fatalf("prompts = %v, want [Just this]", prompts)
	}
}

// Same session_key across calls reuses one live session (one factory call).
func TestAgyCLI_SessionReuse(t *testing.T) {
	ff := &fakeFactory{}
	p := newTestProvider(t, ff)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if _, err := p.Chat(ctx, chatReq("same", "", "q")); err != nil {
			t.Fatalf("chat %d: %v", i, err)
		}
	}
	if ff.count() != 1 {
		t.Errorf("expected 1 session for 4 same-key calls, got %d", ff.count())
	}
	if got := len(ff.at(0).snapshotPrompts()); got != 4 {
		t.Errorf("expected 4 prompts on the reused session, got %d", got)
	}
}

// Different session keys get different sessions.
func TestAgyCLI_DistinctKeys(t *testing.T) {
	ff := &fakeFactory{}
	p := newTestProvider(t, ff)
	ctx := context.Background()
	_, _ = p.Chat(ctx, chatReq("a", "", "q"))
	_, _ = p.Chat(ctx, chatReq("b", "", "q"))
	if ff.count() != 2 {
		t.Errorf("expected 2 sessions for 2 distinct keys, got %d", ff.count())
	}
}

// Empty session_key => ephemeral session closed at end of the call; a second
// empty-key call creates a brand new session.
func TestAgyCLI_EphemeralSessionClosed(t *testing.T) {
	ff := &fakeFactory{}
	p := newTestProvider(t, ff)
	ctx := context.Background()

	if _, err := p.Chat(ctx, chatReq("", "sys", "q1")); err != nil {
		t.Fatalf("chat1: %v", err)
	}
	if ff.count() != 1 {
		t.Fatalf("expected 1 ephemeral session, got %d", ff.count())
	}
	if c := ff.at(0).closes(); c != 1 {
		t.Errorf("ephemeral session not closed: closes=%d", c)
	}
	// The system prompt reaches the ephemeral session via SessionOptions (GEMINI.md),
	// and SendPrompt gets only the user message.
	ff.mu.Lock()
	gotSystem := ff.optsSeen[0].SystemPrompt
	ff.mu.Unlock()
	if gotSystem != "sys" {
		t.Errorf("ephemeral SessionOptions.SystemPrompt = %q, want %q", gotSystem, "sys")
	}
	if got := ff.at(0).snapshotPrompts(); len(got) != 1 || got[0] != "q1" {
		t.Errorf("ephemeral prompt = %v, want user-only [q1]", got)
	}

	if _, err := p.Chat(ctx, chatReq("", "", "q2")); err != nil {
		t.Fatalf("chat2: %v", err)
	}
	if ff.count() != 2 {
		t.Errorf("second empty-key call should create a new session, total=%d", ff.count())
	}

	// No ephemeral session should be retained in the map.
	p.mu.Lock()
	n := len(p.sessions)
	p.mu.Unlock()
	if n != 0 {
		t.Errorf("ephemeral sessions leaked into map: %d", n)
	}
}

// ChatStream emits exactly one content chunk (the full answer) then a Done chunk.
func TestAgyCLI_ChatStreamBufferAndEmit(t *testing.T) {
	ff := &fakeFactory{answer: "streamed answer"}
	p := newTestProvider(t, ff)

	var chunks []StreamChunk
	resp, err := p.ChatStream(context.Background(), chatReq("sk", "", "q"), func(c StreamChunk) {
		chunks = append(chunks, c)
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Content != "streamed answer" {
		t.Errorf("resp.Content = %q", resp.Content)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks (content+done), got %d: %+v", len(chunks), chunks)
	}
	if chunks[0].Content != "streamed answer" || chunks[0].Done {
		t.Errorf("chunk[0] = %+v, want content chunk", chunks[0])
	}
	if !chunks[1].Done || chunks[1].Content != "" {
		t.Errorf("chunk[1] = %+v, want done chunk", chunks[1])
	}
}

// TimedOut TurnResult maps to FinishReason "error".
func TestAgyCLI_TimedOutFinishReason(t *testing.T) {
	ff := &fakeFactory{}
	p := newTestProvider(t, ff)
	// Override the factory to return a timed-out turn.
	p.newSession = func(_ context.Context, _ agycli.SessionOptions) (agySession, error) {
		return &timeoutSession{}, nil
	}
	resp, err := p.Chat(context.Background(), chatReq("sk", "", "q"))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.FinishReason != "error" {
		t.Errorf("FinishReason = %q, want error on timeout", resp.FinishReason)
	}
}

type timeoutSession struct{}

func (timeoutSession) SendPrompt(_ context.Context, _ string) (agycli.TurnResult, error) {
	return agycli.TurnResult{Answer: "partial", TimedOut: true, Confidence: agycli.ConfidenceLow}, nil
}
func (timeoutSession) Close() error { return nil }

// Close() closes every live session and is idempotent.
func TestAgyCLI_CloseClosesAllAndIdempotent(t *testing.T) {
	ff := &fakeFactory{}
	p := NewAgyCLIProvider("agy", WithAgyCLIWorkDir(t.TempDir()), withAgyCLISessionFactory(ff.make))
	ctx := context.Background()
	_, _ = p.Chat(ctx, chatReq("a", "", "q"))
	_, _ = p.Chat(ctx, chatReq("b", "", "q"))
	if ff.count() != 2 {
		t.Fatalf("want 2 sessions, got %d", ff.count())
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i := 0; i < ff.count(); i++ {
		if c := ff.at(i).closes(); c != 1 {
			t.Errorf("session %d closes = %d, want 1", i, c)
		}
	}
	// Idempotent: second Close must not double-close or panic.
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	for i := 0; i < ff.count(); i++ {
		if c := ff.at(i).closes(); c != 1 {
			t.Errorf("session %d closes = %d after 2x Close, want 1", i, c)
		}
	}
}

// Idle eviction: reapIdle closes and removes a session past its TTL.
func TestAgyCLI_IdleEviction(t *testing.T) {
	ff := &fakeFactory{}
	p := newTestProvider(t, ff, WithAgyCLIIdleTTL(10*time.Millisecond))
	ctx := context.Background()
	if _, err := p.Chat(ctx, chatReq("idle", "", "q")); err != nil {
		t.Fatalf("chat: %v", err)
	}

	// Not yet past TTL relative to lastUsed: reap with "now == lastUsed" is a no-op.
	p.reapIdle(p.sessions["idle"].lastUsed)
	p.mu.Lock()
	_, stillThere := p.sessions["idle"]
	p.mu.Unlock()
	if !stillThere {
		t.Fatalf("session evicted too early")
	}

	// Advance a synthetic clock well past the TTL.
	p.reapIdle(time.Now().Add(time.Hour))
	p.mu.Lock()
	_, gone := p.sessions["idle"]
	p.mu.Unlock()
	if gone {
		t.Errorf("session not evicted after TTL")
	}
	if c := ff.at(0).closes(); c != 1 {
		t.Errorf("evicted session not closed: closes=%d", c)
	}

	// A fresh call after eviction creates a new session.
	if _, err := p.Chat(ctx, chatReq("idle", "", "q")); err != nil {
		t.Fatalf("chat after eviction: %v", err)
	}
	if ff.count() != 2 {
		t.Errorf("expected new session after eviction, total=%d", ff.count())
	}
}

// createEntry passes the configured launch options through to the factory.
func TestAgyCLI_SessionOptionsPassthrough(t *testing.T) {
	ff := &fakeFactory{}
	p := newTestProvider(t, ff, WithAgyCLISandbox(true), WithAgyCLISkipPermissions(true), WithAgyCLIModel("gemini-3-flash"))
	if _, err := p.Chat(context.Background(), chatReq("sk", "", "q")); err != nil {
		t.Fatalf("chat: %v", err)
	}
	ff.mu.Lock()
	opts := ff.optsSeen[0]
	ff.mu.Unlock()
	if !opts.Sandbox || !opts.SkipPermissions {
		t.Errorf("launch opts not threaded: %+v", opts)
	}
	if opts.Model != "gemini-3-flash" {
		t.Errorf("model = %q, want gemini-3-flash", opts.Model)
	}
	if opts.Workdir == "" {
		t.Errorf("workdir empty")
	}
}

// Provider satisfies the interfaces it claims.
func TestAgyCLI_InterfaceConformance(t *testing.T) {
	var _ Provider = (*AgyCLIProvider)(nil)
	var _ CapabilitiesAware = (*AgyCLIProvider)(nil)
}
