package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// BridgeToolNames is the subset of GoClaw tools exposed via the MCP bridge.
// Excluded: spawn (agent loop), create_forum_topic (channels).
var BridgeToolNames = map[string]bool{
	// Filesystem
	"read_file":  true,
	"write_file": true,
	"list_files": true,
	"edit":       true,
	"exec":       true,
	// Web
	"web_search": true,
	"web_fetch":  true,
	// Memory & knowledge
	"memory_search":   true,
	"memory_get":      true,
	"notebook_recall": true,
	"remember_shared": true,
	"remember_agent":  true,
	"skill_search":    true,
	"use_skill":       true,
	// Media
	"read_image":   true,
	"create_image": true,
	"tts":          true,
	// Browser automation
	"browser": true,
	// Scheduler
	"cron": true,
	// Messaging (send text/files to channels)
	"message": true,
	// Sessions (read + send)
	"sessions_list":    true,
	"session_status":   true,
	"sessions_history": true,
	"sessions_send":    true,
	// Team tools (context from X-Agent-ID/X-Channel/X-Chat-ID headers)
	"team_tasks": true,
}

// Memory-tool families gated by the per-agent memory mode. The VAULT family is
// hidden from a notebook-mode agent and the NOTEBOOK family from a vault-mode
// agent, so the LLM never sees (and therefore never probes) a tool that would
// only no-op for its mode. Mode "both" / unset exposes all (default — safe).
//
// These names are a SUBSET of BridgeToolNames; the filter touches ONLY these six
// names. Every other registered bridge tool (read_file, exec, web_search, the
// per-agent external/odoo MCP tools, …) is left untouched. memory_expand is not
// currently in BridgeToolNames (so never bridge-exposed) but is named here for
// registry correctness — naming an absent tool in the filter is a harmless no-op.
var (
	vaultMemoryTools = map[string]bool{
		"memory_search": true,
		"memory_get":    true,
		"memory_expand": true,
	}
	notebookMemoryTools = map[string]bool{
		"notebook_recall": true,
		"remember_shared": true,
		"remember_agent":  true,
	}
)

