# iface:provider-registry

I have everything needed. Let me verify the `package` declaration and confirm `Register`/`RegisterForTenant` accept the `Provider` interface (not concrete), and double-check there's no `Capabilities()` on the `Provider` interface itself versus the optional `CapabilitiesAware`. All confirmed from the reads. Here is the complete report.

---

# agy-cli Provider — EXACT Go Contracts

All files are in package `providers` (`/Users/stanleykao72/Documents/goclaw-fork/internal/providers/`). Import path: `github.com/<...>/internal/providers` (the registry depends on `github.com/google/uuid`).

## 1. `Provider` interface (all methods)

`types.go:51-66`

```go
// Provider is the interface all LLM providers must implement.
type Provider interface {
	// Chat sends messages to the LLM and returns a response.
	// tools defines available tool schemas; model overrides the default.
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)

	// ChatStream sends messages and streams response chunks via callback.
	// Returns the final complete response after streaming ends.
	ChatStream(ctx context.Context, req ChatRequest, onChunk func(StreamChunk)) (*ChatResponse, error)

	// DefaultModel returns the provider's default model name.
	DefaultModel() string

	// Name returns the provider identifier (e.g. "anthropic", "openai").
	Name() string
}
```

The interface has exactly 4 methods: `Chat`, `ChatStream`, `DefaultModel() string`, `Name() string`. `Capabilities()` is NOT on this interface — it is the separate optional `CapabilitiesAware` (see §5).

## 2. `ChatRequest`, `ChatResponse` structs (every field + json tag + comment)

`types.go:74-101`

```go
// ChatRequest contains the input for a Chat/ChatStream call.
type ChatRequest struct {
	Messages []Message        `json:"messages"`
	Tools    []ToolDefinition `json:"tools,omitempty"`
	Model    string           `json:"model,omitempty"`
	Options  map[string]any   `json:"options,omitempty"`
}

// ChatResponse is the result from an LLM call.
type ChatResponse struct {
	Content      string     `json:"content"`
	Thinking     string     `json:"thinking,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	FinishReason string     `json:"finish_reason"` // "stop", "tool_calls", "length"
	Usage        *Usage     `json:"usage,omitempty"`

	// Phase is Codex-specific (gpt-5.3-codex): "commentary" or "final_answer".
	// Agent loop must persist this on assistant messages for Codex performance.
	Phase string `json:"phase,omitempty"`

	// RawAssistantContent preserves the raw content blocks array from the provider response.
	// Used by Anthropic to pass thinking blocks back in tool use loops (required by API).
	RawAssistantContent json.RawMessage `json:"-"`

	// ThinkingSignature is the accumulated signature from streaming thinking blocks.
	// Required by Anthropic API for tool use passback when thinking is enabled.
	ThinkingSignature string `json:"-"`
}
```

## 3. `StreamChunk` struct + the onChunk callback shape

`types.go:103-108`

```go
// StreamChunk is a piece of a streaming response.
type StreamChunk struct {
	Content  string `json:"content,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	Done     bool   `json:"done,omitempty"`
}
```

The onChunk callback shape, from the `Provider.ChatStream` method signature (`types.go:59`), confirmed verbatim across all implementations (e.g. `anthropic_stream.go:11`, `acp_provider.go:145`, `claude_cli_chat.go:77`, `chatgpt_oauth_router.go:83`):

```go
onChunk func(StreamChunk)
```

Full method signature:

```go
ChatStream(ctx context.Context, req ChatRequest, onChunk func(StreamChunk)) (*ChatResponse, error)
```

## 4. `ToolCall`, `Usage`, `Message`, `ToolDefinition` structs

`Message` — `types.go:126-151`

```go
// Message represents a conversation message.
type Message struct {
	Role       string         `json:"role"` // "system", "user", "assistant", "tool"
	Content    string         `json:"content"`
	Thinking   string         `json:"thinking,omitempty"`   // reasoning_content for thinking models (Kimi, DeepSeek, etc.)
	Images     []ImageContent `json:"-"`                    // vision: base64 images (runtime only, never persisted to DB)
	MediaRefs  []MediaRef     `json:"media_refs,omitempty"` // persistent media file references
	ToolCalls  []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"` // for role="tool" responses
	IsError    bool           `json:"is_error,omitempty"`     // for role="tool" responses

	// Phase is a Codex-specific field (gpt-5.3-codex) indicating message purpose.
	// Values: "commentary" (intermediate), "final_answer" (closeout), or "" (unset).
	// Must be persisted and passed back in subsequent requests for Codex performance.
	// Other providers ignore this field.
	Phase string `json:"phase,omitempty"`

	// RawAssistantContent carries raw provider content blocks through tool loop iterations.
	// Anthropic requires thinking blocks to be passed back exactly as received.
	RawAssistantContent json.RawMessage `json:"-"`

	// CreatedAt records when this message was added to the session.
	// Pointer type so that older messages (stored before this field existed) deserialize as nil,
	// allowing the frontend to fall back to synthetic timestamps.
	CreatedAt *time.Time `json:"created_at,omitempty"`
}
```

`ToolCall` — `types.go:153-160`

```go
// ToolCall represents a tool invocation requested by the LLM.
type ToolCall struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Arguments  map[string]any    `json:"arguments"`
	Metadata   map[string]string `json:"metadata,omitempty"`    // provider-specific (e.g. Gemini thought_signature)
	ParseError string            `json:"parse_error,omitempty"` // set when arguments JSON was malformed/truncated
}
```

`ToolDefinition` (+ its nested `ToolFunctionSchema`) — `types.go:162-174`

```go
// ToolDefinition describes a tool available to the LLM.
type ToolDefinition struct {
	Type     string             `json:"type"` // "function"
	Function ToolFunctionSchema `json:"function"`
}

// ToolFunctionSchema is the schema for a function tool.
type ToolFunctionSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      *bool          `json:"strict,omitempty"` // OpenAI strict mode — constrained decoding
}
```

`Usage` — `types.go:176-184`

```go
// Usage tracks token consumption.
type Usage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens     int `json:"cache_read_input_tokens,omitempty"`
	ThinkingTokens      int `json:"thinking_tokens,omitempty"`
}
```

Supporting types referenced by `Message` (needed to compile, both in `types.go`):

`ImageContent` — `types.go:110-114`
```go
type ImageContent struct {
	MimeType string `json:"mime_type"` // e.g. "image/jpeg"
	Data     string `json:"data"`      // base64-encoded image bytes
}
```

`MediaRef` — `types.go:119-124`
```go
type MediaRef struct {
	ID       string `json:"id"`             // unique media ID (uuid)
	MimeType string `json:"mime_type"`      // e.g. "image/jpeg", "application/pdf"
	Kind     string `json:"kind"`           // "image", "video", "audio", "document"
	Path     string `json:"path,omitempty"` // absolute workspace path (persisted for /v1/files/ serving)
}
```

## 5. `ProviderCapabilities` struct (every field) and the `CapabilitiesAware` interface

`capabilities.go:5-22`

```go
// ProviderCapabilities declares what a provider supports.
// Queried by pipeline to choose code paths (streaming vs non-streaming, etc.)
type ProviderCapabilities struct {
	Streaming        bool   // supports ChatStream()
	ToolCalling      bool   // supports tools in request
	StreamWithTools  bool   // can stream while tool calls are in-flight
	Thinking         bool   // supports extended thinking / reasoning
	Vision           bool   // supports image inputs
	CacheControl     bool   // supports cache_control blocks (Anthropic)
	MaxContextWindow int    // default context window for default model
	TokenizerID      string // for tokencount package mapping
}

// CapabilitiesAware is optionally implemented by Provider.
// Pipeline checks this to choose code path.
type CapabilitiesAware interface {
	Capabilities() ProviderCapabilities
}
```

Note: `ProviderCapabilities` fields have NO json tags (plain struct fields). To satisfy `CapabilitiesAware`, implement exactly: `func (p *AgyCLIProvider) Capabilities() ProviderCapabilities` (matches existing pattern at `dashscope.go:56`, `anthropic.go:110`, `claude_cli.go:144`).

## 6. `ThinkingCapable` interface

`types.go:68-72`

```go
// ThinkingCapable is optionally implemented by providers that support extended thinking.
// Used to gate thinking_level injection so it's not sent to providers that ignore it.
type ThinkingCapable interface {
	SupportsThinking() bool
}
```

Implementation pattern (verbatim from `anthropic.go:107`): `func (p *AnthropicProvider) SupportsThinking() bool { return true }`.

## 7. Registry type + EXACT `Register` / `RegisterForTenant` signatures

The registry type — `registry.go:19-28` (package `providers`):

```go
// Registry manages available LLM providers with per-tenant isolation.
// Providers are keyed by "tenantID/name". Get falls back to master tenant.
type Registry struct {
	providers     map[string]Provider
	mu            sync.RWMutex
	tenantFromCtx func(context.Context) uuid.UUID // injected to avoid circular import with store

	// roundRobinCounters stores shared round-robin state keyed by "tenantID/providerName"
	// so that ChatGPTOAuthRouter instances (created per-request) share rotation state.
	roundRobinMu       sync.Mutex
	roundRobinCounters map[string]int
}
```

`Register` — `registry.go:68-71`. Accepts the **`Provider` interface** (not concrete):

```go
// Register adds a provider to the registry under the master tenant.
func (r *Registry) Register(provider Provider) {
	r.RegisterForTenant(MasterTenantID, provider)
}
```

`RegisterForTenant` — `registry.go:73-85`. Accepts `uuid.UUID` + the **`Provider` interface**:

```go
// RegisterForTenant adds a provider under a specific tenant.
// If a provider with the same tenant+name already exists, it is closed before replacement.
func (r *Registry) RegisterForTenant(tenantID uuid.UUID, provider Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := compoundKey(tenantID, provider.Name())
	if old, ok := r.providers[key]; ok {
		if c, ok := old.(io.Closer); ok {
			c.Close()
		}
	}
	r.providers[key] = provider
}
```

`tenantID` is of type `uuid.UUID` from `github.com/google/uuid`. `MasterTenantID` is exported (`registry.go:15`): `var MasterTenantID = uuid.Must(uuid.Parse("0193a5b0-7000-7000-8000-000000000001"))`. Both methods are pointer-receiver on `*Registry`. Constructor: `func NewRegistry(tenantFromCtx func(context.Context) uuid.UUID) *Registry` (`registry.go:32`).

---

## Additional facts the agy-cli provider must know

- **Optional `io.Closer`**: the registry calls `Close()` on replacement/unregister/shutdown if the provider implements `io.Closer` (`registry.go:80-82`, `97-99`, `134`). If agy-cli holds resources (subprocess, conn), implement `func (p *AgyCLIProvider) Close() error`.
- **Required `Name()`** must be stable and unique per tenant — it is the lookup key suffix (`compoundKey(tenantID, provider.Name())`, `registry.go:54-56`, `78`).
- **`ChatRequest.Options` keys** (string consts, `types.go:9-27`) the provider may read: `OptMaxTokens="max_tokens"`, `OptTemperature="temperature"`, `OptThinkingLevel="thinking_level"`, `OptReasoningEffort="reasoning_effort"`, `OptEnableThinking="enable_thinking"`, `OptThinkingBudget="thinking_budget"`, `OptStripThinking="strip_thinking"`, `OptServiceTier="service_tier"`, `OptFastMode="fast_mode"`, `OptPromptCacheKey="prompt_cache_key"`, `OptPromptCacheRetention="prompt_cache_retention"`.
- **Imports for types.go contracts**: `context`, `encoding/json` (for `json.RawMessage`), `time` (for `*time.Time`). For capabilities: `net/http` (only `ProviderAdapter` uses it; not needed if you only implement `Provider`/`CapabilitiesAware`/`ThinkingCapable`).
- **`ProviderAdapter` / `AdapterFactory` / `ProviderConfig`** (`capabilities.go:24-57`) are an OPTIONAL alternate path (adapter composed inside a provider), not required to satisfy `Provider`. The minimal compiling agy-cli provider needs only: `Chat`, `ChatStream`, `DefaultModel`, `Name` (mandatory), plus optionally `Capabilities`, `SupportsThinking`, `Close`.

---

# claude:core-types-auth

These helpers live in `claude_cli_session.go` (out of the 3 files I was assigned, but directly relevant to items 4 and 7). Here are the verbatim definitions.

## 7 (continued). `defaultCLIWorkDir()` + adjacent session helpers

`claude_cli_session.go:174-208`

```go
// defaultCLIWorkDir returns dataDir/cli-workspaces.
func defaultCLIWorkDir() string {
	return filepath.Join(config.ResolvedDataDirFromEnv(), "cli-workspaces")
}

