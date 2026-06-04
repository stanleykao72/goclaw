package lineworks

import (
	"context"
	"errors"
	"log/slog"

	lw "github.com/nextlevelbuilder/goclaw/internal/lineworks"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

// maxSendChunks bounds how many chunks a single outbound delivery will send,
// so a pathologically long agent response cannot spam a chat.
const maxSendChunks = 5

// botClient is the outbound surface the channel needs to deliver messages. The
// factory's clientAdapter bridges the SDK's *lineworks.Client to it (the SDK
// already exposes the two per-endpoint send methods; the adapter only adds
// InvalidateToken). Keeping it as a local interface decouples the channel from
// the concrete SDK type and makes the send paths unit-testable with a fake.
//
// Routing follows the Bot send API: a 1:1 reply goes to the user
// (POST /bots/{botId}/users/{userId}/messages) via SendTextToUser; a group/room
// reply goes to the channel (POST /bots/{botId}/channels/{channelId}/messages)
// via SendTextToChannel.
//
// InvalidateToken drops the cached access token so the next call re-mints one
// via the JWT-bearer flow — used by the auth-error retry below.
type botClient interface {
	SendTextToUser(ctx context.Context, userID, text string) error
	SendTextToChannel(ctx context.Context, channelID, text string) error
	InvalidateToken()
}

// Send delivers an outbound message to a LINE WORKS chat. It satisfies the
// channels.Channel Send contract (called by the bus dispatcher).
//
// The peer is recovered from msg.Metadata, populated on the inbound side by
// handleMessageEvent: metaPeerKind distinguishes group vs direct, and
// metaUserID / metaChannelID carry the resource ids. When metadata is absent
// (e.g. a plugin-initiated push that built the OutboundMessage from scratch),
// we fall back to treating msg.ChatID as a 1:1 userId — the common
// workflow-plugin case where 待辦/日報 replies go back to the requesting user.
func (c *Channel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	userID, channelID := c.routePeer(msg.ChatID, msg.Metadata)

	text := formatForLineWorks(msg.Content)
	if text == "" && len(msg.Media) == 0 {
		return nil
	}

	if text != "" {
		chunks := splitMessage(text, maxTextLength)
		if len(chunks) > maxSendChunks {
			chunks = chunks[:maxSendChunks]
		}
		for _, chunk := range chunks {
			if err := c.sendText(ctx, userID, channelID, chunk); err != nil {
				return err
			}
		}
	}

	// LINE WORKS v1 scope has no push-by-URL image primitive in this adapter
	// (the client exposes text sends only). Media attachments are surfaced as
	// a text line carrying the caption + URL so an agent reply is never
	// silently dropped. Rich-media upload is out of scope for v1.
	for _, m := range msg.Media {
		line := m.URL
		if m.Caption != "" {
			line = m.Caption + "\n" + m.URL
		}
		if err := c.sendText(ctx, userID, channelID, line); err != nil {
			slog.Error("LINEWORKS: failed to send media line", "chatID", msg.ChatID, "err", err)
		}
	}

	return nil
}

// sendText sends one text message with a single token-refresh retry. The
// content is routed to the user (1:1) or channel (group) endpoint based on
// whether channelID is set. If the first attempt fails with an auth error
// (expired/invalid/revoked access token), the cached token is invalidated and
// the send is retried exactly once.
func (c *Channel) sendText(ctx context.Context, userID, channelID, text string) error {
	if c.client == nil {
		// Nil client path mirrors line.sendChunks: unit tests that exercise
		// higher-level helpers without a real client get a silent no-op.
		return nil
	}

	send := func() error {
		if channelID != "" {
			return c.client.SendTextToChannel(ctx, channelID, text)
		}
		return c.client.SendTextToUser(ctx, userID, text)
	}

	err := send()
	if err != nil && isAuthError(err) {
		slog.Warn("LINEWORKS: send got auth error, refreshing token and retrying once", "err", err)
		c.client.InvalidateToken()
		err = send()
	}
	if err != nil {
		slog.Error("LINEWORKS: send text failed", "userID", userID, "channelID", channelID, "err", err)
		return err
	}
	return nil
}

// isAuthError reports whether err is a token-expiry / unauthorized error from
// the lineworks client, signalling a single token-refresh retry is worthwhile.
func isAuthError(err error) bool {
	return errors.Is(err, lw.ErrUnauthorized)
}

// routePeer resolves a stored chat key into the (userID, channelID) pair the
// Bot send API expects. Inbound metadata is authoritative; when absent we fall
// back to treating the key as a 1:1 userId.
func (c *Channel) routePeer(chatID string, metadata map[string]string) (userID, channelID string) {
	if metadata != nil {
		if metadata[metaPeerKind] == peerGroup {
			cid := metadata[metaChannelID]
			if cid == "" {
				cid = chatID
			}
			return "", cid
		}
		if uid := metadata[metaUserID]; uid != "" {
			return uid, ""
		}
	}
	return chatID, ""
}
