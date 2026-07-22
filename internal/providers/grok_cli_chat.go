package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// hexUpper is the uppercase hex alphabet for RFC-3986 percent-encoding of the
// grok per-cwd session directory name.
const hexUpper = "0123456789ABCDEF"

// Chat runs the grok CLI synchronously (--output-format json) and returns the
// final response. There is no image stdin path (Vision is unsupported here).
func (p *GrokCLIProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	systemPrompt, userMsg, _, priorTurns := extractFromMessages(req.Messages)
	if systemPrompt == "" {
		systemPrompt = p.systemPrompt
	}
	sessionKey := extractStringOpt(req.Options, OptSessionKey)
	model := req.Model
	if model == "" {
		model = p.defaultModel
	}

	unlock := p.lockSession(sessionKey)
	defer unlock()

	workDir := p.ensureWorkDir(sessionKey)

	cliSessionID := deriveSessionUUID(sessionKey)
	exists := grokSessionExists(workDir, cliSessionID)
	// Cold subprocess (session dir absent): seed the loop's compacted prior turns
	// as a text preamble so a respawn doesn't lose context. Mirrors claude_cli.
	if !exists {
		if preamble := buildColdSeedPreamble(priorTurns); preamble != "" {
			userMsg = preamble + userMsg
		}
	}

	args := buildGrokArgs(model, userMsg, systemPrompt, p.permMode, "json", workDir, cliSessionID, exists)

	cmd := exec.CommandContext(ctx, p.cliPath, args...)
	cmd.Dir = workDir
	cmd.Env = filterGrokEnv(os.Environ())

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	slog.Debug("grok-cli exec", "cmd", fmt.Sprintf("%s %s", p.cliPath, strings.Join(args, " ")), "workdir", workDir)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("grok-cli: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}

	return parseGrokJSONResponse(output)
}

// ChatStream runs the grok CLI with streaming-json output, calling onChunk for
// each thought/text delta and returning the final response assembled from the
// terminal "end" event. Grok's stream is line-delimited JSON with exactly three
// event types: {"type":"thought"|"text"|"end"} (§2.2).
func (p *GrokCLIProvider) ChatStream(ctx context.Context, req ChatRequest, onChunk func(StreamChunk)) (*ChatResponse, error) {
	systemPrompt, userMsg, _, priorTurns := extractFromMessages(req.Messages)
	if systemPrompt == "" {
		systemPrompt = p.systemPrompt
	}
	sessionKey := extractStringOpt(req.Options, OptSessionKey)
	model := req.Model
	if model == "" {
		model = p.defaultModel
	}

	unlock := p.lockSession(sessionKey)
	defer unlock()

	workDir := p.ensureWorkDir(sessionKey)

	cliSessionID := deriveSessionUUID(sessionKey)
	exists := grokSessionExists(workDir, cliSessionID)
	if !exists {
		if preamble := buildColdSeedPreamble(priorTurns); preamble != "" {
			userMsg = preamble + userMsg
		}
	}

	args := buildGrokArgs(model, userMsg, systemPrompt, p.permMode, "streaming-json", workDir, cliSessionID, exists)

	cmd := exec.CommandContext(ctx, p.cliPath, args...)
	cmd.WaitDelay = 5 * time.Second // force-close pipes if process lingers after kill
	cmd.Dir = workDir
	cmd.Env = filterGrokEnv(os.Environ())

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("grok-cli stdout pipe: %w", err)
	}

	fullCmd := fmt.Sprintf("%s %s", p.cliPath, strings.Join(args, " "))
	slog.Debug("grok-cli stream exec", "cmd", fullCmd, "workdir", workDir)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("grok-cli start: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, StdioScanBufInit), StdioScanBufMax)

	var finalResp ChatResponse
	var contentBuf strings.Builder
	var sawEnd bool

	for scanner.Scan() {
		if ctx.Err() != nil {
			break // context cancelled (abort) → exit immediately
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var ev grokStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			slog.Debug("grok-cli: skip malformed stream line", "error", err)
			continue
		}

		switch ev.Type {
		case "thought":
			if ev.Data != "" {
				finalResp.Thinking += ev.Data
				onChunk(StreamChunk{Thinking: ev.Data})
			}
		case "text":
			if ev.Data != "" {
				contentBuf.WriteString(ev.Data)
				onChunk(StreamChunk{Content: ev.Data})
			}
		case "end":
			sawEnd = true
			finalResp.FinishReason = grokFinishReason(ev.StopReason)
			if ev.Usage != nil {
				finalResp.Usage = mapGrokUsage(ev.Usage)
			}
		}
	}

	// Context cancelled (abort): best-effort reap (bounded by WaitDelay), return.
	if ctx.Err() != nil {
		_ = cmd.Wait()
		return nil, ctx.Err()
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("grok-cli: stream read error: %w", err)
	}

	// grok accumulates text via "text" events; the "end" event carries no text.
	finalResp.Content = contentBuf.String()

	if err := cmd.Wait(); err != nil {
		if finalResp.Content != "" {
			return &finalResp, nil // partial content is still useful
		}
		return nil, fmt.Errorf("grok-cli: %w (stderr: %s)", err, strings.TrimSpace(stderrBuf.String()))
	}

	// Fallback if no "end" event was received.
	if !sawEnd && finalResp.FinishReason == "" {
		finalResp.FinishReason = "stop"
	}

	onChunk(StreamChunk{Done: true})
	return &finalResp, nil
}

// buildGrokArgs constructs the grok CLI argv (§2.4). Order: the proven flag set
// (`-p <prompt> --permission-mode <mode> --output-format <fmt>`), then optional
// --model / --rules / --cwd, then the session flag. The system prompt is
// injected via the first-class --rules flag (append semantics; probe A) and is
// emitted only when non-empty. Session continuity branches first-turn vs later
// (probe B): --session-id mints a new session, --resume continues an existing
// one — grok hard-errors if the wrong one is sent, so the caller must branch on
// on-disk existence rather than always sending either flag. The two flags are
// mutually exclusive per invocation.
func buildGrokArgs(model, prompt, systemPrompt, permMode, outputFormat, workDir string, sessionID uuid.UUID, sessionExists bool) []string {
	if permMode == "" {
		permMode = "bypassPermissions"
	}
	args := []string{
		"-p", prompt,
		"--permission-mode", permMode,
		"--output-format", outputFormat,
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if systemPrompt != "" {
		args = append(args, "--rules", systemPrompt)
	}
	if workDir != "" {
		args = append(args, "--cwd", workDir)
	}
	sid := sessionID.String()
	if sessionExists {
		args = append(args, "--resume", sid)
	} else {
		args = append(args, "--session-id", sid)
	}
	return args
}

// ensureWorkDir creates and returns a stable work directory for the given
// session key (used as the grok --cwd, which also namespaces its session store).
func (p *GrokCLIProvider) ensureWorkDir(sessionKey string) string {
	safe := sanitizePathSegment(sessionKey)
	dir := filepath.Join(p.baseWorkDir, safe)

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := os.MkdirAll(dir, 0755); err != nil {
		slog.Warn("grok-cli: failed to create workdir", "dir", dir, "error", err)
		return os.TempDir()
	}
	return dir
}

// grokSessionExists reports whether a grok CLI session directory exists for the
// given work directory and session UUID. grok stores sessions as a DIRECTORY at
// ~/.grok/sessions/<percent-encoded-absolute-cwd>/<uuid>/, namespaced by --cwd
// (probe B). The existence check MUST use the same workDir passed as --cwd.
func grokSessionExists(workDir string, sessionID uuid.UUID) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	// Resolve symlinks to match the CLI's --cwd canonicalization (macOS: /var → /private/var),
	// otherwise a symlinked workdir encodes to a different session path and turn-2 detection
	// fails, re-sending --session-id on an existing session (grok errors "already in use").
	abs, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		if abs, err = filepath.Abs(workDir); err != nil {
			abs = workDir
		}
	}
	dir := filepath.Join(home, ".grok", "sessions", grokEncodeCWD(abs), sessionID.String())
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// grokEncodeCWD percent-encodes an absolute path the way grok names its per-cwd
// session directory: every byte outside the RFC-3986 unreserved set
// [A-Za-z0-9-._~] becomes %XX (uppercase hex), so '/' → %2F.
func grokEncodeCWD(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		c := path[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexUpper[c>>4])
		b.WriteByte(hexUpper[c&0x0f])
	}
	return b.String()
}

// filterGrokEnv removes GROK* env vars to prevent nested-session conflicts when
// GoClaw itself runs under a grok CLI, mirroring filterCLIEnv's CLAUDE* strip.
// grok authenticates via files under ~/.grok (subscription login), not env, so
// stripping the GROK* prefix does not disturb auth.
func filterGrokEnv(environ []string) []string {
	var filtered []string
	for _, e := range environ {
		key := e
		if before, _, ok := strings.Cut(e, "="); ok {
			key = before
		}
		if strings.HasPrefix(key, "GROK") {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}