// deriveSessionUUID creates a deterministic UUID v5 from a session key string.
func deriveSessionUUID(sessionKey string) uuid.UUID {
	if sessionKey == "" {
		return uuid.New() // fallback: random
	}
	return uuid.NewSHA1(uuid.NameSpaceDNS, []byte(sessionKey))
}

// sessionFileExists checks if a Claude CLI session file exists for the given work directory.
// Claude CLI resolves symlinks (e.g. /var/folders → /private/var/folders on macOS)
// before encoding the path, so we must do the same.
func sessionFileExists(workDir string, sessionID uuid.UUID) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	// Resolve symlinks to match CLI's path encoding (macOS: /var → /private/var)
	resolved, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		resolved = workDir
	}
	// Claude CLI stores sessions at: ~/.claude/projects/<encoded-path>/<session-id>.jsonl
	// CLI replaces path separators, "_", ".", and ":" with "-" in the path encoding.
	// On Windows: C:\Users\foo → C--Users-foo (backslash + colon both become "-")
	// On macOS/Linux: /home/foo → -home-foo (forward slash becomes "-")
	encoded := strings.NewReplacer(string(filepath.Separator), "-", "_", "-", ".", "-", ":", "-").Replace(resolved)
	sessionFile := filepath.Join(home, ".claude", "projects", encoded, sessionID.String()+".jsonl")
	_, err = os.Stat(sessionFile)
	return err == nil
}
```

Plus `bridgeContextFromOpts` (`claude_cli_session.go:160-172`), which maps the Opt* constants from §5 into a `BridgeContext`:

```go
// bridgeContextFromOpts builds a BridgeContext from the Options map.
func bridgeContextFromOpts(opts map[string]any) BridgeContext {
	return BridgeContext{
		AgentID:   extractStringOpt(opts, OptAgentID),
		UserID:    extractStringOpt(opts, OptUserID),
		Channel:   extractStringOpt(opts, OptChannel),
		ChatID:    extractStringOpt(opts, OptChatID),
		PeerKind:  extractStringOpt(opts, OptPeerKind),
		Workspace: extractStringOpt(opts, OptWorkspace),
		TenantID:  extractStringOpt(opts, OptTenantID),
		LocalKey:  extractStringOpt(opts, OptLocalKey),
	}
}
```

**Agy-mapping notes (CRITICAL — the session model is the biggest divergence):**

- `defaultCLIWorkDir` → portable as-is (`dataDir/cli-workspaces`); just rename label.
- `deriveSessionUUID` → **highly relevant and portable**. It deterministically derives a UUID v5 (SHA1, DNS namespace) from the session key. Agy resumes only via `--conversation <uuid>` — so this exact pattern (session-key → deterministic uuid) is the bridge: GoClaw can keep a stable per-session uuid to feed agy's `--conversation`. This function maps cleanly.
- `sessionFileExists` → **does NOT map**. It probes claude's on-disk session store at `~/.claude/projects/<encoded-path>/<session-id>.jsonl` with claude's specific path-encoding scheme. Agy persists conversations at `~/.gemini/antigravity-cli/conversations/<uuid>.db` — a SQLite file keyed directly by uuid, NOT a path-encoded `.jsonl`. So the existence check becomes a simple `os.Stat(~/.gemini/antigravity-cli/conversations/<uuid>.db)` — no workdir-path encoding, no symlink-resolution dance, no `.jsonl` extension. The entire encoding/symlink logic is dead for agy.

---

# Agy-mapping summary (everything claude-specific that won't port)

| Item | Claude mechanism | Agy constraint → consequence |
|------|------------------|------------------------------|
| `permMode` field + `WithClaudeCLIPermMode` + `bypassPermissions` default | `--permission-mode` | agy has no permission-mode flag → **drop field + option** |
| `hooksSettingsPath` + `hooksCleanup` + `WithClaudeCLISecurityHooks` + `BuildCLIHooksConfig` | generated `settings.json` w/ PreToolUse hooks | **agy has no `--settings`/hooks** → **whole feature drops**; shell-deny + workspace restriction has no agy equivalent (feature gap to flag) |
| `OptDisableTools` honoring | `--disallowedTools` | **agy has no `--disallowedTools`** → opt key survives but cannot be enforced |
| JSON parsing (`cliJSONResponse`, `cliStreamEvent`, all of `claude_cli_types.go`) | `--output-format json` / `stream-json` | **agy has no `--output-format`** → these response types do not apply; output parsing must be rewritten for agy's native output |
| `Capabilities.Streaming` / `StreamWithTools` | depend on `stream-json` | reconsider — agy can't emit the same JSON stream |
| `Capabilities.Thinking` / `MaxContextWindow:200_000` / `TokenizerID:"cl100k_base"` | claude model facts | replace with agy/gemini values |
| `sessionFileExists` path-encoding + `~/.claude/projects/<enc>/<id>.jsonl` | claude session store | replace with `os.Stat(~/.gemini/antigravity-cli/conversations/<uuid>.db)` |
| Session resume (caller of these, not in-file) | `--session-id` | **agy has no `--session-id`; resume only via `--conversation <uuid>`** → resume call site must change |
| `CheckClaudeAuthStatus` | `claude auth status --json` → `{loggedIn,email,subscriptionType}` | agy auth model is Gemini/Google (conversations under `~/.gemini/`); `subscriptionType` meaningless → struct + subcommand need agy equivalent |
| `mcpConfigData` / `mcpConfigDirs` / `WithClaudeCLIMCPConfigData` + MCP Opt* keys | per-session `--mcp-config` | only ports if agy supports MCP config injection (unstated) |

**Ports cleanly:** `name`/`cliPath`/`baseWorkDir`/`defaultModel` fields (with new default values), `Name`/`DefaultModel`, `lockSession` (re-key on conversation uuid), `mu`/`sessionMu`, `defaultCLIWorkDir`, `deriveSessionUUID` (this is the key→`--conversation` uuid bridge), `bridgeContextFromOpts` + non-MCP-dependent Opt* keys, the option-application pattern in `NewClaudeCLIProvider`.

**Files actually read:** `claude_cli.go` (full, 180 lines), `claude_cli_types.go` (full, 45 lines), `claude_cli_auth.go` (full, 41 lines). **Out-of-scope but quoted because items 4 & 7 reference them:** `claude_cli_session.go:160-208` (`defaultCLIWorkDir`, `deriveSessionUUID`, `sessionFileExists`, `bridgeContextFromOpts`). **Referenced-but-not-quoted (defined in other files):** `BuildCLIHooksConfig` (`claude_cli_hooks.go`), `MCPConfigData` (`claude_cli_mcp.go`), `ProviderCapabilities` (`capabilities.go`) — say so if you want these extracted too.

---

# claude:chat-session-exec

I now have everything needed. Here is the full report.

---

# claude_cli Chat/ChatStream — verbatim source + exec-lifecycle explanation

Files: `/Users/stanleykao72/Documents/goclaw-fork/internal/providers/claude_cli_chat.go` and `/Users/stanleykao72/Documents/goclaw-fork/internal/providers/claude_cli_session.go`. Supporting types/parsers in `claude_cli_types.go` and `claude_cli_parse.go`.

## 1. `Chat` (verbatim — claude_cli_chat.go:17-74)

```go
// Chat runs the CLI synchronously and returns the final response.
func (p *ClaudeCLIProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	systemPrompt, userMsg, images := extractFromMessages(req.Messages)
	sessionKey := extractStringOpt(req.Options, OptSessionKey)
	model := req.Model
	if model == "" {
		model = p.defaultModel
	}
	if err := validateCLIModel(model); err != nil {
		return nil, err
	}

	unlock := p.lockSession(sessionKey)
	defer unlock()

	workDir := p.ensureWorkDir(sessionKey)
	if systemPrompt != "" {
		p.writeClaudeMD(workDir, systemPrompt)
	}

	cliSessionID := deriveSessionUUID(sessionKey)
	disableTools := extractBoolOpt(req.Options, OptDisableTools)
	bc := bridgeContextFromOpts(req.Options)
	mcpPath := p.resolveMCPConfigPath(ctx, sessionKey, bc)
	// Claude CLI >= v2.1.87 requires matching input/output formats.
	// When images are present, buildArgs adds --input-format stream-json,
	// so output format must also be stream-json.
	outputFmt := "json"
	if len(images) > 0 {
		outputFmt = "stream-json"
	}
	args := p.buildArgs(model, workDir, mcpPath, cliSessionID, outputFmt, len(images) > 0, disableTools)

	var stdin *bytes.Reader
	if len(images) > 0 {
		stdin = buildStreamJSONInput(userMsg, images)
	} else {
		args = append(args, "--", userMsg)
	}

	cmd := exec.CommandContext(ctx, p.cliPath, args...)
	cmd.Dir = workDir
	cmd.Env = filterCLIEnv(os.Environ())
	if stdin != nil {
		cmd.Stdin = stdin
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	slog.Debug("claude-cli exec", "cmd", fmt.Sprintf("%s %s", p.cliPath, strings.Join(args, " ")), "workdir", workDir)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("claude-cli: %w (stderr: %s)", err, stderr.String())
	}

	return parseJSONResponse(output)
}
```

### Exec lifecycle (`Chat`)
1. `extractFromMessages` splits `req.Messages` into `systemPrompt` (last `role=="system"`), `userMsg` (last `role=="user"`), and `images` (that user message's `.Images`).
2. `sessionKey` is read from `req.Options[OptSessionKey]` (`OptSessionKey == "session_key"`). Model defaults to `p.defaultModel`; `validateCLIModel` restricts to `sonnet`/`opus`/`haiku`.
3. `p.lockSession(sessionKey)` takes a per-session mutex (serializes concurrent calls on the same session); released via `defer`.
4. `ensureWorkDir(sessionKey)` returns the stable per-session workdir; if a system prompt exists, it is written to `CLAUDE.md` (the CLI auto-reads it every run, including on `--resume`).
5. `deriveSessionUUID(sessionKey)` → deterministic UUIDv5. MCP config path resolved per-session (`resolveMCPConfigPath`).
6. **Output format selection**: `json` normally, but `stream-json` when images are present (CLI ≥ v2.1.87 requires input/output formats to match, and images force `--input-format stream-json`).
7. `buildArgs(...)` assembles flags (see §3).
8. **Stdin vs argv routing**: with images → `stdin` is a `*bytes.Reader` of a stream-json user message (`buildStreamJSONInput`); without images → the plain text is appended as a trailing positional arg `"--", userMsg`.
9. `exec.CommandContext(ctx, p.cliPath, args...)` — ctx-bound so cancellation kills the process. `cmd.Dir = workDir`; `cmd.Env = filterCLIEnv(os.Environ())` (strips `CLAUDE*` except `CLAUDE_CODE_OAUTH_TOKEN`). Stdin set only when images.
10. `stderr` captured into a `bytes.Buffer`.
11. **Capture**: `cmd.Output()` runs to completion and returns stdout bytes. On error → wrapped with stderr text.
12. **Response build**: `parseJSONResponse(output)` — tries JSON array, then per-line JSON objects, then raw-text fallback. It extracts `result` text (or concatenated `assistant` text blocks), maps `cliUsage` → `Usage{PromptTokens, CompletionTokens, TotalTokens}`, and sets `FinishReason` = `"stop"` (or `"error"` if `subtype=="error"`).

## 2. `ChatStream` (verbatim — claude_cli_chat.go:76-240)

```go
// ChatStream runs the CLI with stream-json output, calling onChunk for each text delta.
func (p *ClaudeCLIProvider) ChatStream(ctx context.Context, req ChatRequest, onChunk func(StreamChunk)) (*ChatResponse, error) {
	systemPrompt, userMsg, images := extractFromMessages(req.Messages)
	sessionKey := extractStringOpt(req.Options, OptSessionKey)
	model := req.Model
	if model == "" {
		model = p.defaultModel
	}
	if err := validateCLIModel(model); err != nil {
		return nil, err
	}

	slog.Debug("claude-cli: acquiring session lock", "session_key", sessionKey)
	unlock := p.lockSession(sessionKey)
	slog.Debug("claude-cli: session lock acquired", "session_key", sessionKey)
	defer func() {
		unlock()
		slog.Debug("claude-cli: session lock released", "session_key", sessionKey)
	}()

	workDir := p.ensureWorkDir(sessionKey)
	if systemPrompt != "" {
		p.writeClaudeMD(workDir, systemPrompt)
	}

	cliSessionID := deriveSessionUUID(sessionKey)
	disableTools := extractBoolOpt(req.Options, OptDisableTools)
	bc := bridgeContextFromOpts(req.Options)
	mcpPath := p.resolveMCPConfigPath(ctx, sessionKey, bc)
	args := p.buildArgs(model, workDir, mcpPath, cliSessionID, "stream-json", len(images) > 0, disableTools)

	var stdin *bytes.Reader
	if len(images) > 0 {
		stdin = buildStreamJSONInput(userMsg, images)
	} else {
		args = append(args, "--", userMsg)
	}

	cmd := exec.CommandContext(ctx, p.cliPath, args...)
	cmd.WaitDelay = 5 * time.Second // force-close pipes if process lingers after kill
	cmd.Dir = workDir
	cmd.Env = filterCLIEnv(os.Environ())
	if stdin != nil {
		cmd.Stdin = stdin
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("claude-cli stdout pipe: %w", err)
	}

	fullCmd := fmt.Sprintf("%s %s", p.cliPath, strings.Join(args, " "))
	slog.Debug("claude-cli stream exec", "cmd", fullCmd, "workdir", workDir)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude-cli start: %w", err)
	}

	// Debug log file: only enabled when GOCLAW_DEBUG=1
	var debugFile *os.File
	if os.Getenv("GOCLAW_DEBUG") == "1" {
		debugLogPath := filepath.Join(workDir, "cli-debug.log")
		debugFile, _ = os.OpenFile(debugLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if debugFile != nil {
			fmt.Fprintf(debugFile, "=== CMD: %s\n=== WORKDIR: %s\n=== TIME: %s\n\n", fullCmd, workDir, time.Now().Format(time.RFC3339))
			defer debugFile.Close()
		}
	}

	// Parse stream-json line-by-line
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, StdioScanBufInit), StdioScanBufMax)

	var finalResp ChatResponse
	var contentBuf strings.Builder

	for scanner.Scan() {
		if ctx.Err() != nil {
			break // context cancelled (abort) → exit immediately
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Write raw line to debug log
		if debugFile != nil {
			fmt.Fprintf(debugFile, "%s\n", line)
		}

		var ev cliStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			slog.Debug("claude-cli: skip malformed stream line", "error", err)
			continue
		}

		switch ev.Type {
		case "assistant":
			if ev.Message == nil {
				continue
			}
			text, thinking := extractStreamContent(ev.Message)
			if text != "" {
				contentBuf.WriteString(text)
				onChunk(StreamChunk{Content: text})
			}
			if thinking != "" {
				onChunk(StreamChunk{Thinking: thinking})
			}

		case "result":
			if ev.Result != "" {
				finalResp.Content = ev.Result
			} else {
				finalResp.Content = contentBuf.String()
			}
			finalResp.FinishReason = "stop"
			if ev.Subtype == "error" {
				finalResp.FinishReason = "error"
			}
			if ev.Usage != nil {
				finalResp.Usage = &Usage{
					PromptTokens:     ev.Usage.InputTokens,
					CompletionTokens: ev.Usage.OutputTokens,
					TotalTokens:      ev.Usage.InputTokens + ev.Usage.OutputTokens,
				}
			}
		}
	}

	// Context cancelled (abort): best-effort reap (bounded by WaitDelay), then return.
	if ctx.Err() != nil {
		_ = cmd.Wait()
		return nil, ctx.Err()
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("claude-cli: stream read error: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		if debugFile != nil {
			fmt.Fprintf(debugFile, "\n=== STDERR:\n%s\n=== EXIT ERROR: %v\n", stderrBuf.String(), err)
		}
		// If we got partial content, return it with the error
		if finalResp.Content != "" {
			return &finalResp, nil
		}
		return nil, fmt.Errorf("claude-cli: %w (stderr: %s)", err, stderrBuf.String())
	}
	if debugFile != nil && stderrBuf.Len() > 0 {
		fmt.Fprintf(debugFile, "\n=== STDERR:\n%s\n", stderrBuf.String())
	}

	// Fallback if no "result" event was received
	if finalResp.Content == "" {
		finalResp.Content = contentBuf.String()
		finalResp.FinishReason = "stop"
	}

	onChunk(StreamChunk{Done: true})
	return &finalResp, nil
}
```

### Exec lifecycle (`ChatStream`)
1. Same prologue as `Chat` (extract messages, validate model, **per-session lock**, ensure workdir, write `CLAUDE.md`, derive session UUID, resolve MCP path) — but output format is **always `"stream-json"`**.
2. Stdin/argv routing identical to `Chat` (images → stdin stream-json; else `"--", userMsg`).
3. `cmd.WaitDelay = 5 * time.Second` — if the process lingers after a kill, pipes are force-closed after 5s (prevents `Wait()` hanging on abort).
4. **Stdout is a pipe** (`cmd.StdoutPipe()`), not buffered capture; stderr still buffered. `cmd.Start()` launches asynchronously.
5. Optional debug log file at `workDir/cli-debug.log` when `GOCLAW_DEBUG=1`, capturing every raw line + final stderr/exit.
6. **Per-delta streaming**: a `bufio.Scanner` over stdout (buffer 256 KB init / 10 MB max — `StdioScanBufInit`/`StdioScanBufMax`) reads stream-json **line by line**. Each line is unmarshalled into `cliStreamEvent`; malformed lines are skipped.
   - `type=="assistant"`: `extractStreamContent` pulls text + thinking from content blocks. Text is appended to `contentBuf` **and** emitted via `onChunk(StreamChunk{Content: text})`. Thinking (non-empty) is emitted via `onChunk(StreamChunk{Thinking: thinking})` (not accumulated into content).
   - `type=="result"`: assembles the **final `ChatResponse`** — `Content` = `ev.Result` if present else `contentBuf.String()`; `FinishReason` = `"stop"` (or `"error"` if `subtype=="error"`); `Usage` mapped from `cliUsage`.
7. **Abort handling**: each loop iteration checks `ctx.Err()` and breaks; after the loop, if cancelled, it does a best-effort `cmd.Wait()` (bounded by `WaitDelay`) and returns `ctx.Err()`.
8. **Final assembly**: `scanner.Err()` → stream read error. `cmd.Wait()` error → if partial content exists, return it without error; otherwise wrap with stderr. If no `result` event arrived, fall back to `contentBuf` with `FinishReason="stop"`. Finally emit a terminal `onChunk(StreamChunk{Done: true})` and return `&finalResp`.

## 3. `buildArgs` (verbatim — claude_cli_session.go:30-73)

```go
func (p *ClaudeCLIProvider) buildArgs(model, workDir, mcpConfigPath string, cliSessionID uuid.UUID, outputFormat string, hasImages, disableTools bool) []string {
	args := []string{
		"-p",
		"--output-format", outputFormat,
		"--model", model,
		"--permission-mode", p.permMode,
		"--verbose",
	}

	if mcpConfigPath != "" {
		args = append(args, "--mcp-config", mcpConfigPath)
	}

	// Session persistence: check if CLI session file exists on disk.
	// If exists → --resume (continue conversation). If not → --session-id (create new).
	// Session files live at ~/.claude/projects/<sanitized-workdir>/<uuid>.jsonl
	sid := cliSessionID.String()
	if sessionFileExists(workDir, cliSessionID) {
		args = append(args, "--resume", sid)
	} else {
		args = append(args, "--session-id", sid)
	}

	if hasImages {
		args = append(args, "--input-format", "stream-json")
	}

	if disableTools {
		// Summoner: disable all tools entirely via disallowedTools
		args = append(args, "--disallowedTools", "Bash,Edit,Read,Write,Glob,Grep,WebFetch,WebSearch,TodoRead,TodoWrite,NotebookRead,NotebookEdit")
	} else if mcpConfigPath != "" {
		// Chat with MCP bridge: disable CLI built-in tools, only allow MCP bridge tools.
		// This ensures all tool execution goes through GoClaw's controlled MCP bridge.
		args = append(args, "--disallowedTools", "Bash,Edit,Read,Write,Glob,Grep,WebFetch,WebSearch,TodoRead,TodoWrite,NotebookRead,NotebookEdit")
	}

	if p.hooksSettingsPath != "" {
		args = append(args, "--settings", p.hooksSettingsPath)
	}

	return args
}
```

### Flag-by-flag (and the condition)
- `-p` — always (headless/print mode).
- `--output-format <outputFormat>` — always; value is `"json"` (Chat, no images), or `"stream-json"` (ChatStream always; Chat with images).
- `--model <model>` — always.
- `--permission-mode <p.permMode>` — always (provider-configured).
- `--verbose` — always.
- `--mcp-config <mcpConfigPath>` — only if a per-session MCP config path was resolved (non-empty).
- `--resume <uuid>` — if `sessionFileExists(workDir, cliSessionID)` (session `.jsonl` already on disk → continue conversation).
- `--session-id <uuid>` — otherwise (new session).
- `--input-format stream-json` — only if `hasImages`.
- `--disallowedTools Bash,Edit,Read,Write,Glob,Grep,WebFetch,WebSearch,TodoRead,TodoWrite,NotebookRead,NotebookEdit` — if `disableTools` (Summoner: kill all tools), **else** if `mcpConfigPath != ""` (force all tool calls through GoClaw's MCP bridge). Same disallow-list either way.
- `--settings <p.hooksSettingsPath>` — only if hooks settings path is configured.
- Trailing `"--", userMsg` is **not** added here — `Chat`/`ChatStream` append it after `buildArgs` when there are no images.

## 4. Session-ID handling (`--resume` vs `--session-id`)

- **Source of the goclaw session key**: `sessionKey := extractStringOpt(req.Options, OptSessionKey)` where `OptSessionKey == "session_key"` (`claude_cli.go:10`). So yes — it comes from `req.Options["session_key"]`.
- **goclaw key → CLI session UUID mapping** (`deriveSessionUUID`, claude_cli_session.go:179-185):
  ```go
  func deriveSessionUUID(sessionKey string) uuid.UUID {
      if sessionKey == "" {
          return uuid.New() // fallback: random
      }
      return uuid.NewSHA1(uuid.NameSpaceDNS, []byte(sessionKey))
  }
  ```
  Deterministic UUIDv5 (SHA1, DNS namespace) — the same goclaw `session_key` always maps to the same CLI session UUID. Empty key → random UUID (ephemeral, never resumes).
- **resume vs new decision** (`sessionFileExists`, claude_cli_session.go:187-208): it checks for the CLI's session transcript on disk at `~/.claude/projects/<encoded-workdir>/<uuid>.jsonl`. The workdir path is run through `filepath.EvalSymlinks` (macOS `/var → /private/var`) then encoded by replacing each of `os.PathSeparator`, `_`, `.`, `:` with `-`. If that `.jsonl` exists → `--resume <uuid>`; otherwise → `--session-id <uuid>`.
- **Reset** (`ResetCLISession`, claude_cli_session.go:258-285): deletes the `.jsonl` transcript and `CLAUDE.md` so the next call starts fresh (avoids `--resume`-ing poisoned history).

> Mapping for agy-cli: this is exactly the seam to replace. The CLI version maps `session_key → UUIDv5 → on-disk .jsonl → --resume/--session-id`. The agy-cli PTY version will instead map the goclaw session to a `--conversation`-based resume (plain-text output, no stream-json envelopes), so it won't probe `~/.claude/projects/...jsonl` — it'll track the conversation id from agy's own output/state.

## 5. Workdir creation logic (`ensureWorkDir`, claude_cli_session.go:87-101)

```go
func (p *ClaudeCLIProvider) ensureWorkDir(sessionKey string) string {
	safe := sanitizePathSegment(sessionKey)
	dir := filepath.Join(p.baseWorkDir, safe)

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := os.MkdirAll(dir, 0755); err != nil {
		slog.Warn("claude-cli: failed to create workdir", "dir", dir, "error", err)
		return os.TempDir()
	}
	return dir
}
```

- Path = `p.baseWorkDir / sanitizePathSegment(sessionKey)`. The base defaults to `defaultCLIWorkDir()` = `<dataDir>/cli-workspaces` (claude_cli_session.go:175-177).
- `sanitizePathSegment` guards against path traversal / null bytes / over-length.
- Created with `os.MkdirAll(dir, 0755)` under `p.mu` (process-wide lock for directory creation). On failure it falls back to `os.TempDir()`.
- The directory is **stable per session key** — the same key reuses the same workdir, which is what makes `CLAUDE.md` persistence and `.jsonl` resume detection coherent across calls. The system prompt is written there as `CLAUDE.md` (`writeClaudeMD`, idempotent — skips write if content unchanged).

## 6. Images / multimodal handling (`--input-format stream-json`)

- **Detection**: `extractFromMessages` returns `images []ImageContent` from the last user message. Presence is `len(images) > 0`.
- **Format coupling**: images force `buildArgs` to add `--input-format stream-json`; because the CLI requires matching input/output formats, `Chat` also switches `--output-format` to `stream-json` when images are present (ChatStream is already stream-json).
- **Stdin payload** (`buildStreamJSONInput`, claude_cli_session.go:218-253): instead of passing text as a trailing argv arg, the whole user turn is marshalled to a single stream-json line fed via stdin:

```go
func buildStreamJSONInput(text string, images []ImageContent) *bytes.Reader {
	var contentBlocks []map[string]any

	for _, img := range images {
		blockType := "image"
		if img.MimeType == "application/pdf" {
			blockType = "document"
		}
		contentBlocks = append(contentBlocks, map[string]any{
			"type": blockType,
			"source": map[string]any{
				"type":       "base64",
				"media_type": img.MimeType,
				"data":       img.Data,
			},
		})
	}

	if text != "" {
		contentBlocks = append(contentBlocks, map[string]any{
			"type": "text",
			"text": text,
		})
	}

	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": contentBlocks,
		},
	}

	data, _ := json.Marshal(msg)
	return bytes.NewReader(data)
}
```

- **Block typing by MIME**: `application/pdf` → `"document"` block; everything else → `"image"` block (unknown MIME also emitted as `image`, letting upstream Anthropic reject it explicitly). Each block carries a `source` of `{type: "base64", media_type, data}`. The text (if any) is appended as a trailing `text` block. The envelope is `{type:"user", message:{role:"user", content:[...]}}`, marshalled once and wrapped in a `*bytes.Reader` set as `cmd.Stdin`.
- **Without images**, none of this applies — the text goes as the trailing positional arg `"--", userMsg` and stdin stays nil.

> Mapping for agy-cli: the PTY variant drops the stream-json stdin envelope and json/stream-json output parsing entirely. It will: drive a PTY (not `StdoutPipe`), read **plain text** output (so no `cliStreamEvent`/`cliStreamMsg` parsing, no `result`/`assistant` envelopes, no `cliUsage`), and resume via a `--conversation` id rather than the `.jsonl`-probe `--resume/--session-id` scheme. The structural skeleton to mirror is identical: extract messages → validate model → per-session lock → ensure stable workdir → write system prompt → build args → exec under ctx with filtered env → stream chunks via `onChunk` → assemble final `ChatResponse{Content, Usage, FinishReason}`. Usage/finishReason will need a different (or absent/heuristic) source since plain-text output won't carry token counters.

---

# claude:parse

I have everything needed. Here is my complete analysis.

---

# Part 1 — Verbatim extraction: `claude_cli_parse.go` stream-json parsing

## 1a. Full stream-json / JSON parsing logic

The CLI parser (`claude_cli_parse.go` + the line-by-line scanner in `claude_cli_chat.go`) recognizes a small, fixed set of event shapes. They are driven off a top-level `"type"` discriminator. The relevant struct definitions live in `claude_cli_types.go`:

- `cliJSONResponse` (the `--output-format json` envelope): `type` ("result"), `subtype` ("success"|"error"), `result` (the text), `session_id`, `model`, `cost_usd`, `usage`.
- `cliStreamEvent` (one line of `--output-format stream-json`): `type` ("assistant"|"result"|"system"), `subtype`, `message` (`*cliStreamMsg`, for assistant), `result` (for result), `model`, `cost_usd`, `usage`.
- `cliStreamMsg`: `{ content: []cliContentBlock }`.
- `cliContentBlock`: `type` ("text"|"thinking"|"tool_use"|"tool_result"), `text`, `thinking`.
- `cliUsage`: `input_tokens`, `output_tokens`.

There are **three ingestion routes**, tried in order by `parseJSONResponse(data)`:

**Route A — whole output is a JSON array** (`parseJSONArray`): requires `trimmed[0] == '['`, unmarshals to `[]json.RawMessage`, iterates. For each element it unmarshals an anonymous struct of `{type, subtype, result, message, usage}` and switches on `type`:
- `case "result"`: `resultText = ev.Result`; if `subtype == "error"` then `finishReason = "error"`; if `usage != nil`, builds `Usage{PromptTokens: input, CompletionTokens: output, TotalTokens: input+output}`.
- `case "assistant"`: unmarshals `ev.Message` into `cliStreamMsg`, walks `msg.Content`, and for each `block.Type == "text"` appends `block.Text` to an `assistantText strings.Builder`.

After the loop, content preference is: `resultText` first, else concatenated `assistantText`. If both empty → returns `nil`. Otherwise returns `ChatResponse{Content, FinishReason, Usage}`.

**Route B — newline-delimited JSON objects** (fallback): splits `data` on `\n`, trims each line, skips empties, calls `parseSingleJSONResult(line)`. That function unmarshals one line into `cliJSONResponse`, returns `nil` unless `resp.Type == "result"`, then builds `ChatResponse{Content: resp.Result, FinishReason: "stop"}` (overridden to `"error"` if `subtype == "error"`), with `Usage` mapped the same way. Returns on the **first** `result` line found.

**Route C — last resort** (plain text): `strings.TrimSpace(string(data))`; if empty → error `"claude-cli: empty response"`; else returns `ChatResponse{Content: trimmed, FinishReason: "stop"}`. This is the only path that treats arbitrary non-JSON output as the answer.

**Streaming path** (`claude_cli_chat.go` `ChatStream`, the live counterpart): `bufio.Scanner` over stdout, one `cliStreamEvent` per line:
- `type == "assistant"`: `extractStreamContent(ev.Message)` → emits `StreamChunk{Content: text}` and accumulates into `contentBuf`; `thinking` → `StreamChunk{Thinking: thinking}`.
- `type == "result"`: sets `finalResp.Content = ev.Result` (or `contentBuf.String()` if `Result` empty), `FinishReason` = "stop"/"error", maps `Usage`.
- Malformed lines are skipped with a debug log. After the loop, if no result event seen, `finalResp.Content = contentBuf.String()`.

## 1b. Helpers that extract the final assistant text

- `parseJSONArray` — the array reducer (text-preference logic above), the primary final-text extractor for batch `json` output.
- `parseSingleJSONResult` — extracts final text from a single `result` line (NDJSON case).
- `extractStreamContent(msg *cliStreamMsg) (text, thinking string)` — the per-message block reducer: two `strings.Builder`s, `case "text"` → textBuf, `case "thinking"` → thinkBuf. This is the live-stream text extractor.

## Key gaps relevant to the agy task

1. **`session_id` is parsed structurally but never consumed.** `cliJSONResponse.SessionID` exists, but neither `parseJSONResponse`, `parseJSONArray`, nor `parseSingleJSONResult` reads it, and `ChatResponse` has no session-id field. Session continuity is achieved entirely out-of-band via deterministic UUID v5 (`deriveSessionUUID`) + on-disk session-file probing (`sessionFileExists`) → `--resume`/`--session-id` flags. **There is no system/init event handling and no session-id-from-output flow.**
2. **`tool_use` / `tool_result` are recognized as enum values but dropped.** `extractStreamContent` only handles `text` and `thinking`; `parseJSONArray` only handles `text`. Tool calls never become `ToolCall` objects from the CLI parser. (`ToolCall` exists in `types.go:154`; `ToolCalling: true` is advertised in `claude_cli.go:147` but tool execution is routed through the MCP bridge, not surfaced from output parsing.)
3. **No agy parser exists yet** — `internal/providers/agy_*` is empty; this is greenfield.

The clean takeaway for agy: the existing CLI design depends entirely on structured JSON. agy gives none, so an `agy_cli_parse.go` is the moral equivalent of **Route C only** — there is no array/NDJSON/event scaffolding to lean on. Everything reduces to "produce the single best-guess final-answer string from a noisy text blob," plus optional out-of-band session handling exactly like the CLI does (UUID + flags, never from output).

---

# Part 2 — Proposed strategy for `agy_cli_parse.go`

## Problem framing

agy `-p` over a PTY emits a human TUI stream: ANSI color/style SGR codes, cursor-movement and line-erase sequences, alternate-screen toggles, spinner frames (Braille `⠋⠙…`, `|/-\`), live-updating tool-execution panels, and finally the answer. There is **no machine boundary** marking where the answer begins. Unlike claude-cli, we cannot key off `"type":"result"`. The parser is a best-effort text salvage operation, and its contract should be: return the most plausible final-answer text, plus a confidence signal, never panic, never return raw ANSI garbage.

## Pipeline (ordered stages)

**Stage 0 — Capture discipline (out of scope of the parser but constrains it).** Prefer to neutralize noise at the source so the parser has less to undo: set `TERM=dumb`, `NO_COLOR=1`, `CI=1`, `COLUMNS` to a wide fixed value, and disable paging (`PAGER=cat`). If agy honors any of these, spinners/colors largely vanish and the parser's job collapses to trimming. The parser must still be robust if they're ignored, but document the assumption.

**Stage 1 — Normalize line endings & carriage-return overwrites.** PTY spinners and progress lines repaint by emitting `\r` (and sometimes cursor-up + erase) to overwrite the same physical line. Process the byte stream so that a `\r` not followed by `\n` *resets the current logical line to empty* (the later content wins). This single step kills the vast majority of spinner/progress noise because every intermediate frame is overwritten by its successor and only the last write of each line survives. Handle `\r\n` → `\n` first so Windows line endings aren't mangled.

**Stage 2 — Strip ANSI / control sequences.** Remove, in order:
- CSI sequences: `ESC [ ... <final-byte 0x40–0x7E>` (covers SGR color `m`, cursor moves `A/B/C/D/H/f`, erase `J/K`, etc.).
- OSC sequences: `ESC ] ... (BEL | ESC \)` (terminal title sets, hyperlinks `ESC]8;;`).
- Two-byte sequences and charset selects: `ESC (` … , `ESC =`, `ESC >`, alternate-screen `ESC[?1049h/l` (caught by CSI rule), bracketed-paste toggles.
- Lone control chars except `\n` and `\t`: strip `\x00–\x08`, `\x0b`, `\x0c`, `\x0e–\x1f`, `\x7f`. Keep `\t` (may be meaningful indentation) — but consider expanding tabs.

Implement as a single precompiled regexp set (or a small hand-rolled state machine, which is more robust against partial/split sequences than regex if you ever stream-process). Given agy output is captured whole before parsing (like claude-cli's `cmd.Output()`), regex over the full buffer is acceptable and simplest.

**Stage 3 — Drop spinner/tool-UI lines.** After Stage 1–2, residual noise is whole lines that are UI chrome, not answer. Heuristics to discard a line:
- Pure spinner glyph lines: line (after trim) is only Braille spinner chars `[\x{2800}-\x{28FF}]` or `[|/\-\\]` optionally + a short status word.
- Box-drawing/panel borders: lines composed mostly of `─ │ ┌ ┐ └ ┘ ├ ┤ ╭ ╮ ╰ ╯ ═ ║ • · …` and similar (Unicode box-drawing block `\x{2500}-\x{257F}` + common bullets).
- Tool-execution status prefixes: lines starting with known agy UI markers (e.g. `✓ `, `✗ `, `⏺ `, `● `, `⎿ `, `Running…`, `Tool:`, `[tool]`, elapsed-time tags like `(1.2s)`). This list must be **maintained as a denylist constant** (mirror how `claude_cli_deny_patterns.go` keeps a pattern list) and version-pinned, since agy's TUI strings can change between releases.
- Progress/percentage lines: `^\s*\d+%` or `\[\s*=*>*\.*\s*\]`.

Crucially, make removal **conservative**: only drop a line if it matches a noise pattern *and* contains little-to-no alphabetic answer content. Never drop a line just because it's short.

**Stage 4 — Locate the final-answer region.** This is the hard part since there's no delimiter. Strategy, in priority order:
1. **Sentinel injection (most robust — do this if at all possible).** Wrap the user prompt so the model is instructed to bracket its answer, e.g. emit `<<<AGY_ANSWER_BEGIN>>>` … `<<<AGY_ANSWER_END>>>`. Then extraction is a deterministic substring between sentinels — no heuristics needed. This is the single highest-leverage move and converts agy from "scrape" to "parse." Fall back to heuristics only if sentinels are absent (model ignored instruction / truncated).
2. **Last-block heuristic.** After noise removal, the answer is almost always the final contiguous run of non-noise text lines, after the last tool-UI panel. Take everything after the last discarded tool/spinner block, trim leading/trailing blank lines.
3. **Marker heuristic.** If agy prints a stable "final answer" lead-in (some TUIs print a distinct glyph/label before the assistant's prose), split on the last occurrence of that marker. Keep this in the same maintained denylist/marker constant.

**Stage 5 — Final cleanup.** Collapse 3+ consecutive blank lines to one, `strings.TrimSpace` the whole result, strip a trailing prompt/echo if agy re-prints the prompt, and return. If the result is empty after all stages, fall back to returning the Stage-2 (ANSI-stripped) full text rather than nothing — mirroring claude-cli Route C's "never lose the answer entirely" philosophy — and flag low confidence.

## Output contract

Return something richer than a bare string so callers can react to degraded extraction:
- `Content string` — the extracted answer.
- `FinishReason string` — "stop"; set "error" if agy exit code non-zero or stderr indicates failure.
- `Confidence` enum (`high` = sentinel matched / `medium` = clean last-block / `low` = Route-C fallback used). Log low-confidence extractions for tuning.
- Session id: handle exactly like claude-cli — **out of band** via deterministic UUID + flags, *not* scraped from PTY text. Do not attempt to parse a session id out of agy's human output.

## Concrete failure modes to guard against

1. **ANSI sequence spanning a chunk boundary** — if you ever stream-process instead of buffering whole, a CSI can be split across reads. Mitigation: buffer full output (agy `-p` is request/response, like `cmd.Output()`), or use a stateful stripper that holds incomplete escape sequences.
2. **Answer text legitimately contains box-drawing or spinner-like characters** (code blocks with ASCII tables, a message about `|/-\`). Mitigation: only treat box-drawing/spinner lines as noise when they dominate the line and there's no surrounding answer prose; never strip inside a detected code fence.
3. **Spinner repaint via cursor-up rather than `\r`** — some TUIs move the cursor up N lines and rewrite, which Stage-1's `\r` logic won't catch. Mitigation: Stage-2 strips the cursor-move CSI, and Stage-3's whole-line spinner denylist catches the orphaned frames; additionally dedupe consecutive identical lines.
4. **Color codes embedded mid-word** (`foo\x1b[0mbar` → must become `foobar`, not `foo bar`). Mitigation: strip with zero-width replacement, never substitute a space.
5. **Truncated output** (process killed, context cancelled, PTY EOF mid-answer). Mitigation: return whatever salvaged text exists with `FinishReason` reflecting the error and low confidence, mirroring claude-cli's "if we got partial content, return it with the error."
6. **No sentinel + answer is short / interleaved with a trailing tool call** — last-block heuristic grabs a tool panel instead of prose. Mitigation: prefer sentinels; when absent, require the chosen block to have a minimum alpha-char ratio and not match the tool denylist.
7. **Prompt echo** — PTY may echo the typed prompt back. Mitigation: detect and strip a leading region equal to (or prefixed by) the submitted prompt.
8. **agy TUI string drift across versions** — denylist markers go stale. Mitigation: keep all UI markers/regexes in one version-pinned constant block with a comment citing the agy version tested against (same pattern as `claude_cli_deny_patterns.go`), and add table-driven golden-file tests with captured real PTY transcripts so regressions surface on agy upgrades.
9. **OSC-8 hyperlinks** (`ESC]8;;URL ESC\ text ESC]8;; ESC\`) — naive stripping can delete the visible link text. Mitigation: strip the OSC wrapper bytes but preserve the inner visible text.
10. **Locale/encoding** — Braille/box-drawing are multibyte UTF-8; byte-level regex must be UTF-8-safe. Mitigation: operate on runes / use `\x{…}` unicode classes, not raw byte ranges, for the glyph denylists.

## Testing approach

Table-driven golden tests with **captured raw PTY byte transcripts** (store as testdata fixtures, including the ESC bytes) → expected clean answer, exactly analogous to how `claude_cli_session_test.go` asserts on exact shapes. Cover: clean (TERM=dumb) output, full-ANSI output, spinner-heavy output, output with a tool panel before the answer, truncated output, answer-contains-table-chars, and sentinel-wrapped output.

---

## Files referenced
- `/Users/stanleykao72/Documents/goclaw-fork/internal/providers/claude_cli_parse.go` — the parser (verbatim logic above)
- `/Users/stanleykao72/Documents/goclaw-fork/internal/providers/claude_cli_types.go` — CLI JSON struct shapes
- `/Users/stanleykao72/Documents/goclaw-fork/internal/providers/claude_cli_chat.go` — live stream-json scanner + session/flag wiring (lines 154–206 event switch; 36–55 session handling)
- `/Users/stanleykao72/Documents/goclaw-fork/internal/providers/claude_cli_session.go` — out-of-band session id (`deriveSessionUUID`, `sessionFileExists`) — the model agy session handling should copy
- `/Users/stanleykao72/Documents/goclaw-fork/internal/providers/claude_cli_session_test.go` — test style to mirror (table-driven, exact-shape assertions)
- `/Users/stanleykao72/Documents/goclaw-fork/internal/providers/claude_cli_deny_patterns.go` — existing maintained-pattern-constant precedent for the agy UI marker denylist
- `/Users/stanleykao72/Documents/goclaw-fork/internal/providers/types.go:83,104,154,177` — `ChatResponse` / `StreamChunk` / `ToolCall` / `Usage` definitions the agy parser must populate

---

# claude:mcp-hooks

No existing agy references in the goclaw-fork repo (this is a greenfield translation). I have all the verbatim source I need. Here is the complete extraction and analysis.

---

# claude_cli MCP + hooks — verbatim extraction & agy translation analysis

Files read in full: `internal/providers/claude_cli_mcp.go`, `internal/providers/claude_cli_hooks.go`. Supporting: `claude_cli.go` (struct + options), `claude_cli_session.go` (`buildArgs`/`resolveMCPConfigPath`), `claude_cli_deny_patterns.go` (`ShellDenyPatterns`), `cmd/gateway_providers.go` (wiring).

## 1. `MCPConfigData` struct + `BuildCLIMCPConfigData` signature

Verbatim (`claude_cli_mcp.go:34-77`):

```go
// MCPConfigData holds the base MCP server entries built at startup.
// Per-session configs are written via WriteMCPConfig with agent context injected.
type MCPConfigData struct {
	Servers        map[string]any // external MCP server entries (stdio/sse/http)
	GatewayAddr    string
	GatewayToken   string
	AgentMCPLookup MCPServerLookup // optional: resolves per-agent MCP servers from DB
}

func BuildCLIMCPConfigData(servers map[string]*config.MCPServerConfig, gatewayAddr string, gatewayToken ...string) *MCPConfigData
```

What `BuildCLIMCPConfigData` produces: iterates `servers`, skips any where `!srv.IsEnabled()`, converts each via `mcpServerEntryToConfig` into the CLI per-server map, keyed by server name in `Servers`. It captures `gatewayAddr` and an optional variadic `gatewayToken` (only `[0]` used). It deliberately does **NOT** add the `goclaw-bridge` entry — that is injected per-session in `WriteMCPConfig` (comment lines 44-45).

The per-server JSON shape comes from `mcpServerEntryToConfig` (`claude_cli_mcp.go:216-247`), switching on `Transport`:
- `stdio` → `{"command":..., "args":[...], "env":{...}}`
- `sse` → `{"url":..., "type":"sse", "headers":{...}}`
- `streamable-http` → `{"url":..., "type":"http", "headers":{...}}`

The final on-disk document (`claude_cli_mcp.go:180`) is wrapped as:

```json
{ "mcpServers": { "<name>": { ...entry... }, "goclaw-bridge": {...} } }
```

This is exactly the file passed to `claude --mcp-config <path>` (`claude_cli_session.go:42`).

## 2. How the per-session MCP config file is written (path, format, bridge entry + identity injection)

Entry point `WriteMCPConfig` (`claude_cli_mcp.go:101-103`) delegates to `writeMCPConfigInternal` (lines 105-213). Flow:

**Path** — NOT in the agent's workDir, on purpose. `mcpConfigBaseDir()` returns `ResolvedDataDirFromEnv()/mcp-configs` (lines 81-83), then per-session subdir `sanitizePathSegment(sessionKey)`, then file `mcp-config.json`:

```
<dataDir>/mcp-configs/<safe-session-key>/mcp-config.json   (docstring example: ~/.goclaw/mcp-configs/<safe-session-key>/mcp-config.json)
```

Dir created `0700`, file written `0600`. Comment (lines 79-80, 98-99): kept outside workDir **"so agent cannot read tokens from the MCP config files."**

**Write mechanics** — `json.MarshalIndent({"mcpServers": servers})` (line 180); skip-write if byte-identical to existing (lines 197-199); atomic temp-file `+ ".tmp"` then `os.Rename` to avoid partial reads (lines 201-210). Returns the final path (or `""` on any failure / empty config).

**Base copy + per-agent injection** — shallow-copies `d.Servers` (lines 112-113) so the shared base map isn't mutated. If `d.AgentMCPLookup != nil && agentID != ""`, resolves per-agent DB-backed servers and adds any whose name doesn't already exist (won't override static/bridge entries) (lines 116-126).

**`goclaw-bridge` entry + identity injection** (lines 129-173) — only when `GatewayAddr != ""`. Builds a `headers` map; every value is rejected if it contains `\r \n \x00` (CRLF/null header-injection guard). Headers, all from `BridgeContext`:

| Header | Source field |
|--------|-------------|
| `Authorization: Bearer <token>` | `GatewayToken` |
| `X-Agent-ID` | `bc.AgentID` |
| `X-User-ID` | `bc.UserID` |
| `X-Channel` | `bc.Channel` |
| `X-Chat-ID` | `bc.ChatID` |
| `X-Peer-Kind` | `bc.PeerKind` |
| `X-Workspace` | `bc.Workspace` |
| `X-Tenant-ID` | `bc.TenantID` |
| `X-Local-Key` | `bc.LocalKey` |
| `X-Session-Key` | `sessionKey` |
| `X-Bridge-Sig` | HMAC over all context fields |

The bridge server entry itself (lines 166-173):

```go
bridgeEntry := map[string]any{
    "url":  fmt.Sprintf("http://%s/mcp/bridge", d.GatewayAddr),
    "type": "http",
}
if len(headers) > 0 { bridgeEntry["headers"] = headers }
servers["goclaw-bridge"] = bridgeEntry
```

**Anti-forgery** — `X-Bridge-Sig` is set only when `GatewayToken != "" && (agentID != "" || userID != "")` via `SignBridgeContext` (lines 267-275): HMAC-SHA256 keyed by the gateway token over `agentID|userID|channel|chatID|peerKind|workspace|tenantID` plus pipe-joined `extra` (here `localKey`, `sessionKey`). `VerifyBridgeContext` (lines 283-305) checks the current format and three backward-compatible fallbacks (no-localKey, no-tenantID, no-workspace+tenantID), returning `(ok, tenantVerified)` — callers must not trust `X-Tenant-ID` when `tenantVerified == false`.

So identity is injected NOT as `Opt*` argv values but as **HTTP headers baked into the per-session MCP config file**, HMAC-signed so the bridge server can authenticate the calling agent/user/tenant. The CLI consumes it purely via `--mcp-config <path>`; built-in tools are disabled (`--disallowedTools ...`, `claude_cli_session.go:62-66`) so all tool calls route through the signed bridge.

## 3. `BuildCLIHooksConfig` / hooks settings.json shape & enforced security

Signature (`claude_cli_hooks.go:16`):

```go
func BuildCLIHooksConfig(workspace string, restrictToWorkspace bool) (string, func(), error)
```

Writes two files into `os.TempDir()/goclaw-cli-hooks/` with an 8-char uuid suffix: `hook-<id>.sh` (mode `0755`) and `settings-<id>.json` (mode `0600`); returns the settings path plus a cleanup func that removes both.

**settings.json shape** (`generateSettingsJSON`, lines 48-82) — registers the same hook script as a `command`-type `PreToolUse` hook for four matchers:

```json
{
  "hooks": {
    "PreToolUse": [
      { "matcher": "Bash",  "hooks": [{"type":"command","command":"<hookPath>"}] },
      { "matcher": "Write", "hooks": [{"type":"command","command":"<hookPath>"}] },
      { "matcher": "Edit",  "hooks": [{"type":"command","command":"<hookPath>"}] },
      { "matcher": "Read",  "hooks": [{"type":"command","command":"<hookPath>"}] }
    ]
  }
}
```

Passed to the CLI as `--settings <settingsPath>` (`claude_cli_session.go:69`).

**What the hook enforces** (`generateHookScript`, lines 85-207) — bash script (`set -euo pipefail`) reads the tool call from stdin, extracts `.tool_name` / `.tool_input` via `jq`, and emits a `PreToolUse` allow/deny decision JSON:

1. **Shell deny patterns** — for `Bash`, greps `.command` against all `ShellDenyPatterns` regexes; any match → `deny("security: shell command blocked by deny pattern")`. `ShellDenyPatterns` (`claude_cli_deny_patterns.go`, mirrors `internal/tools/shell.go`) covers destructive file ops (`rm -rf`, `mkfs`, `dd if=`, fork bombs, `shutdown/reboot`), data exfiltration (`curl|sh`, `curl -d/-F/-T/-X POST`, `wget --post`, `nslookup/dig`, `/dev/tcp/`), reverse shells (`nc -e`, `socat`, `openssl s_client`, scripting-lang socket one-liners, `mkfifo`), and eval/code-injection (`eval $`, `base64 -d | sh`).
2. **Workspace path restriction** — emitted only when `restrictToWorkspace && workspace != ""`. For `Write`/`Edit`/`Read`, `realpath -m`-resolves `.file_path` and `deny("security: path outside workspace boundary")` unless it has the `$WORKSPACE` prefix. Workspace string is single-quote-escaped before embedding.
3. Default fall-through → `allow`.

Wiring (`cmd/gateway_providers.go`): chat provider `WithClaudeCLISecurityHooks(<workspace>, ...)` (line 216); summoner path uses `WithClaudeCLISecurityHooks("", true)` (line 321) — i.e. deny-patterns active, path restriction skipped because `workspace == ""`.

---

# ANALYSIS — translating this to an `agy_cli_mcp.go`

## A. The structural mismatch

`claude` CLI accepts an explicit `--mcp-config <file>` flag, so GoClaw writes a fresh, per-session file in a token-safe location (`<dataDir>/mcp-configs/<session>/mcp-config.json`) outside the workDir and points the CLI at it. **agy has no `--mcp-config` flag.** It reads MCP servers only from two fixed on-disk locations:

- **Global**: `~/.gemini/config/...` (agy's global MCP server registry)
- **Per-workspace**: `<workspace>/.agents/mcp_config.json`

agy also has **no `--settings` / hooks equivalent** — there is no PreToolUse interception point. Its only safety lever is `--sandbox`.

This means the translation is fundamentally about **where the file lands and how identity is delivered**, not the JSON content (which can stay structurally similar).

## B. How `agy_cli_mcp.go` should translate `MCPConfigData`

Reuse `MCPConfigData`, `BridgeContext`, `mcpServerEntryToConfig`, `SignBridgeContext`, and the `goclaw-bridge` HTTP-with-signed-headers entry **unchanged** — none of that is claude-specific; the bridge identity model (Authorization + `X-*` headers + `X-Bridge-Sig` HMAC) is transport-agnostic and works for any MCP client that supports HTTP servers with custom headers.

The differences are confined to: (1) target path, (2) JSON top-level key, (3) the fact that the file is the agent's discovered config rather than a flag argument.

**1. Target path — write to the workspace location agy actually reads.**
Instead of `<dataDir>/mcp-configs/<session>/mcp-config.json`, write to `<workspace>/.agents/mcp_config.json` (the per-workspace file agy discovers). Keep the atomic temp+rename and skip-if-unchanged logic verbatim.

**2. Top-level JSON key — match agy's schema.**
The claude file wraps servers under `"mcpServers"`. agy's `.agents/mcp_config.json` uses agy's own top-level key/shape; `agy_cli_mcp.go` must emit that shape (e.g. `{"mcpServers": {...}}` or agy's equivalent — verify against agy's loader). The per-server entries (`command/args/env`, `url/type/headers`) are MCP-standard and likely map 1:1; the `goclaw-bridge` HTTP entry with signed headers is reused as-is.

**3. Identity injection — same mechanism, different delivery.**
Identity stays in the `goclaw-bridge` HTTP headers + `X-Bridge-Sig` HMAC, exactly as in claude_cli. The CLI invocation drops `--mcp-config` entirely; agy picks the file up by location. `resolveMCPConfigPath`'s argv side-effect (`args = append(args, "--mcp-config", path)`) has no agy analog and is removed.

## C. The hard, unavoidable regression: NO hooks/deny-pattern layer in agy

**State this explicitly: the entire `claude_cli_hooks.go` security layer has no agy equivalent.** agy exposes no `--settings`, no `PreToolUse` hook, no command-hook interception. Therefore:

- `ShellDenyPatterns` enforcement (blocking `rm -rf`, `curl|sh`, reverse shells, `dd if=`, etc. at tool-call time) — **cannot be reproduced** in `agy_cli_mcp.go`. There is no hook to run them.
- Workspace path restriction on `Read`/`Write`/`Edit` (the `realpath -m` + `$WORKSPACE` prefix check) — **cannot be reproduced** as a per-tool-call gate.

The only security control agy offers is **`--sandbox`**, a coarse OS/container-level isolation flag. It is categorically different from GoClaw's fine-grained, pattern-based PreToolUse deny: `--sandbox` confines the process environment but does not inspect or veto individual shell commands or file paths against GoClaw's deny list. So `agy_cli_mcp.go` should pass `--sandbox` as the best-available substitute, while documenting that it is **strictly weaker** than the claude_cli hook layer — the deny-pattern and path-boundary guarantees are lost on agy and must instead be relied upon at the MCP-bridge boundary (which is where built-in tools are already disabled for claude via `--disallowedTools`).

A mitigating design note: because the claude_cli flow already disables CLI built-in tools and forces everything through the signed `goclaw-bridge` (so the bridge server is the real policy enforcement point), the most robust path for agy is the same — route all tool execution through the bridge and enforce deny-patterns/path-restrictions **server-side at the bridge**, rather than depending on a client-side hook that agy cannot provide. That makes the missing hook layer a recoverable gap if (and only if) agy can likewise be constrained to bridge-only tools; if agy cannot disable its built-in Bash/file tools, then native (unhooked, only `--sandbox`-confined) tool calls remain a real residual risk with no equivalent of the claude deny layer.

## D. Summary table

| Concern | claude_cli (today) | agy_cli (translation) |
|---|---|---|
| Config delivery | `--mcp-config <file>` flag | fixed file: `<workspace>/.agents/mcp_config.json` (+ global `~/.gemini/config/...`) |
| File location | `<dataDir>/mcp-configs/<session>/mcp-config.json` (outside workDir, token-safe) | workspace `.agents/` — **note: inside workspace, less token-isolated than claude's out-of-workDir placement** |
| Top-level JSON key | `"mcpServers"` | agy's expected key/shape (verify vs agy loader) |
| Per-server entries | `mcpServerEntryToConfig` (stdio/sse/http) | reused unchanged |
| Bridge identity | `goclaw-bridge` HTTP entry + `X-*` headers + `X-Bridge-Sig` HMAC | reused unchanged |
| Per-agent DB MCP injection | `AgentMCPLookup` | reused unchanged |
| Atomic write / skip-if-unchanged | temp+rename, 0600/0700 | reused (target dir differs) |
| Shell deny patterns | PreToolUse hook (`--settings`) | **NO equivalent** — gone |
| Workspace path restriction | PreToolUse hook | **NO equivalent** — gone |
| Process isolation | (hooks) | `--sandbox` only (coarse, strictly weaker) |

Relevant source paths: `/Users/stanleykao72/Documents/goclaw-fork/internal/providers/claude_cli_mcp.go`, `/Users/stanleykao72/Documents/goclaw-fork/internal/providers/claude_cli_hooks.go`, `/Users/stanleykao72/Documents/goclaw-fork/internal/providers/claude_cli_deny_patterns.go`, `/Users/stanleykao72/Documents/goclaw-fork/internal/providers/claude_cli_session.go` (buildArgs lines 32-73, resolveMCPConfigPath 76-85), `/Users/stanleykao72/Documents/goclaw-fork/internal/providers/claude_cli.go` (struct 44-56, options 88-118), `/Users/stanleykao72/Documents/goclaw-fork/cmd/gateway_providers.go` (wiring lines 213-216, 321-325). No existing agy/gemini provider code exists in the repo — this is a greenfield translation.

---

# config-registration-buildenv

I now have everything confirmed. Key finding on `creack/pty`: it is a **transitive (indirect) dependency** pulled in via `charmbracelet/huh` → `charmbracelet/x/xpty`, but it is **NOT listed in go.mod** (only in go.sum) and **NOT imported by any goclaw source code**. So for agy-cli it must be added as a direct dependency. Config format is **JSON5** (json5.Unmarshal), not yaml/viper — the yaml struct tags exist but are unused by the loader.

Here is the integration checklist.

---

# agy-cli Integration Checklist — wiring extracted from claude-cli

Repo: `/Users/stanleykao72/Documents/goclaw-fork` · module `github.com/nextlevelbuilder/goclaw`

## 1. Config struct definition

### `ClaudeCLIConfig` — `internal/config/config_channels.go:230-236` (verbatim)
```go
// ClaudeCLIConfig configures the Claude CLI provider (uses subscription, not API key).
type ClaudeCLIConfig struct {
	CLIPath     string `json:"cli_path" yaml:"cli_path"`           // path to claude binary (default: "claude")
	Model       string `json:"model" yaml:"model"`                 // default model alias (default: "sonnet")
	BaseWorkDir string `json:"base_work_dir" yaml:"base_work_dir"` // base dir for agent workspaces
	PermMode    string `json:"perm_mode" yaml:"perm_mode"`         // permission mode (default: "bypassPermissions")
}
```

### Embedding in parent `ProvidersConfig` — `internal/config/config_channels.go:198-222` (verbatim, abridged to relevant lines)
```go
// ProvidersConfig maps provider name to its config.
type ProvidersConfig struct {
	Anthropic  ProviderConfig  `json:"anthropic"`
	...
	Ollama      OllamaConfig    `json:"ollama"`       // local Ollama instance (no API key needed)
	OllamaCloud ProviderConfig  `json:"ollama_cloud"` // Ollama Cloud (API key required)
	ClaudeCLI   ClaudeCLIConfig `json:"claude_cli"`
	ACP         ACPConfig       `json:"acp"`
	Novita         ProviderConfig  `json:"novita"`          // Novita AI (OpenAI-compatible endpoint)
	BytePlus       ProviderConfig  `json:"byteplus"`        // BytePlus ModelArk (Seed 2.0)
	BytePlusCoding ProviderConfig  `json:"byteplus_coding"` // BytePlus ModelArk Coding Plan
}
```
`ProvidersConfig` is itself reached via `Config.Providers` at `internal/config/config.go:47`: `Providers ProvidersConfig \`json:"providers"\``.

### Config file format — **JSON5, NOT yaml/viper**
`internal/config/config_load.go:66-85`: `Load()` does `os.ReadFile(path)` then `json5.Unmarshal(data, cfg)`. The `yaml:"..."` struct tags are **dead/cosmetic** — no yaml or viper decoder is wired. Env overrides are applied on top via `applyEnvOverrides()`.

### Env-var wiring — `internal/config/config_load.go:152-155` (verbatim)
```go
	// Claude CLI provider
	envStr("GOCLAW_CLAUDE_CLI_PATH", &c.Providers.ClaudeCLI.CLIPath)
	envStr("GOCLAW_CLAUDE_CLI_MODEL", &c.Providers.ClaudeCLI.Model)
	envStr("GOCLAW_CLAUDE_CLI_WORK_DIR", &c.Providers.ClaudeCLI.BaseWorkDir)
```
(Note: `PermMode` has NO env override — config-file only.)

### Also wired into `HasAnyProvider()` — `internal/config/config_channels.go:321` (verbatim)
```go
		p.ClaudeCLI.CLIPath != "" ||
```
agy-cli must add an analogous `p.AgyCLI.CLIPath != ""` clause here, or `HasAnyProvider()` won't count an agy-only install.

---

## 2. Registration sites (verbatim, file:line)

### A. Global `Register` (config-driven) — `cmd/gateway_providers.go:198-220`
```go
	// Claude CLI provider (subscription-based, no API key needed)
	if cfg.Providers.ClaudeCLI.CLIPath != "" {
		cliPath := cfg.Providers.ClaudeCLI.CLIPath
		var opts []providers.ClaudeCLIOption
		if cfg.Providers.ClaudeCLI.Model != "" {
			opts = append(opts, providers.WithClaudeCLIModel(cfg.Providers.ClaudeCLI.Model))
		}
		if cfg.Providers.ClaudeCLI.BaseWorkDir != "" {
			opts = append(opts, providers.WithClaudeCLIWorkDir(cfg.Providers.ClaudeCLI.BaseWorkDir))
		}
		if cfg.Providers.ClaudeCLI.PermMode != "" {
			opts = append(opts, providers.WithClaudeCLIPermMode(cfg.Providers.ClaudeCLI.PermMode))
		}
		// Build per-session MCP config: external MCP servers + GoClaw bridge
		gatewayAddr := loopbackAddr(cfg.Gateway.Host, cfg.Gateway.Port)
		mcpData := providers.BuildCLIMCPConfigData(cfg.Tools.McpServers, gatewayAddr, cfg.Gateway.Token)
		opts = append(opts, providers.WithClaudeCLIMCPConfigData(mcpData))
		// Enable GoClaw security hooks (shell deny patterns, path restrictions)
		opts = append(opts, providers.WithClaudeCLISecurityHooks(
			cfg.Providers.ClaudeCLI.BaseWorkDir, true))
		registry.Register(providers.NewClaudeCLIProvider(cliPath, opts...))
		slog.Info("registered provider", "name", "claude-cli")
	}
```

### B. Per-tenant `RegisterForTenant` (DB-driven) — `cmd/gateway_providers.go:300-330`
```go
	for _, p := range dbProviders {
		// Claude CLI doesn't need API key
		if !p.Enabled {
			continue
		}
		if p.ProviderType == store.ProviderClaudeCLI {
			cliPath := p.APIBase // reuse APIBase field for CLI path
			if cliPath == "" {
				cliPath = "claude"
			}
			// Validate: only accept "claude" or absolute path
			if cliPath != "claude" && !filepath.IsAbs(cliPath) {
				slog.Warn("security.claude_cli: invalid path from DB, using default", "path", cliPath)
				cliPath = "claude"
			}
			if _, err := exec.LookPath(cliPath); err != nil {
				slog.Warn("claude-cli: binary not found, skipping", "path", cliPath, "error", err)
				continue
			}
			var cliOpts []providers.ClaudeCLIOption
			cliOpts = append(cliOpts, providers.WithClaudeCLIName(p.Name))
			cliOpts = append(cliOpts, providers.WithClaudeCLISecurityHooks("", true))
			if gatewayAddr != "" {
				mcpData := providers.BuildCLIMCPConfigData(nil, gatewayAddr, gatewayToken)
				mcpData.AgentMCPLookup = buildMCPServerLookup(mcpStore)
				cliOpts = append(cliOpts, providers.WithClaudeCLIMCPConfigData(mcpData))
			}
			registry.RegisterForTenant(p.TenantID, providers.NewClaudeCLIProvider(cliPath, cliOpts...))
			slog.Info("registered provider from DB", "name", p.Name)
			continue
		}
```
Note: DB path reuses the `APIBase` field to carry the CLI binary path (`cliPath := p.APIBase`), and gates on `store.ProviderClaudeCLI` — agy-cli needs its own `store.ProviderAgyCLI` constant.

### C. HTTP in-memory registration — `internal/http/providers.go:159-174`
```go
	// Claude CLI doesn't need an API key — register immediately
	if p.ProviderType == store.ProviderClaudeCLI {
		cliPath := p.APIBase // reuse APIBase field for CLI path
		if cliPath == "" {
			cliPath = "claude"
		}
		var cliOpts []providers.ClaudeCLIOption
		cliOpts = append(cliOpts, providers.WithClaudeCLISecurityHooks("", true))
		if h.gatewayAddr != "" {
			mcpData := providers.BuildCLIMCPConfigData(nil, h.gatewayAddr, pkgGatewayToken)
			mcpData.AgentMCPLookup = h.mcpLookup
			cliOpts = append(cliOpts, providers.WithClaudeCLIMCPConfigData(mcpData))
		}
		h.providerReg.RegisterForTenant(p.TenantID, providers.NewClaudeCLIProvider(cliPath, cliOpts...))
		return
	}
```

### D. HTTP — three more special-case sites in `internal/http/providers.go`
- **Auth-status route** — `:133-134`:
```go
	// Claude CLI auth status (global — not per-provider)
	mux.HandleFunc("GET /v1/providers/claude-cli/auth-status", h.auth(h.handleClaudeCLIAuthStatus))
```
- **SSRF allowlist** (localhost provider types skip URL validation) — `:240-246`:
```go
// localProviderTypes are provider types that legitimately run on localhost
// (e.g. Ollama, Claude CLI). SSRF checks are skipped for these.
var localProviderTypes = map[string]bool{
	store.ProviderOllama:    true,
	store.ProviderClaudeCLI: true,
	store.ProviderACP:       true,
}
```
- **Singleton guard** (one CLI provider per instance) — `:328-342`:
```go
	// Only one Claude CLI provider is allowed per instance (1 machine = 1 auth session).
	// Mutex serializes check+create to prevent TOCTOU race.
	if p.ProviderType == store.ProviderClaudeCLI {
		h.cliMu.Lock()
		defer h.cliMu.Unlock()

		existing, _ := h.store.ListProviders(r.Context())
		for _, ep := range existing {
			if ep.ProviderType == store.ProviderClaudeCLI {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": i18n.T(locale, i18n.MsgAlreadyExists, "Claude CLI provider", "only one is allowed per instance"),
				})
				return
			}
		}
	}
```
Decision point for agy-cli: whether agy is also a 1-per-instance singleton (same auth-session constraint) — if yes, mirror this guard with a separate `agyMu` mutex (don't share `cliMu`, or claude+agy will serialize against each other unnecessarily, and the singleton check is keyed on provider type anyway).

### E. Tools special-casing — `internal/tools/read_image.go`
- Priority list `:35`: `var visionProviderPriority = []string{"openrouter", "gemini", "anthropic", "claude-cli", "dashscope"}`
- Model-default map `:43`: `"claude-cli": "",`
- `disable_tools` flag `:147-153`:
```go
	// claude-cli spawns the Claude CLI binary; loading its built-in MCP
	// toolset costs latency we don't need for a one-shot vision call. Keep
	// this flag scoped to claude-cli so other providers don't receive
	// options they ignore (or worse, choke on in the future).
	if providerName == "claude-cli" {
		opts["disable_tools"] = true
	}
```

### F. Tools special-casing — `internal/tools/read_document.go`
- Priority list `:55`: `var documentProviderPriority = []string{"gemini", "anthropic", "claude-cli", "openrouter", "dashscope"}`
- Model-default map `:62`: `"claude-cli": "",`

### G. Tools special-casing — `internal/tools/read_document_resolve.go:93-99`
```go
	// Scope disable_tools to claude-cli only — it's a CLI-bridge-specific
	// option that skips loading the built-in MCP toolset for one-shot calls.
	// Other providers silently ignore unknown keys today, but leaking
	// provider-specific flags into the shared Options map couples layers.
	if providerName == "claude-cli" {
		opts["disable_tools"] = true
	}
```
For each of E/F/G, add `"agy-cli"` to the priority slices, a `"agy-cli": ""` entry to the model-default maps, and OR-in `|| providerName == "agy-cli"` on the `disable_tools` conditionals (assuming agy is also a CLI-bridge provider that skips its built-in toolset for one-shot vision/doc calls).

---

## 3. go.mod — go version + PTY library

- `go` line — `go.mod:3`: `go 1.26.0`
- **`github.com/creack/pty` is NOT a direct dependency.** It does NOT appear in `go.mod` at all (grep returned 0). It is present only in `go.sum` (`go.sum:153-154`, version `v1.1.24`) as a **transitive/indirect** dep. `go mod why github.com/creack/pty` resolves the chain as `cmd → charmbracelet/huh → charmbracelet/huh.test → charmbracelet/x/xpty → creack/pty`. No goclaw source file imports `creack/pty` (grep of `*.go` found nothing).
- **Implication for agy-cli:** if agy needs a PTY, you must add `github.com/creack/pty` as an explicit direct require (`go get github.com/creack/pty`). It will resolve to the already-locked `v1.1.24` from go.sum. Alternatively, `charmbracelet/x/conpty` (`go.sum:121`, v0.1.0) and `charmbracelet/x/xpty` (`go.sum:133`, v0.1.2) are also already in go.sum as the cross-platform pty wrappers huh pulls in. Note claude-cli itself does not use a PTY — it drives the binary via stream-json over stdin/stdout (see test pattern below), so if agy-cli mirrors that I/O model you may not need pty at all.

---

## 4. Dockerfile — claude CLI install

`Dockerfile:107-110` installs via **npm** (global), gated behind build arg `ENABLE_CLAUDE_CLI` (declared `:76` `ARG ENABLE_CLAUDE_CLI=false`):
```dockerfile
    if [ "$ENABLE_CLAUDE_CLI" = "true" ]; then \
        npm install -g --cache /tmp/npm-cache @anthropic-ai/claude-code@^2.1.91; \
        rm -rf /tmp/npm-cache; \
    fi; \
```
The node runtime it depends on is conditionally added at `:103-105`:
```dockerfile
        if [ "$ENABLE_NODE" = "true" ] || [ "$ENABLE_CLAUDE_CLI" = "true" ]; then \
            apk add --no-cache nodejs npm; \
        fi; \
```
Credential persistence symlink at `:147-148`/`:155`: `/app/data/.claude` is created and symlinked to `/app/.claude` so CLI auth survives container recreation. agy-cli needs an analogous `ARG ENABLE_AGY_CLI`, its own `npm install -g ...` (or curl, depending on how agy ships), an OR-clause in the node-runtime guard, and its own credentials-dir symlink if it stores auth on disk.

---

## 5. Test pattern (so agy tests can mirror it)

Both test files are `package providers` (white-box, internal) in `internal/providers/`.

### `claude_cli_session_test.go` — input-builder unit test
- **No real binary, no subprocess.** Tests the pure helper `buildStreamJSONInput(prompt, []ImageContent) io.Reader` that constructs the stream-json stdin payload — the function that would be piped to the CLI's stdin. This is the key avoidance technique: the bytes-construction logic is factored into a pure function, so it's testable without ever spawning the CLI.
- **Table-driven:** `cases := []struct{ name string; images []ImageContent; wantTypes []string }{...}` then `for _, tc := range cases { t.Run(tc.name, func(t *testing.T){...}) }`.
- **Assertion style:** reads the reader with `io.ReadAll`, `json.Unmarshal` into an anonymous struct mirroring the `message.content[]` shape, then checks per-block `type` / `source.media_type` and the trailing text block. Uses `t.Fatalf` for setup failures, `t.Errorf` for per-block field mismatches.
- Covers MIME routing (png→image, pdf→document, mixed, unknown→image fallback) plus an empty-prompt edge case (`TestBuildStreamJSONInput_NoText`) asserting no trailing text block.

### `claude_cli_mcp_test.go` — HMAC bridge-signing unit test
- Tests two pure exported funcs: `SignBridgeContext(key, agent, user, channel, chat, kind, workspace, tenant, ...extra) string` and `VerifyBridgeContext(... , sig, ...extra) (ok bool, tenantVerified bool)`. Again **no subprocess** — pure crypto/string functions.
- **Not table-driven** here; instead many small focused `TestXxx_Scenario` functions (determinism, different key, field-order sensitivity, the 3 backward-compat verification "levels", invalid sig, empty fields, extra-param order). Each is a few `Sign`→`Verify`→assert lines.
- **Security-regression framing:** explicit tests that a replayed old signature with an injected tenant header yields `tenantVerified=false` (HMAC doesn't cover the header) — the kind of adversarial case agy's bridge-auth tests should mirror if agy reuses the same `SignBridgeContext`/`VerifyBridgeContext` (these are shared across CLI providers, not claude-specific, so agy likely reuses them verbatim rather than re-implementing).

### Pattern takeaways for agy-cli tests
1. Factor the stdin-payload builder and any auth-signing into **pure exported functions in `package providers`**; test those directly — never spawn the real `agy` binary in unit tests.
2. Use **table-driven `t.Run` subtests** for the payload/MIME matrix; use **many small named functions** for the security/edge matrix.
3. Reuse `SignBridgeContext`/`VerifyBridgeContext` (shared, not provider-specific) rather than duplicating; just add agy-specific payload-shape tests.
4. No helper/fixture funcs or fakes are needed because there's no real I/O — that's the deliberate design that keeps these tests binary-free.

---

### Summary of new sites agy-cli must add
| Area | File | What to add |
|------|------|-------------|
| config struct | `internal/config/config_channels.go` | `AgyCLIConfig` + field in `ProvidersConfig` + clause in `HasAnyProvider()` (:321) |
| env wiring | `internal/config/config_load.go` | 3× `envStr("GOCLAW_AGY_CLI_*", ...)` (:152-155) |
| global register | `cmd/gateway_providers.go` | config-driven block (mirror :198-220) |
| DB register | `cmd/gateway_providers.go` | `store.ProviderAgyCLI` branch (mirror :305-330) |
| http in-mem | `internal/http/providers.go` | `ProviderAgyCLI` branch (:160), auth-status route (:134), SSRF allowlist (:244), singleton guard w/ own mutex (:330) |
| store const | (provider type constants pkg) | `ProviderAgyCLI` constant — referenced by all of the above |
| tools | `read_image.go` / `read_document.go` / `read_document_resolve.go` | add `"agy-cli"` to priority slices + model maps + `disable_tools` conditionals |
| go.mod | `go.mod` | add `github.com/creack/pty` as direct require IF agy needs a pty (currently indirect-only; claude-cli uses stream-json instead) |
| Dockerfile | `Dockerfile` | `ARG ENABLE_AGY_CLI`, install step, node-runtime OR-clause, creds symlink |
| tests | `internal/providers/agy_cli_*_test.go` | pure-function table-driven tests, no real binary |