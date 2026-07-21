package esmithocr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ocrPromptTemplate instructs the vision CLI to open the temp image file and
// emit a single JSON object matching the Result schema. %s is the absolute
// image path — most one-shot CLIs have no shared workspace, so relative paths
// fail. Providers that attach images via a native flag still receive this
// prompt (without relying on tool-use for the pixels).
const ocrPromptTemplate = `Read the image file at %s using your tools (or the attached image). It is a Taiwan invoice or receipt (電子發票/收據/電子發票證明聯), possibly photographed at an angle. Extract the invoice fields and output ONLY a JSON object (no prose, no markdown fences) with exactly these keys:
- invoice_no: string or null (e.g. "AB12345678" or "CS02413936"; strip spaces/hyphens if printed as CS-02413936)
- invoice_date: ISO date string YYYY-MM-DD or null (convert ROC/民國 dates such as 115年07月18日 to Gregorian = year+1911)
- seller_vat: 8-digit string or null — REQUIRED when printed. Look explicitly for labels 賣方 / 賣方統一編號 / 銷售人統編 / 營業人統一編號 / 統一編號 near the vendor. On Taiwan e-invoice proof slips this is often a line like "賣方:42443718". Prefer that 8-digit seller BAN; do NOT use 買方/buyer VAT, 隨機碼, or phone numbers. Digits only, length 8; null only if no such 8-digit seller BAN is visible.
- seller_name: string or null (店名/營業人名稱 at top of receipt when visible)
- sales_amount: number or null (銷售額, pre-tax)
- tax_amount: number or null (稅額)
- total: number or null (總計 / 含稅總額; often labeled 總計)
- confidence: number between 0 and 1 for overall extraction confidence
Priority: if invoice_no or total is readable, also re-scan the image for seller_vat before returning null for it. Use null only when the field truly cannot be read.`

// Environment (no rebuild required):
//
//	ESMITH_OCR_PROVIDER  — CLI profile: agy | claude | codex | grok | gemini
//	                       (alias: ESMITH_OCR_CLI). Default: agy
//	ESMITH_OCR_BIN       — binary name/path override (default = provider bin)
//	ESMITH_OCR_MODEL     — model id/alias for that CLI (default per provider;
//	                       empty string means "omit --model, use CLI default")
//
// Unknown provider names fall back to a print-style argv shape
// (`-p <prompt> --model <m>`) with binary = provider string.
const (
	envOCRProvider = "ESMITH_OCR_PROVIDER"
	envOCRCLI      = "ESMITH_OCR_CLI" // alias of PROVIDER
	envOCRBin      = "ESMITH_OCR_BIN"
	envOCRModel    = "ESMITH_OCR_MODEL"

	defaultOCRProvider = "agy"
)

// visionConfig is the resolved one-shot CLI invocation plan.
type visionConfig struct {
	Provider string // normalized profile key
	Bin      string
	Model    string // may be empty → omit model flag
}

// resultEnvelope is the Claude Code / some CLIs `--output-format json`
// wrapper: the model's text answer lives in .result.
type resultEnvelope struct {
	Result string `json:"result"`
}

// runSpec is the concrete argv + optional post-process inputs for one call.
type runSpec struct {
	Args          []string
	LastMsgPath   string // if set, parse this file instead of stdout (codex -o)
	CleanupPaths  []string
	AttachViaFlag bool // documentation/logging only
}

