package lineworks

import (
	"context"
	"encoding/json"
	"log/slog"
)

// Callback event type discriminators (the top-level "type" field of a callback
// body). Verified against developers.worksmobile.com/jp/docs/bot-callback. The
// eight types: message, postback (content-bearing); join, leave, joined, left
// (group lifecycle); begin, end (1:1 lifecycle).
const (
	eventTypeMessage  = "message"
	eventTypePostback = "postback"
	eventTypeJoin     = "join"
	eventTypeLeave    = "leave"
	eventTypeJoined   = "joined"
	eventTypeLeft     = "left"
	eventTypeBegin    = "begin"
	eventTypeEnd      = "end"
)

// callbackSource is the "source" object common to every callback event.
// ChannelID is empty for 1:1 conversations and set to the room/group id for
// group conversations. UserID is always the sender's resource id.
type callbackSource struct {
	UserID    string `json:"userId"`
	ChannelID string `json:"channelId"`
	DomainID  int    `json:"domainId"`
}

// callbackContent is the "content" object on message events.
type callbackContent struct {
	Type string `json:"type"` // "text", "image", "sticker", ...
	Text string `json:"text"`
}

// callbackEvent is the parsed LINE WORKS callback envelope. Postback "data" is
// TOP-LEVEL (not under content), matching the verified callback contract.
type callbackEvent struct {
	Type       string          `json:"type"`
	Source     callbackSource  `json:"source"`
	IssuedTime string          `json:"issuedTime"`
	Content    callbackContent `json:"content"`
	Data       string          `json:"data"`
}

// handleCallback parses a verified raw callback body and dispatches the single
// event it carries. Called by WebhookHandler (channel.go) AFTER the
// X-WORKS-Signature HMAC has been validated against the bot secret, so the
// body here is trusted. Parse errors are logged and swallowed — LINE WORKS
// expects HTTP 200 regardless, so a malformed event must not trigger a retry
// storm.
func (c *Channel) handleCallback(rawBody []byte) {
	var ev callbackEvent
	if err := json.Unmarshal(rawBody, &ev); err != nil {
		slog.Error("LINEWORKS: callback parse error", "err", err)
		return
	}
	c.handleEvent(ev)
}

// handleEvent dispatches a single parsed LINE WORKS callback event. The eight
// callback types are: message, postback (content-bearing); join, leave,
// joined, left (group lifecycle); begin, end (1:1 lifecycle).
func (c *Channel) handleEvent(ev callbackEvent) {
	switch ev.Type {
	case eventTypePostback:
		c.handlePostback(ev)
	case eventTypeMessage:
		c.handleMessageEvent(ev)
	case eventTypeJoin, eventTypeJoined, eventTypeLeave, eventTypeLeft,
		eventTypeBegin, eventTypeEnd:
		// Lifecycle events. v1 logs and does not act — no auto-greeting and
		// no membership bookkeeping in scope. Recorded at debug so production
		// can confirm delivery.
		slog.Debug("LINEWORKS: lifecycle event",
			"type", ev.Type,
			"userID", ev.Source.UserID,
			"channelID", ev.Source.ChannelID,
		)
	default:
		slog.Debug("LINEWORKS: ignoring unknown callback event", "type", ev.Type)
	}
}

// peerOf returns the (chatID, peerKind) for an event source. A non-empty
// channelId means a group/room conversation keyed by the channel id; empty
// means a 1:1 chat keyed by the user's resource id.
func peerOf(src callbackSource) (chatID, peerKind string) {
	if src.ChannelID != "" {
		return src.ChannelID, peerGroup
	}
	return src.UserID, peerDirect
}

// handlePostback fans a postback event out to every registered hook. The
// channel itself has no opinion on postback payloads — workflow plugins
// interpret ev.Data (TOP-LEVEL per the callback contract) and drive their own
// state machine (待辦/日報 flow).
func (c *Channel) handlePostback(ev callbackEvent) {
	chatID, _ := peerOf(ev.Source)
	c.fanOutPostback(PostbackEvent{
		UserID:    ev.Source.UserID,
		ChatID:    chatID,
		ChannelID: ev.Source.ChannelID,
		Data:      ev.Data,
	})
}

