package line

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/line/line-bot-sdk-go/v7/linebot"
)

// handleEvent dispatches a single LINE webhook event.
func (c *Channel) handleEvent(event *linebot.Event) {
	// Postback events drive the meeting writeback Flex flow. They never
	// need policy filtering or sender bookkeeping — the conversation is
	// always anchored on a draft that already passed those checks at
	// AudioMessage time.
	if event.Type == linebot.EventTypePostback {
		c.handlePostback(event)
		return
	}
	if event.Type != linebot.EventTypeMessage {
		return
	}

	// Determine sender and chat IDs.
	var userID, chatID, peerKind string
	switch event.Source.Type {
	case linebot.EventSourceTypeUser:
		userID = event.Source.UserID
		chatID = event.Source.UserID
		peerKind = "direct"
	case linebot.EventSourceTypeGroup:
		userID = event.Source.UserID
		chatID = event.Source.GroupID
		peerKind = "group"
	case linebot.EventSourceTypeRoom:
		userID = event.Source.UserID
		chatID = event.Source.RoomID
		peerKind = "group"
	default:
		return
	}

	senderID := "line:" + userID

	// Policy check.
	if !c.CheckPolicy(peerKind, c.cfg.DMPolicy, c.cfg.GroupPolicy, senderID) {
		slog.Debug("LINE: message rejected by policy", "sender", senderID, "peerKind", peerKind)
		return
	}

	// Send loading animation (best-effort).
	go c.sendLoadingAnimation(chatID)

	// Cache reply token.
	c.replyTokens.Store(chatID, replyTokenEntry{
		token:      event.ReplyToken,
		receivedAt: time.Now(),
	})

	var text string
	var mediaFiles []string

	switch msg := event.Message.(type) {
	case *linebot.TextMessage:
		text = msg.Text
		// Process any GDrive shared links in the message body asynchronously.
		// The agent still sees the original text via HandleMessage below — a
		// user message like "請整理 https://drive..." should still get an
		// agent acknowledgment, with the actual file ingestion happening in
		// parallel. Successes are silent; failures reply via LINE.
		//
		// Per-URL dedup happens inside ingestGdriveLinks itself so the
		// agent path still receives the original text on resends — only
		// the file download is suppressed.
		go c.ingestGdriveLinks(msg.Text, userID, chatID)
	case *linebot.ImageMessage:
		path, err := c.downloadContent(msg.ID)
		if err != nil {
			slog.Error("LINE: failed to download image", "err", err)
			return
		}
		mediaFiles = append(mediaFiles, path)
	case *linebot.AudioMessage:
		// LINE webhook resend detection — same Message.ID arriving within
		// the dedup TTL means LINE retried because the first response was
		// slow. We MUST NOT re-ingest (would create a second draft) — push
		// a "已收到此會議錄音" reply so the user knows their original send
		// is still in flight.
		if c.dedup != nil && c.dedup.SeenOrMark(audioMessageKey(msg.ID)) {
			slog.Info("LINE: audio message resend detected, skipping ingest",
				"message_id", msg.ID, "chat", chatID)
			_ = c.sendChunks(chatID, []string{
				"⏳ 已收到此會議錄音，正在處理中。完成後會自動傳送選單請你補欄位。",
			})
			return
		}
		// Audio messages bypass the agent and go straight to the
		// km-meeting-pipeline inbox. The downstream cron handles ffmpeg
		// compression and nlm transcription independently.
		if err := c.ingestLineAudio(msg, userID, chatID); err != nil {
			slog.Error("LINE: failed to ingest audio", "err", err, "message_id", msg.ID)
		}
		return
	default:
		// Unsupported message type — ignore.
		return
	}

	metadata := map[string]string{
		"reply_token": event.ReplyToken,
	}

	c.HandleMessage(senderID, chatID, text, mediaFiles, metadata, peerKind)
}

// downloadContent downloads message content to a temp file and returns the path.
func (c *Channel) downloadContent(messageID string) (string, error) {
	resp, err := c.bot.GetMessageContent(messageID).Do()
	if err != nil {
		return "", fmt.Errorf("get message content: %w", err)
	}
	defer resp.Content.Close()

	tmpFile, err := os.CreateTemp("", "line-media-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer tmpFile.Close()

	if _, err := io.Copy(tmpFile, resp.Content); err != nil {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("write media: %w", err)
	}

	// Rename with proper extension based on content type.
	ext := ".jpg" // default
	if ct := resp.ContentType; ct != "" {
		switch {
		case ct == "image/png":
			ext = ".png"
		case ct == "image/gif":
			ext = ".gif"
		}
	}
	finalPath := tmpFile.Name() + ext
	if err := os.Rename(tmpFile.Name(), finalPath); err != nil {
		return tmpFile.Name(), nil // fallback to original name
	}
	return filepath.Clean(finalPath), nil
}

// sendLoadingAnimation sends a loading indicator to the chat via LINE API.
func (c *Channel) sendLoadingAnimation(chatID string) {
	body, _ := json.Marshal(map[string]interface{}{
		"chatId":         chatID,
		"loadingSeconds": loadingSeconds,
	})

	req, err := http.NewRequest(http.MethodPost, loadingAPIURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.ChannelAccessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Warn("LINE: loading animation request failed", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		slog.Warn("LINE: loading animation error", "status", resp.StatusCode, "body", string(respBody))
	}
}
