package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// notebookRecall constants.
const (
	// defaultNLMBinary is the nlm CLI binary name resolved from PATH when
	// GOCLAW_NLM_BINARY / config does not override it.
	defaultNLMBinary = "nlm"

	// phase1FallbackNotebookID is the single shared test notebook used in
	// Phase 1 when neither config nor GOCLAW_NLM_NOTEBOOK supplies a value.
	// Phase 3 replaces resolveNotebookID with a per-(tenant,agent,scope)
	// pointer table + lazy-create; the literal here is only a bootstrap default.
	phase1FallbackNotebookID = "b0dbfb3d-4c18-4006-a01a-a4e09abf597f"

	// nlmExecTimeout bounds the whole nlm subprocess. nlm's own -t flag is set
	// slightly below this so the binary returns a clean error before we SIGKILL.
	nlmExecTimeout      = 120 * time.Second
	nlmInternalTimeoutS = "110"

	// nlmSourceNote is appended to successful answers so the user/LLM knows the
	// grounding came from NotebookLM.
	nlmSourceNote = "\n\n(來源: NotebookLM)"

	// nlmFailSoftMessage is returned on ANY failure so the turn is never broken.
	nlmFailSoftMessage = "NotebookLM 記憶暫時無法存取"
)

// nlmEnvBinary / nlmEnvNotebook are the env var names mirrored in
// config_load.go applyEnvOverrides; the tool also reads them directly so the
// builtin works in every process path (native loop, ACP, CLI bridge) without
// threading extra wiring.
const (
	nlmEnvBinary   = "GOCLAW_NLM_BINARY"
	nlmEnvNotebook = "GOCLAW_NLM_NOTEBOOK"
)

// nlmRunner executes the nlm CLI and returns its raw stdout. It is a struct
// field so tests can inject a fake (no real nlm / network). The real
// implementation uses exec.CommandContext with an argv slice — the question is
// NEVER interpolated into a shell string, so shell metacharacters cannot inject.
type nlmRunner func(ctx context.Context, binary string, args []string) ([]byte, error)

// defaultNLMRunner shells out to the nlm binary with a hard context timeout.
func defaultNLMRunner(ctx context.Context, binary string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	return cmd.Output()
}

