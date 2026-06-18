package providers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/config"
)

// MCPServerEntry represents a single MCP server config for CLI injection.
type MCPServerEntry struct {
	Name      string
	Transport string // "stdio", "sse", "streamable-http"
	Command   string
	Args      []string
	URL       string
	Headers   map[string]string
	Env       map[string]string
}

// MCPServerLookup returns accessible MCP servers for a given agent ID.
// Used to inject per-agent DB-backed MCP servers into CLI MCP config.
type MCPServerLookup func(ctx context.Context, agentID string) []MCPServerEntry

// MCPConfigData holds the base MCP server entries built at startup.
// Per-session configs are written via WriteMCPConfig with agent context injected.
type MCPConfigData struct {
	Servers        map[string]any // external MCP server entries (stdio/sse/http)
	GatewayAddr    string
	GatewayToken   string
	AgentMCPLookup MCPServerLookup // optional: resolves per-agent MCP servers from DB
}

// BuildCLIMCPConfigData builds the base MCP server map from config.
// Does NOT include the goclaw-bridge entry — that's added per-session
// with agent context headers in WriteMCPConfig.
func BuildCLIMCPConfigData(servers map[string]*config.MCPServerConfig, gatewayAddr string, gatewayToken ...string) *MCPConfigData {
	mcpServers := make(map[string]any, len(servers))

	for name, srv := range servers {
		if !srv.IsEnabled() {
			continue
		}
		entry := mcpServerEntryToConfig(MCPServerEntry{
			Name:      name,
			Transport: srv.Transport,
			Command:   srv.Command,
			Args:      srv.Args,
			URL:       srv.URL,
			Headers:   srv.Headers,
			Env:       srv.Env,
		})
		if len(entry) > 0 {
			mcpServers[name] = entry
		}
	}

	token := ""
	if len(gatewayToken) > 0 {
		token = gatewayToken[0]
	}

	return &MCPConfigData{
		Servers:      mcpServers,
		GatewayAddr:  gatewayAddr,
		GatewayToken: token,
	}
}

// mcpConfigBaseDir returns dataDir/mcp-configs, separate from workDir
// so agent cannot read tokens from the MCP config files.
func mcpConfigBaseDir() string {
	return filepath.Join(config.ResolvedDataDirFromEnv(), "mcp-configs")
}

// BridgeContext holds per-call context for MCP bridge headers.
type BridgeContext struct {
	AgentID   string
	UserID    string
	Channel   string
	ChatID    string
	PeerKind  string
	Workspace string
	TenantID  string
	LocalKey  string
	// SenderID is the individual sender (distinct from the group-scoped UserID).
	// Emitted as X-Sender-ID and folded into the HMAC as a trailing extra so it
	// cannot be forged.
	SenderID string
	// ChannelType is the platform discriminator (e.g. "lineworks"), distinct
	// from Channel (the instance name). Emitted as X-Channel-Type and folded
	// into the HMAC alongside SenderID.
	ChannelType string
}

// WriteMCPConfig writes a per-session MCP config file with agent context headers.
// Files are stored at ~/.goclaw/mcp-configs/<safe-session-key>/mcp-config.json,
// outside the agent's workDir so tokens are not exposed.
// Skips write if content is unchanged. Returns the file path.
func (d *MCPConfigData) WriteMCPConfig(ctx context.Context, sessionKey string, bc BridgeContext) string {
	return d.writeMCPConfigInternal(ctx, sessionKey, bc.AgentID, bc.UserID, bc.Channel, bc.ChatID, bc.PeerKind, bc.Workspace, bc.TenantID, bc.LocalKey, bc.SenderID, bc.ChannelType)
}

