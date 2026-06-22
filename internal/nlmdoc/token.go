// Package nlmdoc is the Drive Doc library backing the goclaw × NotebookLM
// 4-tier memory system (spec sub-phase 2.1). Each memory scope is one Google
// Doc in Drive; the Doc is later added to a NotebookLM notebook as a --drive
// source. This package creates folders + Docs and appends text.
//
// AUTH / TOKEN BROKER: we never store our own OAuth client secret. Instead we
// reuse rclone's gdrive remote as a token broker — `rclone about gdrive:`
// forces a refresh and `rclone config show gdrive` exposes the resulting
// access_token (full drive scope, account = stanleykao72 = the nlm account).
//
// CRITICAL DEVIATION FROM THE SPEC TEXT: the Google Docs API
// (docs.googleapis.com) is DISABLED on rclone's shared GCP project and cannot
// be enabled by us, so the spec's "Docs API create + batchUpdate insertText"
// path is unavailable. Everything here is built on the Drive API
// (drive.googleapis.com, which IS enabled): create a Doc via Drive multipart
// upload with text/plain→google-doc conversion, and append via
// export-then-update-media (read-modify-write). The token's drive scope covers
// all of this. See AppendText for the BOM + newline handling this requires.
package nlmdoc

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// rcloneRefreshTimeout / rcloneShowTimeout bound the two rclone subprocesses
// the broker shells out to. They are independent so a slow refresh cannot eat
// the whole budget for reading the config back.
const (
	rcloneRefreshTimeout = 30 * time.Second
	rcloneShowTimeout    = 10 * time.Second

	// tokenExpirySkew re-brokers a token slightly before its real expiry so an
	// in-flight Drive call does not race the boundary.
	tokenExpirySkew = 2 * time.Minute

	// defaultRcloneRemote is the rclone remote name holding the gdrive OAuth
	// token. Overridable via GOCLAW_NLM_RCLONE_REMOTE.
	defaultRcloneRemote = "gdrive"
)

// TokenSource returns a currently-valid Google API access token. The whole
// auth chain is behind this interface so callers (and tests) never need rclone.
type TokenSource interface {
	// Token returns a valid bearer access token, refreshing if needed.
	Token(ctx context.Context) (string, error)
	// Invalidate marks the cached token as stale so the next Token call
	// re-brokers. Call this on a 401 from the Drive API (mid-op expiry).
	Invalidate()
}

// rcloneRunner executes an rclone subcommand and returns its combined stdout.
// Injectable so tests use a fake (no real rclone / network).
type rcloneRunner func(ctx context.Context, args []string) ([]byte, error)

// defaultRcloneRunner shells out to the rclone binary with a context timeout.
func defaultRcloneRunner(ctx context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "rclone", args...)
	return cmd.Output()
}

// brokeredToken is the parsed `token = {...}` JSON from `rclone config show`.
type brokeredToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	Expiry       time.Time `json:"expiry"`
}

// RcloneTokenSource brokers Google access tokens through rclone's gdrive remote.
// It caches the token until (expiry - skew) and re-brokers on demand. Safe for
// concurrent use.
type RcloneTokenSource struct {
	remote string
	runner rcloneRunner

	mu     sync.Mutex
	cached string
	expiry time.Time
}

// NewRcloneTokenSource constructs a broker for the given rclone remote name
// (e.g. "gdrive"). An empty remote falls back to the default.
func NewRcloneTokenSource(remote string) *RcloneTokenSource {
	if remote == "" {
		remote = defaultRcloneRemote
	}
	return &RcloneTokenSource{remote: remote, runner: defaultRcloneRunner}
}

// Token returns a valid bearer token, re-brokering when the cache is empty or
// within tokenExpirySkew of expiry.
func (s *RcloneTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cached != "" && time.Now().Before(s.expiry.Add(-tokenExpirySkew)) {
		return s.cached, nil
	}

	tok, err := s.broker(ctx)
	if err != nil {
		return "", err
	}
	s.cached = tok.AccessToken
	s.expiry = tok.Expiry
	return s.cached, nil
}

// Invalidate clears the cache so the next Token call re-brokers.
func (s *RcloneTokenSource) Invalidate() {
	s.mu.Lock()
	s.cached = ""
	s.expiry = time.Time{}
	s.mu.Unlock()
}

// broker runs the two-step rclone dance: `rclone about <remote>:` to force a
// refresh, then `rclone config show <remote>` to read the refreshed token.
//
// The rclone subprocesses are detached from the CALLER's deadline
// (context.WithoutCancel) and given their own timeouts: a short per-scope ctx
// must not SIGKILL a cold token refresh mid-flight (observed live as
// "rclone about: signal: killed"). The token is cached for ~the access-token
// lifetime (~1h) so this expensive path runs at most ~once/hour, not per scope.
func (s *RcloneTokenSource) broker(ctx context.Context) (*brokeredToken, error) {
	detached := context.WithoutCancel(ctx)

	// Step 1: force a refresh as a side-effect. We ignore the stats output;
	// only failure matters (a broken remote means both ingest and recall die).
	refreshCtx, cancel := context.WithTimeout(detached, rcloneRefreshTimeout)
	defer cancel()
	if _, err := s.runner(refreshCtx, []string{"about", s.remote + ":"}); err != nil {
		return nil, fmt.Errorf("rclone about %s: %w", s.remote, err)
	}

	// Step 2: read the refreshed token JSON out of the config dump.
	showCtx, cancel2 := context.WithTimeout(detached, rcloneShowTimeout)
	defer cancel2()
	out, err := s.runner(showCtx, []string{"config", "show", s.remote})
	if err != nil {
		return nil, fmt.Errorf("rclone config show %s: %w", s.remote, err)
	}

	tok, err := parseRcloneToken(out)
	if err != nil {
		return nil, err
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("rclone config show %s: empty access_token", s.remote)
	}
	return tok, nil
}

// parseRcloneToken extracts and JSON-parses the `token = {...}` line from
// `rclone config show` output. The format is INI-ish: a key, " = ", then a
// single-line JSON value. We parse defensively so an rclone format bump fails
// loudly rather than silently returning an empty token.
func parseRcloneToken(out []byte) (*brokeredToken, error) {
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "token") {
			continue
		}
		// Expect "token = {json}". Split on the first '=' only.
		eq := strings.IndexByte(trimmed, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:eq])
		if key != "token" {
			continue
		}
		val := strings.TrimSpace(trimmed[eq+1:])
		if !strings.HasPrefix(val, "{") {
			return nil, fmt.Errorf("rclone token value is not JSON: %q", val)
		}
		var tok brokeredToken
		if err := json.Unmarshal([]byte(val), &tok); err != nil {
			return nil, fmt.Errorf("parse rclone token JSON: %w", err)
		}
		return &tok, nil
	}
	return nil, fmt.Errorf("no token line found in rclone config show output")
}
