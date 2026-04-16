package acp

// ACP protocol types — client-side subset for GoClaw as ACP client.
// Covers: initialize, session lifecycle, content blocks, and agent→client requests.

// --- Client → Agent Requests ---

// InitializeRequest starts the ACP handshake.
//
// ProtocolVersion is the integer ACP protocol level. Gemini CLI >= 0.38.1
// rejects the handshake when this field is missing or non-numeric with
// jsonrpc error -32603 (`expected number, received string`). The agent may
// cap its response to a lower version than requested — we send the highest
// we understand (currently 2) and let the agent negotiate down.
type InitializeRequest struct {
	ProtocolVersion int        `json:"protocolVersion"`
	ClientInfo      ClientInfo `json:"clientInfo"`
	Capabilities    ClientCaps `json:"capabilities"`
}

// ClientInfo identifies the ACP client.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ClientCaps declares what the client can handle (fs, terminal, etc.).
type ClientCaps struct {
	Fs       *FsCaps       `json:"fs,omitempty"`
	Terminal *TerminalCaps `json:"terminal,omitempty"`
}

// FsCaps declares filesystem capabilities.
type FsCaps struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

// TerminalCaps declares terminal capabilities.
type TerminalCaps struct {
	Enabled bool `json:"enabled"`
}

// InitializeResponse carries the agent's identity and capabilities.
type InitializeResponse struct {
	AgentInfo    AgentInfo `json:"agentInfo"`
	Capabilities AgentCaps `json:"capabilities"`
}

// AgentInfo identifies the ACP agent.
type AgentInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// AgentCaps declares agent capabilities.
type AgentCaps struct {
	LoadSession         bool         `json:"loadSession"`
	PromptCapabilities  *PromptCaps  `json:"promptCapabilities,omitempty"`
	SessionCapabilities *SessionCaps `json:"sessionCapabilities,omitempty"`
}

// PromptCaps describes what content types the agent accepts.
type PromptCaps struct {
	Audio           bool `json:"audio"`
	Image           bool `json:"image"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// SessionCaps describes session-level capabilities.
type SessionCaps struct{}

// --- Session Methods ---

// NewSessionRequest creates a new ACP session.
// NewSessionRequest establishes a new ACP session.
//
// Gemini CLI >= 0.38.1 requires both `cwd` (string) and `mcpServers` (array,
// may be empty) — sending an empty object fails with "expected array,
// received undefined". Cwd defaults to the process working directory if the
// caller leaves it empty; MCPServers is always serialized (never omitempty)
// so an empty slice marshals to `[]` instead of missing entirely.
type NewSessionRequest struct {
	Cwd        string             `json:"cwd"`
	MCPServers []NewSessionMCPCfg `json:"mcpServers"`
}

// NewSessionMCPCfg describes one MCP server the agent should connect to for
// the new session. Empty slice is fine; each entry tells Gemini where to
// find an external tool server.
type NewSessionMCPCfg struct {
	Name    string            `json:"name"`
	URL     string            `json:"url,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// NewSessionResponse carries the new session ID.
type NewSessionResponse struct {
	SessionID string `json:"sessionId"`
}

// PromptRequest sends user content to the agent.
// PromptRequest sends user content to the agent.
//
// The content array serializes under the JSON key "prompt" — Gemini CLI's
// session/prompt validator expects that key; sending "content" raises
// jsonrpc -32603. The Go field name stays as "Content" for continuity with
// other ACP clients in the codebase; only the wire tag moves.
type PromptRequest struct {
	SessionID string         `json:"sessionId"`
	Content   []ContentBlock `json:"prompt"`
}

// PromptResponse is the final response after the agent completes.
type PromptResponse struct {
	StopReason string `json:"stopReason,omitempty"`
}

// CancelNotification requests cooperative cancellation.
type CancelNotification struct {
	SessionID string `json:"sessionId"`
}

// --- Content Blocks ---

// ContentBlock represents a piece of content (text, image, audio).
type ContentBlock struct {
	Type     string `json:"type"` // "text", "image", "audio"
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"` // base64 for image/audio
	MimeType string `json:"mimeType,omitempty"`
}

// --- Agent → Client Notifications ---

// SessionUpdate carries incremental updates during prompt execution.
type SessionUpdate struct {
	Kind       string          `json:"kind"`                 // "message", "toolCall", "plan"
	StopReason string          `json:"stopReason,omitempty"` // "endTurn", "cancelled"
	Message    *MessageUpdate  `json:"message,omitempty"`
	ToolCall   *ToolCallUpdate `json:"toolCall,omitempty"`
}

// MessageUpdate carries an assistant text delta.
type MessageUpdate struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ToolCallUpdate carries tool call progress.
type ToolCallUpdate struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Status  string         `json:"status"` // "running", "completed"
	Content []ContentBlock `json:"content,omitempty"`
}

// --- Agent → Client Requests (fs/terminal/permission) ---

type ReadTextFileRequest struct {
	Path string `json:"path"`
}

type ReadTextFileResponse struct {
	Content string `json:"content"`
}

type WriteTextFileRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type WriteTextFileResponse struct{}

type CreateTerminalRequest struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
}

type CreateTerminalResponse struct {
	TerminalID string `json:"terminalId"`
}

type TerminalOutputRequest struct {
	TerminalID string `json:"terminalId"`
}

type TerminalOutputResponse struct {
	Output     string `json:"output"`
	ExitStatus *int   `json:"exitStatus,omitempty"`
}

type ReleaseTerminalRequest struct {
	TerminalID string `json:"terminalId"`
}

type ReleaseTerminalResponse struct{}

type WaitForTerminalExitRequest struct {
	TerminalID string `json:"terminalId"`
}

type WaitForTerminalExitResponse struct {
	ExitStatus int `json:"exitStatus"`
}

type KillTerminalRequest struct {
	TerminalID string `json:"terminalId"`
}

type KillTerminalResponse struct{}

type RequestPermissionRequest struct {
	ToolName    string `json:"toolName"`
	Description string `json:"description"`
}

type RequestPermissionResponse struct {
	Outcome string `json:"outcome"` // "approved", "denied", "cancelled"
}