func (d *MCPConfigData) writeMCPConfigInternal(ctx context.Context, sessionKey, agentID, userID, channel, chatID, peerKind, workspace, tenantID, localKey, senderID, channelType string) string {
	if d == nil || (len(d.Servers) == 0 && d.GatewayAddr == "" && d.AgentMCPLookup == nil) {
		return ""
	}

	// Shallow-copy the outer map so we can add the bridge entry without mutating the shared base.
	// Inner server entries are not modified, so shallow copy is sufficient.
	servers := make(map[string]any, len(d.Servers)+1)
	maps.Copy(servers, d.Servers)

	// Inject per-agent MCP servers from DB (if lookup is configured and agentID is set)
	if d.AgentMCPLookup != nil && agentID != "" {
		for _, srv := range d.AgentMCPLookup(ctx, agentID) {
			if _, exists := servers[srv.Name]; exists {
				continue // don't override static/bridge entries
			}
			entry := mcpServerEntryToConfig(srv)
			if len(entry) > 0 {
				servers[srv.Name] = entry
			}
		}
	}

	// Build bridge entry with per-session agent context headers
	if d.GatewayAddr != "" {
		headers := make(map[string]string)
		if d.GatewayToken != "" {
			headers["Authorization"] = "Bearer " + d.GatewayToken
		}
		if agentID != "" && !strings.ContainsAny(agentID, "\r\n\x00") {
			headers["X-Agent-ID"] = agentID
		}
		if userID != "" && !strings.ContainsAny(userID, "\r\n\x00") {
			headers["X-User-ID"] = userID
		}
		if channel != "" && !strings.ContainsAny(channel, "\r\n\x00") {
			headers["X-Channel"] = channel
		}
		if chatID != "" && !strings.ContainsAny(chatID, "\r\n\x00") {
			headers["X-Chat-ID"] = chatID
		}
		if peerKind != "" && !strings.ContainsAny(peerKind, "\r\n\x00") {
			headers["X-Peer-Kind"] = peerKind
		}
		if workspace != "" && !strings.ContainsAny(workspace, "\r\n\x00") {
			headers["X-Workspace"] = workspace
		}
		if tenantID != "" && !strings.ContainsAny(tenantID, "\r\n\x00") {
			headers["X-Tenant-ID"] = tenantID
		}
		if localKey != "" && !strings.ContainsAny(localKey, "\r\n\x00") {
			headers["X-Local-Key"] = localKey
		}
		if senderID != "" && !strings.ContainsAny(senderID, "\r\n\x00") {
			headers["X-Sender-ID"] = senderID
		}
		if channelType != "" && !strings.ContainsAny(channelType, "\r\n\x00") {
			headers["X-Channel-Type"] = channelType
		}
		if sessionKey != "" && !strings.ContainsAny(sessionKey, "\r\n\x00") {
			headers["X-Session-Key"] = sessionKey
		}
		// HMAC signature over all context fields to prevent header forgery.
		// channelType + senderID are appended as the 3rd/4th TRAILING extras
		// after localKey, sessionKey — ALWAYS (even when empty) so
		// VerifyBridgeContext can gate senderVerified on the full tier. Configs
		// predating these extras match the [localKey, sessionKey] fallback tier
		// (flag-day-free). Reordering would break in-flight verification.
		if d.GatewayToken != "" && (agentID != "" || userID != "") {
			headers["X-Bridge-Sig"] = SignBridgeContext(d.GatewayToken, agentID, userID, channel, chatID, peerKind, workspace, tenantID, localKey, sessionKey, channelType, senderID)
		}

		bridgeEntry := map[string]any{
			"url":  fmt.Sprintf("http://%s/mcp/bridge", d.GatewayAddr),
			"type": "http",
		}
		if len(headers) > 0 {
			bridgeEntry["headers"] = headers
		}
		servers["goclaw-bridge"] = bridgeEntry
	}

	if len(servers) == 0 {
		return ""
	}

	data, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		slog.Warn("claude-cli: failed to marshal mcp config", "error", err)
		return ""
	}

	// Write to per-session dir outside workDir
	safe := sanitizePathSegment(sessionKey)
	dir := filepath.Join(mcpConfigBaseDir(), safe)
	if err := os.MkdirAll(dir, 0700); err != nil {
		slog.Warn("claude-cli: failed to create mcp config dir", "error", err)
		return ""
	}

	path := filepath.Join(dir, "mcp-config.json")

	// Skip write if unchanged
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(data) {
		return path
	}
	// Atomic write: temp file + rename to prevent partial reads
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		slog.Warn("claude-cli: failed to write mcp config tmp", "error", err)
		return ""
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		slog.Warn("claude-cli: failed to rename mcp config", "error", err)
		return ""
	}

	return path
}

