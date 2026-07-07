package esmithocr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestParseClaudeOutput(t *testing.T) {
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
			res, ok := parseClaudeOutput(tc.stdout)
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

func TestStripFences(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n{\"a\":1}\n```", `{"a":1}`},
		{"  {\"a\":1}  ", `{"a":1}`},
		{"```", ""},
	}
	for _, tc := range cases {
		if got := stripFences(tc.in); got != tc.want {
			t.Errorf("stripFences(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// stubClaude installs a fake `claude` executable (a shell script) as the only
// binary on PATH and returns after registering cleanup via t.Setenv.
func stubClaude(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub not supported on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir)
}

func TestDefaultRunnerStubHappyPath(t *testing.T) {
	stubClaude(t, `printf '%s' '{"result":"{\"invoice_no\":\"XY99887766\",\"invoice_date\":null,\"seller_vat\":null,\"seller_name\":null,\"sales_amount\":null,\"tax_amount\":null,\"total\":null,\"confidence\":0.5}"}'`)
	res, err := defaultRunner(context.Background(), "/tmp/fake.jpg")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.InvoiceNo == nil || *res.InvoiceNo != "XY99887766" {
		t.Fatalf("res = %+v", res)
	}
}

func TestDefaultRunnerUnparseableOutput(t *testing.T) {
	stubClaude(t, `echo "not json at all"`)
	res, err := defaultRunner(context.Background(), "/tmp/fake.jpg")
	if err != nil {
		t.Fatalf("err = %v, want nil for unparseable output", err)
	}
	if res != nil {
		t.Fatalf("res = %+v, want nil", res)
	}
}

func TestDefaultRunnerTimeout(t *testing.T) {
	stubClaude(t, `sleep 10`)
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
	t.Setenv("PATH", t.TempDir()) // empty dir: no claude binary
	res, err := defaultRunner(context.Background(), "/tmp/fake.jpg")
	if err == nil {
		t.Fatal("err = nil, want error when claude binary is missing")
	}
	if res != nil {
		t.Fatalf("res = %+v, want nil", res)
	}
}
