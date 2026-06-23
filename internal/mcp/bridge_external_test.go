package mcp

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// memoryModeToolFilter must hide the wrong-subsystem memory family by the
// per-agent memory mode while leaving every non-memory tool untouched:
//   - notebook → hide {memory_search, memory_get, memory_expand}
//   - vault    → hide {notebook_recall, remember_shared, remember_agent}
//   - both / unset → expose all (safe default)
func TestMemoryModeToolFilter_HidesWrongFamily(t *testing.T) {
	mk := func(name string) mcpgo.Tool {
		return mcpgo.NewToolWithRawSchema(name, name, json.RawMessage(`{"type":"object"}`))
	}
	// Full advertised set: all 6 memory tools + a couple of non-memory tools that
	// must ALWAYS survive the filter regardless of mode.
	full := []mcpgo.Tool{
		mk("memory_search"), mk("memory_get"), mk("memory_expand"),
		mk("notebook_recall"), mk("remember_shared"), mk("remember_agent"),
		mk("read_file"), mk("exec"), mk("odoo-prod__query"),
	}
	names := func(list []mcpgo.Tool) map[string]bool {
		m := make(map[string]bool, len(list))
		for _, tool := range list {
			m[tool.Name] = true
		}
		return m
	}

	cases := []struct {
		name    string
		mode    string // "" => unset
		present []string
		absent  []string
	}{
		{
			name:    "notebook hides vault trio",
			mode:    store.MemoryModeNotebook,
			present: []string{"notebook_recall", "remember_shared", "remember_agent", "read_file", "exec", "odoo-prod__query"},
			absent:  []string{"memory_search", "memory_get", "memory_expand"},
		},
		{
			name:    "vault hides notebook trio",
			mode:    store.MemoryModeVault,
			present: []string{"memory_search", "memory_get", "memory_expand", "read_file", "exec", "odoo-prod__query"},
			absent:  []string{"notebook_recall", "remember_shared", "remember_agent"},
		},
		{
			name: "both keeps all",
			mode: store.MemoryModeBoth,
			present: []string{
				"memory_search", "memory_get", "memory_expand",
				"notebook_recall", "remember_shared", "remember_agent",
				"read_file", "exec", "odoo-prod__query",
			},
		},
		{
			name: "unset defaults to both (all exposed)",
			mode: "",
			present: []string{
				"memory_search", "memory_get", "memory_expand",
				"notebook_recall", "remember_shared", "remember_agent",
				"read_file", "exec", "odoo-prod__query",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.mode != "" {
				ctx = store.WithMemoryMode(ctx, tc.mode)
			}
			// Copy the input: the filter reuses the backing array (list[:0]); a
			// shared slice across subtests would corrupt later cases.
			in := append([]mcpgo.Tool(nil), full...)
			got := names(memoryModeToolFilter(ctx, in))
			for _, n := range tc.present {
				if !got[n] {
					t.Errorf("mode %q: tool %q must be present, exposed set = %v", tc.mode, n, got)
				}
			}
			for _, n := range tc.absent {
				if got[n] {
					t.Errorf("mode %q: tool %q must be HIDDEN, exposed set = %v", tc.mode, n, got)
				}
			}
		})
	}
}

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
