// Package agycli is the VALIDATED-but-UNWIRED plumbing library for driving the
// Google Antigravity CLI (the "agy" binary, tested against agy v1.0.9 (originally
// probed on v1.0.5)).
//
// It deliberately stops at the I/O plumbing layer: it is NOT wired into the
// providers.Provider interface, config, or registration. That integration is
// deferred on purpose. Empirically, "agy -p" behaves as an autonomous coding
// agent (it asks clarifying questions, runs tools, can hang on interaction, and
// triggers clarification on an empty workspace) — not as a chat engine that
// returns a clean structured result for an arbitrary prompt (the way claude-cli
// does). Because that semantic premise does not hold, the full Provider hookup
// is paused. The underlying I/O plumbing, however, is fully validated, so it is
// shipped here as an independent, tested component to be wired up later once
// agy's behavior is reliable or a concrete use case is confirmed.
//
// To stay independently testable and zero-coupled, this package depends only on
// the standard library plus github.com/creack/pty. In particular it must NOT
// import internal/providers.
//
// # Empirical baseline (agy v1.0.9 (originally probed on v1.0.5))
//
//  1. On a non-TTY, "agy -p" hangs until its print-timeout and emits a fully
//     empty stdout (issue #76, unfixed as of v1.0.9). => a PTY is mandatory.
//  2. Under a PTY, "agy -p" returns PLAIN TEXT only: there is no
//     --output-format, no stream-json, and no structured envelope.
//  3. "--conversation <self-minted-uuid>" does NOT resume-or-create; agy mints
//     its own conversation id and stores it at
//     ~/.gemini/antigravity-cli/conversations/<uuid>.db. Moreover, --print
//     resume is broken (Phase 0, agy v1.0.10): --conversation/-c does NOT replay
//     prior context, so snapshot-diff is OBSERVATION-ONLY — it tells you which db
//     a run produced, not a way to continue a conversation. True multi-turn
//     requires a persistent interactive session (future Phase 3), not --print.
//  4. agy is highly agentic: even a trivial prompt may run tools, ask questions,
//     or TIMEOUT. => the parser must tolerate heavy tool UI and the absence of a
//     clean answer.
//
// # W0 correction (2026-07-08, agy v1.0.10 local + v1.0.14 deploy target)
//
// Points 1 and 3 above were OVERTURNED by the W0 probes
// (docs/agy-oneshot-print-provider.md, test-evidence/agy-oneshot-w0/):
// non-TTY "agy -p" completes normally with stdout intact, and
// "--conversation <agy-minted-id>" (captured from --log-file's
// "Created conversation" line) resumes with full cross-process context.
// Phase 0's contrary result was almost certainly polluted by a stale
// Antigravity OAuth token (same symptom signature as the 2026-06-22
// esmith-general incident). The one-shot plumbing lives in oneshot.go /
// fakehome.go; this package is now WIRED into providers via
// agy_cli_provider.go in both interactive and one-shot modes. Two Phase 0
// findings SURVIVE: print mode never loads a workspace (GEMINI.md unread,
// cwd/--add-dir ignored — file refs must be absolute paths in the prompt),
// and --print-timeout can be wedged through, so runs need a hard kill.
package agycli
