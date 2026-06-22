package channels

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// WebhookRoute holds a path and handler pair for mounting on the main gateway mux.
type WebhookRoute struct {
	Path    string
	Handler http.Handler
}

// dispatchOutbound consumes outbound messages from the bus and routes them
// to the appropriate channel. Internal channels are silently skipped.
func (m *Manager) dispatchOutbound(ctx context.Context) {
	slog.Info("outbound dispatcher started")

	for {
		select {
		case <-ctx.Done():
			slog.Info("outbound dispatcher stopped")
			return
		default:
			msg, ok := m.bus.SubscribeOutbound(ctx)
			if !ok {
				continue
			}

			// Skip internal channels
			if IsInternalChannel(msg.Channel) {
				continue
			}

			m.mu.RLock()
			channel, exists := m.channels[msg.Channel]
			m.mu.RUnlock()

			if !exists {
				slog.Warn("unknown channel for outbound message", "channel", msg.Channel)
				continue
			}

			// Filter out temp media files that no longer exist (already sent by another dispatch).
			if len(msg.Media) > 0 {
				tmpDir := os.TempDir()
				filtered := msg.Media[:0]
				for _, media := range msg.Media {
					if media.URL != "" && strings.HasPrefix(media.URL, tmpDir) {
						if _, err := os.Stat(media.URL); err != nil {
							slog.Debug("skipping already-delivered temp media", "path", media.URL)
							continue
						}
					}
					filtered = append(filtered, media)
				}
				msg.Media = filtered
				// If only media was in this message and all files are gone, skip entirely.
				if len(msg.Media) == 0 && msg.Content == "" {
					continue
				}
			}

			// Add tenant context for per-tenant TTS auto-apply
			sendCtx := ctx
			if msg.TenantID != uuid.Nil {
				sendCtx = store.WithTenantID(ctx, msg.TenantID)
			}

			// Add agent audio context for per-agent TTS voice override
			if msg.AgentID != uuid.Nil && len(msg.AgentOtherConfig) > 0 {
				sendCtx = store.WithAgentAudio(sendCtx, store.AgentAudioSnapshot{
					AgentID:     msg.AgentID,
					OtherConfig: msg.AgentOtherConfig,
				})
			}

			if err := channel.Send(sendCtx, msg); err != nil {
				slog.Error("error sending message to channel",
					"channel", msg.Channel,
					"chat_id", msg.ChatID,
					"content_len", len(msg.Content),
					"content_preview", Truncate(msg.Content, 160),
					"error", err,
				)
				// Try to send a text-only error notification back to the chat.
				// Only for media failures — text-only failures likely mean the chat
				// is inaccessible (kicked, blocked, etc.) so retrying won't help.
				if len(msg.Media) > 0 {
					notifyMsg := bus.OutboundMessage{
						Channel:  msg.Channel,
						ChatID:   msg.ChatID,
						Content:  formatChannelSendError(err),
						Metadata: sendErrorMeta(msg.Metadata),
						TenantID: msg.TenantID,
					}
					if err2 := channel.Send(sendCtx, notifyMsg); err2 != nil {
						slog.Warn("failed to send error notification",
							"channel", msg.Channel, "error", err2)
					}
				}
			}

			// Clean up temp media files only. Workspace-generated files are preserved
			// so they remain accessible via workspace/web UI after delivery.
			tmpDir := os.TempDir()
			for _, media := range msg.Media {
				if media.URL != "" && strings.HasPrefix(media.URL, tmpDir) {
					if err := os.Remove(media.URL); err != nil {
						slog.Debug("failed to clean up media file", "path", media.URL, "error", err)
					}
				}
			}
		}
	}
}

// lineWorksWebhookChannel is the subset of a LINE WORKS channel the shared
// webhook dispatcher needs. It is type-asserted from the live channel registry
// at request time so newly hot-loaded bots participate without a remount.
//
// The interface is defined HERE (package channels), not by importing the
// concrete internal/channels/lineworks package, precisely to avoid an import
// cycle: lineworks already imports channels, so channels must NOT import
// lineworks. Any channel exposing VerifyAndHandle (today only
// *lineworks.Channel) is treated as a LINE WORKS webhook bot.
type lineWorksWebhookChannel interface {
	Channel
	// VerifyAndHandle reports whether this bot's secret verifies sig over
	// rawBody; on a match it dispatches the callback asynchronously and returns
	// true. On a non-match it returns false with no side effects.
	VerifyAndHandle(rawBody []byte, sig string) bool
}

