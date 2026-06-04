package lineworksworkflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const mcpRequestTimeout = 15 * time.Second

// ----------------------------------------------------------------------------
// Flow routing
// ----------------------------------------------------------------------------

// flowKind classifies an inbound text into a workflow.
type flowKind int

const (
	flowNone flowKind = iota
	flowTodo
	flowDailyLog
)

// classifyText decides which flow a free-text message triggers. Matching is
// keyword-prefix based and case-insensitive; the remainder after the keyword
// is the flow argument (e.g. the todo title).
//
// Returns (flowNone, "") when nothing matches — the hook layer ignores those
// so this plugin can coexist with an agent path on the same channel.
func classifyText(text string) (flowKind, string) {
	t := strings.TrimSpace(text)
	if t == "" {
		return flowNone, ""
	}
	lower := strings.ToLower(t)

	for _, kw := range []string{"待辦", "todo", "代辦"} {
		if rest, ok := cutKeyword(t, lower, kw); ok {
			return flowTodo, rest
		}
	}
	for _, kw := range []string{"日報", "daily", "回報"} {
		if rest, ok := cutKeyword(t, lower, kw); ok {
			return flowDailyLog, rest
		}
	}
	return flowNone, ""
}

// cutKeyword reports whether lower starts with kw (already lowercased) and if
// so returns the trimmed remainder of the ORIGINAL-cased string.
func cutKeyword(orig, lower, kw string) (string, bool) {
	kwLower := strings.ToLower(kw)
	if !strings.HasPrefix(lower, kwLower) {
		return "", false
	}
	rest := orig[len(kw):]
	// Tolerate a separator after the keyword (space / colon / fullwidth colon).
	rest = strings.TrimLeft(rest, " \t：:　-")
	return strings.TrimSpace(rest), true
}

// ----------------------------------------------------------------------------
// Todo flow
// ----------------------------------------------------------------------------

// runTodoFlow creates a project.task for the resolved user via create_my_todo.
// The task title is the text remainder; empty title is rejected with a hint.
func (h *Hook) runTodoFlow(ctx context.Context, title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "請在「待辦」後面加上要新增的待辦事項，例如：待辦 巡檢 3F 配電盤", nil
	}
	if h.cfg.TodoProjectID == 0 {
		return "尚未設定待辦預設專案（TodoProjectID），請聯絡系統管理員。", nil
	}

	var res struct {
		TaskID int `json:"task_id"`
	}
	err := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "create_my_todo", map[string]any{
		"name":       title,
		"project_id": h.cfg.TodoProjectID,
	}, &res)
	if err != nil {
		return "", fmt.Errorf("create_my_todo: %w", err)
	}
	if res.TaskID == 0 {
		return "待辦建立失敗，請稍後再試。", nil
	}
	return fmt.Sprintf("✅ 已新增待辦（#%d）：%s", res.TaskID, title), nil
}

// ----------------------------------------------------------------------------
// Daily-log flow (idempotent per calendar day)
// ----------------------------------------------------------------------------

// runDailyLogFlow runs autofill → create (or reuse) → submit for today's
// daily log, idempotent per calendar day:
//
//   - If a daily log already exists for today it is reused instead of
//     creating a duplicate (create step skipped).
//   - submit_daily_log's structured "already_submitted" result is treated as
//     success, not an error, so re-sending "日報" the same day is harmless.
//
// note is an optional free-text remainder appended to the autofill summary as
// the log body. Photo upload is wired via uploadPhotos but the text-driven
// entrypoint passes none (photos arrive on separate image events, out of v1
// scope here — the helper is kept for the contract's
// autofill→create→upload→submit shape).
func (h *Hook) runDailyLogFlow(ctx context.Context, note string) (string, error) {
	today := time.Now().Format("2006-01-02")

	// 1. autofill — pull the summary the daily-log form pre-populates with.
	var fill struct {
		TimesheetCount int `json:"timesheet_count"`
		TodoCount      int `json:"todo_count"`
		DoneCount      int `json:"done_count"`
	}
	if err := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "autofill_daily_log", map[string]any{
		"date": today,
	}, &fill); err != nil {
		return "", fmt.Errorf("autofill_daily_log: %w", err)
	}

	// 2. create-or-reuse — idempotency gate.
	logID, reused, err := h.ensureTodayDailyLog(ctx, today, note, fill.TimesheetCount, fill.TodoCount, fill.DoneCount)
	if err != nil {
		return "", err
	}

	// 3. submit — already_submitted / timesheet_required surfaced as text.
	msg, err := h.submitDailyLog(ctx, logID, reused)
	if err != nil {
		return "", err
	}
	return msg, nil
}

// ensureTodayDailyLog returns the daily-log id for today, creating one only if
// none exists yet (idempotent). reused reports whether an existing draft was
// found. The construction.daily.log model is queried by the authenticated
// user's own records (record rule enforces per-user isolation).
func (h *Hook) ensureTodayDailyLog(ctx context.Context, date, note string, timesheets, todos, done int) (logID int, reused bool, err error) {
	// Look for an existing log for this date owned by the current user.
	var rows []struct {
		ID int `json:"id"`
	}
	searchErr := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "search_records", map[string]any{
		"model":  "construction.daily.log",
		"domain": [][]any{{"date", "=", date}},
		"fields": []string{"id"},
		"limit":  1,
	}, &rows)
	if searchErr == nil && len(rows) > 0 {
		return rows[0].ID, true, nil
	}
	// A search failure is non-fatal: fall through to create. create itself
	// is the safety net, and submit's already_submitted guard still applies.

	body := buildDailyLogBody(note, timesheets, todos, done)
	var res struct {
		DailyLogID int `json:"daily_log_id"`
	}
	if cerr := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "create_daily_log", map[string]any{
		"date":        date,
		"description": body,
	}, &res); cerr != nil {
		return 0, false, fmt.Errorf("create_daily_log: %w", cerr)
	}
	if res.DailyLogID == 0 {
		return 0, false, errors.New("create_daily_log returned no id")
	}
	return res.DailyLogID, false, nil
}

