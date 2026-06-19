package mcp

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// convertBridgeToMCPTool must name the mcp-go tool after the BridgeTool's
// registered (prefixed) name and carry a valid raw input schema (docs/26 §11
// C11).
func TestConvertBridgeToMCPTool(t *testing.T) {
	mcpTool := mcpgo.NewToolWithRawSchema("query", "run a query", json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`))
	var clientPtr atomic.Pointer[mcpclient.Client]
	var connected atomic.Bool
	bt := NewBridgeTool("odoo-prod", mcpTool, &clientPtr, "", 30, &connected, uuid.New(), nil)

	got := convertBridgeToMCPTool(bt)
	if got.Name != bt.Name() {
		t.Fatalf("converted tool name = %q, want %q (registered/prefixed name)", got.Name, bt.Name())
	}
	if len(got.RawInputSchema) == 0 {
		t.Fatal("converted tool must carry a raw input schema")
	}
	var schema map[string]any
	if err := json.Unmarshal(got.RawInputSchema, &schema); err != nil {
		t.Fatalf("raw input schema is not valid JSON: %v", err)
	}
}

// makeSeedExternalTools must fail closed: a request whose ctx carries no
// verified agent/tenant (signature did not verify → middleware injected nothing)
// resolves ZERO external tools and returns the ctx untouched, never reading
// r.Header (docs/26 §11 §2.5 S4/S5, C4/C5).
func TestSeedExternalTools_FailClosedNoVerifiedCtx(t *testing.T) {
	st := newFakeBridgeStore(nil)
	pool := NewPool(PoolConfig{})
	seed := makeSeedExternalTools(st, pool, nil, nil)

	base := context.Background()
	got := seed(base, nil) // r is unused on the fail-closed path; nil proves headers aren't read
	if got != base {
		t.Fatal("fail-closed path must return the ctx unchanged")
	}
	if atomic.LoadInt32(&st.listCalls) != 0 {
		t.Fatal("must not call ListAccessible without a verified agent/tenant")
	}
}
