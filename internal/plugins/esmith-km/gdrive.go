package esmithkm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

// gdriveURLRegex finds GDrive shared file URLs anywhere in a text body.
// Captures everything from "http(s)://drive.google.com/file/d/{id}" up to
// the next whitespace, so query strings like ?usp=drivesdk are included
// in the URL passed to the script.
//
// The script does its own file_id extraction; we forward the full URL so
// any user-facing error message can quote what they actually pasted.
var gdriveURLRegex = regexp.MustCompile(
	`https?://drive\.google\.com/file/d/[A-Za-z0-9_-]+(?:[/?][^\s]*)?`,
)

// extractGdriveURLs returns all GDrive URLs found in a message body.
// Returns nil (not empty slice) when none are found so callers can
// early-return cheaply on the common no-link case.
func extractGdriveURLs(text string) []string {
	if text == "" {
		return nil
	}
	return gdriveURLRegex.FindAllString(text, -1)
}

// ingestGdriveResult mirrors the JSON shape that km-meeting-pipeline.sh
// emits on stdout. Both success and failure are JSON; only the populated
// fields differ.
//
// Success:  {"status":"ok","file":"...","method":"...","size":N,"file_id":"..."}
// Failure:  {"error":"<type>","file_id":"...","message":"..."}
type ingestGdriveResult struct {
	Status  string `json:"status,omitempty"`
	Error   string `json:"error,omitempty"`
	FileID  string `json:"file_id,omitempty"`
	Message string `json:"message,omitempty"`
	File    string `json:"file,omitempty"`
	Method  string `json:"method,omitempty"`
	Size    int64  `json:"size,omitempty"`
}

// gdriveErrorMessages maps error types from km-meeting-pipeline.sh to
// user-facing Traditional Chinese reply text. Unknown error types fall
// through to a generic message in replyGdriveError.
var gdriveErrorMessages = map[string]string{
	"invalid_url":     "無法解析這個 GDrive 連結，請貼完整的 https://drive.google.com/file/d/... 連結",
	"not_accessible":  "無法存取此連結。請確認檔案已分享給 stanleykao72@gmail.com，或將分享權限改為「知道連結的人皆可檢視」",
	"download_failed": "下載失敗，請稍後再試或聯絡管理員",
}

// ingestGdriveLinks processes every GDrive URL in a LINE message body.
// Each URL is handed to km-meeting-pipeline.sh ingest-gdrive in sequence
// (not in parallel — the script does I/O-bound rclone/gdown work and
// running them concurrently buys little). On failure, replies to the
// user with an actionable Chinese message. Successes are deliberately
// silent (per design D Open Q1 — avoid LINE notification spam).
//
// On success, the LINE provenance (user_id, chat_id) is patched into
// the .source.json sidecar that bash wrote, so the eventual draft has
// the line_chat_id needed for the Phase 3b project_picker push.
//
// Safe to call as `go c.ingestGdriveLinks(...)` from the webhook handler;
// the LINE event return path is not blocked by ingestion latency.
//
// Note on reply token race: the agent's HandleMessage path will also try
// to use the cached reply token. Whichever finishes first wins; the loser
// silently falls back to PushMessage. Both messages reach the user.
func (h *Hook) ingestGdriveLinks(text, userID, chatID string) {
	urls := extractGdriveURLs(text)
	if len(urls) == 0 {
		return
	}

	scriptPath := h.cfg.PipelineScript

	for _, url := range urls {
		// LINE webhook resend → same URL within dedup TTL means LINE
		// retried the same TextMessage. Suppress the second ingest so we
		// don't double-download or create a second draft. The agent path
		// (HandleMessage in handleEvent) still sees the original text.
		if h.dedup != nil && h.dedup.SeenOrMark(gdriveURLKey(url)) {
			slog.Info("LINE: GDrive URL resend detected, skipping ingest",
				"url", url, "chat", chatID)
			if h.cfg.Sender != nil {
				_ = h.cfg.Sender.SendChunks(chatID, []string{
					"⏳ 已收到此 GDrive 連結，正在處理中。完成後會自動傳送選單請你補欄位。",
				})
			}
			continue
		}
		slog.Info("LINE: ingesting GDrive link", "url", url, "chat", chatID)
		result, err := runIngestGdrive(scriptPath, url)
		if err != nil {
			slog.Error("LINE: ingest-gdrive runner error",
				"err", err, "url", url, "chat", chatID)
			h.replyGdriveError(chatID, "download_failed", err.Error())
			continue
		}
		if result.Status == "ok" {
			slog.Info("LINE: GDrive ingest success",
				"file", result.File,
				"method", result.Method,
				"size", result.Size,
				"chat", chatID,
			)
			if perr := h.patchSidecarWithLineContext(result.File, userID, chatID, url); perr != nil {
				slog.Warn("LINE: sidecar patch failed",
					"err", perr, "file", result.File, "chat", chatID)
			}
			continue
		}
		// Non-ok result: result.Error is one of invalid_url /
		// not_accessible / download_failed.
		slog.Warn("LINE: GDrive ingest failed",
			"error", result.Error,
			"message", result.Message,
			"file_id", result.FileID,
			"chat", chatID,
		)
		h.replyGdriveError(chatID, result.Error, result.Message)
	}
}