// handleMessageEvent processes an inbound message: policy gate, hook fan-out
// for text, then forward to the bus via HandleMessage for the agent path.
func (c *Channel) handleMessageEvent(ev callbackEvent) {
	chatID, peerKind := peerOf(ev.Source)
	senderID := senderPrefix + ev.Source.UserID

	// Policy check — same gate as the line channel (DM allowlist / group
	// policy). senderID carries the "lineworks:" prefix so allow_from entries
	// are namespaced and cannot collide with "line:" senders.
	if !c.CheckPolicy(peerKind, c.cfg.DMPolicy, c.cfg.GroupPolicy, senderID) {
		slog.Debug("LINEWORKS: message rejected by policy", "sender", senderID, "peerKind", peerKind)
		return
	}

	// Only text content is in v1 scope. Non-text (image/sticker/file/location)
	// is acknowledged-and-ignored.
	if ev.Content.Type != contentTypeText {
		slog.Debug("LINEWORKS: ignoring non-text message", "contentType", ev.Content.Type, "sender", senderID)
		return
	}

	text := ev.Content.Text

	// Gate: a synchronous access check that runs before plugins and the agent.
	// A deny blocks the message entirely and (optionally) replies a hint, so an
	// unrecognized sender reaches neither the workflow hooks nor the agent.
	if c.gate != nil {
		gateEv := TextEvent{UserID: ev.Source.UserID, ChatID: chatID, ChannelID: ev.Source.ChannelID, Text: text}
		allow, reply := c.gate.Gate(c.hookContext(), gateEv)
		// A reply (block hint, DM-bind prompt, or bind-success confirmation) is
		// sent whenever present — including when allow=true (success → still
		// proceeds to the agent).
		if reply != "" {
			if err := c.SendText(c.hookContext(), ev.Source.UserID, ev.Source.ChannelID, reply); err != nil {
				slog.Error("LINEWORKS: gate reply send failed", "sender", senderID, "err", err)
			}
		}
		if !allow {
			slog.Info("LINEWORKS: message blocked by gate", "sender", senderID, "peerKind", peerKind)
			return
		}
	}

	// Fan out to hooks (workflow plugin) in parallel with the agent path,
	// mirroring line.handleEvent. The hook event carries UserID + ChannelID so
	// the hook can reply to the right peer (1:1 vs group) without re-deriving.
	c.fanOutText(TextEvent{
		UserID:    ev.Source.UserID,
		ChatID:    chatID,
		ChannelID: ev.Source.ChannelID,
		Text:      text,
	})

	// Metadata carries peer routing so Send() can address the reply to the
	// correct Bot endpoint (users/ vs channels/).
	metadata := map[string]string{
		metaPeerKind:  peerKind,
		metaUserID:    ev.Source.UserID,
		metaChannelID: ev.Source.ChannelID,
	}

	c.HandleMessage(senderID, chatID, text, nil, metadata, peerKind)
}

// hookContext returns c.hookCtx or context.Background() if the channel has not
// been Start'd yet — mirrors line.hookContext so fan-out helpers work in unit
// tests that bypass Start.
func (c *Channel) hookContext() context.Context {
	if c.hookCtx != nil {
		return c.hookCtx
	}
	return context.Background()
}

// fanOutText delivers a TextEvent to every registered hook in its own
// goroutine so one slow hook cannot block others. Tracked via hookWG so Stop
// can drain. Errors are logged, never retried.
func (c *Channel) fanOutText(ev TextEvent) {
	ctx := c.hookContext()
	for _, h := range c.hooks {
		h := h
		c.hookWG.Add(1)
		go func() {
			defer c.hookWG.Done()
			if err := h.OnText(ctx, ev); err != nil {
				slog.Error("LINEWORKS: hook OnText failed", "err", err)
			}
		}()
	}
}

// fanOutPostback delivers a PostbackEvent to every registered hook.
func (c *Channel) fanOutPostback(ev PostbackEvent) {
	ctx := c.hookContext()
	for _, h := range c.hooks {
		h := h
		c.hookWG.Add(1)
		go func() {
			defer c.hookWG.Done()
			if err := h.OnPostback(ctx, ev); err != nil {
				slog.Error("LINEWORKS: hook OnPostback failed", "err", err)
			}
		}()
	}
}
