package agycli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeFakeAgy writes an executable stub that mimics `agy --version` by printing
// the given version banner, and returns its path. It reuses writeStub from
// pty_test.go (same package). The stub never spawns the real agy, so health unit
// tests stay hermetic; the Verify stage exercises the real `agy --version`.
func writeFakeAgy(t *testing.T, dir, banner string) string {
	t.Helper()
	// Echo the banner regardless of args so `--version` (the only flag Healthcheck
	// passes) produces it. A trailing newline flushes the line through the pty.
	body := "#!/bin/sh\nprintf '%s\\n' '" + banner + "'\n"
	return writeStub(t, dir, "agy", body)
}

// writeCreds writes a fake oauth_creds.json into agyHome/<oauthCredsFile>. The
// contents are irrelevant — Healthcheck only checks for presence, never parses
// the token (avoids any risk of leaking a real secret).
func writeCreds(t *testing.T, agyHome string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(agyHome, oauthCredsFile), []byte(`{"fake":"creds"}`), 0o600); err != nil {
		t.Fatalf("write fake creds: %v", err)
	}
}

// writeSettings writes a settings.json with the given selectedType under
// agyHome/antigravity-cli/settings.json.
func writeSettings(t *testing.T, agyHome, selectedType string) {
	t.Helper()
	dir := filepath.Join(agyHome, "antigravity-cli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	body := `{"security":{"auth":{"selectedType":"` + selectedType + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write fake settings: %v", err)
	}
}

// TestHealthcheck_BinaryFoundAndVersion verifies the stub is resolved on PATH and
// its version banner is parsed.
func TestHealthcheck_BinaryFoundAndVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty stubs are POSIX shell scripts")
	}
	binDir := t.TempDir()
	writeFakeAgy(t, binDir, "agy version 1.0.10 (build deadbeef)")

	// Point AGY_HOME at an empty temp dir (no creds, no settings).
	t.Setenv(homeEnv, t.TempDir())
	// Make the stub resolvable by exec.LookPath via PATH.
	t.Setenv("PATH", binDir)

	st, err := Healthcheck(context.Background(), "agy")
	if err != nil {
		t.Fatalf("Healthcheck error: %v", err)
	}
	if !st.BinaryFound {
		t.Errorf("BinaryFound = false, want true")
	}
	if st.BinaryPath != filepath.Join(binDir, "agy") {
		t.Errorf("BinaryPath = %q, want %q", st.BinaryPath, filepath.Join(binDir, "agy"))
	}
	if st.Version != "1.0.10" {
		t.Errorf("Version = %q, want 1.0.10", st.Version)
	}
	if st.Authenticated {
		t.Errorf("Authenticated = true, want false (empty AGY_HOME)")
	}
}

// TestHealthcheck_BinaryByAbsolutePath verifies passing a literal path (not a
// PATH lookup) also resolves.
func TestHealthcheck_BinaryByAbsolutePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty stubs are POSIX shell scripts")
	}
	binDir := t.TempDir()
	stub := writeFakeAgy(t, binDir, "1.2.3")
	t.Setenv(homeEnv, t.TempDir())

	st, err := Healthcheck(context.Background(), stub)
	if err != nil {
		t.Fatalf("Healthcheck error: %v", err)
	}
	if !st.BinaryFound {
		t.Fatalf("BinaryFound = false, want true")
	}
	if st.Version != "1.2.3" {
		t.Errorf("Version = %q, want 1.2.3", st.Version)
	}
}

// TestHealthcheck_BinaryNotFound verifies a missing binary returns an error with
// BinaryFound=false, while still reporting auth state.
func TestHealthcheck_BinaryNotFound(t *testing.T) {
	agyHome := t.TempDir()
	writeCreds(t, agyHome) // auth evidence present even though binary is absent
	t.Setenv(homeEnv, agyHome)
	// Empty PATH so the bare name cannot resolve.
	t.Setenv("PATH", t.TempDir())

	st, err := Healthcheck(context.Background(), "definitely-not-a-real-agy-binary")
	if err == nil {
		t.Fatalf("expected error for missing binary, got nil (status: %+v)", st)
	}
	if st.BinaryFound {
		t.Errorf("BinaryFound = true, want false")
	}
	if st.BinaryPath != "" {
		t.Errorf("BinaryPath = %q, want empty", st.BinaryPath)
	}
	// Auth probe runs independently of the binary resolution.
	if !st.Authenticated {
		t.Errorf("Authenticated = false, want true (creds present)")
	}
}

// TestHealthcheck_AuthenticatedViaCreds verifies the oauth_creds.json signal
// toggles Authenticated.
func TestHealthcheck_AuthenticatedViaCreds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty stubs are POSIX shell scripts")
	}
	binDir := t.TempDir()
	stub := writeFakeAgy(t, binDir, "1.0.0")
	agyHome := t.TempDir()
	writeCreds(t, agyHome)
	t.Setenv(homeEnv, agyHome)

	st, err := Healthcheck(context.Background(), stub)
	if err != nil {
		t.Fatalf("Healthcheck error: %v", err)
	}
	if !st.Authenticated {
		t.Errorf("Authenticated = false, want true (oauth_creds.json present)")
	}
	if st.AuthDetail == "" {
		t.Errorf("AuthDetail empty, want a note about creds")
	}
}

// TestHealthcheck_AuthenticatedViaSettings verifies that settings
// security.auth.selectedType alone marks Authenticated, without a creds file.
func TestHealthcheck_AuthenticatedViaSettings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty stubs are POSIX shell scripts")
	}
	binDir := t.TempDir()
	stub := writeFakeAgy(t, binDir, "1.0.0")
	agyHome := t.TempDir()
	writeSettings(t, agyHome, "oauth-personal")
	t.Setenv(homeEnv, agyHome)

	st, err := Healthcheck(context.Background(), stub)
	if err != nil {
		t.Fatalf("Healthcheck error: %v", err)
	}
	if !st.Authenticated {
		t.Errorf("Authenticated = false, want true (settings selectedType present)")
	}
}

// TestHealthcheck_NotAuthenticated verifies an empty AGY_HOME (no creds, no
// settings) yields Authenticated=false.
func TestHealthcheck_NotAuthenticated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty stubs are POSIX shell scripts")
	}
	binDir := t.TempDir()
	stub := writeFakeAgy(t, binDir, "1.0.0")
	t.Setenv(homeEnv, t.TempDir())

	st, err := Healthcheck(context.Background(), stub)
	if err != nil {
		t.Fatalf("Healthcheck error: %v", err)
	}
	if st.Authenticated {
		t.Errorf("Authenticated = true, want false (empty AGY_HOME)")
	}
	if st.AuthDetail == "" {
		t.Errorf("AuthDetail empty, want a note explaining the absence")
	}
}

// TestHealthcheck_UnparseableVersion verifies that a banner with no version-like
// token leaves Version empty but keeps BinaryFound true and a nil error.
func TestHealthcheck_UnparseableVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty stubs are POSIX shell scripts")
	}
	binDir := t.TempDir()
	stub := writeFakeAgy(t, binDir, "no digits here at all")
	t.Setenv(homeEnv, t.TempDir())

	st, err := Healthcheck(context.Background(), stub)
	if err != nil {
		t.Fatalf("Healthcheck error: %v", err)
	}
	if !st.BinaryFound {
		t.Errorf("BinaryFound = false, want true")
	}
	if st.Version != "" {
		t.Errorf("Version = %q, want empty for unparseable banner", st.Version)
	}
}

// TestParseVersion is a focused unit test on the version regex salvage over a
// range of realistic and edge-case banners.
func TestParseVersion(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"plain", "1.0.10", "1.0.10"},
		{"with_prefix", "agy version 1.2.3", "1.2.3"},
		{"with_build_suffix", "agy 2.0.0-beta.1 (abc123)", "2.0.0-beta.1"},
		{"embedded", "Antigravity CLI v3.4.5 ready", "3.4.5"},
		{"no_version", "no digits here", ""},
		{"two_digit_only", "version 1.2", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseVersion([]byte(tc.raw)); got != tc.want {
				t.Errorf("parseVersion(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestAgyHome_EnvOverride verifies AGY_HOME takes precedence over the default.
func TestAgyHome_EnvOverride(t *testing.T) {
	custom := t.TempDir()
	t.Setenv(homeEnv, custom)
	if got := AgyHome(); got != custom {
		t.Errorf("AgyHome() = %q, want %q", got, custom)
	}
	if got := OAuthCredsPath(); got != filepath.Join(custom, oauthCredsFile) {
		t.Errorf("OAuthCredsPath() = %q, want %q", got, filepath.Join(custom, oauthCredsFile))
	}
	if got := SettingsPath(); got != filepath.Join(custom, "antigravity-cli", "settings.json") {
		t.Errorf("SettingsPath() = %q, want under custom AGY_HOME", got)
	}
}

// TestReadSelectedAuthType_Lenient verifies malformed/missing settings yield ""
// rather than an error.
func TestReadSelectedAuthType_Lenient(t *testing.T) {
	agyHome := t.TempDir()
	t.Setenv(homeEnv, agyHome)

	// Missing file => "".
	if got := readSelectedAuthType(); got != "" {
		t.Errorf("missing settings: got %q, want empty", got)
	}

	// Malformed JSON => "".
	dir := filepath.Join(agyHome, "antigravity-cli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write malformed: %v", err)
	}
	if got := readSelectedAuthType(); got != "" {
		t.Errorf("malformed settings: got %q, want empty", got)
	}

	// Valid but absent field => "".
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"security":{}}`), 0o600); err != nil {
		t.Fatalf("write valid-no-field: %v", err)
	}
	if got := readSelectedAuthType(); got != "" {
		t.Errorf("absent field: got %q, want empty", got)
	}

	// Present field => value.
	writeSettings(t, agyHome, "oauth-personal")
	if got := readSelectedAuthType(); got != "oauth-personal" {
		t.Errorf("present field: got %q, want oauth-personal", got)
	}
}
