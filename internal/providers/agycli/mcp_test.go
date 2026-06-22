package agycli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// SAFETY: every test in this file MUST point AGY_CONFIG_DIR at a throwaway
// t.TempDir() before doing anything. The real ~/.gemini/config/mcp_config.json
// holds the operator's own MCP servers (odoo / odoo-stage38) and must NEVER be
// read-modified-written by a test. setTempConfigDir centralizes that guard.
func setTempConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(configDirEnv, dir)
	return dir
}

func TestAgyConfigDir_EnvOverride(t *testing.T) {
	dir := setTempConfigDir(t)
	if got := AgyConfigDir(); got != dir {
		t.Fatalf("AgyConfigDir() = %q, want %q (AGY_CONFIG_DIR override)", got, dir)
	}
}

func TestAgyMCPConfigPath(t *testing.T) {
	dir := setTempConfigDir(t)
	got := AgyMCPConfigPath()
	want := filepath.Join(dir, "mcp_config.json")
	if got != want {
		t.Fatalf("AgyMCPConfigPath = %q, want %q", got, want)
	}
}

func TestBuildAgyBridgeServers(t *testing.T) {
	// Full shape: url + type:http + Authorization bearer under "goclaw-bridge".
	got := BuildAgyBridgeServers("http://127.0.0.1:5555/mcp/bridge", "tok123")
	entry, ok := got["goclaw-bridge"].(map[string]any)
	if !ok {
		t.Fatalf("no goclaw-bridge entry: %v", got)
	}
	if entry["url"] != "http://127.0.0.1:5555/mcp/bridge" {
		t.Errorf("url = %v", entry["url"])
	}
	if entry["type"] != "http" {
		t.Errorf("type = %v, want http", entry["type"])
	}
	headers, ok := entry["headers"].(map[string]any)
	if !ok || headers["Authorization"] != "Bearer tok123" {
		t.Errorf("headers = %v, want Authorization Bearer tok123", entry["headers"])
	}

	// Empty token => no headers key (bridge-disabled / no-bearer path).
	noTok := BuildAgyBridgeServers("http://127.0.0.1:5555/mcp/bridge", "")
	e2 := noTok["goclaw-bridge"].(map[string]any)
	if _, present := e2["headers"]; present {
		t.Errorf("empty token must omit headers, got %v", e2["headers"])
	}

	// Empty URL => empty map (nothing to write).
	if m := BuildAgyBridgeServers("", "tok"); len(m) != 0 {
		t.Errorf("empty url must yield empty map, got %v", m)
	}
}

func TestMergeAgyMCPConfig_WriteFresh(t *testing.T) {
	setTempConfigDir(t)
	servers := map[string]any{
		"goclaw-bridge": map[string]any{
			"command": "goclaw-bridge",
			"args":    []any{"--mode", "stdio"},
			"env":     map[string]any{"X-Sig": "abc"},
		},
		"fs": map[string]any{
			"command": "mcp-fs",
			"args":    []any{"--root", "/data"},
		},
	}

	path, err := MergeAgyMCPConfig(servers)
	if err != nil {
		t.Fatalf("MergeAgyMCPConfig error: %v", err)
	}
	if want := AgyMCPConfigPath(); path != want {
		t.Fatalf("returned path = %q, want %q", path, want)
	}

	top := readTop(t, path)
	gotServers, ok := top["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing/not a map; top = %#v", top)
	}

	// Inner map must round-trip back to the servers we passed in (value-shape
	// compare via JSON normalization to avoid []any vs []string mismatches).
	if !reflect.DeepEqual(normalizeViaJSON(t, gotServers), normalizeViaJSON(t, servers)) {
		t.Fatalf("mcpServers mismatch:\n got = %#v\nwant = %#v", gotServers, servers)
	}
}

// TestMergeAgyMCPConfig_PreservesExisting pre-seeds a config that already has an
// "odoo" server plus an unrelated top-level key, merges in a new "goclaw-bridge"
// server, and asserts BOTH servers survive AND the sibling top-level key is
// preserved.
func TestMergeAgyMCPConfig_PreservesExisting(t *testing.T) {
	dir := setTempConfigDir(t)
	path := filepath.Join(dir, "mcp_config.json")

	seed := map[string]any{
		"mcpServers": map[string]any{
			"odoo": map[string]any{
				"command": "odoo-mcp",
				"args":    []any{"--db", "prod"},
			},
		},
		"otherTopKey": float64(1),
	}
	seedBytes, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(path, seedBytes, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	bridge := map[string]any{
		"command": "goclaw-bridge",
		"args":    []any{"--mode", "stdio"},
	}
	if _, err := MergeAgyMCPConfig(map[string]any{"goclaw-bridge": bridge}); err != nil {
		t.Fatalf("MergeAgyMCPConfig error: %v", err)
	}

	top := readTop(t, path)

	// Sibling top-level key must be preserved untouched.
	if got, ok := top["otherTopKey"].(float64); !ok || got != 1 {
		t.Fatalf("otherTopKey not preserved: got %#v (ok=%v), want 1", top["otherTopKey"], ok)
	}

	servers, ok := top["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing/not a map; top = %#v", top)
	}
	// Pre-existing untouched server must survive.
	if _, ok := servers["odoo"]; !ok {
		t.Fatalf("existing 'odoo' server was dropped; servers = %#v", servers)
	}
	// New server must be present.
	if _, ok := servers["goclaw-bridge"]; !ok {
		t.Fatalf("new 'goclaw-bridge' server missing; servers = %#v", servers)
	}
	if len(servers) != 2 {
		t.Fatalf("expected 2 servers (odoo + goclaw-bridge), got %d: %#v", len(servers), servers)
	}
}