func envTrim(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

func normalizeProvider(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	switch p {
	case "antigravity":
		return "agy"
	case "openai-codex", "openai_codex":
		return "codex"
	case "claude-code", "claude_code":
		return "claude"
	case "gemini-cli", "gemini_cli":
		return "gemini"
	default:
		return p
	}
}

func defaultBinFor(provider string) string {
	switch provider {
	case "agy":
		return "agy"
	case "claude":
		return "claude"
	case "codex":
		return "codex"
	case "grok":
		return "grok"
	case "gemini":
		return "gemini"
	default:
		// Unknown profile: treat the provider string itself as the binary name.
		if provider == "" {
			return defaultOCRProvider
		}
		return provider
	}
}

func defaultModelFor(provider string) string {
	switch provider {
	case "agy":
		return "Gemini 3.5 Flash (Medium)"
	case "claude":
		// CLI alias; empty would also work (subscription default).
		return "sonnet"
	case "codex":
		return "gpt-5.5"
	case "grok":
		// Empty → CLI default model.
		return ""
	case "gemini":
		return "gemini-2.5-flash"
	default:
		return ""
	}
}

// loadVisionConfig resolves provider/bin/model from env.
func loadVisionConfig() visionConfig {
	provider := envTrim(envOCRProvider)
	if provider == "" {
		provider = envTrim(envOCRCLI)
	}
	if provider == "" {
		provider = defaultOCRProvider
	}
	provider = normalizeProvider(provider)

	bin := envTrim(envOCRBin)
	if bin == "" {
		bin = defaultBinFor(provider)
	}

	// Model: if ESMITH_OCR_MODEL is unset → provider default.
	// If set to empty-looking value we still use default; to force "CLI
	// default / omit flag", set ESMITH_OCR_MODEL=- (dash sentinel) or the
	// provider's empty default.
	model := envTrim(envOCRModel)
	if os.Getenv(envOCRModel) == "" {
		model = defaultModelFor(provider)
	} else if model == "-" || model == "default" {
		model = ""
	}

	return visionConfig{Provider: provider, Bin: bin, Model: model}
}

// buildRunSpec builds argv for a known (or fallback) CLI profile.
// imagePath must be absolute for path-in-prompt providers.
func buildRunSpec(cfg visionConfig, prompt, imagePath string) (runSpec, error) {
	switch cfg.Provider {
	case "agy":
		// agy -p <prompt> --model <m> --dangerously-skip-permissions --print-timeout 55s
		args := []string{"-p", prompt, "--dangerously-skip-permissions", "--print-timeout", "55s"}
		args = appendModel(args, "--model", cfg.Model)
		return runSpec{Args: args}, nil

	case "claude":
		// claude -p <prompt> --output-format json --permission-mode bypassPermissions [--model m]
		args := []string{
			"-p", prompt,
			"--output-format", "json",
			"--permission-mode", "bypassPermissions",
		}
		args = appendModel(args, "--model", cfg.Model)
		return runSpec{Args: args}, nil

	case "codex":
		// codex exec ... -i <image> [-m model] -o <last-msg> <prompt>
		// Native image attach; last message written to a temp file for clean parse.
		last := filepath.Join(os.TempDir(), fmt.Sprintf("esmith-ocr-codex-%d.txt", os.Getpid()))
		// Use a unique name via CreateTemp semantics in caller? keep simple: pid+random
		f, err := os.CreateTemp("", "esmith-ocr-codex-*.txt")
		if err != nil {
			return runSpec{}, fmt.Errorf("codex last-message temp: %w", err)
		}
		last = f.Name()
		_ = f.Close()
		args := []string{
			"exec",
			"--ephemeral",
			"--skip-git-repo-check",
			"--dangerously-bypass-approvals-and-sandbox",
			"-i", imagePath,
			"-o", last,
		}
		args = appendModel(args, "-m", cfg.Model)
		args = append(args, prompt)
		return runSpec{
			Args:          args,
			LastMsgPath:   last,
			CleanupPaths:  []string{last},
			AttachViaFlag: true,
		}, nil

	case "grok":
		// grok -p/--single <prompt> [--model m] --permission-mode bypassPermissions
		args := []string{
			"-p", prompt,
			"--permission-mode", "bypassPermissions",
			"--output-format", "plain",
		}
		args = appendModel(args, "--model", cfg.Model)
		// also accept -m
		return runSpec{Args: args}, nil

	case "gemini":
		// gemini -p <prompt> [-m model] -y (yolo / auto-approve)
		args := []string{"-p", prompt, "-y"}
		args = appendModel(args, "-m", cfg.Model)
		return runSpec{Args: args}, nil

	default:
		// Generic print-style: -p prompt [--model m] --dangerously-skip-permissions
		args := []string{"-p", prompt, "--dangerously-skip-permissions"}
		args = appendModel(args, "--model", cfg.Model)
		return runSpec{Args: args}, nil
	}
}

func appendModel(args []string, flag, model string) []string {
	if model == "" {
		return args
	}
	return append(args, flag, model)
}

// defaultRunner executes a one-shot vision extraction against the image at
// imagePath using the configured CLI provider (agy/claude/codex/grok/gemini/…).
//
// Error contract:
//   - context timeout / cancellation → (nil, nil): treated as "not recognized"
//   - unparseable CLI output → (nil, nil) with a warn log
//   - binary not found → (nil, error): deployment problem worth surfacing
func defaultRunner(ctx context.Context, imagePath string) (*Result, error) {
	cfg := loadVisionConfig()
	prompt := fmt.Sprintf(ocrPromptTemplate, imagePath)

	spec, err := buildRunSpec(cfg, prompt, imagePath)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, p := range spec.CleanupPaths {
			_ = os.Remove(p)
		}
	}()

	cmd := exec.CommandContext(ctx, cfg.Bin, spec.Args...)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			slog.Warn("esmith-ocr: vision run aborted",
				"provider", cfg.Provider, "bin", cfg.Bin, "model", cfg.Model,
				"reason", ctx.Err().Error())
			return nil, nil
		}
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%s CLI not found (provider=%s): %w", cfg.Bin, cfg.Provider, err)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			slog.Warn("esmith-ocr: vision run failed",
				"provider", cfg.Provider, "bin", cfg.Bin, "model", cfg.Model,
				"err", err, "stderr", truncate(string(ee.Stderr), 400))
		} else {
			slog.Warn("esmith-ocr: vision run failed",
				"provider", cfg.Provider, "bin", cfg.Bin, "model", cfg.Model, "err", err)
		}
		return nil, nil
	}

	// Prefer last-message file when the provider wrote one (codex).
	raw := out
	if spec.LastMsgPath != "" {
		if b, rerr := os.ReadFile(spec.LastMsgPath); rerr == nil && len(bytesTrimSpace(b)) > 0 {
			raw = b
		}
	}

	res, ok := parseVisionOutput(raw)
	if !ok {
		slog.Warn("esmith-ocr: vision output unparseable",
			"provider", cfg.Provider, "bin", cfg.Bin, "model", cfg.Model,
			"stdout", truncate(string(raw), 400))
		return nil, nil
	}
	return res, nil
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// parseVisionOutput accepts either:
//  1. plain model text containing a JSON object (agy/grok/gemini/codex last-msg)
//  2. result envelope {"result":"..."} (claude --output-format json)
func parseVisionOutput(stdout []byte) (*Result, bool) {
	text := strings.TrimSpace(string(stdout))
	if text == "" {
		return nil, false
	}
	if res, ok := parseResultEnvelope(stdout); ok {
		return res, true
	}
	body := extractJSONBody(text)
	if body == "" {
		return nil, false
	}
	return decodeResultBody(body)
}