// memoryModeToolFilter narrows the advertised tool list by the request's resolved
// memory mode. It runs at tools/list time (mcp-go applies registered filters in
// handleListTools AFTER the global+session tool merge, with the live request ctx),
// so it sees the full advertised set and can hide process-global builtins that
// SetSessionTools cannot. handleToolCall does NOT apply filters, so a hidden tool
// that is somehow still invoked resolves to its global handler and hits the
// existing Execute-time no-op gate — defense in depth preserved.
func memoryModeToolFilter(ctx context.Context, list []mcpgo.Tool) []mcpgo.Tool {
	var hidden map[string]bool
	switch store.MemoryModeFromCtx(ctx) {
	case store.MemoryModeNotebook:
		hidden = vaultMemoryTools // notebook agent → hide the vault family
	case store.MemoryModeVault:
		hidden = notebookMemoryTools // vault agent → hide the notebook family
	default:
		return list // "both" / unknown → expose all (safe default)
	}
	out := list[:0]
	for _, t := range list {
		if hidden[t.Name] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// NewBridgeServer creates a StreamableHTTPServer that exposes GoClaw tools as MCP tools.
// It reads tools from the registry, filters to BridgeToolNames, and serves them
// over streamable-http transport (stateless mode).
// msgBus is optional; when non-nil, tools that produce media (deliver:true) will
// publish file attachments directly to the outbound bus.
func NewBridgeServer(reg *tools.Registry, version string, msgBus *bus.MessageBus, mcpStore store.MCPServerStore, pool *Pool, grantChecker GrantChecker) http.Handler {
	srv := mcpserver.NewMCPServer("goclaw-bridge", version,
		mcpserver.WithToolCapabilities(false),
		// Per-request visibility gate: hide the wrong-subsystem memory tools so a
		// notebook/vault agent's LLM never probes a tool that would only no-op.
		// BridgeToolNames stays the registered superset; the filter only narrows
		// what each request advertises. The Execute-time no-op gates remain.
		mcpserver.WithToolFilter(memoryModeToolFilter),
	)

	// Register each safe tool from the GoClaw registry. These are SHARED builtins
	// (no per-user state) and live in the process-global tool set — unchanged.
	var registered int
	for name := range BridgeToolNames {
		t, ok := reg.Get(name)
		if !ok {
			continue
		}

		mcpTool := convertToMCPTool(t)
		handler := makeToolHandler(reg, name, msgBus)
		srv.AddTool(mcpTool, handler)
		registered++
	}

	slog.Info("mcp.bridge: tools registered", "count", registered)

	// Force-route per-agent external MCP servers through the bridge (docs/26
	// §11 i-3a). Only when the MCP store + pool are wired: each request resolves
	// the agent's external tools for the verified actor and seeds them onto the
	// ephemeral session (makeSeedExternalTools). This REQUIRES a per-request
	// unique session id so the per-actor tool sets never collide — use
	// StatelessGeneratingSessionIdManager (fresh UUID per session), NOT
	// WithStateLess(true) (whose StatelessSessionIdManager generates "" and would
	// collapse every concurrent request onto one shared key).
	if mcpStore != nil && pool != nil {
		streamable := mcpserver.NewStreamableHTTPServer(srv,
			mcpserver.WithSessionIdManager(&mcpserver.StatelessGeneratingSessionIdManager{}),
			mcpserver.WithHTTPContextFunc(makeSeedExternalTools(mcpStore, pool, grantChecker, msgBus)),
		)
		// Wrap so each request's seeded session-tool entry is reclaimed (the
		// one-shot CLI never sends DELETE → unbounded growth otherwise).
		return &sessionToolCleanup{bridge: streamable}
	}

	// Builtin-only bridge (no external force-route): unchanged stateless mode.
	return mcpserver.NewStreamableHTTPServer(srv,
		mcpserver.WithStateLess(true),
	)
}

// convertToMCPTool converts a GoClaw tools.Tool into an mcp-go Tool.
func convertToMCPTool(t tools.Tool) mcpgo.Tool {
	schema, err := json.Marshal(t.Parameters())
	if err != nil {
		// Fallback: empty object schema
		schema = []byte(`{"type":"object"}`)
	}
	return mcpgo.NewToolWithRawSchema(t.Name(), t.Description(), schema)
}

// makeToolHandler creates a ToolHandlerFunc that delegates to the GoClaw tool registry.
// When msgBus is non-nil and a tool result contains Media paths, the handler publishes
// them as outbound media attachments so files reach the user (e.g. Telegram document).
func makeToolHandler(reg *tools.Registry, toolName string, msgBus *bus.MessageBus) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		args := req.GetArguments()

		// Pass routing context (channel, chatID, peerKind, sessionKey) so native
		// tools can access local_key, session_key etc. for forum topic routing.
		result := reg.ExecuteWithContext(ctx, toolName, args,
			tools.ToolChannelFromCtx(ctx),
			tools.ToolChatIDFromCtx(ctx),
			tools.ToolPeerKindFromCtx(ctx),
			tools.ToolSessionKeyFromCtx(ctx),
			nil,
		)

		if result.IsError {
			return mcpgo.NewToolResultError(result.ForLLM), nil
		}

		// Forward media files to the outbound bus so they reach the user as attachments.
		// This is necessary because Claude CLI processes tool results internally —
		// GoClaw's agent loop never sees result.Media from bridge tool calls.
		forwardMediaToOutbound(ctx, msgBus, toolName, result)

		return mcpgo.NewToolResultText(result.ForLLM), nil
	}
}

// forwardMediaToOutbound publishes media files from a tool result to the outbound bus.
func forwardMediaToOutbound(ctx context.Context, msgBus *bus.MessageBus, toolName string, result *tools.Result) {
	if msgBus == nil || len(result.Media) == 0 {
		return
	}
	channel := tools.ToolChannelFromCtx(ctx)
	chatID := tools.ToolChatIDFromCtx(ctx)
	if channel == "" || chatID == "" {
		slog.Debug("mcp.bridge: skipping media forward, missing channel context",
			"tool", toolName, "channel", channel, "chat_id", chatID)
		return
	}

	var attachments []bus.MediaAttachment
	for _, mf := range result.Media {
		ct := mf.MimeType
		if ct == "" {
			ct = mimeFromExt(filepath.Ext(mf.Path))
		}
		attachments = append(attachments, bus.MediaAttachment{
			URL:         mf.Path,
			ContentType: ct,
		})
	}

	peerKind := tools.ToolPeerKindFromCtx(ctx)
	var meta map[string]string
	if peerKind == "group" {
		meta = map[string]string{"group_id": chatID}
	}
	msgBus.PublishOutbound(bus.OutboundMessage{
		Channel:  channel,
		ChatID:   chatID,
		Media:    attachments,
		Metadata: meta,
	})
	slog.Debug("mcp.bridge: forwarded media to outbound bus",
		"tool", toolName, "channel", channel, "files", len(attachments))
}

// mimeFromExt returns a MIME type for a file extension.
// Uses Go stdlib first, falls back to a small map for types not reliably
// handled by mime.TypeByExtension on all platforms (e.g. .opus, .webp).
func mimeFromExt(ext string) string {
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	switch strings.ToLower(ext) {
	case ".webp":
		return "image/webp"
	case ".opus":
		return "audio/ogg"
	case ".md":
		return "text/markdown"
	default:
		return "application/octet-stream"
	}
}
