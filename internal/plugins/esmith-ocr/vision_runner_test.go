package esmithocr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseResultEnvelope(t *testing.T) {
	envelope := func(result string) []byte {
		out, err := json.Marshal(map[string]string{"result": result})
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		return out
	}

	validJSON := `{"invoice_no":"AB12345678","invoice_date":"2026-07-01","seller_vat":"12345678","seller_name":"Test Co","sales_amount":100,"tax_amount":5,"total":105,"confidence":0.95}`

	cases := []struct {
		name   string
		stdout []byte
		wantOK bool
		check  func(t *testing.T, r *Result)
	}{
		{
			name:   "plain json result",
			stdout: envelope(validJSON),
			wantOK: true,
			check: func(t *testing.T, r *Result) {
				if r.InvoiceNo == nil || *r.InvoiceNo != "AB12345678" {
					t.Errorf("invoice_no = %v", r.InvoiceNo)
				}
				if r.Total == nil || *r.Total != 105 {
					t.Errorf("total = %v", r.Total)
				}
			},
		},
		{
			name:   "json fenced result",
			stdout: envelope("```json\n" + validJSON + "\n```"),
			wantOK: true,
			check: func(t *testing.T, r *Result) {
				if r.SellerVat == nil || *r.SellerVat != "12345678" {
					t.Errorf("seller_vat = %v", r.SellerVat)
				}
			},
		},
		{
			name:   "bare fenced result",
			stdout: envelope("```\n" + validJSON + "\n```"),
			wantOK: true,
		},
		{
			name:   "null fields",
			stdout: envelope(`{"invoice_no":null,"invoice_date":null,"seller_vat":null,"seller_name":null,"sales_amount":null,"tax_amount":null,"total":null,"confidence":0.1}`),
			wantOK: true,
			check: func(t *testing.T, r *Result) {
				if r.InvoiceNo != nil {
					t.Errorf("invoice_no = %v, want nil", *r.InvoiceNo)
				}
				if r.Confidence == nil || *r.Confidence != 0.1 {
					t.Errorf("confidence = %v", r.Confidence)
				}
			},
		},
		{
			name:   "envelope not json",
			stdout: []byte("segmentation fault"),
			wantOK: false,
		},
		{
			name:   "result not json",
			stdout: envelope("Sorry, I cannot read this image."),
			wantOK: false,
		},
		{
			name:   "empty result",
			stdout: envelope(""),
			wantOK: false,
		},
		{
			name:   "fence with no body",
			stdout: envelope("```json\n```"),
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, ok := parseResultEnvelope(tc.stdout)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (res=%+v)", ok, tc.wantOK, res)
			}
			if tc.wantOK && res == nil {
				t.Fatal("ok but nil result")
			}
			if tc.check != nil && res != nil {
				tc.check(t, res)
			}
		})
	}
}

func TestParseVisionOutputPlain(t *testing.T) {
	validJSON := `{"invoice_no":"CS02413936","invoice_date":"2026-07-18","seller_vat":"42443718","seller_name":null,"sales_amount":null,"tax_amount":null,"total":1050,"confidence":0.95}`
	cases := []struct {
		name   string
		stdout string
		wantOK bool
		inv    string
	}{
		{"raw json", validJSON, true, "CS02413936"},
		{"fenced", "```json\n" + validJSON + "\n```", true, "CS02413936"},
		{"with summary tail", validJSON + "\n\n---\n**Summary of work:** Identified fields.\n", true, "CS02413936"},
		{"prose prefix", "Here is the extraction:\n" + validJSON + "\n", true, "CS02413936"},
		{"garbage", "I cannot help with that.", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, ok := parseVisionOutput([]byte(tc.stdout))
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want=%v res=%+v", ok, tc.wantOK, res)
			}
			if tc.wantOK {
				if res == nil || res.InvoiceNo == nil || *res.InvoiceNo != tc.inv {
					t.Fatalf("res=%+v", res)
				}
			}
		})
	}
}

