package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// bridgeSessionListenerPath is the path the per-session loopback listener
// serves the bridge on. It matches the shared /mcp/bridge route so the agy
// global config can use an identical path shape (only the port differs).
const bridgeSessionListenerPath = "/mcp/bridge"

// BridgeSessionListeners starts one dedicated 127.0.0.1 loopback listener per
// agy session — the core of variant 2 of the bridge design. The bound port IS
// the identity binding: each listener injects a single fixed ResolvedIdentity
// (via the shared injectBridgeIdentity helper) before delegating to the SAME
// bridge handler the claude /mcp/bridge route uses. No per-user secret is ever
// written to a shared file or passed on a command line; the only credential on
// the wire is the shared gateway bearer token, which the per-session listener
// re-checks via tokenAuthMiddleware.
type BridgeSessionListeners struct {
	bridgeHandler http.Handler
	token         string
	agentStore    store.AgentStore

	// mu guards listeners + nextID. listeners tracks every live per-session
	// loopback srv keyed by a monotonic handle so CloseAll (gateway shutdown)
	// can tear them all down even if a provider leaked one. Each Start adds an
	// entry; the returned closer removes + shuts down its own entry exactly once.
	mu        sync.Mutex
	listeners map[uint64]*http.Server
	nextID    uint64
}

// NewBridgeSessionListeners builds a manager bound to the shared bridge handler,
// the gateway bearer token, and the agent store (needed so injected identities
// pick up the per-agent key / memory backend / shell deny groups). The handler
// and token are the same instances the claude /mcp/bridge route uses; this
// manager only fronts them with per-session identity injection.
func NewBridgeSessionListeners(bridgeHandler http.Handler, token string, agentStore store.AgentStore) *BridgeSessionListeners {
	return &BridgeSessionListeners{
		bridgeHandler: bridgeHandler,
		token:         token,
		agentStore:    agentStore,
		listeners:     make(map[uint64]*http.Server),
	}
}

// Start binds a fresh ephemeral 127.0.0.1:0 listener whose handler injects id
// into the request context and delegates to the shared bridge handler. It
// returns the bound URL (http://127.0.0.1:<port>/mcp/bridge) for the session to
// connect to, plus a closer that stops the listener. The bearer token is still
// required on every request (tokenAuthMiddleware) — the port carries no secret,
// it only binds identity.
//
// CONCURRENCY (probe 2026-06-22, agy v1.0.10 — MEASURED): the per-session URL is
// written into agy's SHARED global ~/.gemini/config/mcp_config.json by the
// provider just before launch. agy reads that config ONLY at process startup and
// snapshots its MCP server set then — it does NOT re-read on mtime/content change
// mid-session (probe: turn 1 config→server A; overwrite config→server B; turn 2
// in the SAME agy stayed connected to A, B never received an initialize). So
// OVERLAPPING live sessions are SAFE on the shared file: each agy binds its own
// per-session loopback URL at launch, and a later overwrite by another session
// does not affect an already-connected agy. Each port stays bound to a SINGLE
// fixed identity and its closer tears the port down at session end (fails CLOSED:
// a torn-down port refuses connection; the bearer is still required). agy has no
// mid-session config reload and no --mcp-config flag today — see
// docs/agy-bridge-design.md.
func (m *BridgeSessionListeners) Start(id ResolvedIdentity) (url string, closer func() error, err error) {
	// 127.0.0.1 only: never expose the per-session listener off-host.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("bridge session listener: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle(bridgeSessionListenerPath, tokenAuthMiddleware(m.token,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := injectBridgeIdentity(r.Context(), id, m.agentStore)
			m.bridgeHandler.ServeHTTP(w, r.WithContext(ctx))
		}),
	))

	srv := &http.Server{Handler: mux}

	// Track the server so CloseAll (gateway shutdown) can reclaim it even if the
	// provider never calls the returned closer (crash/leak). The closer removes
	// its own entry so a normal session teardown doesn't grow the map.
	m.mu.Lock()
	handle := m.nextID
	m.nextID++
	m.listeners[handle] = srv
	m.mu.Unlock()

	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			// Listener died unexpectedly; the session will fail its next request
			// and can be torn down by the caller. Nothing to recover here.
			_ = serveErr
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	url = fmt.Sprintf("http://127.0.0.1:%d%s", port, bridgeSessionListenerPath)

	var closeOnce sync.Once
	closer = func() error {
		var shutErr error
		closeOnce.Do(func() {
			m.mu.Lock()
			delete(m.listeners, handle)
			m.mu.Unlock()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			shutErr = srv.Shutdown(shutdownCtx)
		})
		return shutErr
	}
	return url, closer, nil
}

// StartForBridgeContext is the agy-provider-facing entry point. It maps a
// providers.BridgeContext (built from the request Options Opt* keys by the agy
// CLI provider) into a ResolvedIdentity and starts a per-session loopback
// listener bound to it. TenantVerified and SenderVerified are forced true: in
// variant 2 the loopback PORT is the credential, so an identity goclaw mints for
// a session it itself launched is trusted by construction (mirrors the deleted
// BridgeSessionStore.Issue semantics). A non-parsable AgentID is left uuid.Nil
// so injectBridgeIdentity simply skips agent injection (same as the claude
// path's guard) rather than failing the launch.
//
// *BridgeSessionListeners therefore satisfies the providers.AgyBridgeListeners
// interface structurally; the agy provider depends only on that interface, never
// on the gateway package, so no import cycle is introduced.
func (m *BridgeSessionListeners) StartForBridgeContext(bc providers.BridgeContext, sessionKey string) (url string, closer func() error, err error) {
	id := ResolvedIdentity{
		UserID:      bc.UserID,
		TenantID:    bc.TenantID,
		Channel:     bc.Channel,
		ChatID:      bc.ChatID,
		PeerKind:    bc.PeerKind,
		Workspace:   bc.Workspace,
		LocalKey:    bc.LocalKey,
		SessionKey:  sessionKey,
		ChannelType: bc.ChannelType,
		SenderID:    bc.SenderID,
		// goclaw mints this identity for a session it launched: the port is the
		// credential, so tenant + sender are trusted on issuance.
		TenantVerified: true,
		SenderVerified: true,
	}
	if bc.AgentID != "" {
		if aid, perr := uuid.Parse(bc.AgentID); perr == nil {
			id.AgentID = aid
		}
	}
	return m.Start(id)
}

// CloseAll shuts down every per-session listener still tracked by the manager.
// Called from gateway shutdown so abandoned/leaked per-session ports (e.g. from
// a provider that crashed before reaping) are reclaimed. Safe to call multiple
// times and concurrently with per-session closers; each srv.Shutdown is
// idempotent and the map is drained under the lock.
func (m *BridgeSessionListeners) CloseAll() {
	m.mu.Lock()
	srvs := make([]*http.Server, 0, len(m.listeners))
	for h, srv := range m.listeners {
		srvs = append(srvs, srv)
		delete(m.listeners, h)
	}
	m.mu.Unlock()

	for _, srv := range srvs {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(shutdownCtx)
		cancel()
	}
}