// parseResultEnvelope unmarshals {"result":"<text>"} wrappers and decodes
// the inner Result payload (after optional markdown fences).
func parseResultEnvelope(stdout []byte) (*Result, bool) {
	var env resultEnvelope
	if err := json.Unmarshal(stdout, &env); err != nil {
		return nil, false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(stdout, &top); err != nil {
		return nil, false
	}
	if _, has := top["result"]; !has {
		return nil, false
	}
	body := stripFences(env.Result)
	if body == "" {
		return nil, false
	}
	return decodeResultBody(body)
}

func decodeResultBody(body string) (*Result, bool) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		var res Result
		if err2 := json.Unmarshal([]byte(body), &res); err2 != nil {
			return nil, false
		}
		normalizeResult(&res)
		return &res, true
	}
	if !hasInvoiceKey(raw) {
		return nil, false
	}
	res := resultFromMap(raw)
	normalizeResult(res)
	return res, true
}

func hasInvoiceKey(raw map[string]any) bool {
	for _, k := range []string{
		"invoice_no", "invoice_date", "seller_vat", "seller_name",
		"sales_amount", "tax_amount", "total", "confidence",
	} {
		if _, ok := raw[k]; ok {
			return true
		}
	}
	return false
}

// extractJSONBody pulls the first JSON object out of free-form CLI stdout.
// Strips common "Summary of work" tails and optional markdown fences.
func extractJSONBody(s string) string {
	s = stripSummaryOfWork(s)
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "```") {
		if body := extractFencedBlock(s); body != "" {
			body = strings.TrimSpace(body)
			if strings.HasPrefix(body, "{") {
				if obj := firstJSONObject(body); obj != "" {
					return obj
				}
			}
		}
	}
	return firstJSONObject(s)
}

