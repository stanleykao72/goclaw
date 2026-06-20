package agycli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

// homeEnv overrides the agy home directory (default ~/.gemini). It exists so the
// auth/health probe can be pointed at a throwaway t.TempDir() in tests instead of
// the real ~/.gemini location — which holds the operator's actual OAuth creds and
// settings and must NEVER be read-modify-written by tests.
const homeEnv = "AGY_HOME"

// oauthCredsFile is the basename of agy's stored Google OAuth credentials inside
// AgyHome(). Its mere presence is treated as evidence that agy has been authed at
// least once (we do not parse or validate the token — that would risk leaking the
// secret and is beyond a liveness probe).
const oauthCredsFile = "oauth_creds.json"

// healthVersionTimeout bounds the `agy --version` probe. --version is a safe,
// non-interactive command (NOTE: `agy version`, without dashes, IS interactive
// and would hang without a TTY — we always use --version), but agy still needs a
// pty (issue #76), so we drive it through RunWithPTY and cap it tightly: a
// version banner returns near-instantly, so anything slow means trouble.
const healthVersionTimeout = 15 * time.Second

// versionRe extracts a dotted semver-ish version (e.g. "1.0.10") from the
// ANSI-stripped `agy --version` output. agy prints a short banner that contains
// the version somewhere in it; we pull the first token shaped like MAJOR.MINOR.PATCH
// (with an optional pre-release/build suffix) rather than assuming an exact layout.
//
// No leading \b: agy commonly prefixes the version with "v" (e.g. "agy v1.0.10"),
// and \b would not match between the word char "v" and the first digit. The
// MAJOR.MINOR.PATCH shape plus the trailing \b is specific enough on its own; the
// FindString below returns the first match, which is the version token.
var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.\-]+)?\b`)

// HealthStatus is the result of probing whether agy is installed and authed.
type HealthStatus struct {
	BinaryFound   bool   // the binary arg resolved on PATH (or as a literal path)
	BinaryPath    string // absolute path the binary resolved to ("" if not found)
	Version       string // parsed from `agy --version` ("" if unparseable/not run)
	Authenticated bool   // true if OAuth creds are present (creds file or settings auth type)
	AuthDetail    string // human-readable note on what auth evidence was found
}

// AgyHome returns agy's home/config directory.
//
// Default ~/.gemini; override via env AGY_HOME (used by tests so they never touch
// the operator's real ~/.gemini, which holds live OAuth creds and MCP servers).
// If the user's home dir cannot be resolved and no override is set, it falls back
// to a relative ".gemini" so the caller still gets a non-empty path to stat.
func AgyHome() string {
	if override := os.Getenv(homeEnv); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Best-effort fallback; a stat against this will simply find nothing.
		return ".gemini"
	}
	return filepath.Join(home, ".gemini")
}

// OAuthCredsPath returns the path agy stores its Google OAuth credentials at:
// <AgyHome>/oauth_creds.json.
func OAuthCredsPath() string {
	return filepath.Join(AgyHome(), oauthCredsFile)
}

// SettingsPath returns agy's settings file path:
// <AgyHome>/antigravity-cli/settings.json. The security.auth.selectedType field
// inside it records which auth method agy is configured to use.
func SettingsPath() string {
	return filepath.Join(AgyHome(), "antigravity-cli", "settings.json")
}

// Healthcheck probes whether agy is installed and authenticated, mirroring the
// verification sequence used by the Hermes antigravity-cli skill (resolve binary
// -> `agy --version` -> inspect creds/settings).
//
// Steps:
//  1. Resolve binary via exec.LookPath. If it cannot be found, returns a
//     HealthStatus with BinaryFound=false and a non-nil error so callers can
//     short-circuit before trying to drive agy.
//  2. Run "<binary> --version" through RunWithPTY (agy silently drops stdout on a
//     bare pipe — issue #76 — so a pty is mandatory even for --version), then
//     salvage the text via ParseAgyOutput and regex out the version. A missing
//     version is non-fatal: BinaryFound stays true and the error is nil; only
//     Version is left empty.
//  3. Check auth by the presence of <AgyHome>/oauth_creds.json and/or
//     security.auth.selectedType in SettingsPath(). AgyHome() is overridable via
//     env AGY_HOME (default os.UserHomeDir()/.gemini) so tests use a temp dir.
//
// The returned error is non-nil ONLY when the binary cannot be resolved; a found
// binary that fails to print a parseable version still yields a usable status
// (BinaryFound=true) with a nil error, since auth evidence is independent of the
// version banner.
func Healthcheck(ctx context.Context, binary string) (HealthStatus, error) {
	var st HealthStatus

	// Auth evidence is independent of the binary probe, so determine it up front;
	// even a non-resolvable binary still benefits from reporting auth state.
	st.Authenticated, st.AuthDetail = probeAuth()

	path, err := exec.LookPath(binary)
	if err != nil {
		// Binary not installed / not on PATH. Surface as an error so callers can
		// short-circuit, but keep the (already-probed) auth fields populated.
		return st, err
	}
	st.BinaryFound = true
	st.BinaryPath = path

	// `agy --version` is the safe, non-interactive probe. Drive it through a pty
	// because agy emits nothing on a bare pipe (issue #76). Bound it tightly: a
	// version banner is near-instant.
	res, runErr := RunWithPTY(ctx, path, RunOptions{
		Args:    []string{"--version"},
		Timeout: healthVersionTimeout,
	})
	if runErr == nil {
		st.Version = parseVersion(res.Output)
	}
	// A run error or an unparseable banner is non-fatal: the binary is installed,
	// which is the contract for a nil error once LookPath succeeds.

	return st, nil
}

// parseVersion salvages the agy version string from raw `agy --version` PTY
// output. It first strips agy's TUI noise via ParseAgyOutput, then falls back to
// the ANSI-stripped full text, matching the first dotted version-like token in
// either. Returns "" when no version-shaped token is present.
func parseVersion(raw []byte) string {
	// Prefer the salvaged answer text (ParseAgyOutput collapses CR repaints and
	// strips ANSI), but also scan the plain ANSI-stripped output as a fallback in
	// case the version sits in a line the salvage heuristic discarded as noise.
	if v := versionRe.FindString(ParseAgyOutput(raw).Content); v != "" {
		return v
	}
	if v := versionRe.FindString(string(StripANSI(raw))); v != "" {
		return v
	}
	return ""
}

// probeAuth reports whether agy appears authenticated, with a human-readable
// detail. It checks two independent signals and never errors (absence of either
// signal simply means "not authed by that signal"):
//
//   - presence of <AgyHome>/oauth_creds.json (OAuth flow has stored creds), and
//   - security.auth.selectedType in SettingsPath().
//
// Either signal is sufficient to mark Authenticated=true; the detail records what
// was found so a caller can distinguish "creds file present" from "settings only".
func probeAuth() (bool, string) {
	credsFound := fileExists(OAuthCredsPath())
	authType := readSelectedAuthType()

	switch {
	case credsFound && authType != "":
		return true, "oauth_creds.json present; settings auth=" + authType
	case credsFound:
		return true, "oauth_creds.json present"
	case authType != "":
		return true, "settings auth=" + authType
	default:
		return false, "no oauth_creds.json and no settings auth type"
	}
}

// readSelectedAuthType reads SettingsPath() and returns the nested
// security.auth.selectedType value, or "" if the file is missing, unreadable,
// malformed, or the field is absent/empty. It is deliberately lenient: any read
// or parse problem is treated as "no auth-type evidence", not an error, since the
// creds file is the primary signal.
func readSelectedAuthType() string {
	data, err := os.ReadFile(SettingsPath())
	if err != nil {
		return ""
	}
	var s struct {
		Security struct {
			Auth struct {
				SelectedType string `json:"selectedType"`
			} `json:"auth"`
		} `json:"security"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return ""
	}
	return s.Security.Auth.SelectedType
}

// fileExists reports whether path exists and is a regular file (not a directory).
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
