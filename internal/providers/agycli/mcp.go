package agycli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// configDirEnv is the env var that overrides agy's config dir. It exists so
// tests can point AgyConfigDir / AgyMCPConfigPath / MergeAgyMCPConfig at a
// throwaway t.TempDir() instead of the real ~/.gemini/config.
//
// SAFETY: every test that touches MCP config MUST set this (via t.Setenv to a
// t.TempDir()). The real ~/.gemini/config/mcp_config.json holds the operator's
// own MCP servers (e.g. odoo / odoo-stage38); a read-modify-write of that file
// must never happen from a test.
const configDirEnv = "AGY_CONFIG_DIR"

// AgyConfigDir returns agy's config dir, where it discovers MCP server
// definitions: the global ~/.gemini/config.
//
// This is overridable via the env var AGY_CONFIG_DIR (used by tests so they
// never touch the real ~/.gemini). If the home dir cannot be resolved and no
// override is set, it falls back to a relative path so the caller still gets a
// non-empty value.
//
// Phase 0 (Q2, agy v1.0.10): agy reads MCP servers from
// ~/.gemini/config/mcp_config.json (home-global), top-level "mcpServers", stdio
// shape {command, args, env}. The previous <workspace>/.agents/mcp_config.json
// location was never read by agy.
func AgyConfigDir() string {
	if override := os.Getenv(configDirEnv); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Best-effort fallback; a stat/read against this will simply find nothing.
		return filepath.Join(".gemini", "config")
	}
	return filepath.Join(home, ".gemini", "config")
}

// AgyMCPConfigPath returns the MCP config file agy discovers:
// AgyConfigDir()/mcp_config.json.
func AgyMCPConfigPath() string {
	return filepath.Join(AgyConfigDir(), "mcp_config.json")
}

// bridgeServerName is the key under "mcpServers" for the goclaw bridge entry in
// agy's global config. Stable so MergeAgyMCPConfig overwrites the same key each
// session launch rather than accumulating per-session entries.
const bridgeServerName = "goclaw-bridge"

// BuildAgyBridgeServers builds the static-shape "goclaw-bridge" server map for
// agy's global MCP config: a single http-type entry pointing at the per-session
// loopback bridgeURL with a Bearer Authorization header.
//
// Variant 2 (docs/agy-bridge-design.md): the entry carries NO per-user secret
// and NO X-* identity headers. Identity is bound by the loopback PORT inside
// bridgeURL (one port == one identity, minted server-side). The only credential
// on the wire is the SHARED gateway bearer token, which is not a per-user secret.
// This keeps the shared global config file free of per-user secrets even though
// the URL itself is per-session.
//
// Returns an empty map when bridgeURL is empty (nothing to write); callers
// should skip the launch-time write in that case. gatewayToken may be empty
// (then no Authorization header is emitted), matching the bridge-disabled path.
func BuildAgyBridgeServers(bridgeURL, gatewayToken string) map[string]any {
	if bridgeURL == "" {
		return map[string]any{}
	}
	entry := map[string]any{
		"url":  bridgeURL,
		"type": "http",
	}
	if gatewayToken != "" {
		entry["headers"] = map[string]any{
			"Authorization": "Bearer " + gatewayToken,
		}
	}
	return map[string]any{bridgeServerName: entry}
}