// WebhookHandlers returns all webhook handlers from channels that implement WebhookChannel.
// Used to mount webhook routes on the main gateway mux.
//
// LINE WORKS channels are intentionally EXCLUDED here: their callback payload
// carries no bot id, so multiple bots must share a single path demuxed by HMAC.
// The gateway mounts that one shared handler via LineWorksWebhookDispatcher
// instead; emitting a per-instance route here would register the same path
// (/webhook/lineworks) more than once and PANIC the stdlib ServeMux. Every
// other webhook channel type (Feishu, Facebook, pancake, bitrix24, …) keeps its
// own distinct path and flows through this loop unchanged.
func (m *Manager) WebhookHandlers() []WebhookRoute {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var routes []WebhookRoute
	for _, ch := range m.channels {
		// Skip LINE WORKS bots — handled by the dedicated shared dispatcher.
		if _, isLW := ch.(lineWorksWebhookChannel); isLW {
			continue
		}
		if wh, ok := ch.(WebhookChannel); ok {
			if path, handler := wh.WebhookHandler(); path != "" && handler != nil {
				routes = append(routes, WebhookRoute{Path: path, Handler: handler})
			}
		}
	}
	return routes
}

// LineWorksWebhookDispatcher returns the single shared webhook handler for ALL
// LINE WORKS bots in this process, to be mounted ONCE on the gateway mux at
// "/webhook/lineworks". ok is false when no LINE WORKS channel is registered
// (so the caller mounts nothing).
//
// Demux-by-HMAC: the LINE WORKS callback body has no bot id, so the dispatcher
// identifies the target bot as "the first registered LINE WORKS channel whose
// bot secret verifies the X-WORKS-Signature over the raw body". The candidate
// set is collected from the live registry under RLock ON EACH REQUEST, so a bot
// hot-loaded after startup is seen immediately — the path is mounted exactly
// once, never remounted, which is what keeps the stdlib ServeMux from panicking
// regardless of how many bots exist.
//
// Security: the raw body is verified (constant-time HMAC, via VerifyAndHandle →
// lineworks.VerifySignature) BEFORE any bot parses or trusts it; a body that no
// secret verifies is never dispatched. The handler always replies 200 on the
// success path before heavy work (the dispatch is async inside VerifyAndHandle),
// preserving LINE WORKS retry-storm avoidance; 401 when no candidate matches.
// Secrets and raw bodies are never logged.
func (m *Manager) LineWorksWebhookDispatcher() (path string, handler http.Handler, ok bool) {
	// Presence check only — the candidate set is re-collected per request so
	// hot-loaded bots are picked up without a remount.
	m.mu.RLock()
	hasAny := false
	for _, ch := range m.channels {
		if _, isLW := ch.(lineWorksWebhookChannel); isLW {
			hasAny = true
			break
		}
	}
	m.mu.RUnlock()
	if !hasAny {
		return "", nil, false
	}

	const lwWebhookPath = "/webhook/lineworks"
	const lwSignatureHeader = "X-WORKS-Signature"

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Error("LINEWORKS webhook: read body failed", "err", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sig := r.Header.Get(lwSignatureHeader)

		// Collect the LIVE set of LINE WORKS bots at REQUEST time so hot-loaded
		// bots participate without remounting the path.
		m.mu.RLock()
		candidates := make([]lineWorksWebhookChannel, 0, 4)
		for _, ch := range m.channels {
			if lwch, isLW := ch.(lineWorksWebhookChannel); isLW {
				candidates = append(candidates, lwch)
			}
		}
		m.mu.RUnlock()

		// First bot whose secret verifies the HMAC owns this callback. On a
		// match VerifyAndHandle dispatches asynchronously; reply 200 immediately.
		for _, lwch := range candidates {
			if lwch.VerifyAndHandle(rawBody, sig) {
				w.WriteHeader(http.StatusOK)
				return
			}
		}

		slog.Warn("LINEWORKS webhook: invalid signature: no matching lineworks bot",
			"candidates", len(candidates))
		w.WriteHeader(http.StatusUnauthorized)
	})

	return lwWebhookPath, h, true
}

