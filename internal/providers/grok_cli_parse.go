package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// grokUsage maps the grok CLI usage counters (§2.1). input_tokens/output_tokens
// are the billable prompt/completion counts; cache_read_input_tokens and
// reasoning_tokens are tracked separately, and total_tokens is grok's own total
// (which includes cache/reasoning and so differs from input+output).
type grokUsage struct {
	InputTokens          int `json:"input_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	ReasoningTokens      int `json:"reasoning_tokens"`
	TotalTokens          int `json:"total_tokens"`
}

// grokJSONResponse is the single JSON object emitted by
// `grok -p ... --output-format json` (§2.1).
type grokJSONResponse struct {
	Text       string     `json:"text"`
	Thought    string     `json:"thought"`
	StopReason string     `json:"stopReason"`
	SessionID  string     `json:"sessionId"`
	Usage      *grokUsage `json:"usage"`
}

// grokStreamEvent is a single line from `--output-format streaming-json` (§2.2).
// Exactly three event types occur: "thought" and "text" carry Data; "end"
// carries StopReason, SessionId and Usage.
type grokStreamEvent struct {
	Type       string     `json:"type"`       // "thought" | "text" | "end"
	Data       string     `json:"data"`       // for "thought"/"text"
	StopReason string     `json:"stopReason"` // for "end"
	SessionId  string     `json:"sessionId"`  // for "end"
	Usage      *grokUsage `json:"usage"`      // for "end"
}

// parseGrokJSONResponse parses the grok CLI JSON output into a ChatResponse.
// grok emits a single JSON object; some builds may emit line-delimited objects,
// in which case the last parseable object wins.
func parseGrokJSONResponse(data []byte) (*ChatResponse, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("grok-cli: empty response")
	}

	var resp *grokJSONResponse
	if trimmed[0] == '{' {
		var r grokJSONResponse
		if err := json.Unmarshal(trimmed, &r); err == nil {
			resp = &r
		}
	}
	if resp == nil {
		// Fallback: one JSON object per line — take the last that parses.
		for line := range bytes.SplitSeq(trimmed, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 || line[0] != '{' {
				continue
			}
			var r grokJSONResponse
			if err := json.Unmarshal(line, &r); err == nil {
				resp = &r
			}
		}
	}
	if resp == nil {
		return nil, fmt.Errorf("grok-cli: unparseable response: %s", truncateForErr(trimmed, 256))
	}

	cr := &ChatResponse{
		Content:      resp.Text,
		Thinking:     resp.Thought,
		FinishReason: grokFinishReason(resp.StopReason),
	}
	if resp.Usage != nil {
		cr.Usage = mapGrokUsage(resp.Usage)
	}
	return cr, nil
}

// grokFinishReason maps grok's stopReason to the internal finish reason:
// "EndTurn" → "stop", anything else → "error".
func grokFinishReason(stopReason string) string {
	if stopReason == "EndTurn" {
		return "stop"
	}
	return "error"
}

// mapGrokUsage converts grok usage counters into the internal Usage struct.
// TotalTokens uses grok's reported total (not input+output, which excludes
// cache/reasoning). Cache-read and reasoning counts are surfaced for pricing.
func mapGrokUsage(u *grokUsage) *Usage {
	return &Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		ThinkingTokens:   u.ReasoningTokens,
	}
}

// truncateForErr returns at most n bytes of s as a string for error messages.
func truncateForErr(s []byte, n int) string {
	if len(s) > n {
		return string(s[:n]) + "…"
	}
	return string(s)
}