// TestMergeAgyMCPConfig_KeyCollisionCallerWins seeds a server under "odoo" then
// merges a different definition under the same key; the caller's entry must win.
func TestMergeAgyMCPConfig_KeyCollisionCallerWins(t *testing.T) {
	dir := setTempConfigDir(t)
	path := filepath.Join(dir, "mcp_config.json")

	seed := map[string]any{
		"mcpServers": map[string]any{
			"odoo": map[string]any{"command": "OLD", "args": []any{"--old"}},
		},
	}
	seedBytes, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, seedBytes, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	newEntry := map[string]any{"command": "NEW", "args": []any{"--new"}}
	if _, err := MergeAgyMCPConfig(map[string]any{"odoo": newEntry}); err != nil {
		t.Fatalf("MergeAgyMCPConfig error: %v", err)
	}

	servers := readTop(t, path)["mcpServers"].(map[string]any)
	got := servers["odoo"].(map[string]any)
	if got["command"] != "NEW" {
		t.Fatalf("caller did not win on key collision: command = %v, want NEW", got["command"])
	}
}

func TestMergeAgyMCPConfig_EmptyServersNoFile(t *testing.T) {
	setTempConfigDir(t)

	for _, m := range []map[string]any{nil, {}} {
		path, err := MergeAgyMCPConfig(m)
		if err != nil {
			t.Fatalf("MergeAgyMCPConfig(empty) error: %v", err)
		}
		if path != "" {
			t.Fatalf("MergeAgyMCPConfig(empty) path = %q, want \"\"", path)
		}
	}

	// No file should have been created.
	if _, err := os.Stat(AgyMCPConfigPath()); !os.IsNotExist(err) {
		t.Fatalf("config file should not exist for empty servers + no file; stat err = %v", err)
	}
}

// TestMergeAgyMCPConfig_EmptyServersExistingFile verifies that an empty servers
// map with a pre-existing file normalizes the file (does not delete it) and
// preserves its contents.
func TestMergeAgyMCPConfig_EmptyServersExistingFile(t *testing.T) {
	dir := setTempConfigDir(t)
	path := filepath.Join(dir, "mcp_config.json")

	seed := map[string]any{
		"mcpServers": map[string]any{
			"odoo": map[string]any{"command": "odoo-mcp"},
		},
		"otherTopKey": float64(1),
	}
	seedBytes, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, seedBytes, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	got, err := MergeAgyMCPConfig(map[string]any{})
	if err != nil {
		t.Fatalf("MergeAgyMCPConfig(empty, existing) error: %v", err)
	}
	if got != path {
		t.Fatalf("returned path = %q, want %q", got, path)
	}

	top := readTop(t, path)
	if _, ok := top["mcpServers"].(map[string]any)["odoo"]; !ok {
		t.Fatalf("existing 'odoo' server dropped on empty-servers merge; top = %#v", top)
	}
	if v, ok := top["otherTopKey"].(float64); !ok || v != 1 {
		t.Fatalf("otherTopKey not preserved on empty-servers merge: %#v", top["otherTopKey"])
	}
}

func TestMergeAgyMCPConfig_FilePerms(t *testing.T) {
	dir := setTempConfigDir(t)

	path, err := MergeAgyMCPConfig(map[string]any{"x": map[string]any{"command": "c"}})
	if err != nil {
		t.Fatalf("MergeAgyMCPConfig error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file perm = %o, want 0600", perm)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("config dir perm = %o, want 0700", perm)
	}
}

func TestMergeAgyMCPConfig_NoLeftoverTmp(t *testing.T) {
	dir := setTempConfigDir(t)

	path, err := MergeAgyMCPConfig(map[string]any{"x": map[string]any{"command": "c"}})
	if err != nil {
		t.Fatalf("MergeAgyMCPConfig error: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("unexpected dir contents %v, want only %q", names, filepath.Base(path))
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file %q after atomic write", e.Name())
		}
	}
}

