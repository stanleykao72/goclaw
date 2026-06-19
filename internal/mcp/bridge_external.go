package mcp

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// makeSeedExternalTools builds the WithHTTPContextFunc that force-routes every
// per-agent external MCP server through this bridge (docs/26 §11 i-3a, §11.S
// steps 13-14). It runs once per POST: handlePost binds the ephemeral session to
// ctx BEFORE invoking the context func (mcp-go v0.44.0 streamable_http.go:383
// then :385), so we can resolve the agent's external tools for the verified
// actor and seed them onto the session via SetSessionTools. handleListTools and
// handleToolCall both consult GetSessionTools, so the tools are visible to BOTH
// tools/list and tools/call (server.go:1368 / :1442).
//
// Trust boundary (docs/26 §11 §2.5 S5 / condition C4): identity is derived
// EXCLUSIVELY from the HMAC-verified ctx populated by bridgeContextMiddleware,
// NEVER from r.Header. A request whose signature did not verify never reaches a
// populated ctx (agentID/tenantID stay zero) → fail-closed, zero external tools.
//
// Per-request isolation (docs/26 §11 §5 condition 6 / C7): the bridge is built
// with WithSessionIdManager(&StatelessGeneratingSessionIdManager{}) so each
// session id is a fresh UUID — external tools live ONLY in this request's
// session-tool map, never in the process-global registry (C8), so a concurrent
// request for another actor can never see or name-guess them.
func makeSeedExternalTools(st store.MCPServerStore, pool *Pool, gc GrantChecker, msgBus *bus.MessageBus) mcpserver.HTTPContextFunc {
	return func(ctx context.Context, _ *http.Request) context.Context {
		// S5/C4: identity ONLY from the verified ctx, never r.Header.
		agentID := store.AgentIDFromContext(ctx)
		tenantID := store.TenantIDFromContext(ctx)
		// S4/C5 fail-closed: without a verified agent + tenant there is no
		// scope for ListAccessible — surface zero external tools.
		if agentID == uuid.Nil || tenantID == uuid.Nil {
			return ctx
		}
		userID := store.UserIDFromContext(ctx)
		senderID := store.SenderIDFromContext(ctx)
		channelType := tools.ToolChannelTypeFromCtx(ctx)
		peerKind := tools.ToolPeerKindFromCtx(ctx)

		extTools, actorID, err := ResolveExternalBridgeTools(ctx, st, pool, gc,
			tenantID, agentID, userID, senderID, peerKind, channelType)
		if err != nil || len(extTools) == 0 {
			return ctx
		}

		session := mcpserver.ClientSessionFromContext(ctx)
		swt, ok := session.(mcpserver.SessionWithTools)
		if !ok {
			return ctx
		}

		serverTools := make(map[string]mcpserver.ServerTool, len(extTools))
		for _, t := range extTools {
			bt, ok := t.(*BridgeTool)
			if !ok {
				continue
			}
			serverTools[bt.Name()] = mcpserver.ServerTool{
				Tool:    convertBridgeToMCPTool(bt),
				Handler: makeExternalToolHandler(bt, actorID, msgBus),
			}
		}
		swt.SetSessionTools(serverTools)
		return ctx
	}
}

// makeExternalToolHandler wraps a per-actor BridgeTool's Execute as an mcp-go
// tool handler. It re-keys the execute-time ctx UserID on the RESOLVED actor
// (docs/26 §11 §2.7 / condition 3) so the grant recheck in BridgeTool.Execute
// (grantChecker.IsAllowed, bridge_tool.go:188-192) keys on the actor, not the
// group-scoped X-User-ID. The override is scoped to THIS external tool's
// execution ONLY — builtin bridge tools (memory etc.) keep the group userID, so
// group-shared memory is NOT split per-sender.
//
// The closure binds THIS request's bt (captured serverID + per-actor connection),
// so mcp-go's global tool fallthrough can never resolve another actor's tool by
// name (docs/26 §11 C8).
func makeExternalToolHandler(bt *BridgeTool, actorID string, msgBus *bus.MessageBus) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if actorID != "" {
			ctx = store.WithUserID(ctx, actorID)
		}
		result := bt.Execute(ctx, req.GetArguments())
		if result == nil {
			return mcpgo.NewToolResultError("bridge tool returned no result"), nil
		}
		if result.IsError {
			return mcpgo.NewToolResultError(result.ForLLM), nil
		}
		// Forward media files to the outbound bus so they reach the user — the
		// Claude CLI processes tool results internally and the agent loop never
		// sees result.Media from bridge tool calls (same as builtin path).
		forwardMediaToOutbound(ctx, msgBus, bt.Name(), result)
		return mcpgo.NewToolResultText(result.ForLLM), nil
	}
}

