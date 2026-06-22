package config

import (
	"os"
	"path/filepath"
	"testing"
)

// --- AgyCLIConfig parses from JSON5 ---

func TestLoad_AgyCLIConfig_FromJSON5(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json5")

	content := `{
		"providers": {
			"agy_cli": {
				"cli_path": "/usr/local/bin/agy",
				"model": "gemini-pro",
				"base_work_dir": "/tmp/agy-workspaces",
				"sandbox": true,
				"skip_permissions": true,
			},
		},
	}`
	os.WriteFile(cfgPath, []byte(content), 0644)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	agy := cfg.Providers.AgyCLI
	if agy.CLIPath != "/usr/local/bin/agy" {
		t.Errorf("cli_path: got %q, want /usr/local/bin/agy", agy.CLIPath)
	}
	if agy.Model != "gemini-pro" {
		t.Errorf("model: got %q, want gemini-pro", agy.Model)
	}
	if agy.BaseWorkDir != "/tmp/agy-workspaces" {
		t.Errorf("base_work_dir: got %q, want /tmp/agy-workspaces", agy.BaseWorkDir)
	}
	if !agy.Sandbox {
		t.Errorf("sandbox: got false, want true")
	}
	if !agy.SkipPermissions {
		t.Errorf("skip_permissions: got false, want true")
	}
}

// --- HasAnyProvider true when only agy CLIPath is set ---

func TestHasAnyProvider_AgyCLIOnly(t *testing.T) {
	c := &Config{}
	if c.HasAnyProvider() {
		t.Fatal("empty config should not report any provider")
	}
	c.Providers.AgyCLI.CLIPath = "/usr/local/bin/agy"
	if !c.HasAnyProvider() {
		t.Fatal("agy_cli.cli_path set should make HasAnyProvider true")
	}
}

// --- Env override wiring for agy CLI ---

func TestLoad_AgyCLIEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json5")
	os.WriteFile(cfgPath, []byte(`{}`), 0644)

	t.Setenv("GOCLAW_AGY_CLI_PATH", "/opt/agy")
	t.Setenv("GOCLAW_AGY_CLI_MODEL", "gemini-flash")
	t.Setenv("GOCLAW_AGY_CLI_WORK_DIR", "/data/agy")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if cfg.Providers.AgyCLI.CLIPath != "/opt/agy" {
		t.Errorf("env cli_path: got %q, want /opt/agy", cfg.Providers.AgyCLI.CLIPath)
	}
	if cfg.Providers.AgyCLI.Model != "gemini-flash" {
		t.Errorf("env model: got %q, want gemini-flash", cfg.Providers.AgyCLI.Model)
	}
	if cfg.Providers.AgyCLI.BaseWorkDir != "/data/agy" {
		t.Errorf("env work_dir: got %q, want /data/agy", cfg.Providers.AgyCLI.BaseWorkDir)
	}
}
