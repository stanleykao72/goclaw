package providers

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/providers/agycli"
)

// agy_cli_provider.go bridges goclaw's Provider interface to a persistent
// interactive agy ("Google Antigravity CLI") session.
//
// THE CRITICAL CONSTRAINT (Phase 0, agy v1.0.10): "agy --print" has NO
// cross-turn memory — every --print invocation is a fresh conversation and
// --conversation/-c do NOT replay prior context. The ONLY way to get multi-turn
// memory is to keep ONE live interactive agy process open (an agycli.Session,
// driven over tmux) and feed every subsequent turn into the SAME process.
//
// Therefore this provider keeps a map of live sessions keyed by the goclaw
// session_key (OptSessionKey). The first turn of a session may carry a system
// prompt (prepended to the user message, since agy has no separate system-prompt
// channel); every later turn sends only the user message because the live agy
// process already remembers the earlier turns. An empty session_key yields a
// per-call ephemeral session that is Closed at the end of that call.
//
// agy is NOT a token-streaming engine — it renders a TUI and we salvage the
// final answer once the turn reaches the ready footer. ChatStream therefore
// buffers the whole answer and emits it as a single chunk, then a Done chunk.

const (
	// agyCLIDefaultName is the provider identifier used as the registry key suffix.
	agyCLIDefaultName = "agy-cli"
	// agyCLIDefaultModel is the default model passed to agy at session launch.
	agyCLIDefaultModel = "gemini-3-pro"
	// agyCLIDefaultIdleTTL is how long a live session may sit unused before the
	// background reaper closes it. agy holds a real process + tmux session, so we
	// reap idle ones to bound resource use.
	agyCLIDefaultIdleTTL = 15 * time.Minute
	// agyCLIDefaultBinary is the fallback binary name when cliPath is empty.
	agyCLIDefaultBinary = "agy"
	// agyCLIReapInterval is how often the background reaper scans for idle sessions.
	agyCLIReapInterval = 1 * time.Minute
	// agyCLIMaxContextWindow is the advertised default context window for agy/gemini.
	agyCLIMaxContextWindow = 1_000_000
	// agyCLITokenizerID maps to the tokencount package for rough sizing.
	agyCLITokenizerID = "cl100k_base"
)

// agySession is the minimal slice of *agycli.Session this provider drives. It is
// an interface purely so unit tests can inject a fake that records prompts and
// returns canned TurnResults without spawning a real agy process / tmux.
type agySession interface {
	SendPrompt(ctx context.Context, text string) (agycli.TurnResult, error)
	Close() error
}

// sessionEntry holds one live agy session plus its bookkeeping. mu serializes
// concurrent turns on the SAME session (overlapping send-keys would interleave);
// lastUsed drives idle eviction; firstTurnDone gates the one-time system-prompt
// prepend.
type sessionEntry struct {
	sess          agySession
	mu            sync.Mutex
	lastUsed      time.Time
	firstTurnDone bool
}

// AgyCLIProvider implements Provider by driving persistent interactive agy
// sessions (one per goclaw session_key) so multi-turn memory works.
type AgyCLIProvider struct {
	name            string // provider name (default: "agy-cli")
	cliPath         string // path to agy binary (default: "agy")
	defaultModel    string // default model alias (default: "gemini-3-pro")
	baseWorkDir     string // base dir for per-session agy workspaces
	sandbox         bool   // launch agy with --sandbox
	skipPermissions bool   // launch agy with --dangerously-skip-permissions
	idleTTL         time.Duration

	mu       sync.Mutex // protects the sessions map + workdir creation
	sessions map[string]*sessionEntry

	// newSession is the session factory, injectable for tests. The default wraps
	// agycli.NewSession (which takes binary as its 2nd arg) by closing over cliPath.
	newSession func(ctx context.Context, opts agycli.SessionOptions) (agySession, error)

	closed    chan struct{} // closed by Close() to stop the reaper
	closeOnce sync.Once     // makes Close idempotent
}

// AgyCLIOption configures the provider.
type AgyCLIOption func(*AgyCLIProvider)

// WithAgyCLIName overrides the provider name (default: "agy-cli").
func WithAgyCLIName(name string) AgyCLIOption {
	return func(p *AgyCLIProvider) {
		if name != "" {
			p.name = name
		}
	}
}

// WithAgyCLIModel sets the default model alias.
func WithAgyCLIModel(model string) AgyCLIOption {
	return func(p *AgyCLIProvider) {
		if model != "" {
			p.defaultModel = model
		}
	}
}

// WithAgyCLIWorkDir sets the base work directory for per-session workspaces.
func WithAgyCLIWorkDir(dir string) AgyCLIOption {
	return func(p *AgyCLIProvider) {
		if dir != "" {
			p.baseWorkDir = dir
		}
	}
}

// WithAgyCLISandbox toggles launching agy with --sandbox.
func WithAgyCLISandbox(v bool) AgyCLIOption {
	return func(p *AgyCLIProvider) {
		p.sandbox = v
	}
}

