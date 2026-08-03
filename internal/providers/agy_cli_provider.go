package providers

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/providers/agycli"
)

// agy_cli_provider.go bridges goclaw's Provider interface to agy ("Google
// Antigravity CLI") in one of two modes:
//
//   - INTERACTIVE (default): a persistent tmux-driven agy session per goclaw
//     session_key. Built when Phase 0 (agy v1.0.10) concluded "--print" had no
//     cross-turn memory. System prompt rides <workdir>/GEMINI.md at session
//     creation; every turn sends only the user message into the live process.
//   - ONE-SHOT (WithAgyCLIOneShot / config one_shot): W0 probes (2026-07-08,
//     docs/agy-oneshot-print-provider.md) overturned Phase 0 — "--print
//     --conversation <agy-minted-id>" DOES resume with full context — so each
//     turn is an independent --print run resuming the session's conversation.
//     GEMINI.md is NOT read in print mode (no workspace concept), so standing
//     instructions install via a discarded seed turn instead.
//
// Both modes keep a map of sessions keyed by the goclaw session_key
// (OptSessionKey). An empty session_key yields a per-call ephemeral session
// that is Closed at the end of that call.
//
// agy is NOT a token-streaming engine — it renders a TUI and we salvage the
// final answer once the turn reaches the ready footer. ChatStream therefore
// buffers the whole answer and emits it as a single chunk, then a Done chunk.

// AgyCLIProviderName is the default registry name for the agy CLI provider.
// Exported so cmd/gateway can look the provider up after construction (to wire
// the per-session bridge once BuildMux has built the listeners) without
// duplicating the literal.
const AgyCLIProviderName = "agy-cli"