func TestMergeAgyMCPConfig_SkipIfUnchanged(t *testing.T) {
	setTempConfigDir(t)
	servers := map[string]any{"x": map[string]any{"command": "c", "args": []any{"--a"}}}

	path, err := MergeAgyMCPConfig(servers)
	if err != nil {
		t.Fatalf("first merge error: %v", err)
	}

	infoBefore, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after first merge: %v", err)
	}

	// Sleep a hair so any actual rewrite would produce a distinguishable mtime on
	// filesystems with coarse timestamp resolution.
	time.Sleep(20 * time.Millisecond)

	path2, err := MergeAgyMCPConfig(servers)
	if err != nil {
		t.Fatalf("second merge error: %v", err)
	}
	if path2 != path {
		t.Fatalf("second merge path = %q, want %q", path2, path)
	}

	infoAfter, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after second merge: %v", err)
	}
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Fatalf("file was rewritten on identical merged content: mtime %v -> %v",
			infoBefore.ModTime(), infoAfter.ModTime())
	}
}

// TestMergeAgyMCPConfig_PreexistingDirGetsChmod0700 verifies that a config dir
// created earlier with loose (0755) perms is tightened to 0700 by a merge —
// MkdirAll alone is a no-op on an existing dir.
func TestMergeAgyMCPConfig_PreexistingDirGetsChmod0700(t *testing.T) {
	dir := setTempConfigDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod config dir loose: %v", err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o755 {
		t.Skipf("filesystem does not honour 0755 dir perms (got %o); cannot test chmod tightening", info.Mode().Perm())
	}

	if _, err := MergeAgyMCPConfig(map[string]any{"x": map[string]any{"command": "c"}}); err != nil {
		t.Fatalf("MergeAgyMCPConfig error: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir after merge: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("pre-existing config dir perm = %o, want 0700", perm)
	}
}

// TestMergeAgyMCPConfig_OverwritesLooseFilePerms verifies the final config is
// 0600 even when a pre-existing target file had looser perms.
func TestMergeAgyMCPConfig_OverwritesLooseFilePerms(t *testing.T) {
	dir := setTempConfigDir(t)
	path := filepath.Join(dir, "mcp_config.json")

	// Seed a loose-perm file with different content so the write isn't skipped.
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatalf("seed loose file: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod seed file: %v", err)
	}

	if _, err := MergeAgyMCPConfig(map[string]any{"x": map[string]any{"command": "c"}}); err != nil {
		t.Fatalf("MergeAgyMCPConfig error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file perm = %o, want 0600", perm)
	}
}

// TestMergeAgyMCPConfig_MalformedExistingErrors verifies a syntactically broken
// existing config surfaces a wrapped error rather than silently clobbering the
// user's file.
func TestMergeAgyMCPConfig_MalformedExistingErrors(t *testing.T) {
	dir := setTempConfigDir(t)
	path := filepath.Join(dir, "mcp_config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed malformed file: %v", err)
	}

	_, err := MergeAgyMCPConfig(map[string]any{"x": map[string]any{"command": "c"}})
	if err == nil {
		t.Fatal("expected error parsing malformed existing config, got nil")
	}
	if !strings.HasPrefix(err.Error(), "agycli: ") {
		t.Fatalf("error not wrapped with agycli prefix: %v", err)
	}
}

// TestMergeAgyMCPConfig_WrappedErrorOnUnwritableTarget forces a failure by
// locking the config dir read-only so CreateTemp inside it fails. The returned
// error must be wrapped with the agycli prefix and no temp file may leak.
func TestMergeAgyMCPConfig_WrappedErrorOnUnwritableTarget(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory perms do not prevent writes")
	}

	dir := setTempConfigDir(t)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod config dir read-only: %v", err)
	}
	// Restore writable perms so t.TempDir cleanup can remove the tree.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	// Verify the read-only dir actually blocks writes (some CI/root setups don't).
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err == nil {
		os.Remove(probe)
		t.Skip("filesystem does not enforce 0500 dir perms; cannot test write failure")
	}

	_, err := MergeAgyMCPConfig(map[string]any{"x": map[string]any{"command": "c"}})
	if err == nil {
		t.Fatal("expected error writing under unwritable dir, got nil")
	}
	if !strings.HasPrefix(err.Error(), "agycli: ") {
		t.Fatalf("error not wrapped with agycli prefix: %v", err)
	}

	// No stray temp file should remain in the dir.
	_ = os.Chmod(dir, 0o700)
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatalf("readdir: %v", rerr)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file %q after failed write", e.Name())
		}
	}
}

// readTop reads and decodes the top-level JSON object at path.
func readTop(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return top
}

// normalizeViaJSON marshals then unmarshals v so comparisons are done on
// canonical JSON value types (map[string]any / []any / float64).
func normalizeViaJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("normalize marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("normalize unmarshal: %v", err)
	}
	return out
}