// patchSidecarWithLineContext adds line_user_id / line_chat_id (and the
// original_url so backfill audits can trace) to the bash-written sidecar.
// The bash side writes an initial sidecar with source_type=gdrive but
// without LINE provenance — we merge in the missing fields here so the
// downstream publish-odoo init builds a draft with line_chat_id set,
// which is what the Phase 3b draft watcher needs to push the picker.
//
// `file` is the basename inside meetingsInboxDir; we resolve the
// matching .source.json next to it.
func (h *Hook) patchSidecarWithLineContext(file, userID, chatID, originalURL string) error {
	if file == "" {
		return nil
	}
	sidecarPath := filepath.Join(h.cfg.InboxDir, file+".source.json")
	data, err := os.ReadFile(sidecarPath)
	if err != nil {
		// No bash-written sidecar — write one fresh so downstream still works.
		fresh := map[string]any{
			"source_type":   "gdrive",
			"line_user_id":  userID,
			"line_chat_id":  chatID,
			"original_url":  originalURL,
			"received_at":   time.Now().UTC().Format(time.RFC3339),
		}
		raw, _ := json.MarshalIndent(fresh, "", "  ")
		return os.WriteFile(sidecarPath, raw, 0o644)
	}

	var existing map[string]any
	if err := json.Unmarshal(data, &existing); err != nil {
		return fmt.Errorf("parse existing sidecar: %w", err)
	}
	if existing == nil {
		existing = map[string]any{}
	}
	existing["line_user_id"] = userID
	existing["line_chat_id"] = chatID
	if _, ok := existing["original_url"]; !ok {
		existing["original_url"] = originalURL
	}

	raw, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(sidecarPath, raw, 0o644)
}

// runIngestGdrive executes the meeting-pipeline script and parses its
// stdout JSON. The script's contract is "always emit JSON on stdout, even
// on exit code 1", so a non-zero exit is not necessarily a runner error.
func runIngestGdrive(scriptPath, url string) (*ingestGdriveResult, error) {
	cmd := exec.Command("bash", scriptPath, "ingest-gdrive", url)
	out, runErr := cmd.Output()

	// Even on exit 1, the script should have written JSON to stdout.
	// Only when stdout is empty do we treat runErr as authoritative.
	if len(bytes.TrimSpace(out)) == 0 {
		if runErr != nil {
			var ee *exec.ExitError
			if errors.As(runErr, &ee) {
				return nil, fmt.Errorf("script exited %d with no stdout (stderr: %s)",
					ee.ExitCode(), string(ee.Stderr))
			}
			return nil, fmt.Errorf("script execution failed: %w", runErr)
		}
		return nil, errors.New("script returned empty stdout")
	}

	var result ingestGdriveResult
	if jerr := json.Unmarshal(bytes.TrimSpace(out), &result); jerr != nil {
		return nil, fmt.Errorf("parse stdout JSON: %w (raw: %q)", jerr, string(out))
	}
	return &result, nil
}

// replyGdriveError sends a single LINE message explaining the failure.
// Uses the same reply-token-then-push fallback as the main agent send
// path via sendChunks.
func (h *Hook) replyGdriveError(chatID, errType, detail string) {
	msg, ok := gdriveErrorMessages[errType]
	if !ok {
		msg = "GDrive 連結處理失敗：" + errType
	}
	// For generic download failures, append a short technical detail so
	// the user has something to forward to maintainers. Cap length to
	// keep the LINE bubble readable.
	if errType == "download_failed" && detail != "" && len(detail) < 200 {
		msg = msg + "\n（技術細節：" + detail + "）"
	}

	if h.cfg.Sender == nil {
		slog.Warn("LINE: no sender configured, skipping GDrive error reply",
			"chat", chatID, "error_type", errType)
		return
	}
	if err := h.cfg.Sender.SendChunks(chatID, []string{msg}); err != nil {
		slog.Error("LINE: failed to send GDrive error reply",
			"err", err, "chat", chatID, "error_type", errType)
	}
}