// buildDailyLogBody renders the autofill summary plus optional note into the
// daily log description (HTML allowed).
func buildDailyLogBody(note string, timesheets, todos, done int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "工時 %d 筆 / 待辦 %d 項 / 完成 %d 項", timesheets, todos, done)
	if n := strings.TrimSpace(note); n != "" {
		b.WriteString("<br/>")
		b.WriteString(n)
	}
	return b.String()
}

// submitDailyLog calls submit_daily_log and maps its structured error codes
// (already_submitted / access_denied / timesheet_required) to user-facing
// text. already_submitted is treated as success (idempotent re-send).
func (h *Hook) submitDailyLog(ctx context.Context, logID int, reused bool) (string, error) {
	var res struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Code    string `json:"code"`
	}
	err := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "submit_daily_log", map[string]any{
		"log_id": logID,
	}, &res)

	// The structured-error codes can come back either as an MCP-level error
	// payload or in the result body, depending on the tool. Inspect both.
	code := res.Code
	if code == "" && err != nil {
		code = classifyMCPErrorCode(err)
	}

	switch code {
	case "already_submitted":
		return "ℹ️ 今天的日報先前已送出，無需重複提交。", nil
	case "timesheet_required":
		return "⚠️ 今天尚未填寫工時，請先登記工時後再送出日報。", nil
	case "access_denied":
		return "⚠️ 沒有提交此日報的權限，請聯絡系統管理員。", nil
	}
	if err != nil {
		return "", fmt.Errorf("submit_daily_log (id=%d): %w", logID, err)
	}

	prefix := "✅ 日報已送出"
	if reused {
		prefix = "✅ 已沿用今天的日報草稿並送出"
	}
	return fmt.Sprintf("%s（#%d）。", prefix, logID), nil
}

// classifyMCPErrorCode best-effort extracts a known structured code from an
// MCP error message string.
func classifyMCPErrorCode(err error) string {
	msg := strings.ToLower(err.Error())
	for _, code := range []string{"already_submitted", "timesheet_required", "access_denied"} {
		if strings.Contains(msg, code) {
			return code
		}
	}
	return ""
}

// uploadPhotos uploads field photos for the daily-log flow via
// upload_field_photo. Kept to complete the contract's
// autofill→create→upload→submit shape; the v1 text entrypoint passes no
// photos (image-content callbacks are out of scope here). working_plan_id and
// semantic_key are required by the tool.
func (h *Hook) uploadPhotos(ctx context.Context, workingPlanID int, semanticKey map[string]any, photos []photoUpload) error {
	for _, p := range photos {
		var res struct {
			AttachmentID int `json:"attachment_id"`
		}
		if err := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "upload_field_photo", map[string]any{
			"working_plan_id": workingPlanID,
			"semantic_key":    semanticKey,
			"photo_base64":    p.Base64,
			"filename":        p.Filename,
		}, &res); err != nil {
			return fmt.Errorf("upload_field_photo %q: %w", p.Filename, err)
		}
	}
	return nil
}

// photoUpload is one base64 photo bound for upload_field_photo.
type photoUpload struct {
	Filename string
	Base64   string
}

// ----------------------------------------------------------------------------
// MCP transport (mirrors esmith-km/conversation.go's mcpToolCall)
// ----------------------------------------------------------------------------

type mcpRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int            `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *mcpError) Error() string {
	return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message)
}

// mcpToolCall posts a JSON-RPC tools/call to the Odoo MCP server and decodes
// the structuredContent (or text content) payload into dst.
//
// Package-level var so tests can stub it (original := mcpToolCall;
// mcpToolCall = func(...){...}). Endpoint + token come from Hook.cfg, keeping
// this function free of env reads.
var mcpToolCall = func(ctx context.Context, endpoint, token, tool string, args map[string]any, dst any) error {
	if endpoint == "" || token == "" {
		return errors.New("lineworks-workflow: MCP endpoint/token not configured (cfg.MCPURL / cfg.MCPToken)")
	}

	body := mcpRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      tool,
			"arguments": args,
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	reqCtx, cancel := context.WithTimeout(ctx, mcpRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("mcp http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mcp http %d", resp.StatusCode)
	}

	var envelope mcpResponse
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("mcp decode: %w", err)
	}
	if envelope.Error != nil {
		return envelope.Error
	}

	var wrapper struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(envelope.Result, &wrapper); err != nil {
		return fmt.Errorf("mcp result envelope: %w", err)
	}
	if len(wrapper.StructuredContent) > 0 && string(wrapper.StructuredContent) != "null" {
		return json.Unmarshal(wrapper.StructuredContent, dst)
	}
	if len(wrapper.Content) > 0 && wrapper.Content[0].Type == "text" {
		return json.Unmarshal([]byte(wrapper.Content[0].Text), dst)
	}
	return errors.New("mcp result has no usable payload")
}
