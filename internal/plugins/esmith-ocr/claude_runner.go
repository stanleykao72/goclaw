package esmithocr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// ocrPromptTemplate instructs claude to read the temp image file and emit a
// single JSON object matching the Result schema. %s is the image path.
const ocrPromptTemplate = `Read the image file at %s. It is a Taiwan invoice or receipt (電子發票/收據), possibly handwritten. Extract the invoice fields and output ONLY a JSON object (no prose, no markdown fences) with exactly these keys:
- invoice_no: string or null (e.g. "AB12345678")
- invoice_date: ISO date string YYYY-MM-DD or null (convert ROC/民國 dates to Gregorian)
- seller_vat: 8-digit string or null (賣方統一編號)
- seller_name: string or null
- sales_amount: number or null (銷售額, pre-tax)
- tax_amount: number or null (稅額)
- total: number or null (總計)
- confidence: number between 0 and 1 for overall extraction confidence
Use null for any field you cannot read with reasonable confidence.`

// cliEnvelope is the `claude -p --output-format json` stdout wrapper. The
// model's text answer lives in .result as a string.
type cliEnvelope struct {
	Result string `json:"result"`
}

// defaultRunner executes a one-shot `claude -p` vision extraction against the
// image at imagePath.
//
// Error contract:
//   - context timeout / cancellation → (nil, nil): treated as "not recognized"
//   - unparseable CLI output → (nil, nil) with a warn log
//   - claude binary not found → (nil, error): deployment problem worth surfacing
func defaultRunner(ctx context.Context, imagePath string) (*Result, error) {
	prompt := fmt.Sprintf(ocrPromptTemplate, imagePath)
	cmd := exec.CommandContext(ctx, "claude",
		"-p", prompt,
		"--output-format", "json",
		"--permission-mode", "bypassPermissions")
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			// Timeout or cancellation: CommandContext already killed the
			// process. Not recognized, not an error.
			slog.Warn("esmith-ocr: claude run aborted", "reason", ctx.Err().Error())
			return nil, nil
		}
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("claude CLI not found: %w", err)
		}
		slog.Warn("esmith-ocr: claude run failed", "err", err)
		return nil, nil
	}
	res, ok := parseClaudeOutput(out)
	if !ok {
		slog.Warn("esmith-ocr: claude output unparseable")
		return nil, nil
	}
	return res, nil
}

// parseClaudeOutput unmarshals the CLI JSON envelope, strips optional
// markdown code fences from .result, and decodes the Result payload.
// Returns ok=false when any stage fails.
func parseClaudeOutput(stdout []byte) (*Result, bool) {
	var env cliEnvelope
	if err := json.Unmarshal(stdout, &env); err != nil {
		return nil, false
	}
	body := stripFences(env.Result)
	if body == "" {
		return nil, false
	}
	var res Result
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return nil, false
	}
	return &res, true
}

// stripFences removes a surrounding markdown code fence (```json ... ``` or
// ``` ... ```) if present, returning the trimmed inner text.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence line (``` or ```json).
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = s[idx+1:]
	} else {
		return ""
	}
	// Drop a trailing closing fence.
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
