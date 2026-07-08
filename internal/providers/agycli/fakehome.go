package agycli

import (
	"fmt"
	"os"
	"path/filepath"
)

// fakehome.go builds the per-session isolated HOME used by one-shot --print
// runs to solve the MCP-config identity race (design O1,
// docs/agy-oneshot-print-provider.md §2.5).
//
// Every one-shot run re-reads agy's global ~/.gemini/config/mcp_config.json at
// process startup. The goclaw-bridge entry in that file points at a
// PER-SESSION loopback URL whose port IS the session's identity, so two
// concurrent sessions sharing the global file could hand one session's bridge
// to the other (cross-user identity mixup). Instead, each session gets its own
// fake HOME whose .gemini mirrors the real one via per-entry symlinks — auth
// token, conversations db, settings all SHARED — except .gemini/config, which
// is a private directory holding this session's own mcp_config.json.
//
// W0 P2 verified on the Linux deploy target (agy v1.0.14): with this layout
// agy authenticates through the symlinked token AND loads MCP servers from the
// fake home's config. (On macOS the auth side fails — Keychain/HOME
// interaction — which affects local dev only, not deployment.)

// fakeHomeGeminiDir is the dotdir agy resolves under $HOME.
const fakeHomeGeminiDir = ".gemini"

// fakeHomeConfigSubdir is the one realGemini entry NOT symlinked: it is
// recreated as a private directory so the session's mcp_config.json is
// isolated from the global one and from other sessions'.
const fakeHomeConfigSubdir = "config"

// BuildFakeHome populates fakeHome (created if needed) as an isolated HOME for
// one agy one-shot session:
//
//   - fakeHome/.gemini/<entry> is symlinked to realGemini/<entry> for every
//     entry EXCEPT "config" (auth/state/conversations shared with the real
//     home; a stale symlink from a prior build is re-pointed).
//   - fakeHome/.gemini/config/mcp_config.json is written fresh: the REAL
//     global config (realGemini/config/mcp_config.json, if any) is used as the
//     base so the operator's other MCP servers keep working, then servers are
//     merged over it (caller wins on key collision — this is where the
//     per-session goclaw-bridge entry lands).
//
// A missing realGemini yields an error: without the real auth material an agy
// run could trigger an interactive OAuth flow that headless mode cannot
// complete.
//
// The caller owns fakeHome's lifecycle (os.RemoveAll on session close/reap);
// removing it never touches the real ~/.gemini because only symlinks are
// deleted.
func BuildFakeHome(fakeHome, realGemini string, servers map[string]any) error {
	entries, err := os.ReadDir(realGemini)
	if err != nil {
		return fmt.Errorf("agycli: read real gemini dir %s: %w", realGemini, err)
	}

	geminiDir := filepath.Join(fakeHome, fakeHomeGeminiDir)
	configDir := filepath.Join(geminiDir, fakeHomeConfigSubdir)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("agycli: mkdir %s: %w", configDir, err)
	}

	for _, e := range entries {
		name := e.Name()
		if name == fakeHomeConfigSubdir {
			continue
		}
		link := filepath.Join(geminiDir, name)
		target := filepath.Join(realGemini, name)
		// Re-point a pre-existing symlink (idempotent rebuild); never follow it.
		if _, lerr := os.Lstat(link); lerr == nil {
			if rerr := os.Remove(link); rerr != nil {
				return fmt.Errorf("agycli: replace stale link %s: %w", link, rerr)
			}
		}
		if err := os.Symlink(target, link); err != nil {
			return fmt.Errorf("agycli: symlink %s -> %s: %w", link, target, err)
		}
	}

	// Base = the real global config so the operator's other servers survive;
	// merge the session's servers (bridge entry) over it.
	var baseRaw []byte
	realConfig := filepath.Join(realGemini, fakeHomeConfigSubdir, "mcp_config.json")
	if data, rerr := os.ReadFile(realConfig); rerr == nil {
		baseRaw = data
	} else if !os.IsNotExist(rerr) {
		return fmt.Errorf("agycli: read real mcp config %s: %w", realConfig, rerr)
	}

	merged, err := mergeMCPServersDoc(baseRaw, servers)
	if err != nil {
		return fmt.Errorf("agycli: merge fake-home mcp config: %w", err)
	}
	cfgPath := filepath.Join(configDir, "mcp_config.json")
	if err := os.WriteFile(cfgPath, merged, 0o600); err != nil {
		return fmt.Errorf("agycli: write %s: %w", cfgPath, err)
	}
	return nil
}

// RealGeminiDir returns the operator's real ~/.gemini directory (the symlink
// source for BuildFakeHome). Overridable in tests via the AGY_REAL_GEMINI_DIR
// env var so tests never depend on (or touch) the developer's actual ~/.gemini.
func RealGeminiDir() (string, error) {
	if override := os.Getenv("AGY_REAL_GEMINI_DIR"); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("agycli: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".gemini"), nil
}
