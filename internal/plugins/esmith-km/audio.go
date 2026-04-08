package esmithkm

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// audioContentTypeToExt maps LINE audio Content-Type headers to file extensions.
// Unknown types fall back to ".bin"; the km-meeting-pipeline.sh upload cron
// will skip non-audio extensions, so unknown files do not enter the pipeline.
var audioContentTypeToExt = map[string]string{
	"audio/m4a":   ".m4a",
	"audio/x-m4a": ".m4a", // LINE iOS voice messages use this MIME type
	"audio/mp4":   ".m4a",
	"audio/aac":   ".aac",
	"audio/x-aac": ".aac",
	"audio/mp3":   ".mp3",
	"audio/mpeg":  ".mp3",
	"audio/wav":   ".wav",
	"audio/wave":  ".wav",
	"audio/x-wav": ".wav",
	"audio/ogg":   ".ogg",
	"audio/opus":  ".opus",
}

// extensionForAudio returns a file extension for the given audio Content-Type.
// Returns ".bin" and false for unknown types so the caller can log a warning.
func extensionForAudio(contentType string) (string, bool) {
	if ext, ok := audioContentTypeToExt[contentType]; ok {
		return ext, true
	}
	return ".bin", false
}

// senderShort returns up to 8 leading chars from the LINE user ID.
// Falls back to "unknown" if userID is empty (group messages where the
// sender ID is unavailable).
func senderShort(userID string) string {
	if userID == "" {
		return "unknown"
	}
	if len(userID) <= 8 {
		return userID
	}
	return userID[:8]
}

// audioSidecar is the JSON written next to each ingested audio file so the
// km pipeline can preserve LINE provenance through the rest of the pipeline.
type audioSidecar struct {
	SourceType          string `json:"source_type"`
	LineUserID          string `json:"line_user_id"`
	LineChatID          string `json:"line_chat_id"`
	LineMessageID       string `json:"line_message_id"`
	OriginalContentType string `json:"original_content_type"`
	ReceivedAt          string `json:"received_at"`
}

// ingestAudio moves a pre-downloaded LINE audio file from its tmp path to
// the configured inbox, renaming it per the km-meeting-pipeline convention,
// and writes a sidecar JSON so the downstream cron knows the LINE provenance.
//
// The channel adapter is responsible for downloading the file BEFORE calling
// OnAudio — the plugin just takes ownership of the tmp path. This keeps the
// channel package free of any km-specific knowledge.
//
// This function deliberately does NOT call HandleMessage — audio messages have
// no text content for the GoClaw agent and would just confuse it. The
// downstream km-meeting-pipeline.sh cron will pick up the file and run it
// through ffmpeg compression + nlm transcription independently.
func (h *Hook) ingestAudio(tmpPath, contentType, messageID, userID, chatID string) error {
	if tmpPath == "" {
		return fmt.Errorf("empty tmp path")
	}

	ext, known := extensionForAudio(contentType)
	if !known {
		slog.Warn("LINE audio: unknown content type, falling back to .bin",
			"content_type", contentType, "message_id", messageID)
	}

	now := time.Now()
	fileName := fmt.Sprintf("%s_%s_line_%s%s",
		now.Format("20060102"),
		now.Format("150405"),
		senderShort(userID),
		ext,
	)
	finalPath := filepath.Join(h.cfg.InboxDir, fileName)

	if err := os.MkdirAll(h.cfg.InboxDir, 0o755); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("ensure inbox dir: %w", err)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		// Cross-device rename can fail (e.g. /tmp on tmpfs, /data on disk).
		// Fall back to copy + remove.
		if cerr := copyFile(tmpPath, finalPath); cerr != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("move to inbox: %w", cerr)
		}
		os.Remove(tmpPath)
	}

	sidecar := audioSidecar{
		SourceType:          "line_audio",
		LineUserID:          userID,
		LineChatID:          chatID,
		LineMessageID:       messageID,
		OriginalContentType: contentType,
		ReceivedAt:          now.UTC().Format(time.RFC3339),
	}
	sidecarPath := filepath.Join(h.cfg.InboxDir, fileName+".source.json")
	if data, jerr := json.MarshalIndent(sidecar, "", "  "); jerr == nil {
		if werr := os.WriteFile(sidecarPath, data, 0o644); werr != nil {
			slog.Warn("LINE audio: failed to write sidecar",
				"err", werr, "path", sidecarPath)
		}
	}

	slog.Info("LINE audio: ingested",
		"file", fileName,
		"content_type", contentType,
		"sender", senderShort(userID),
		"chat", chatID,
	)
	return nil
}

// copyFile is a fallback for cross-device rename failures.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
