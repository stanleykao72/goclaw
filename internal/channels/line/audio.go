package line

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/line/line-bot-sdk-go/v7/linebot"
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

// downloadAudioContent fetches a LINE audio message body to a temp file and
// returns the file path plus the response Content-Type. The caller is
// responsible for moving / renaming the file out of the temp area.
func (c *Channel) downloadAudioContent(messageID string) (string, string, error) {
	resp, err := c.bot.GetMessageContent(messageID).Do()
	if err != nil {
		return "", "", fmt.Errorf("get audio content: %w", err)
	}
	defer resp.Content.Close()

	tmp, err := os.CreateTemp("", "line-audio-*")
	if err != nil {
		return "", "", fmt.Errorf("create temp file: %w", err)
	}

	if _, err := io.Copy(tmp, resp.Content); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", "", fmt.Errorf("write audio: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", "", fmt.Errorf("close temp file: %w", err)
	}

	return tmp.Name(), resp.ContentType, nil
}

// ingestLineAudio downloads an AudioMessage, names it per the meeting-pipeline
// convention, and moves it into /data/km/meetings/inbox/. A sidecar JSON is
// written alongside so the downstream cron knows the LINE provenance.
//
// This function deliberately does NOT call HandleMessage — audio messages have
// no text content for the GoClaw agent and would just confuse it. The
// downstream km-meeting-pipeline.sh cron will pick up the file and run it
// through ffmpeg compression + nlm transcription independently.
func (c *Channel) ingestLineAudio(msg *linebot.AudioMessage, userID, chatID string) error {
	tmpPath, contentType, err := c.downloadAudioContent(msg.ID)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	ext, known := extensionForAudio(contentType)
	if !known {
		slog.Warn("LINE audio: unknown content type, falling back to .bin",
			"content_type", contentType, "message_id", msg.ID)
	}

	now := time.Now()
	fileName := fmt.Sprintf("%s_%s_line_%s%s",
		now.Format("20060102"),
		now.Format("150405"),
		senderShort(userID),
		ext,
	)
	finalPath := filepath.Join(meetingsInboxDir, fileName)

	if err := os.MkdirAll(meetingsInboxDir, 0o755); err != nil {
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
		LineMessageID:       msg.ID,
		OriginalContentType: contentType,
		ReceivedAt:          now.UTC().Format(time.RFC3339),
	}
	sidecarPath := filepath.Join(meetingsInboxDir, fileName+".source.json")
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
