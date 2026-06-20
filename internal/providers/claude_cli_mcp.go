package providers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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

// bridgeHeaderEncPrefix marks a base64url-encoded bridge context header value.
//
// HTTP header field-values must be ISO-8859-1 (latin-1). The Node-based `claude`
// CLI's MCP HTTP client REJECTS non-latin-1 header bytes, so when a bridge
// context header carries non-ASCII content — e.g. `X-Channel`, `X-Session-Key`
// or `X-Workspace` derived from a CJK group name like "主管群" — the CLI fails
// to connect to goclaw-bridge entirely (mcp_servers status "failed"), making
// ALL bridge tools (builtins AND force-routed external MCP tools) unavailable
// for that session. The old CRLF-only guard did not catch this.
//
// EncodeBridgeHeaderValue keeps pure printable-ASCII values verbatim (no
// behavior change, no HMAC impact) and base64url-encodes anything else behind
// this sentinel. DecodeBridgeHeaderValue (called by the gateway middleware)
// reverses it. The HMAC is always computed and verified over the RAW (decoded)
// value, so signing/verification is unaffected by transport encoding.
const bridgeHeaderEncPrefix = "=?b64?"

// EncodeBridgeHeaderValue returns an ASCII-safe transport encoding of a bridge
// context header value. See bridgeHeaderEncPrefix.
func EncodeBridgeHeaderValue(v string) string {
	if isPrintableASCII(v) {
		return v
	}
	return bridgeHeaderEncPrefix + base64.RawURLEncoding.EncodeToString([]byte(v))
}

// DecodeBridgeHeaderValue reverses EncodeBridgeHeaderValue. A value without the
// sentinel (legacy ASCII headers, or any non-encoded value) is returned as-is.
// A malformed encoded value is returned verbatim so HMAC verification then
// fail-closes rather than trusting a corrupted value.
func DecodeBridgeHeaderValue(v string) string {
	if !strings.HasPrefix(v, bridgeHeaderEncPrefix) {
		return v
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(v, bridgeHeaderEncPrefix))
	if err != nil {
		return v
	}
	return string(raw)
}

// isPrintableASCII reports whether s contains only printable ASCII (0x20-0x7E),
// i.e. it is safe to emit verbatim as an HTTP header value.
func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

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

// MCPConfigData holds the base MCP server entries built at startup.
// Per-session configs are written via WriteMCPConfig with agent context injected.
//
// Per-agent DB-backed external MCP servers are NO LONGER injected here (docs/26
// §11 i-3b): they are force-routed through the goclaw-bridge so every external
// tool call traverses grant recheck + per-user credential resolution. Only
// static config-file servers (Servers) plus the goclaw-bridge entry are written.
type MCPConfigData struct {
	Servers      map[string]any // static config-file MCP server entries (stdio/sse/http)
	GatewayAddr  string
	GatewayToken string
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
	if d == nil || (len(d.Servers) == 0 && d.GatewayAddr == "") {
		return ""
	}

	// Shallow-copy the outer map so we can add the bridge entry without mutating the shared base.
	// Inner server entries are not modified, so shallow copy is sufficient.
	// Per-agent DB external servers are intentionally NOT injected here (docs/26
	// §11 i-3b) — they reach the CLI ONLY via the goclaw-bridge entry below,
	// which enforces grant recheck + per-user creds. The only keys written are
	// the static config-file servers plus goclaw-bridge.
	servers := make(map[string]any, len(d.Servers)+1)
	maps.Copy(servers, d.Servers)

	// Build bridge entry with per-session agent context headers
	if d.GatewayAddr != "" {
		headers := make(map[string]string)
		if d.GatewayToken != "" {
			headers["Authorization"] = "Bearer " + d.GatewayToken
		}
		// Context header values are emitted via EncodeBridgeHeaderValue so that
		// non-ASCII content (e.g. a CJK group name in channel/workspace/session
		// key) is base64url-encoded and stays latin-1-safe — otherwise the
		// Node-based claude CLI rejects the header and the whole goclaw-bridge
		// connection fails. Pure-ASCII values pass through verbatim. The HMAC
		// below is computed over the RAW values, so signing is unaffected.
		if agentID != "" {
			headers["X-Agent-ID"] = EncodeBridgeHeaderValue(agentID)
		}
		if userID != "" {
			headers["X-User-ID"] = EncodeBridgeHeaderValue(userID)
		}
		if channel != "" {
			headers["X-Channel"] = EncodeBridgeHeaderValue(channel)
		}
		if chatID != "" {
			headers["X-Chat-ID"] = EncodeBridgeHeaderValue(chatID)
		}
		if peerKind != "" {
			headers["X-Peer-Kind"] = EncodeBridgeHeaderValue(peerKind)
		}
		if workspace != "" {
			headers["X-Workspace"] = EncodeBridgeHeaderValue(workspace)
		}
		if tenantID != "" {
			headers["X-Tenant-ID"] = EncodeBridgeHeaderValue(tenantID)
		}
		if localKey != "" {
			headers["X-Local-Key"] = EncodeBridgeHeaderValue(localKey)
		}
		if senderID != "" {
			headers["X-Sender-ID"] = EncodeBridgeHeaderValue(senderID)
		}
		if channelType != "" {
			headers["X-Channel-Type"] = EncodeBridgeHeaderValue(channelType)
		}
		if sessionKey != "" {
			headers["X-Session-Key"] = EncodeBridgeHeaderValue(sessionKey)
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
