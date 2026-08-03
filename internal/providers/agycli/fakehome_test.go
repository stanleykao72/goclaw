package agycli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// newRealGemini builds a stand-in for the operator's ~/.gemini with auth-ish
// files and an existing global mcp_config.json holding one operator server.
func newRealGemini(t *testing.T) string {
	t.Helper()
	real := filepath.Join(t.TempDir(), "real-gemini")
	if err := os.MkdirAll(filepath.Join(real, "antigravity-cli", "conversations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(real, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"oauth_creds.json", "google_accounts.json", "installation_id"} {
		if err := os.WriteFile(filepath.Join(real, f), []byte("real-"+f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	globalCfg := `{"mcpServers":{"operator-odoo":{"command":"/usr/local/bin/odoo-mcp"}},"otherTopLevel":"preserved"}`
	if err := os.WriteFile(filepath.Join(real, "config", "mcp_config.json"), []byte(globalCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return real
}

func readMCPConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("invalid json in %s: %v", path, err)
	}
	return doc
}

func TestBuildFakeHome_SymlinksAllButConfig(t *testing.T) {
	real := newRealGemini(t)
	fakeHome := filepath.Join(t.TempDir(), "fh")

	servers := BuildAgyBridgeServers("http://127.0.0.1:45678/mcp", "tok123")
	if err := BuildFakeHome(fakeHome, real, servers); err != nil {
		t.Fatal(err)
	}

	gem := filepath.Join(fakeHome, ".gemini")
	// Every real entry except config is a symlink pointing back at real.
	for _, name := range []string{"oauth_creds.json", "google_accounts.json", "installation_id", "antigravity-cli"} {
		link := filepath.Join(gem, name)
		fi, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("missing %s: %v", link, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is not a symlink", link)
		}
		target, _ := os.Readlink(link)
		if target != filepath.Join(real, name) {
			t.Fatalf("%s -> %s, want %s", link, target, filepath.Join(real, name))
		}
	}
	// config is a REAL directory, not a symlink.
	cfgInfo, err := os.Lstat(filepath.Join(gem, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if cfgInfo.Mode()&os.ModeSymlink != 0 || !cfgInfo.IsDir() {
		t.Fatal("config must be a private real directory")
	}
}

func TestBuildFakeHome_MergesBridgeOverGlobalConfig(t *testing.T) {
	real := newRealGemini(t)
	fakeHome := filepath.Join(t.TempDir(), "fh")

	servers := BuildAgyBridgeServers("http://127.0.0.1:45678/mcp", "tok123")
	if err := BuildFakeHome(fakeHome, real, servers); err != nil {
		t.Fatal(err)
	}

	doc := readMCPConfig(t, filepath.Join(fakeHome, ".gemini", "config", "mcp_config.json"))
	if doc["otherTopLevel"] != "preserved" {
		t.Fatal("unknown top-level keys from the global config must be preserved")
	}
	mcp, _ := doc["mcpServers"].(map[string]any)
	if mcp == nil {
		t.Fatal("missing mcpServers")
	}
	if _, ok := mcp["operator-odoo"]; !ok {
		t.Fatal("operator's existing server must survive the merge")
	}
	bridge, _ := mcp["goclaw-bridge"].(map[string]any)
	if bridge == nil {
		t.Fatal("missing goclaw-bridge entry")
	}
	if bridge["url"] != "http://127.0.0.1:45678/mcp" {
		t.Fatalf("bridge url = %v", bridge["url"])
	}
	// The REAL global config must be untouched.
	realDoc := readMCPConfig(t, filepath.Join(real, "config", "mcp_config.json"))
	realMCP, _ := realDoc["mcpServers"].(map[string]any)
	if _, leaked := realMCP["goclaw-bridge"]; leaked {
		t.Fatal("bridge entry leaked into the real global config")
	}
}

func TestBuildFakeHome_NoGlobalConfigStillWritesBridge(t *testing.T) {
	real := newRealGemini(t)
	if err := os.Remove(filepath.Join(real, "config", "mcp_config.json")); err != nil {
		t.Fatal(err)
	}
	fakeHome := filepath.Join(t.TempDir(), "fh")
	if err := BuildFakeHome(fakeHome, real, BuildAgyBridgeServers("http://127.0.0.1:1/mcp", "")); err != nil {
		t.Fatal(err)
	}
	doc := readMCPConfig(t, filepath.Join(fakeHome, ".gemini", "config", "mcp_config.json"))
	mcp, _ := doc["mcpServers"].(map[string]any)
	if _, ok := mcp["goclaw-bridge"]; !ok {
		t.Fatal("bridge entry missing when no global config exists")
	}
}

func TestBuildFakeHome_IdempotentRebuild(t *testing.T) {
	real := newRealGemini(t)
	fakeHome := filepath.Join(t.TempDir(), "fh")
	servers := BuildAgyBridgeServers("http://127.0.0.1:1/mcp", "")
	if err := BuildFakeHome(fakeHome, real, servers); err != nil {
		t.Fatal(err)
	}
	// Second build over the same dir must not fail on existing symlinks and
	// must refresh the config.
	servers2 := BuildAgyBridgeServers("http://127.0.0.1:2/mcp", "")
	if err := BuildFakeHome(fakeHome, real, servers2); err != nil {
		t.Fatalf("rebuild failed: %v", err)
	}
	doc := readMCPConfig(t, filepath.Join(fakeHome, ".gemini", "config", "mcp_config.json"))
	mcp, _ := doc["mcpServers"].(map[string]any)
	bridge, _ := mcp["goclaw-bridge"].(map[string]any)
	if bridge["url"] != "http://127.0.0.1:2/mcp" {
		t.Fatalf("config not refreshed on rebuild: %v", bridge["url"])
	}
}

func TestBuildFakeHome_MissingRealGeminiErrors(t *testing.T) {
	fakeHome := filepath.Join(t.TempDir(), "fh")
	if err := BuildFakeHome(fakeHome, filepath.Join(t.TempDir(), "nope"), nil); err == nil {
		t.Fatal("expected error for missing real gemini dir")
	}
}

func TestBuildFakeHome_RemoveAllLeavesRealIntact(t *testing.T) {
	real := newRealGemini(t)
	fakeHome := filepath.Join(t.TempDir(), "fh")
	if err := BuildFakeHome(fakeHome, real, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(fakeHome); err != nil {
		t.Fatal(err)
	}
	// Symlink targets must survive fake-home removal.
	if _, err := os.Stat(filepath.Join(real, "oauth_creds.json")); err != nil {
		t.Fatalf("real auth material damaged by fake-home removal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, "antigravity-cli", "conversations")); err != nil {
		t.Fatalf("real conversations dir damaged: %v", err)
	}
}

func TestRealGeminiDir_EnvOverride(t *testing.T) {
	t.Setenv("AGY_REAL_GEMINI_DIR", "/tmp/override-gemini")
	got, err := RealGeminiDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/override-gemini" {
		t.Fatalf("RealGeminiDir() = %q", got)
	}
}

func TestBuildFakeHome_StripsStaleGlobalBridgeEntry(t *testing.T) {
	real := newRealGemini(t)
	// An interactive session wrote its own goclaw-bridge entry into the
	// GLOBAL config; copying it into a session's isolated config would hand
	// this session another session's bridge identity.
	stale := `{"mcpServers":{"operator-odoo":{"command":"/usr/local/bin/odoo-mcp"},"goclaw-bridge":{"url":"http://127.0.0.1:59999/mcp/other","type":"http"}}}`
	if err := os.WriteFile(filepath.Join(real, "config", "mcp_config.json"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeHome := filepath.Join(t.TempDir(), "fh")

	// No bridge for this session: the stale entry must simply vanish.
	if err := BuildFakeHome(fakeHome, real, nil); err != nil {
		t.Fatal(err)
	}
	doc := readMCPConfig(t, filepath.Join(fakeHome, ".gemini", "config", "mcp_config.json"))
	mcp, _ := doc["mcpServers"].(map[string]any)
	if _, leaked := mcp["goclaw-bridge"]; leaked {
		t.Fatal("stale global bridge entry leaked into the isolated config")
	}
	if _, ok := mcp["operator-odoo"]; !ok {
		t.Fatal("operator server must survive the strip")
	}

	// With a bridge: THIS session's entry replaces the stale one.
	fakeHome2 := filepath.Join(t.TempDir(), "fh2")
	if err := BuildFakeHome(fakeHome2, real, BuildAgyBridgeServers("http://127.0.0.1:40001/mcp", "")); err != nil {
		t.Fatal(err)
	}
	doc2 := readMCPConfig(t, filepath.Join(fakeHome2, ".gemini", "config", "mcp_config.json"))
	mcp2, _ := doc2["mcpServers"].(map[string]any)
	bridge, _ := mcp2["goclaw-bridge"].(map[string]any)
	if bridge == nil || bridge["url"] != "http://127.0.0.1:40001/mcp" {
		t.Fatalf("session bridge entry wrong: %v", mcp2["goclaw-bridge"])
	}
}