// SendToChannel delivers a message to a specific channel by name.
func (m *Manager) SendToChannel(ctx context.Context, channelName, chatID, content string) error {
	m.mu.RLock()
	channel, exists := m.channels[channelName]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("channel %s not found", channelName)
	}

	msg := bus.OutboundMessage{
		Channel: channelName,
		ChatID:  chatID,
		Content: content,
	}

	return channel.Send(ctx, msg)
}

// SendMediaToChannel delivers a message with media attachments to a specific channel by name.
// media must be non-empty; use SendToChannel for text-only messages.
// Returns ErrMediaUnsupported if the channel type does not support media.
func (m *Manager) SendMediaToChannel(ctx context.Context, channelName, chatID, content string, media []bus.MediaAttachment) error {
	if len(media) == 0 {
		return fmt.Errorf("SendMediaToChannel: media slice must not be empty; use SendToChannel for text-only messages")
	}

	m.mu.RLock()
	channel, exists := m.channels[channelName]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("channel %s not found", channelName)
	}

	if !IsMediaCapable(channel.Type()) {
		return fmt.Errorf("%w: %s (%s)", ErrMediaUnsupported, channelName, channel.Type())
	}

	msg := bus.OutboundMessage{
		Channel: channelName,
		ChatID:  chatID,
		Content: content,
		Media:   media,
	}

	return channel.Send(ctx, msg)
}

// --- Send error notification helpers ---

// telegramAPIDescRe extracts the human-readable description from Telegram Bot API errors.
// Example: `telego: sendPhoto: api: 400 "Bad Request: not enough rights to send photos to the chat"`
//
//	→ "not enough rights to send photos to the chat"
var telegramAPIDescRe = regexp.MustCompile(`"Bad Request:\s*(.+?)"`)

// formatChannelSendError converts a channel.Send error into a user-friendly message.
// Never exposes raw library/HTTP details.
func formatChannelSendError(err error) string {
	raw := err.Error()
	lower := strings.ToLower(raw)

	// Telegram "Bad Request: <description>" — extract description
	if m := telegramAPIDescRe.FindStringSubmatch(raw); len(m) == 2 {
		return fmt.Sprintf("⚠️ Send failed: %s", m[1])
	}

	// Common Telegram API errors (non-Bad Request)
	switch {
	case strings.Contains(lower, "not enough rights"):
		return "⚠️ Send failed: bot doesn't have permission to send this type of message."
	case strings.Contains(lower, "chat not found"):
		return "⚠️ Send failed: chat not found."
	case strings.Contains(lower, "bot was blocked"):
		return "⚠️ Send failed: bot was blocked by the user."
	case strings.Contains(lower, "user is deactivated"):
		return "⚠️ Send failed: user account is deactivated."
	case strings.Contains(lower, "too many requests") || strings.Contains(lower, "flood"):
		return "⚠️ Send failed: rate limited by Telegram. Please try again later."
	case strings.Contains(lower, "file is too big") || strings.Contains(lower, "wrong file"):
		return "⚠️ Send failed: file is too large or invalid for Telegram."
	}

	// Generic fallback — don't expose internals
	return "⚠️ Failed to deliver message. Check bot logs for details."
}

// sendErrorMeta copies only the routing fields from outbound metadata.
// Strips reply_to_message_id, placeholder_key, audio_as_voice, etc.
// that could cause unintended side effects on the error notification.
func sendErrorMeta(orig map[string]string) map[string]string {
	if orig == nil {
		return nil
	}
	meta := make(map[string]string)
	for _, k := range []string{"local_key", "message_thread_id"} {
		if v := orig[k]; v != "" {
			meta[k] = v
		}
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}