// WithAgyCLISkipPermissions toggles launching agy with --dangerously-skip-permissions.
func WithAgyCLISkipPermissions(v bool) AgyCLIOption {
	return func(p *AgyCLIProvider) {
		p.skipPermissions = v
	}
}

// WithAgyCLIIdleTTL sets how long an idle live session is kept before the reaper
// closes it. Non-positive values are ignored (the default is retained).
func WithAgyCLIIdleTTL(d time.Duration) AgyCLIOption {
	return func(p *AgyCLIProvider) {
		if d > 0 {
			p.idleTTL = d
		}
	}
}

// withAgyCLISessionFactory injects a custom session factory (test-only seam).
func withAgyCLISessionFactory(f func(ctx context.Context, opts agycli.SessionOptions) (agySession, error)) AgyCLIOption {
	return func(p *AgyCLIProvider) {
		if f != nil {
			p.newSession = f
		}
	}
}

// NewAgyCLIProvider creates a provider that drives persistent interactive agy
// sessions. cliPath == "" defaults to "agy".
func NewAgyCLIProvider(cliPath string, opts ...AgyCLIOption) *AgyCLIProvider {
	if cliPath == "" {
		cliPath = agyCLIDefaultBinary
	}
	p := &AgyCLIProvider{
		name:         agyCLIDefaultName,
		cliPath:      cliPath,
		defaultModel: agyCLIDefaultModel,
		baseWorkDir:  defaultCLIWorkDir(),
		idleTTL:      agyCLIDefaultIdleTTL,
		sessions:     make(map[string]*sessionEntry),
		closed:       make(chan struct{}),
	}
	// Default factory: wrap agycli.NewSession, closing over the resolved binary.
	p.newSession = func(ctx context.Context, so agycli.SessionOptions) (agySession, error) {
		return agycli.NewSession(ctx, p.cliPath, so)
	}
	for _, opt := range opts {
		opt(p)
	}
	go p.reapLoop()
	return p
}

func (p *AgyCLIProvider) Name() string         { return p.name }
func (p *AgyCLIProvider) DefaultModel() string { return p.defaultModel }

// Capabilities implements CapabilitiesAware. agy is a TUI-driven coding agent:
// it does not token-stream, does not surface tool calls to us as structured
// ToolCalls, and we do not feed it images. So everything is false except the
// context-window / tokenizer hints.
func (p *AgyCLIProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Streaming:        false,
		ToolCalling:      false,
		StreamWithTools:  false,
		Thinking:         false,
		Vision:           false,
		CacheControl:     false,
		MaxContextWindow: agyCLIMaxContextWindow,
		TokenizerID:      agyCLITokenizerID,
	}
}

// Chat runs one turn against the agy session for the request's session_key and
// returns the final response. See the file header for the multi-turn model.
func (p *AgyCLIProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	return p.runTurn(ctx, req, nil)
}

// ChatStream runs one turn like Chat, but emits the (already-complete) answer as
// a single content chunk followed by a Done chunk. agy is NOT a token-streaming
// engine — the answer is salvaged from the TUI only once the turn finishes, so
// "streaming" here is buffer-and-emit, not incremental.
func (p *AgyCLIProvider) ChatStream(ctx context.Context, req ChatRequest, onChunk func(StreamChunk)) (*ChatResponse, error) {
	resp, err := p.runTurn(ctx, req, onChunk)
	if err != nil {
		return nil, err
	}
	if onChunk != nil {
		onChunk(StreamChunk{Content: resp.Content})
		onChunk(StreamChunk{Done: true})
	}
	return resp, nil
}

// runTurn is the shared core of Chat/ChatStream. onChunk is unused for content
// emission here (ChatStream emits after the full answer is known); it is threaded
// only to keep the call sites symmetric.
func (p *AgyCLIProvider) runTurn(ctx context.Context, req ChatRequest, _ func(StreamChunk)) (*ChatResponse, error) {
	systemPrompt, userMsg, _, _ := extractFromMessages(req.Messages)
	sessionKey := extractStringOpt(req.Options, OptSessionKey)

	model := req.Model
	if model == "" {
		model = p.defaultModel
	}

	// Empty session_key => ephemeral session: create, use once, close at end.
	if sessionKey == "" {
		entry, err := p.createEntry(ctx, sessionKey, model)
		if err != nil {
			return nil, err
		}
		defer entry.sess.Close()
		return p.turnOnEntry(ctx, entry, systemPrompt, userMsg)
	}

	entry, err := p.getOrCreateEntry(ctx, sessionKey, model)
	if err != nil {
		return nil, err
	}
	return p.turnOnEntry(ctx, entry, systemPrompt, userMsg)
}

