package providers

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// readWrittenMCPConfig writes a config and returns the unmarshalled
// {"mcpServers": ...} key set.
func writeAndReadKeys(t *testing.T, d *MCPConfigData, bc BridgeContext) map[string]any {
	t.Helper()
	t.Setenv("GOCLAW_DATA_DIR", t.TempDir())
	path := d.WriteMCPConfig(context.Background(), "sess-keyset", bc)
	if path == "" {
		t.Fatal("WriteMCPConfig returned empty path")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var parsed struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	return parsed.MCPServers
}

// TestWriteMCPConfig_KeySetExactlyStaticPlusBridge is the docs/26 §11 §3.3 / B2
// isolation invariant: after the i-3b direct-inject deletion, the written
// mcpServers key set is EXACTLY the static config-file servers plus
// goclaw-bridge — NEVER a per-agent DB server. The bypass is provably closed.
func TestWriteMCPConfig_KeySetExactlyStaticPlusBridge(t *testing.T) {
	d := &MCPConfigData{
		Servers: map[string]any{
			"static-a": map[string]any{"type": "http", "url": "http://a"},
			"static-b": map[string]any{"type": "stdio", "command": "b"},
		},
		GatewayAddr:  "127.0.0.1:9",
		GatewayToken: "tok",
	}
	// An agent id is set — under the OLD code this would have triggered the
	// AgentMCPLookup DB direct-inject. It must NOT add any DB server key now.
	keys := writeAndReadKeys(t, d, BridgeContext{AgentID: "11111111-1111-1111-1111-111111111111", UserID: "u1"})

	want := map[string]bool{"static-a": true, "static-b": true, "goclaw-bridge": true}
	if len(keys) != len(want) {
		t.Fatalf("key set = %v, want exactly %v", keysOf(keys), want)
	}
	for k := range keys {
		if !want[k] {
			t.Fatalf("unexpected mcpServers key %q (per-agent DB server must NOT be injected); got %v", k, keysOf(keys))
		}
	}
}

// Negative: with NO static servers but a gateway + agent, the only key is
// goclaw-bridge — never a DB server.
func TestWriteMCPConfig_OnlyBridgeWhenNoStatic(t *testing.T) {
	d := &MCPConfigData{GatewayAddr: "127.0.0.1:9", GatewayToken: "tok"}
	keys := writeAndReadKeys(t, d, BridgeContext{AgentID: "22222222-2222-2222-2222-222222222222", UserID: "u1"})
	if len(keys) != 1 {
		t.Fatalf("expected only goclaw-bridge, got %v", keysOf(keys))
	}
	if _, ok := keys["goclaw-bridge"]; !ok {
		t.Fatalf("expected goclaw-bridge key, got %v", keysOf(keys))
	}
}

// condition 4 (§11 §3.3 nil-guard): an empty config (no Servers, no GatewayAddr)
// returns "" early — the removal of the `&& d.AgentMCPLookup == nil` clause did
// NOT change this behavior.
func TestWriteMCPConfig_EmptyReturnsEmpty(t *testing.T) {
	t.Setenv("GOCLAW_DATA_DIR", t.TempDir())
	d := &MCPConfigData{}
	if path := d.WriteMCPConfig(context.Background(), "sess-empty", BridgeContext{}); path != "" {
		t.Fatalf("empty config must return empty path, got %q", path)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