// nlmResponse is the JSON shape returned by `nlm query notebook ...`.
// nlm wraps the result under a "value" object: {"value":{"answer":"..."}}.
// Older/flat shapes ({"answer":"..."}) are tolerated via the top-level Answer.
// Errors arrive as {"status":"error","error":"..."}.
type nlmResponse struct {
	Answer string `json:"answer"`
	Value  *struct {
		Answer string `json:"answer"`
	} `json:"value"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

// NotebookRecallTool answers a question grounded in the caller's accumulated
// NotebookLM memory. It is a general builtin available to ALL agent types.
//
// SECURITY (single NotebookLM account, no per-notebook ACL — trust boundary is
// goclaw resolving the notebook server-side):
//   - The InputSchema is {question} ONLY. The LLM cannot choose the notebook.
//   - The notebook id is resolved INSIDE Execute via resolveNotebookID(ctx)
//     from the injected identity, never from tool args. This is why nlm's own
//     MCP server (whose notebook_query takes `notebook` as a caller arg) is NOT
//     registered — the bridge forwards LLM args verbatim and would let user A
//     read user B's notebook.
type NotebookRecallTool struct {
	// binary is the configured nlm binary path/name. Empty → env → PATH default.
	binary string
	// notebook is the configured notebook id. Empty → env → Phase 1 fallback.
	notebook string
	// runner is injectable so tests never touch real nlm.
	runner nlmRunner
}

// NewNotebookRecallTool constructs the tool. binary and notebook may be empty;
// they are resolved at execution time (config → env → default) so a zero-value
// constructor (NewNotebookRecallTool("", "")) is valid for every wiring path.
func NewNotebookRecallTool(binary, notebook string) *NotebookRecallTool {
	return &NotebookRecallTool{
		binary:   binary,
		notebook: notebook,
		runner:   defaultNLMRunner,
	}
}

func (t *NotebookRecallTool) Name() string { return "notebook_recall" }

func (t *NotebookRecallTool) Description() string {
	return "Answer a question grounded in the user's accumulated NotebookLM memory/knowledge. " +
		"Use when the user asks about previously shared information, documents, or past context."
}

func (t *NotebookRecallTool) Parameters() map[string]any {
	// SECURITY: question ONLY — no notebook/notebookId/scope. The LLM must not
	// be able to choose which notebook is queried.
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{
				"type":        "string",
				"description": "The question to answer from the user's NotebookLM knowledge.",
			},
		},
		"required": []string{"question"},
	}
}

// resolveBinary picks the nlm binary: configured value → GOCLAW_NLM_BINARY → "nlm".
func (t *NotebookRecallTool) resolveBinary() string {
	if t.binary != "" {
		return t.binary
	}
	if v := strings.TrimSpace(os.Getenv(nlmEnvBinary)); v != "" {
		return v
	}
	return defaultNLMBinary
}

// resolveNotebookID resolves the notebook id from the INJECTED IDENTITY context,
// never from tool args. Phase 1: returns the configured / env notebook for
// everyone (single shared test notebook), but still reads + logs the ctx
// identity to prove the plumbing and prepare the Phase 3 upgrade to a
// per-(tenant,agent,scope) pointer table + lazy-create.
func (t *NotebookRecallTool) resolveNotebookID(ctx context.Context) (string, error) {
	// Identity plumbing — read every dimension Phase 3 will key on.
	agentID := store.AgentIDFromContext(ctx)
	userID := store.UserIDFromContext(ctx)
	senderID := store.SenderIDFromContext(ctx)
	tenantID := store.TenantIDFromContext(ctx)
	channelType := ToolChannelTypeFromCtx(ctx)
	peerKind := ToolPeerKindFromCtx(ctx)
	chatID := ToolChatIDFromCtx(ctx)

	slog.Info("notebook_recall.resolve",
		"agent_id", agentID,
		"user_id", userID,
		"sender_id", senderID,
		"tenant_id", tenantID,
		"channel_type", channelType,
		"peer_kind", peerKind,
		"chat_id", chatID,
	)

	// Phase 1 resolution: configured value → env → literal fallback.
	if t.notebook != "" {
		return t.notebook, nil
	}
	if v := strings.TrimSpace(os.Getenv(nlmEnvNotebook)); v != "" {
		return v, nil
	}
	return phase1FallbackNotebookID, nil
}

func (t *NotebookRecallTool) Execute(ctx context.Context, args map[string]any) *Result {
	question, _ := args["question"].(string)
	question = strings.TrimSpace(question)
	if question == "" {
		return ErrorResult("question is required")
	}

	notebookID, err := t.resolveNotebookID(ctx)
	if err != nil || notebookID == "" {
		slog.Warn("notebook_recall.resolve_failed", "error", err)
		return t.failSoft()
	}

	binary := t.resolveBinary()

	// Hard timeout on the whole subprocess; nlm's -t is set just under it.
	execCtx, cancel := context.WithTimeout(ctx, nlmExecTimeout)
	defer cancel()

	// argv slice — question is a SEPARATE element, never shell-interpolated.
	// Command: nlm query notebook <NB_ID> <question> -t 110
	//
	// NOTE: nlm's -c / --conversation-id takes a TEXT VALUE (a conversation id
	// for follow-ups), it is NOT a boolean "continue" flag. Phase 1 is
	// single-shot with no prior conversation, so -c is intentionally omitted —
	// passing a bare -c would swallow the next token (-t) as its value and
	// corrupt the command. Phase 3 (per-(tenant,agent,scope) state) can thread a
	// real conversation id here.
	cmdArgs := []string{
		"query", "notebook",
		notebookID,
		question,
		"-t", nlmInternalTimeoutS,
	}

	out, err := t.runner(execCtx, binary, cmdArgs)
	if err != nil {
		slog.Warn("notebook_recall.exec_failed",
			"binary", binary,
			"error", err,
			"stderr", stderrSnippet(err),
		)
		return t.failSoft()
	}

	answer, ok := parseNLMAnswer(out)
	if !ok || answer == "" {
		slog.Warn("notebook_recall.parse_failed", "stdout_len", len(out))
		return t.failSoft()
	}

	return NewResult(answer + nlmSourceNote)
}

// failSoft returns a graceful, non-error result so the turn is never broken.
func (t *NotebookRecallTool) failSoft() *Result {
	return NewResult(nlmFailSoftMessage)
}

// parseNLMAnswer extracts the answer from nlm's JSON stdout. nlm returns
// {"value":{"answer":"..."}} on success and {"status":"error","error":"..."}
// on failure; a flat {"answer":"..."} is also tolerated. Any non-JSON prefix
// before the first '{' is skipped.
func parseNLMAnswer(out []byte) (string, bool) {
	if i := bytes.IndexByte(out, '{'); i > 0 {
		out = out[i:]
	}
	var resp nlmResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", false
	}
	if resp.Status == "error" {
		return "", false
	}
	if resp.Value != nil && strings.TrimSpace(resp.Value.Answer) != "" {
		return strings.TrimSpace(resp.Value.Answer), true
	}
	return strings.TrimSpace(resp.Answer), true
}

// stderrSnippet pulls captured stderr from an *exec.ExitError for logging only.
func stderrSnippet(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		s := string(exitErr.Stderr)
		if len(s) > 500 {
			s = s[:500]
		}
		return s
	}
	return ""
}
