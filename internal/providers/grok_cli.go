package providers

import (
	"sync"
)

// grokCLIMaxContextWindow is the advertised default context window for grok-4.5.
const grokCLIMaxContextWindow = 256_000

// GrokCLIProvider implements Provider by shelling out to the `grok` CLI binary
// (the Grok Build CLI shipped in cmux.app). It acts as a thin proxy, mirroring
// ClaudeCLIProvider: the CLI manages session history, tool execution, and
// context; GoClaw only forwards the latest user message and streams back text +
// thinking. Turns route through the user's grok.com subscription (no xAI API
// key), which makes it distinct from the OpenAI-compatible `xai` HTTP provider.
type GrokCLIProvider struct {
	name         string     // provider name (default: "grok-cli")
	cliPath      string     // path to grok binary (default: "grok")
	defaultModel string     // default: "grok-4.5"
	baseWorkDir  string     // base dir for agent workspaces (used as --cwd)
	permMode     string     // permission mode (default: "bypassPermissions")
	systemPrompt string     // optional system prompt injected via --rules when a request carries none
	mu           sync.Mutex // protects workdir creation
	sessionMu    sync.Map   // key: string, value: *sync.Mutex — per-session lock
}

// GrokCLIOption configures the provider.
type GrokCLIOption func(*GrokCLIProvider)

// WithGrokCLIName overrides the provider name (default: "grok-cli").
func WithGrokCLIName(name string) GrokCLIOption {
	return func(p *GrokCLIProvider) {
		if name != "" {
			p.name = name
		}
	}
}

// WithGrokCLIModel sets the default model alias (default: "grok-4.5").
func WithGrokCLIModel(model string) GrokCLIOption {
	return func(p *GrokCLIProvider) {
		if model != "" {
			p.defaultModel = model
		}
	}
}

// WithGrokCLIWorkDir sets the base work directory (passed to the CLI as --cwd).
func WithGrokCLIWorkDir(dir string) GrokCLIOption {
	return func(p *GrokCLIProvider) {
		if dir != "" {
			p.baseWorkDir = dir
		}
	}
}

// WithGrokCLIPermMode sets the permission mode (default: "bypassPermissions").
func WithGrokCLIPermMode(mode string) GrokCLIOption {
	return func(p *GrokCLIProvider) {
		if mode != "" {
			p.permMode = mode
		}
	}
}

// WithGrokCLISystemPrompt sets a fallback system prompt injected via the grok
// CLI's first-class `--rules` flag (appends to grok's built-in system prompt).
// It is used only when a request's messages carry no system message of their
// own; a per-request system message always takes precedence.
func WithGrokCLISystemPrompt(prompt string) GrokCLIOption {
	return func(p *GrokCLIProvider) {
		p.systemPrompt = prompt
	}
}

// NewGrokCLIProvider creates a provider that invokes the grok CLI.
func NewGrokCLIProvider(cliPath string, opts ...GrokCLIOption) *GrokCLIProvider {
	if cliPath == "" {
		cliPath = "grok"
	}
	p := &GrokCLIProvider{
		name:         "grok-cli",
		cliPath:      cliPath,
		defaultModel: "grok-4.5",
		baseWorkDir:  defaultCLIWorkDir(),
		permMode:     "bypassPermissions",
		// sessionMu is zero-value ready (sync.Map)
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *GrokCLIProvider) Name() string         { return p.name }
func (p *GrokCLIProvider) DefaultModel() string { return p.defaultModel }

// Capabilities implements CapabilitiesAware for pipeline code-path selection.
// GrokCLI is subprocess-based — no HTTP adapter, capabilities only. Vision is
// false: the image stdin path is intentionally not implemented for this pass.
func (p *GrokCLIProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Streaming:        true,
		ToolCalling:      true,
		StreamWithTools:  true,
		Thinking:         true,
		Vision:           false,
		CacheControl:     false,
		MaxContextWindow: grokCLIMaxContextWindow,
		TokenizerID:      "o200k_base",
	}
}

// Close implements io.Closer. GrokCLI holds no per-session temp files to clean
// up (no MCP config dirs, no generated settings), so this is a no-op today; the
// method exists to satisfy the same interface as ClaudeCLIProvider.
func (p *GrokCLIProvider) Close() error { return nil }

// lockSession acquires a per-session mutex to prevent concurrent CLI calls on
// the same session (grok stores sessions per-uuid and rejects concurrent use).
func (p *GrokCLIProvider) lockSession(sessionKey string) func() {
	actual, _ := p.sessionMu.LoadOrStore(sessionKey, &sync.Mutex{})
	m := actual.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}