// mcpServerEntryToConfig converts an MCPServerEntry to the CLI MCP config format.
func mcpServerEntryToConfig(srv MCPServerEntry) map[string]any {
	entry := make(map[string]any)
	switch srv.Transport {
	case "stdio":
		if srv.Command != "" {
			entry["command"] = srv.Command
		}
		if len(srv.Args) > 0 {
			entry["args"] = srv.Args
		}
		if len(srv.Env) > 0 {
			entry["env"] = srv.Env
		}
	case "sse":
		if srv.URL != "" {
			entry["url"] = srv.URL
			entry["type"] = "sse"
		}
		if len(srv.Headers) > 0 {
			entry["headers"] = srv.Headers
		}
	case "streamable-http":
		if srv.URL != "" {
			entry["url"] = srv.URL
			entry["type"] = "http"
		}
		if len(srv.Headers) > 0 {
			entry["headers"] = srv.Headers
		}
	}
	return entry
}

// sanitizePathSegment makes a string safe for use as a single filesystem directory name.
// Replaces path separators and special chars, strips null bytes, handles ".." traversal,
// and truncates to 255 chars.
func sanitizePathSegment(s string) string {
	safe := strings.NewReplacer(":", "-", "/", "-", "\\", "-", "\x00", "").Replace(s)
	// Collapse any ".." sequences to prevent traversal
	safe = strings.ReplaceAll(safe, "..", "_")
	if len(safe) > 255 {
		safe = safe[:255]
	}
	if safe == "" || safe == "." {
		safe = "default"
	}
	return safe
}

// SignBridgeContext computes HMAC-SHA256 over all bridge context fields to prevent forgery.
// Payload: agentID|userID|channel|chatID|peerKind|workspace|tenantID
func SignBridgeContext(key, agentID, userID, channel, chatID, peerKind, workspace, tenantID string, extra ...string) string {
	mac := hmac.New(sha256.New, []byte(key))
	var payload strings.Builder
	payload.WriteString(agentID + "|" + userID + "|" + channel + "|" + chatID + "|" + peerKind + "|" + workspace + "|" + tenantID)
	for _, e := range extra {
		payload.WriteString("|" + e)
	}
	mac.Write([]byte(payload.String()))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyBridgeContext checks the HMAC signature against the expected bridge context.
// Returns (ok, tenantVerified, senderVerified):
//   - ok: the signature matches some (possibly backward-compat) tier.
//   - tenantVerified: the tenantID field was HMAC-covered (false on pre-tenantID tiers).
//   - senderVerified: the channelType+senderID extras (positions 2,3) were HMAC-covered.
//     True ONLY at the full tier. Callers must NOT trust X-Channel-Type / X-Sender-ID
//     when false (forge resistance: an attacker with an old-format sig who injects a
//     sender header lands on a fallback tier → senderVerified=false).
//
// Falls back to older formats for sessions whose MCP config predates a field.
func VerifyBridgeContext(key, agentID, userID, channel, chatID, peerKind, workspace, tenantID, sig string, extra ...string) (bool, bool, bool) {
	// L1 — full extras (localKey|sessionKey|channelType|senderID). The ONLY tier
	// covering the sender extras, so the ONLY tier returning senderVerified=true,
	// and only when those extras are actually present (len>=4).
	expected := SignBridgeContext(key, agentID, userID, channel, chatID, peerKind, workspace, tenantID, extra...)
	if hmac.Equal([]byte(expected), []byte(sig)) {
		return true, true, len(extra) >= 4
	}
	// L2 — localKey+sessionKey only (config predates channelType/senderID, or the
	// J-rollout transition window). tenantID still covered; sender extras are not.
	if len(extra) >= 2 {
		base := SignBridgeContext(key, agentID, userID, channel, chatID, peerKind, workspace, tenantID, extra[:2]...)
		if hmac.Equal([]byte(base), []byte(sig)) {
			return true, true, false
		}
	}
	// L3 — without extra fields (pre-localKey sessions).
	noExtra := SignBridgeContext(key, agentID, userID, channel, chatID, peerKind, workspace, tenantID)
	if hmac.Equal([]byte(noExtra), []byte(sig)) {
		return true, true, false
	}
	// L4 — without tenantID (pre-tenantID sessions).
	noTenant := SignBridgeContext(key, agentID, userID, channel, chatID, peerKind, workspace, "")
	if hmac.Equal([]byte(noTenant), []byte(sig)) {
		return true, false, false
	}
	// L5 — without workspace or tenantID (oldest sessions).
	old := SignBridgeContext(key, agentID, userID, channel, chatID, peerKind, "", "")
	if hmac.Equal([]byte(old), []byte(sig)) {
		return true, false, false
	}
	return false, false, false
}