// turnOnEntry serializes the turn on the session, prepends the system prompt on
// the first turn only, sends the prompt, and maps the TurnResult to a ChatResponse.
func (p *AgyCLIProvider) turnOnEntry(ctx context.Context, entry *sessionEntry, systemPrompt, userMsg string) (*ChatResponse, error) {
	entry.mu.Lock()
	defer entry.mu.Unlock()

	prompt := userMsg
	if !entry.firstTurnDone && systemPrompt != "" {
		// agy has no separate system-prompt channel; fold it into the first turn.
		prompt = systemPrompt + "\n\n" + userMsg
	}

	turn, err := entry.sess.SendPrompt(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("agy-cli: send prompt: %w", err)
	}

	// Mark the session as having taken its first turn (so the system prompt is
	// never prepended again) and refresh idle bookkeeping.
	entry.firstTurnDone = true
	entry.lastUsed = time.Now()

	if turn.Confidence == agycli.ConfidenceLow {
		slog.Debug("agy-cli: low-confidence answer extraction", "name", p.name)
	}

	finishReason := "stop"
	if turn.TimedOut {
		finishReason = "error"
	}
	return &ChatResponse{
		Content:      turn.Answer,
		FinishReason: finishReason,
	}, nil
}

// getOrCreateEntry returns the live session for sessionKey, creating it under the
// provider lock if absent. Concurrent turns on the same key then serialize on the
// entry's own mutex (mirrors claude-cli's per-session locking).
func (p *AgyCLIProvider) getOrCreateEntry(ctx context.Context, sessionKey, model string) (*sessionEntry, error) {
	p.mu.Lock()
	if entry, ok := p.sessions[sessionKey]; ok {
		p.mu.Unlock()
		return entry, nil
	}
	p.mu.Unlock()

	// Create outside the provider lock (NewSession can block on tmux/ready), then
	// re-check under the lock so a racing creator wins-once.
	entry, err := p.createEntry(ctx, sessionKey, model)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	if existing, ok := p.sessions[sessionKey]; ok {
		p.mu.Unlock()
		// Lost the race: discard the one we just built.
		_ = entry.sess.Close()
		return existing, nil
	}
	if p.isClosed() {
		// Provider shut down while we were creating; don't leak the session.
		p.mu.Unlock()
		_ = entry.sess.Close()
		return nil, fmt.Errorf("agy-cli: provider is closed")
	}
	p.sessions[sessionKey] = entry
	p.mu.Unlock()
	return entry, nil
}

// createEntry builds a fresh live session for sessionKey (no map registration).
func (p *AgyCLIProvider) createEntry(ctx context.Context, sessionKey, model string) (*sessionEntry, error) {
	workDir := p.ensureWorkDir(sessionKey)
	sess, err := p.newSession(ctx, agycli.SessionOptions{
		Workdir:         workDir,
		Model:           model,
		SkipPermissions: p.skipPermissions,
		Sandbox:         p.sandbox,
	})
	if err != nil {
		return nil, fmt.Errorf("agy-cli: new session: %w", err)
	}
	return &sessionEntry{sess: sess, lastUsed: time.Now()}, nil
}

// ensureWorkDir returns a stable per-session workdir under baseWorkDir and creates
// it. Falls back to os.TempDir() on failure. Empty session keys get a "default"
// segment via sanitizePathSegment.
func (p *AgyCLIProvider) ensureWorkDir(sessionKey string) string {
	dir := filepath.Join(p.baseWorkDir, sanitizePathSegment(sessionKey))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("agy-cli: failed to create workdir", "dir", dir, "error", err)
		return os.TempDir()
	}
	return dir
}

// Close closes all live sessions and stops the idle reaper. Implements io.Closer
// and is idempotent.
func (p *AgyCLIProvider) Close() error {
	p.closeOnce.Do(func() {
		close(p.closed)
		p.mu.Lock()
		entries := p.sessions
		p.sessions = make(map[string]*sessionEntry)
		p.mu.Unlock()
		for key, entry := range entries {
			if err := entry.sess.Close(); err != nil {
				slog.Warn("agy-cli: failed to close session", "session_key", key, "error", err)
			}
		}
	})
	return nil
}

// isClosed reports whether Close() has been called.
func (p *AgyCLIProvider) isClosed() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

// reapLoop periodically evicts idle sessions until the provider is closed.
func (p *AgyCLIProvider) reapLoop() {
	ticker := time.NewTicker(agyCLIReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.closed:
			return
		case <-ticker.C:
			p.reapIdle(time.Now())
		}
	}
}

// reapIdle closes and removes every session unused for longer than idleTTL. It is
// exported-to-package (lowercase) so tests can drive eviction deterministically
// without waiting on the ticker.
func (p *AgyCLIProvider) reapIdle(now time.Time) {
	p.mu.Lock()
	var toClose []*sessionEntry
	for key, entry := range p.sessions {
		if now.Sub(entry.lastUsed) > p.idleTTL {
			toClose = append(toClose, entry)
			delete(p.sessions, key)
		}
	}
	p.mu.Unlock()

	for _, entry := range toClose {
		if err := entry.sess.Close(); err != nil {
			slog.Warn("agy-cli: failed to close idle session", "error", err)
		}
	}
}
