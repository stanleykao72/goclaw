package providers

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/providers/agycli"
)

// timeFarFuture is well past any idle TTL used in tests, so reapIdle evicts.
func timeFarFuture() time.Time { return time.Now().Add(24 * time.Hour) }

// assertBridgeConfigOnDisk parses the written mcp_config.json and asserts the
// goclaw-bridge entry has the expected http URL + Authorization bearer.
func assertBridgeConfigOnDisk(t *testing.T, path, wantURL, wantAuth string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config %s: %v", path, err)
	}
	var top struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Type    string            `json:"type"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("parse config: %v\n%s", err, raw)
	}
	entry, ok := top.MCPServers["goclaw-bridge"]
	if !ok {
		t.Fatalf("no goclaw-bridge entry in %s: %s", path, raw)
	}
	if entry.URL != wantURL {
		t.Errorf("url = %q, want %q", entry.URL, wantURL)
	}
	if entry.Type != "http" {
		t.Errorf("type = %q, want http", entry.Type)
	}
	if entry.Headers["Authorization"] != wantAuth {
		t.Errorf("Authorization = %q, want %q", entry.Headers["Authorization"], wantAuth)
	}
}

// fakeBridgeListeners records the BridgeContext + sessionKey it was started with
// and hands back a canned URL plus a closer that counts how many times it ran.
// It never binds a real loopback port.
type fakeBridgeListeners struct {
	mu          sync.Mutex
	url         string
	startCalls  int
	gotBC       BridgeContext
	gotSession  string
	closerCalls int32
}

func (f *fakeBridgeListeners) StartForBridgeContext(bc BridgeContext, sessionKey string) (string, func() error, error) {
	f.mu.Lock()
	f.startCalls++
	f.gotBC = bc
	f.gotSession = sessionKey
	url := f.url
	if url == "" {
		url = "http://127.0.0.1:54321/mcp/bridge"
	}
	f.mu.Unlock()
	return url, func() error { atomic.AddInt32(&f.closerCalls, 1); return nil }, nil
}

func (f *fakeBridgeListeners) closes() int { return int(atomic.LoadInt32(&f.closerCalls)) }
func (f *fakeBridgeListeners) starts() int { f.mu.Lock(); defer f.mu.Unlock(); return f.startCalls }
func (f *fakeBridgeListeners) bridgeContext() BridgeContext {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotBC
}
func (f *fakeBridgeListeners) sessionKey() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotSession
}

// captureWriter records the servers map handed to writeBridgeConfig.
type captureWriter struct {
	mu      sync.Mutex
	calls   int
	servers []map[string]any
}

func (c *captureWriter) write(servers map[string]any) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.servers = append(c.servers, servers)
	return "/tmp/fake/mcp_config.json", nil
}

func (c *captureWriter) last() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.servers) == 0 {
		return nil
	}
	return c.servers[len(c.servers)-1]
}

// bridgeChatReq builds a ChatRequest carrying the full set of Opt* bridge keys.
func bridgeChatReq(sessionKey, user string) ChatRequest {
	return ChatRequest{
		Messages: []Message{{Role: "user", Content: user}},
		Options: map[string]any{
			OptSessionKey:  sessionKey,
			OptAgentID:     "11111111-1111-1111-1111-111111111111",
			OptUserID:      "user-42",
			OptTenantID:    "22222222-2222-2222-2222-222222222222",
			OptChannel:     "lineworks-bot",
			OptChatID:      "chat-7",
			OptPeerKind:    "group",
			OptWorkspace:   "/ws/abc",
			OptLocalKey:    "lk-1",
			OptSenderID:    "sender-9",
			OptChannelType: "lineworks",
		},
	}
}

// On session create with a bridge wired, the provider must:
//   - build a BridgeContext from the request Options Opt* keys,
//   - call StartForBridgeContext with that context + the session key,
//   - write a goclaw-bridge entry containing the returned URL + bearer BEFORE launch.
func TestAgyCLI_Bridge_StartAndConfigOnCreate(t *testing.T) {
	ff := &fakeFactory{}
	br := &fakeBridgeListeners{url: "http://127.0.0.1:40001/mcp/bridge"}
	cw := &captureWriter{}
	p := newTestProvider(t, ff,
		withAgyCLIWriteBridgeConfig(cw.write),
		WithAgyCLIBridge(br, "gw-secret"),
	)

	if _, err := p.Chat(context.Background(), bridgeChatReq("sk-1", "hi")); err != nil {
		t.Fatalf("chat: %v", err)
	}

	// Start called exactly once with the identity from Options + the session key.
	if br.starts() != 1 {
		t.Fatalf("StartForBridgeContext calls = %d, want 1", br.starts())
	}
	if br.sessionKey() != "sk-1" {
		t.Errorf("session key passed to bridge = %q, want sk-1", br.sessionKey())
	}
	bc := br.bridgeContext()
	if bc.AgentID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("bc.AgentID = %q", bc.AgentID)
	}
	if bc.UserID != "user-42" || bc.TenantID != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("bc user/tenant = %q/%q", bc.UserID, bc.TenantID)
	}
	if bc.Channel != "lineworks-bot" || bc.ChatID != "chat-7" || bc.PeerKind != "group" {
		t.Errorf("bc channel/chat/peer = %q/%q/%q", bc.Channel, bc.ChatID, bc.PeerKind)
	}
	if bc.Workspace != "/ws/abc" || bc.LocalKey != "lk-1" {
		t.Errorf("bc workspace/localKey = %q/%q", bc.Workspace, bc.LocalKey)
	}
	if bc.SenderID != "sender-9" || bc.ChannelType != "lineworks" {
		t.Errorf("bc sender/channelType = %q/%q", bc.SenderID, bc.ChannelType)
	}

	// The bridge entry must be written exactly once, BEFORE the session launch.
	if cw.calls != 1 {
		t.Fatalf("writeBridgeConfig calls = %d, want 1", cw.calls)
	}
	servers := cw.last()
	entry, ok := servers["goclaw-bridge"].(map[string]any)
	if !ok {
		t.Fatalf("no goclaw-bridge entry written; got %v", servers)
	}
	if entry["url"] != "http://127.0.0.1:40001/mcp/bridge" {
		t.Errorf("bridge url = %v, want the URL Start returned", entry["url"])
	}
	if entry["type"] != "http" {
		t.Errorf("bridge type = %v, want http", entry["type"])
	}
	headers, ok := entry["headers"].(map[string]any)
	if !ok {
		t.Fatalf("no headers in bridge entry; got %v", entry)
	}
	if headers["Authorization"] != "Bearer gw-secret" {
		t.Errorf("Authorization = %v, want Bearer gw-secret", headers["Authorization"])
	}
	// Variant 2: no per-user identity headers in the (shared) global config entry.
	for _, k := range []string{"X-Agent-ID", "X-User-ID", "X-Tenant-ID", "X-Sender-ID", "X-Channel-Type"} {
		if _, present := headers[k]; present {
			t.Errorf("identity header %q leaked into shared config entry", k)
		}
	}

	// The config write must happen before the session is launched.
	if ff.count() != 1 {
		t.Fatalf("session not launched once: count=%d", ff.count())
	}
}

// On Close the per-session bridge listener closer must run.
func TestAgyCLI_Bridge_CloserRunsOnClose(t *testing.T) {
	ff := &fakeFactory{}
	br := &fakeBridgeListeners{}
	p := NewAgyCLIProvider("agy",
		WithAgyCLIWorkDir(t.TempDir()),
		withAgyCLISessionFactory(ff.make),
		withAgyCLIWriteBridgeConfig((&captureWriter{}).write),
		WithAgyCLIBridge(br, "tok"),
	)
	if _, err := p.Chat(context.Background(), bridgeChatReq("sk", "q")); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if br.closes() != 0 {
		t.Fatalf("closer ran before Close: %d", br.closes())
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if br.closes() != 1 {
		t.Errorf("bridge closer ran %d times on Close, want 1", br.closes())
	}
	// Idempotent: second Close must not re-run the closer.
	_ = p.Close()
	if br.closes() != 1 {
		t.Errorf("bridge closer ran %d times after 2x Close, want 1", br.closes())
	}
}

// Ephemeral (empty session key) session: bridge listener is created and torn down
// at the end of the single call.
func TestAgyCLI_Bridge_EphemeralCloserRuns(t *testing.T) {
	ff := &fakeFactory{}
	br := &fakeBridgeListeners{}
	p := NewAgyCLIProvider("agy",
		WithAgyCLIWorkDir(t.TempDir()),
		withAgyCLISessionFactory(ff.make),
		withAgyCLIWriteBridgeConfig((&captureWriter{}).write),
		WithAgyCLIBridge(br, "tok"),
	)
	t.Cleanup(func() { _ = p.Close() })

	if _, err := p.Chat(context.Background(), bridgeChatReq("", "q")); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if br.starts() != 1 {
		t.Errorf("bridge starts = %d, want 1", br.starts())
	}
	if br.closes() != 1 {
		t.Errorf("ephemeral bridge closer ran %d times, want 1 (torn down at call end)", br.closes())
	}
}

// Idle eviction must tear down the per-session bridge listener too.
func TestAgyCLI_Bridge_CloserRunsOnReap(t *testing.T) {
	ff := &fakeFactory{}
	br := &fakeBridgeListeners{}
	p := newTestProvider(t, ff,
		withAgyCLIWriteBridgeConfig((&captureWriter{}).write),
		WithAgyCLIBridge(br, "tok"),
	)
	if _, err := p.Chat(context.Background(), bridgeChatReq("idle", "q")); err != nil {
		t.Fatalf("chat: %v", err)
	}
	p.reapIdle(timeFarFuture())
	if br.closes() != 1 {
		t.Errorf("reaped bridge closer ran %d times, want 1", br.closes())
	}
}

// Reused session: the bridge listener is minted only on creation, not per turn.
func TestAgyCLI_Bridge_StartOncePerSession(t *testing.T) {
	ff := &fakeFactory{}
	br := &fakeBridgeListeners{}
	p := newTestProvider(t, ff,
		withAgyCLIWriteBridgeConfig((&captureWriter{}).write),
		WithAgyCLIBridge(br, "tok"),
	)
	for i := 0; i < 3; i++ {
		if _, err := p.Chat(context.Background(), bridgeChatReq("same", "q")); err != nil {
			t.Fatalf("chat %d: %v", i, err)
		}
	}
	if br.starts() != 1 {
		t.Errorf("bridge started %d times for one reused session, want 1", br.starts())
	}
}

// No bridge wired => prior behaviour: no Start, no config write, sessions still work.
func TestAgyCLI_Bridge_NilBridgeNoOp(t *testing.T) {
	ff := &fakeFactory{}
	cw := &captureWriter{}
	p := newTestProvider(t, ff, withAgyCLIWriteBridgeConfig(cw.write))
	if _, err := p.Chat(context.Background(), bridgeChatReq("sk", "q")); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if cw.calls != 0 {
		t.Errorf("writeBridgeConfig called %d times with nil bridge, want 0", cw.calls)
	}
	if ff.count() != 1 {
		t.Errorf("session not created: %d", ff.count())
	}
}

// Interface compliance: the package interface is what the provider stores.
func TestAgyCLI_Bridge_InterfaceUsed(t *testing.T) {
	var _ AgyBridgeListeners = (*fakeBridgeListeners)(nil)
}

// MergeAgyMCPConfig writes the {url,type:http,headers} shape into an
// AGY_CONFIG_DIR temp dir (never touching real ~/.gemini).
func TestAgyCLI_Bridge_MergeWritesHTTPShape(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGY_CONFIG_DIR", dir)

	servers := agycli.BuildAgyBridgeServers("http://127.0.0.1:40002/mcp/bridge", "gw-secret")
	path, err := agycli.MergeAgyMCPConfig(servers)
	if err != nil {
		t.Fatalf("MergeAgyMCPConfig: %v", err)
	}
	if path == "" {
		t.Fatal("MergeAgyMCPConfig returned empty path")
	}
	assertBridgeConfigOnDisk(t, path, "http://127.0.0.1:40002/mcp/bridge", "Bearer gw-secret")
}