const (
	// agyCLIDefaultName is the provider identifier used as the registry key suffix.
	agyCLIDefaultName = AgyCLIProviderName
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

// AgyBridgeListeners is the gateway-side per-session loopback bridge manager,
// reduced to the single method the agy provider needs. It is an interface so the
// provider depends only on this contract — never on the gateway package — which
// avoids an import cycle (gateway imports providers). *gateway.BridgeSessionListeners
// satisfies it structurally via its StartForBridgeContext method.
//
// StartForBridgeContext binds a fresh 127.0.0.1 loopback port to the identity in
// bc (variant 2: port == identity), returning the bridge URL the agy session
// should connect to plus a closer that tears the port down when the session ends.
type AgyBridgeListeners interface {
	StartForBridgeContext(bc BridgeContext, sessionKey string) (url string, closer func() error, err error)
}

// sessionEntry holds one live agy session plus its bookkeeping. mu serializes
// concurrent turns on the SAME session (overlapping send-keys would interleave);
// lastUsed drives idle eviction. The system prompt is delivered via GEMINI.md at
// session creation (not per turn), so no first-turn bookkeeping is needed here.
//
// bridgeCloser, when non-nil, tears down the per-session loopback bridge listener
// bound to this session's identity. It is invoked exactly once on session
// Close/reap so the 127.0.0.1 port is reclaimed when the agy process ends.
type sessionEntry struct {
	sess         agySession
	mu           sync.Mutex
	lastUsed     time.Time
	bridgeCloser func() error

	// One-shot mode only (sess == nil): there is no live process.
	// conversationID is the agy-minted conversation every turn resumes
	// (captured from the seed turn's --log-file); model is pinned at creation
	// for launch-parity with the interactive path; env carries the per-session
	// HOME override pointing at fakeHome, the isolated home dir built for
	// MCP-config identity isolation (O1) and removed on Close/reap.
	conversationID string
	model          string
	env            []string
	fakeHome       string
}

// closeEntry closes the live agy session and tears down its per-session bridge
// listener (if any). Both are attempted regardless of the other's error so a
// failing session Close never leaks the loopback port. The first non-nil error
// is returned.
func closeEntry(entry *sessionEntry) error {
	var firstErr error
	if entry.sess != nil {
		if err := entry.sess.Close(); err != nil {
			firstErr = err
		}
	}
	if entry.fakeHome != "" {
		// One-shot mode: only symlinks + this session's private config live
		// here; RemoveAll never touches the real ~/.gemini targets. An
		// in-flight turn (entry.mu held) still has a running agy child with
		// HOME under this dir — deleting it out from under the child would
		// break that turn, so cleanup is deferred until the turn releases the
		// lock. Close() must not block for up to a full turn, hence TryLock +
		// background finish rather than a plain Lock.
		fakeHome := entry.fakeHome
		bridgeCloser := entry.bridgeCloser
		if entry.mu.TryLock() {
			entry.mu.Unlock()
			if err := os.RemoveAll(fakeHome); err != nil && firstErr == nil {
				firstErr = err
			}
		} else {
			go func() {
				entry.mu.Lock()
				defer entry.mu.Unlock()
				if err := os.RemoveAll(fakeHome); err != nil {
					slog.Warn("agy-cli: deferred fake-home cleanup failed", "dir", fakeHome, "error", err)
				}
				if bridgeCloser != nil {
					if err := bridgeCloser(); err != nil {
						slog.Warn("agy-cli: deferred bridge close failed", "error", err)
					}
				}
			}()
			// The goroutine owns the bridge closer too (the in-flight turn may
			// still be talking to the bridge); skip the synchronous close below.
			return firstErr
		}
	}
	if entry.bridgeCloser != nil {
		if err := entry.bridgeCloser(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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
	oneShot         bool   // drive one-shot --print runs instead of a persistent tmux session
	idleTTL         time.Duration

	mu       sync.Mutex // protects the sessions map + workdir creation
	sessions map[string]*sessionEntry

	// bridge, when non-nil, wires per-session goclaw MCP bridge access for each
	// agy session: createEntry mints a per-session loopback listener bound to the
	// session's identity and writes the corresponding bridge entry into agy's
	// global MCP config just before launch. gatewayToken is the shared bearer
	// written into that (static-shape) config entry. nil bridge => no MCP bridge
	// (agy uses whatever is already in its global config), preserving the prior
	// behaviour bit-for-bit.
	bridge       AgyBridgeListeners
	gatewayToken string

	// writeBridgeConfig writes the goclaw-bridge entry into agy's global MCP
	// config and returns the config path. Injectable for tests so unit tests can
	// assert the written shape against an AGY_CONFIG_DIR temp dir (and never touch
	// real ~/.gemini). The default calls agycli.MergeAgyMCPConfig.
	writeBridgeConfig func(servers map[string]any) (string, error)

	// newSession is the session factory, injectable for tests. The default wraps
	// agycli.NewSession (which takes binary as its 2nd arg) by closing over cliPath.
	newSession func(ctx context.Context, opts agycli.SessionOptions) (agySession, error)

	// runPrint executes one one-shot --print run (one-shot mode only).
	// Injectable for tests so unit tests can fake agy turns without spawning a
	// process. The default is agycli.RunPrint.
	runPrint func(ctx context.Context, binary string, opts agycli.PrintOptions, extraEnv []string) (agycli.PrintResult, error)

	// realGeminiDir resolves the operator's real ~/.gemini (the fake-home
	// symlink source). Injectable for tests. Default: agycli.RealGeminiDir.
	realGeminiDir func() (string, error)

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

// WithAgyCLIOneShot toggles one-shot mode: every turn is an independent
// "agy --print --conversation <id>" run instead of a persistent interactive
// tmux session. Multi-turn memory rides on agy's conversation store (W0
// probes, docs/agy-oneshot-print-provider.md). Off (the default) preserves the
// interactive path bit-for-bit.
func WithAgyCLIOneShot(v bool) AgyCLIOption {
	return func(p *AgyCLIProvider) {
		p.oneShot = v
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

// WithAgyCLIBridge wires per-session goclaw MCP bridge access. listeners is the
// gateway's per-session loopback bridge manager (variant 2: port == identity);
// gatewayToken is the shared bearer written into agy's global MCP config entry.
// When listeners is nil the option is a no-op and the provider behaves exactly as
// before (no bridge). Threaded in by cmd/gateway after BuildMux constructs the
// listeners (it is nil until then), so this is applied via a setter at runtime
// rather than only at construction — see SetBridge.
func WithAgyCLIBridge(listeners AgyBridgeListeners, gatewayToken string) AgyCLIOption {
	return func(p *AgyCLIProvider) {
		p.bridge = listeners
		p.gatewayToken = gatewayToken
	}
}

// SetBridge wires the per-session bridge after construction. The agy provider is
// registered before the gateway's BridgeSessionListeners exists (it is built in
// BuildMux), so cmd/gateway calls this once the manager is available. Passing a
// nil listeners leaves the provider bridge-less.
func (p *AgyCLIProvider) SetBridge(listeners AgyBridgeListeners, gatewayToken string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bridge = listeners
	p.gatewayToken = gatewayToken
}

// withAgyCLISessionFactory injects a custom session factory (test-only seam).
func withAgyCLISessionFactory(f func(ctx context.Context, opts agycli.SessionOptions) (agySession, error)) AgyCLIOption {
	return func(p *AgyCLIProvider) {
		if f != nil {
			p.newSession = f
		}
	}
}

// withAgyCLIRunPrint injects a custom one-shot runner (test-only seam).
func withAgyCLIRunPrint(f func(ctx context.Context, binary string, opts agycli.PrintOptions, extraEnv []string) (agycli.PrintResult, error)) AgyCLIOption {
	return func(p *AgyCLIProvider) {
		if f != nil {
			p.runPrint = f
		}
	}
}

// withAgyCLIRealGeminiDir injects a custom real-gemini resolver (test-only seam)
// so fake-home tests never touch the developer's actual ~/.gemini.
func withAgyCLIRealGeminiDir(f func() (string, error)) AgyCLIOption {
	return func(p *AgyCLIProvider) {
		if f != nil {
			p.realGeminiDir = f
		}
	}
}

// withAgyCLIWriteBridgeConfig injects a custom bridge-config writer (test-only
// seam) so tests can capture the servers map passed at launch without touching
// the real agy config dir.
func withAgyCLIWriteBridgeConfig(f func(servers map[string]any) (string, error)) AgyCLIOption {
	return func(p *AgyCLIProvider) {
		if f != nil {
			p.writeBridgeConfig = f
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
	// Default bridge-config writer: agy's global ~/.gemini/config/mcp_config.json
	// (AGY_CONFIG_DIR-overridable). Injectable so tests assert the written shape
	// against a temp dir without touching real ~/.gemini.
	p.writeBridgeConfig = agycli.MergeAgyMCPConfig
	// One-shot mode defaults (unused while oneShot is false).
	p.runPrint = agycli.RunPrint
	p.realGeminiDir = agycli.RealGeminiDir
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

	// Build the bridge identity from the SAME Opt* keys the claude path uses
	// (agent/user/tenant/channel/chat/peer/workspace/local-key/sender/channel-type).
	// It is consumed only when a bridge is wired AND the session is created here;
	// reused sessions keep the identity they were minted with.
	bc := bridgeContextFromOpts(req.Options)

	model := req.Model
	if model == "" {
		model = p.defaultModel
	}

	// Empty session_key => ephemeral session: create, use once, close at end. The
	// system prompt from THIS call is written to the session's GEMINI.md at creation.
	if sessionKey == "" {
		entry, err := p.createEntry(ctx, sessionKey, model, systemPrompt, bc)
		if err != nil {
			return nil, err
		}
		// Tear down both the session AND its per-session bridge listener.
		defer func() { _ = closeEntry(entry) }()
		return p.turnOnEntry(ctx, entry, userMsg)
	}

	// Sessions are created lazily on first Chat: the systemPrompt captured here is
	// the one that reaches GEMINI.md at creation. Subsequent turns reuse the live
	// session (GEMINI.md already written) and only send the user message.
	entry, err := p.getOrCreateEntry(ctx, sessionKey, model, systemPrompt, bc)
	if err != nil {
		return nil, err
	}
	return p.turnOnEntry(ctx, entry, userMsg)
}

// turnOnEntry serializes the turn on the session, sends ONLY the user message,
// and maps the TurnResult to a ChatResponse. The system prompt is never folded
// into the prompt — it was written to the session's GEMINI.md at creation so agy
// reads it as system/context instructions (see the file header).
func (p *AgyCLIProvider) turnOnEntry(ctx context.Context, entry *sessionEntry, userMsg string) (*ChatResponse, error) {
	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.sess == nil {
		return p.oneShotTurnLocked(ctx, entry, userMsg)
	}

	turn, err := entry.sess.SendPrompt(ctx, userMsg)
	if err != nil {
		return nil, fmt.Errorf("agy-cli: send prompt: %w", err)
	}

	// Refresh idle bookkeeping.
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
// entry's own mutex (mirrors claude-cli's per-session locking). systemPrompt is
// used ONLY when the session is created here (written to GEMINI.md); once a
// session exists it is reused unchanged and systemPrompt is ignored.
func (p *AgyCLIProvider) getOrCreateEntry(ctx context.Context, sessionKey, model, systemPrompt string, bc BridgeContext) (*sessionEntry, error) {
	p.mu.Lock()
	if entry, ok := p.sessions[sessionKey]; ok {
		p.mu.Unlock()
		return entry, nil
	}
	p.mu.Unlock()

	// Create outside the provider lock (NewSession can block on tmux/ready), then
	// re-check under the lock so a racing creator wins-once.
	entry, err := p.createEntry(ctx, sessionKey, model, systemPrompt, bc)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	if existing, ok := p.sessions[sessionKey]; ok {
		p.mu.Unlock()
		// Lost the race: discard the one we just built (incl. its bridge listener).
		_ = closeEntry(entry)
		return existing, nil
	}
	if p.isClosed() {
		// Provider shut down while we were creating; don't leak the session
		// (or its bridge listener).
		p.mu.Unlock()
		_ = closeEntry(entry)
		return nil, fmt.Errorf("agy-cli: provider is closed")
	}
	p.sessions[sessionKey] = entry
	p.mu.Unlock()
	return entry, nil
}

// createEntry builds a fresh live session for sessionKey (no map registration).
// systemPrompt is passed to SessionOptions.SystemPrompt so NewSession writes it to
// <workDir>/GEMINI.md before launch (agy reads it as system/context instructions).
//
// When a bridge is wired, the goclaw MCP bridge is provisioned for this session
// BEFORE the agy process is launched, in this strict order:
//  1. Start a per-session 127.0.0.1 loopback listener bound to bc's identity
//     (variant 2: the port IS the credential).
//  2. Write the static-shape goclaw-bridge entry (that listener's URL + shared
//     bearer) into agy's global MCP config. This MUST precede launch because agy
//     reads the config and connects to the bridge during startup.
//  3. Launch agy (NewSession).
//
// CONCURRENCY (probe 2026-06-22, MEASURED): the config file is SHARED, but agy
// reads it ONLY at startup and does NOT re-read mid-session, so a concurrent
// session overwriting the file after THIS agy has launched does not affect it —
// overlapping sessions are safe. Step 2 must still precede launch so the fresh
// agy snapshots THIS session's URL. See BridgeSessionListeners.Start and
// docs/agy-bridge-design.md.
//
// If anything fails after the listener is started, the listener is torn down so
// no loopback port leaks.
func (p *AgyCLIProvider) createEntry(ctx context.Context, sessionKey, model, systemPrompt string, bc BridgeContext) (*sessionEntry, error) {
	if p.oneShot {
		return p.createOneShotEntry(ctx, sessionKey, model, systemPrompt, bc)
	}

	// Snapshot bridge deps under the lock (SetBridge may run concurrently).
	p.mu.Lock()
	bridge := p.bridge
	gatewayToken := p.gatewayToken
	writeCfg := p.writeBridgeConfig
	p.mu.Unlock()

	var bridgeCloser func() error
	if bridge != nil {
		// 1. Bind a per-session loopback listener to this session's identity.
		bridgeURL, closer, err := bridge.StartForBridgeContext(bc, sessionKey)
		if err != nil {
			return nil, fmt.Errorf("agy-cli: start bridge listener: %w", err)
		}
		bridgeCloser = closer

		// 2. Write the bridge entry into agy's global config BEFORE launch so the
		// freshly-spawned agy loads THIS session's bridge URL.
		if writeCfg != nil {
			servers := agycli.BuildAgyBridgeServers(bridgeURL, gatewayToken)
			if _, werr := writeCfg(servers); werr != nil {
				// Could not publish the bridge entry — reclaim the port and fail
				// rather than launch an agy that can't reach the bridge.
				_ = bridgeCloser()
				return nil, fmt.Errorf("agy-cli: write bridge config: %w", werr)
			}
		}
	}

	// 3. Launch agy.
	workDir := p.ensureWorkDir(sessionKey)
	sess, err := p.newSession(ctx, agycli.SessionOptions{
		Workdir:         workDir,
		Model:           model,
		SystemPrompt:    systemPrompt,
		SkipPermissions: p.skipPermissions,
		Sandbox:         p.sandbox,
	})
	if err != nil {
		if bridgeCloser != nil {
			_ = bridgeCloser()
		}
		return nil, fmt.Errorf("agy-cli: new session: %w", err)
	}
	return &sessionEntry{sess: sess, lastUsed: time.Now(), bridgeCloser: bridgeCloser}, nil
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
			if err := closeEntry(entry); err != nil {
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
		if err := closeEntry(entry); err != nil {
			slog.Warn("agy-cli: failed to close idle session", "error", err)
		}
	}
}

// ---------------------------------------------------------------------------
// One-shot --print mode (docs/agy-oneshot-print-provider.md)
// ---------------------------------------------------------------------------
//
// W0 probes (2026-07-08) overturned the Phase 0 constraint quoted in this
// file's header: "agy --print --conversation <agy-minted-id>" DOES resume with
// full context across processes (verified on v1.0.10 and on the v1.0.14 deploy
// target). One-shot mode therefore keeps NO live process: each turn is an
// independent --print run resuming the session's conversation. The system
// prompt cannot ride on GEMINI.md here (print mode never loads a workspace),
// so it is installed by a discarded SEED TURN whose run also mints the
// conversation ID (agycli.BuildSeedPrompt).

// agyOneShotPrintTimeout is the --print-timeout for one-shot turns; the hard
// wall-clock kill sits printTimeoutGrace above it inside agycli.RunPrint.
const agyOneShotPrintTimeout = agycli.DefaultRunTimeout

// createOneShotEntry provisions a one-shot session: per-session bridge
// listener, isolated fake HOME (O1) when a bridge is wired, then a seed turn
// that installs the standing instructions and mints the conversation ID.
//
// Bridge provisioning order matters just like the interactive path — the fake
// home's mcp_config.json must exist before the FIRST run so the seed turn's
// agy already sees this session's bridge entry. Unlike the interactive path,
// the GLOBAL config is never written: isolation is the whole point (O1,
// spec §2.5), so writeBridgeConfig is not used here.
func (p *AgyCLIProvider) createOneShotEntry(ctx context.Context, sessionKey, model, systemPrompt string, bc BridgeContext) (*sessionEntry, error) {
	// Snapshot bridge deps under the lock (SetBridge may run concurrently).
	p.mu.Lock()
	bridge := p.bridge
	gatewayToken := p.gatewayToken
	p.mu.Unlock()

	workDir := p.ensureWorkDir(sessionKey)

	var (
		bridgeCloser func() error
		fakeHome     string
		env          []string
	)
	cleanupOnErr := func() {
		if fakeHome != "" {
			_ = os.RemoveAll(fakeHome)
		}
		if bridgeCloser != nil {
			_ = bridgeCloser()
		}
	}

	// A fake HOME is built for EVERY one-shot session, bridge or not: the real
	// global mcp_config.json may carry another (interactive) session's
	// goclaw-bridge entry, and a bridge-less run against the real HOME would
	// load it and assume that session's identity. BuildFakeHome additionally
	// strips any stale goclaw-bridge key from the copied base config.
	var servers map[string]any
	if bridge != nil {
		bridgeURL, closer, err := bridge.StartForBridgeContext(bc, sessionKey)
		if err != nil {
			return nil, fmt.Errorf("agy-cli: start bridge listener: %w", err)
		}
		bridgeCloser = closer
		servers = agycli.BuildAgyBridgeServers(bridgeURL, gatewayToken)
	}

	realGemini, err := p.realGeminiDir()
	if err != nil {
		cleanupOnErr()
		return nil, fmt.Errorf("agy-cli: resolve real gemini dir: %w", err)
	}
	// MkdirTemp (not a fixed name) so concurrent ephemeral entries — which
	// share the "default" workdir segment — never collide on one fake home.
	fakeHome, err = os.MkdirTemp(workDir, "home-*")
	if err != nil {
		cleanupOnErr()
		return nil, fmt.Errorf("agy-cli: mint fake home: %w", err)
	}
	if err := agycli.BuildFakeHome(fakeHome, realGemini, servers); err != nil {
		cleanupOnErr()
		return nil, err
	}
	env = []string{"HOME=" + fakeHome}

	// Seed turn: install standing instructions (system prompt + text-only
	// channel directive) and mint the conversation ID. Its answer is discarded.
	seed, err := p.runPrint(ctx, p.cliPath, agycli.PrintOptions{
		Prompt:          agycli.BuildSeedPrompt(systemPrompt),
		Model:           model,
		Sandbox:         p.sandbox,
		SkipPermissions: p.skipPermissions,
		PrintTimeout:    agyOneShotPrintTimeout,
	}, env)
	if err != nil {
		cleanupOnErr()
		return nil, fmt.Errorf("agy-cli: seed turn: %w", err)
	}
	if seed.TimedOut {
		cleanupOnErr()
		return nil, fmt.Errorf("agy-cli: seed turn timed out after %s (stderr tail: %s)", seed.Duration, tailForLog(seed.Stderr))
	}
	if seed.ConversationID == "" {
		cleanupOnErr()
		return nil, fmt.Errorf("agy-cli: seed turn produced no conversation id (stderr tail: %s)", tailForLog(seed.Stderr))
	}

	return &sessionEntry{
		lastUsed:       time.Now(),
		bridgeCloser:   bridgeCloser,
		conversationID: seed.ConversationID,
		model:          model,
		env:            env,
		fakeHome:       fakeHome,
	}, nil
}

// oneShotTurnLocked runs one user turn as an independent --print run resuming
// the entry's conversation. Caller holds entry.mu.
//
// A timed-out run maps to FinishReason "error" and is NEVER retried here: W0
// P3 showed a killed turn may have completed server-side (the conversation
// advances), so a retry could double-execute the user's request.
func (p *AgyCLIProvider) oneShotTurnLocked(ctx context.Context, entry *sessionEntry, userMsg string) (*ChatResponse, error) {
	// Bump at turn START too, so a long-running turn is not reaped mid-run.
	entry.lastUsed = time.Now()

	res, err := p.runPrint(ctx, p.cliPath, agycli.PrintOptions{
		Prompt:          userMsg,
		Model:           entry.model,
		Sandbox:         p.sandbox,
		SkipPermissions: p.skipPermissions,
		PrintTimeout:    agyOneShotPrintTimeout,
		Conversation:    entry.conversationID,
	}, entry.env)
	if err != nil {
		return nil, fmt.Errorf("agy-cli: print turn: %w", err)
	}

	answer, conf, timedOut := agycli.ExtractPrintAnswer(res.Stdout)
	if res.TimedOut {
		timedOut = true
	}
	entry.lastUsed = time.Now()

	// A hard failure (non-zero exit, nothing salvageable) must surface as an
	// error, not as a silent empty "stop" answer — expired auth tokens and
	// invalid conversation ids land here.
	if !timedOut && res.ExitCode != 0 && answer == "" {
		return nil, fmt.Errorf("agy-cli: print turn failed (exit %d, stderr tail: %s)", res.ExitCode, tailForLog(res.Stderr))
	}

	if conf == agycli.ConfidenceLow {
		slog.Debug("agy-cli: low-confidence answer extraction", "name", p.name, "one_shot", true)
	}

	finishReason := "stop"
	if timedOut {
		finishReason = "error"
	}
	return &ChatResponse{
		Content:      answer,
		FinishReason: finishReason,
	}, nil
}

// tailForLog returns the last ~200 bytes of s, newline-flattened, for embedding
// in error messages without dumping whole streams into logs.
func tailForLog(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[len(s)-200:]
	}
	return strings.ReplaceAll(s, "\n", " | ")
}