// convertBridgeToMCPTool converts a per-agent external BridgeTool into an mcp-go
// Tool (docs/26 §11.S step 14 / C11). Mirrors convertToMCPTool's marshal-error
// fallback but with a richer empty schema ({"type":"object","properties":{}})
// matching inputSchemaToMap's OpenAI-strict shape (bridge_tool.go:325-328). Sets
// ONLY RawInputSchema (NewToolWithRawSchema) to avoid the InputSchema/
// RawInputSchema conflict path.
func convertBridgeToMCPTool(bt *BridgeTool) mcpgo.Tool {
	schema, err := json.Marshal(bt.Parameters())
	if err != nil {
		schema = []byte(`{"type":"object","properties":{}}`)
	}
	return mcpgo.NewToolWithRawSchema(bt.Name(), bt.Description(), schema)
}

// sessionToolCleanup wraps the bridge StreamableHTTPServer to delete each
// request's seeded session-tool entry after the POST completes (docs/26 §11 §5
// condition 6 / C6 / R7). The one-shot `claude --print` path never sends a DELETE
// (mcp-go deletes the entry only on terminate, streamable_http.go:694-695), so
// without this the shared sessionToolsStore grows unbounded (one entry per
// session id per message → memory DoS).
//
// makeSeedExternalTools re-seeds on EVERY POST (handlePost always runs the
// context func), so deleting after each POST is safe: a follow-up tools/call in
// the same CLI run re-seeds before handleToolCall reads the session tools.
type sessionToolCleanup struct {
	bridge *mcpserver.StreamableHTTPServer
}

func (c *sessionToolCleanup) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cap := &sessionIDCapture{ResponseWriter: w}
	c.bridge.ServeHTTP(cap, r)

	if r.Method != http.MethodPost {
		return
	}
	// Session id is in the response header on initialize, and supplied by the
	// client on every subsequent POST.
	sid := cap.sessionID
	if sid == "" {
		sid = r.Header.Get("Mcp-Session-Id")
	}
	if sid == "" {
		return
	}
	// Fire a terminate straight at the StreamableHTTPServer (bypassing the
	// auth/context middleware) so handleDelete evicts the session-tool entry.
	// StatelessGeneratingSessionIdManager.Terminate is a no-op that permits the
	// delete (streamable_http.go:1360), so the store entry is reclaimed.
	del, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, r.URL.Path, nil)
	if err != nil {
		return
	}
	del.Header.Set("Mcp-Session-Id", sid)
	c.bridge.ServeHTTP(discardResponseWriter{}, del)
}

// sessionIDCapture records the Mcp-Session-Id the bridge writes on the response
// (set for the initialize reply, streamable_http.go:491) while passing the
// response through untouched. It preserves http.Flusher for the SSE paths.
type sessionIDCapture struct {
	http.ResponseWriter
	sessionID string
	captured  bool
}

func (c *sessionIDCapture) capture() {
	if !c.captured {
		c.sessionID = c.Header().Get("Mcp-Session-Id")
		c.captured = true
	}
}

func (c *sessionIDCapture) WriteHeader(code int) {
	c.capture()
	c.ResponseWriter.WriteHeader(code)
}

func (c *sessionIDCapture) Write(b []byte) (int, error) {
	c.capture()
	return c.ResponseWriter.Write(b)
}

func (c *sessionIDCapture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// discardResponseWriter swallows the internal terminate request's response.
type discardResponseWriter struct{}

func (discardResponseWriter) Header() http.Header        { return http.Header{} }
func (discardResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (discardResponseWriter) WriteHeader(int)            {}