var summaryOfWorkRe = regexp.MustCompile(`(?is)\n-{0,3}\s*\**\s*Summary of work\s*:?\s*\**\s*\n.*$`)

func stripSummaryOfWork(s string) string {
	return summaryOfWorkRe.ReplaceAllString(s, "")
}

func extractFencedBlock(s string) string {
	start := strings.Index(s, "```")
	if start < 0 {
		return ""
	}
	rest := s[start+3:]
	if nl := strings.Index(rest, "\n"); nl >= 0 {
		first := strings.TrimSpace(rest[:nl])
		if first == "" || !strings.Contains(first, "{") {
			rest = rest[nl+1:]
		}
	}
	end := strings.Index(rest, "```")
	if end < 0 {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(rest[:end])
}

func firstJSONObject(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			if c == '\\' {
				esc = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func resultFromMap(raw map[string]any) *Result {
	res := &Result{}
	res.InvoiceNo = coerceStringPtr(raw["invoice_no"])
	res.InvoiceDate = coerceStringPtr(raw["invoice_date"])
	res.SellerVat = coerceStringPtr(raw["seller_vat"])
	res.SellerName = coerceStringPtr(raw["seller_name"])
	res.SalesAmount = coerceFloatPtr(raw["sales_amount"])
	res.TaxAmount = coerceFloatPtr(raw["tax_amount"])
	res.Total = coerceFloatPtr(raw["total"])
	res.Confidence = coerceFloatPtr(raw["confidence"])
	return res
}

func coerceStringPtr(v any) *string {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		if s == "" || s == "null" {
			return nil
		}
		return &s
	case float64:
		s := strconv.FormatInt(int64(t), 10)
		return &s
	case json.Number:
		s := t.String()
		return &s
	default:
		s := strings.TrimSpace(fmt.Sprint(t))
		if s == "" || s == "<nil>" {
			return nil
		}
		return &s
	}
}

func coerceFloatPtr(v any) *float64 {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case float64:
		return &t
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return nil
		}
		return &f
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return nil
		}
		return &f
	default:
		return nil
	}
}

// normalizeResult cleans seller_vat to exactly 8 digits when possible.
func normalizeResult(res *Result) {
	if res == nil || res.SellerVat == nil {
		return
	}
	digits := regexp.MustCompile(`\D`).ReplaceAllString(*res.SellerVat, "")
	if len(digits) == 8 {
		res.SellerVat = &digits
		return
	}
	if regexp.MustCompile(`^\d{8}$`).MatchString(strings.TrimSpace(*res.SellerVat)) {
		s := strings.TrimSpace(*res.SellerVat)
		res.SellerVat = &s
		return
	}
	res.SellerVat = nil
}

// stripFences removes a surrounding markdown code fence if present.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = s[idx+1:]
	} else {
		return ""
	}
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