func TestStripFences(t *testing.T) {
	obj := `{"a":1}`
	cases := []struct {
		in, want string
	}{
		{obj, obj},
		{"```json\n" + obj + "\n```", obj},
		{"```\n" + obj + "\n```", obj},
		{"  " + obj + "  ", obj},
		{"```", ""},
	}
	for _, tc := range cases {
		if got := stripFences(tc.in); got != tc.want {
			t.Errorf("stripFences(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeProvider(t *testing.T) {
	cases := map[string]string{
		"agy":           "agy",
		"Antigravity":  "agy",
		"CLAUDE":        "claude",
		"claude-code":   "claude",
		"codex":         "codex",
		"openai-codex":  "codex",
		"grok":          "grok",
		"gemini":        "gemini",
		"gemini-cli":    "gemini",
	}
	for in, want := range cases {
		if got := normalizeProvider(in); got != want {
			t.Errorf("normalizeProvider(%q)=%q want %q", in, got, want)
		}
	}
}

func TestBuildRunSpecProfiles(t *testing.T) {
	prompt := "EXTRACT"
	img := "/tmp/inv.jpg"

	t.Run("agy", func(t *testing.T) {
		spec, err := buildRunSpec(visionConfig{Provider: "agy", Model: "Gemini 3.5 Flash (Medium)"}, prompt, img)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(spec.Args, " ")
		if !strings.Contains(joined, "-p "+prompt) {
			t.Fatalf("args=%v", spec.Args)
		}
		if !strings.Contains(joined, "--dangerously-skip-permissions") {
			t.Fatalf("missing skip-permissions: %v", spec.Args)
		}
		if !strings.Contains(joined, "Gemini 3.5 Flash (Medium)") {
			t.Fatalf("missing model: %v", spec.Args)
		}
	})

	t.Run("claude", func(t *testing.T) {
		spec, err := buildRunSpec(visionConfig{Provider: "claude", Model: "sonnet"}, prompt, img)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(spec.Args, " ")
		if !strings.Contains(joined, "--output-format json") {
			t.Fatalf("args=%v", spec.Args)
		}
		if !strings.Contains(joined, "--permission-mode bypassPermissions") {
			t.Fatalf("args=%v", spec.Args)
		}
	})

	t.Run("codex", func(t *testing.T) {
		spec, err := buildRunSpec(visionConfig{Provider: "codex", Model: "gpt-5.5"}, prompt, img)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			for _, p := range spec.CleanupPaths {
				_ = os.Remove(p)
			}
		}()
		if len(spec.Args) < 2 || spec.Args[0] != "exec" {
			t.Fatalf("args=%v", spec.Args)
		}
		joined := strings.Join(spec.Args, " ")
		if !strings.Contains(joined, "-i "+img) {
			t.Fatalf("missing image flag: %v", spec.Args)
		}
		if spec.LastMsgPath == "" {
			t.Fatal("expected LastMsgPath")
		}
		if !spec.AttachViaFlag {
			t.Fatal("expected AttachViaFlag")
		}
	})

	t.Run("grok", func(t *testing.T) {
		spec, err := buildRunSpec(visionConfig{Provider: "grok", Model: "grok-4"}, prompt, img)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(spec.Args, " ")
		if !strings.Contains(joined, "--permission-mode bypassPermissions") {
			t.Fatalf("args=%v", spec.Args)
		}
	})

	t.Run("gemini", func(t *testing.T) {
		spec, err := buildRunSpec(visionConfig{Provider: "gemini", Model: "gemini-2.5-flash"}, prompt, img)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(spec.Args, " ")
		if !strings.Contains(joined, "-y") {
			t.Fatalf("args=%v", spec.Args)
		}
	})

	t.Run("omit empty model", func(t *testing.T) {
		spec, err := buildRunSpec(visionConfig{Provider: "agy", Model: ""}, prompt, img)
		if err != nil {
			t.Fatal(err)
		}
		for i, a := range spec.Args {
			if a == "--model" {
				t.Fatalf("unexpected --model at %d in %v", i, spec.Args)
			}
		}
	})
}

func TestLoadVisionConfigEnv(t *testing.T) {
	t.Setenv(envOCRProvider, "claude")
	t.Setenv(envOCRBin, "")
	t.Setenv(envOCRModel, "opus")
	// Clear alias so provider wins
	t.Setenv(envOCRCLI, "")
	cfg := loadVisionConfig()
	if cfg.Provider != "claude" || cfg.Bin != "claude" || cfg.Model != "opus" {
		t.Fatalf("cfg=%+v", cfg)
	}

	t.Setenv(envOCRProvider, "")
	t.Setenv(envOCRCLI, "codex")
	t.Setenv(envOCRModel, "")
	// Unset model env entirely for default — t.Setenv("") still sets empty;
	// loadVisionConfig treats unset vs empty: os.Getenv("") returns "" for both
	// when Setenv to "". Use dash sentinel for CLI default.
	t.Setenv(envOCRModel, "-")
	cfg = loadVisionConfig()
	if cfg.Provider != "codex" || cfg.Bin != "codex" || cfg.Model != "" {
		t.Fatalf("cfg=%+v", cfg)
	}
}

// stubOCR installs a fake vision binary and points ESMITH_OCR_* at it.
// PATH keeps /bin so sleep/etc remain available for timeout tests.
func stubOCR(t *testing.T, binName, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub not supported on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, binName)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	t.Setenv(envOCRProvider, binName) // profile name = binary for generic/agy-like
	t.Setenv(envOCRBin, binName)
	t.Setenv(envOCRModel, "test-model")
}

func TestDefaultRunnerStubHappyPath(t *testing.T) {
	// Plain JSON stdout (agy/grok/gemini style). Use provider=agy so args match.
	stubOCR(t, "agy", `printf '%s' '{"invoice_no":"XY99887766","invoice_date":null,"seller_vat":null,"seller_name":null,"sales_amount":null,"tax_amount":null,"total":null,"confidence":0.5}'`)
	t.Setenv(envOCRProvider, "agy")
	res, err := defaultRunner(context.Background(), "/tmp/fake.jpg")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.InvoiceNo == nil || *res.InvoiceNo != "XY99887766" {
		t.Fatalf("res = %+v", res)
	}
}

func TestDefaultRunnerClaudeEnvelope(t *testing.T) {
	stubOCR(t, "claude", `printf '%s' '{"result":"{\"invoice_no\":\"CL11112222\",\"invoice_date\":null,\"seller_vat\":null,\"seller_name\":null,\"sales_amount\":null,\"tax_amount\":null,\"total\":null,\"confidence\":0.4}"}'`)
	t.Setenv(envOCRProvider, "claude")
	t.Setenv(envOCRBin, "claude")
	res, err := defaultRunner(context.Background(), "/tmp/fake.jpg")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.InvoiceNo == nil || *res.InvoiceNo != "CL11112222" {
		t.Fatalf("res = %+v", res)
	}
}

func TestDefaultRunnerUnparseableOutput(t *testing.T) {
	stubOCR(t, "agy", `echo "not json at all"`)
	t.Setenv(envOCRProvider, "agy")
	res, err := defaultRunner(context.Background(), "/tmp/fake.jpg")
	if err != nil {
		t.Fatalf("err = %v, want nil for unparseable output", err)
	}
	if res != nil {
		t.Fatalf("res = %+v, want nil", res)
	}
}

func TestDefaultRunnerTimeout(t *testing.T) {
	stubOCR(t, "agy", `exec sleep 10`)
	t.Setenv(envOCRProvider, "agy")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := defaultRunner(ctx, "/tmp/fake.jpg")
	if err != nil {
		t.Fatalf("err = %v, want nil on timeout", err)
	}
	if res != nil {
		t.Fatalf("res = %+v, want nil on timeout", res)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("runner did not honor context timeout (took %v)", elapsed)
	}
}

func TestDefaultRunnerCommandNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv(envOCRProvider, "agy")
	t.Setenv(envOCRBin, "agy")
	res, err := defaultRunner(context.Background(), "/tmp/fake.jpg")
	if err == nil {
		t.Fatal("err = nil, want error when binary is missing")
	}
	if res != nil {
		t.Fatalf("res = %+v, want nil", res)
	}
}