// MergeAgyMCPConfig merges servers into agy's global MCP config and atomically
// writes it back, returning the config path.
//
// Behaviour:
//   - Reads any existing AgyMCPConfigPath(), json.Unmarshal-ing it. Unknown
//     top-level keys are preserved verbatim so sibling configuration is never
//     dropped.
//   - Merges servers into the top-level "mcpServers" map BY KEY: the caller's
//     entries win on a key collision, while existing entries for keys the caller
//     does not mention are left untouched. Each server entry is the stdio shape
//     agy expects, e.g. {"command": "...", "args": [...], "env": {...}}.
//   - The AgyConfigDir() directory is created (MkdirAll 0700) and tightened to
//     0700 if it already existed with looser perms (tighten-only — a deliberately
//     read-only dir is left alone, see hardenDirPerms).
//   - The file is written with mode 0600 via a unique temp file in the same dir
//     plus os.Rename, so readers never observe a partial config and concurrent
//     writers don't clobber a shared temp name.
//   - If the merged content is byte-identical to the existing file, the write is
//     skipped (path, nil) so the file's mtime stays stable.
//   - Empty servers AND no existing file: nothing is created; returns ("", nil).
//     (If a file already exists, an empty servers map still triggers a normalize/
//     rewrite pass, but a byte-identical normalization is skipped as above.)
//
// This library stays decoupled from package providers: it does NOT build the
// "goclaw-bridge" server entry or sign X-* bridge headers. The CALLER assembles
// the servers map (including any bridge entry and HMAC-signed headers via
// providers.SignBridgeContext) before passing it here.
func MergeAgyMCPConfig(servers map[string]any) (string, error) {
	path := AgyMCPConfigPath()

	// Read existing config (if any) so we can preserve unknown top-level keys and
	// merge into its mcpServers map rather than clobbering it.
	existingRaw, readErr := os.ReadFile(path)
	hadFile := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", fmt.Errorf("agycli: read %s: %w", path, readErr)
	}

	// Empty servers + no existing file => nothing to do.
	if len(servers) == 0 && !hadFile {
		return "", nil
	}

	// Decode the existing top-level object (preserving every key) into a generic
	// map. A missing/empty file starts from an empty object.
	top := map[string]any{}
	if hadFile && len(existingRaw) > 0 {
		if err := json.Unmarshal(existingRaw, &top); err != nil {
			return "", fmt.Errorf("agycli: parse existing %s: %w", path, err)
		}
	}

	// Locate (or create) the top-level "mcpServers" map. If the existing value is
	// some other JSON type (malformed config), replace it with a fresh map rather
	// than failing — the caller's intent is to install servers.
	mcpServers, _ := top["mcpServers"].(map[string]any)
	if mcpServers == nil {
		mcpServers = map[string]any{}
	}
	// Merge by key: caller wins on collision, existing untouched keys preserved.
	for name, entry := range servers {
		mcpServers[name] = entry
	}
	top["mcpServers"] = mcpServers

	data, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return "", fmt.Errorf("agycli: marshal mcp config: %w", err)
	}

	dir := AgyConfigDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("agycli: mkdir %s: %w", dir, err)
	}
	// MkdirAll is a no-op when dir already exists, so a pre-existing config dir
	// keeps its old (possibly loose) permissions. The config holds HMAC-signed
	// bridge secrets, so harden the dir by stripping any group/other bits — but
	// only ever TIGHTEN: never re-grant owner write to a directory the operator
	// deliberately locked down, since silently widening it would defeat that
	// intent and mask a genuine write failure.
	if err := hardenDirPerms(dir); err != nil {
		return "", fmt.Errorf("agycli: chmod %s: %w", dir, err)
	}

	// Skip write if the merged content already matches byte-for-byte. This keeps
	// the file's mtime stable so callers can use it to detect real changes.
	if hadFile && string(existingRaw) == string(data) {
		return path, nil
	}

	// Atomic write: unique temp file + rename so a reader never sees a
	// half-written config and concurrent writers don't clobber each other on a
	// shared temp name. The temp file is created in the same dir so the rename is
	// atomic.
	tmp, err := os.CreateTemp(dir, "mcp_config-*.json.tmp")
	if err != nil {
		return "", fmt.Errorf("agycli: create temp in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Clean up the temp file on any error path before rename succeeds.
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", fmt.Errorf("agycli: write %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("agycli: close %s: %w", tmpPath, err)
	}
	// CreateTemp makes the file 0600, but be explicit in case of umask/quirks
	// across platforms — the config carries signed bridge secrets.
	if err := os.Chmod(tmpPath, 0600); err != nil {
		return "", fmt.Errorf("agycli: chmod %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("agycli: rename %s: %w", path, err)
	}
	committed = true
	// Rename can leave the target's perms subject to a pre-existing file's mode,
	// so re-assert 0600 on the final path.
	if err := os.Chmod(path, 0600); err != nil {
		return "", fmt.Errorf("agycli: chmod %s: %w", path, err)
	}

	return path, nil
}

// hardenDirPerms strips group/other permission bits from path (clearing 0o077),
// preserving the owner bits. It is a tighten-only operation: a loose 0755 dir is
// brought down to 0700, while an already-restricted dir (e.g. 0500) is left
// untouched so a deliberately read-only target is neither widened nor masked.
// The chmod is skipped when the target mode already equals the current perms, to
// avoid unnecessary metadata writes (and mtime churn on some filesystems).
func hardenDirPerms(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	cur := info.Mode().Perm()
	target := cur &^ os.FileMode(0o077) // drop group + other bits, keep owner bits
	if cur == target {
		return nil
	}
	return os.Chmod(path, target)
}
