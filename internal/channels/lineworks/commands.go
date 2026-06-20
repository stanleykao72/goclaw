package lineworks

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// handleBotCommand intercepts Tier 1 slash commands before the agent path. It
// runs only for messages that have already passed the mention gate (group) or
// are 1:1, so the leading "@bot" mention (if any) has typically been stripped by
// the caller — but we strip a leading mention again here defensively so a
// command like "@goclaw /reset" is recognized regardless of call order.
//
// Returns true when the message was handled as a command (caller must stop and
// NOT dispatch to the agent). Returns false for non-commands and for unknown
// commands, which pass through to the agent untouched (we do not swallow them).
//
// Command replies are routed to the correct LINE WORKS Bot endpoint via
// peerOf(ev.Source): a group/room reply goes to the channel endpoint, a 1:1
// reply to the user endpoint. State-changing commands (/reset, /stop, /stopall)
// publish a channel-agnostic bus.InboundMessage carrying Metadata[command];
// the gateway consumer (handleResetCommand / handleStopCommand) performs the
// actual session reset / run cancellation.
func (c *Channel) handleBotCommand(ctx context.Context, ev callbackEvent) bool {
	// Strip a leading mention first so "@bot /cmd" still parses as a command.
	// Idempotent when there is no leading mention.
	text := stripBotMention(ev.Content.Text, c.botNamesSnapshot())
	text = strings.TrimLeft(text, " \t")
	if !strings.HasPrefix(text, "/") {
		return false
	}

	// First whitespace-delimited token is the command. Strip a trailing
	// "@botname" suffix (e.g. "/help@goclaw") so commands addressed to the bot
	// by name are recognized.
	cmd := strings.SplitN(text, " ", 2)[0]
	cmd = strings.ToLower(cmd)
	cmd = strings.SplitN(cmd, "@", 2)[0]

	// Scope every downstream store call (writer ACL check, admin handlers) to
	// this channel's tenant; mirrors the telegram channel. Without this, admin
	// stores fall back to the master tenant and silently operate on the wrong
	// partition on a non-master deployment.
	ctx = store.WithTenantID(ctx, c.TenantID())

	chatID, peerKind := peerOf(ev.Source)
	senderID := senderPrefix + ev.Source.UserID

	// Resolve the requesting user's preferred language once for this command so
	// every fixed reply below is localized to them. Falls back to "en".
	lang := c.resolveUserLang(ctx, ev.Source.UserID)

	switch cmd {
	case "/reset", "/new":
		// In groups, restrict reset of the shared conversation history once file
		// writers are designated. The gateway consumer performs no authorization
		// of its own, so the gate must live here.
		//
		// Open-until-configured: when NO file writer is set for the group, anyone
		// may reset (no ACL = no restriction), so the feature is usable out of the
		// box; once /addwriter designates writers, only they may reset. This
		// matches the /addwriter bootstrap semantics. Fail-open on a store error
		// so a DB hiccup never blocks resets.
		if peerKind == peerGroup && c.configPermStore != nil {
			if agentID, err := c.resolveAgentUUID(ctx); err == nil {
				groupID := fmt.Sprintf("group:%s:%s", c.Name(), ev.Source.ChannelID)
				existing, lerr := c.configPermStore.ListFileWriters(ctx, agentID, groupID)
				if lerr != nil {
					slog.Warn("LINEWORKS: reset writer list failed (fail-open)", "err", lerr, "sender", ev.Source.UserID)
				} else if len(existing) > 0 {
					// Writers configured → enforce. Check against the canonical
					// senderPrefix-qualified id (matches the /addwriter grant
					// format + the file-write ACL); the bare user id would never
					// match a correctly-stored writer grant.
					isWriter, perr := c.configPermStore.CheckPermission(ctx, agentID, groupID, store.ConfigTypeFileWriter, senderPrefix+ev.Source.UserID)
					if perr != nil {
						slog.Warn("LINEWORKS: reset writer check failed (fail-open)", "err", perr, "sender", ev.Source.UserID)
					} else if !isWriter {
						c.replyCommand(ctx, ev, localize(lang, keyResetWriterGate))
						return true
					}
				}
			}
		}
		c.publishCommand(ev, chatID, peerKind, senderID, "reset", "/reset")
		c.replyCommand(ctx, ev, localize(lang, keyResetDone))
		return true

	case "/stop":
		c.publishCommand(ev, chatID, peerKind, senderID, "stop", "/stop")
		// Feedback (success/failure) is published by the consumer once the
		// cancel result is known; we add only a tiny ack here.
		c.replyCommand(ctx, ev, localize(lang, keyStopOne))
		return true

	case "/stopall":
		c.publishCommand(ev, chatID, peerKind, senderID, "stopall", "/stopall")
		c.replyCommand(ctx, ev, localize(lang, keyStopAll))
		return true

	case "/help":
		c.replyCommand(ctx, ev, localize(lang, keyHelp))
		return true

	case "/status":
		c.replyCommand(ctx, ev, c.statusText(lang))
		return true

	// --- Tier 2 admin commands (text carries the full argument string) ---
	case "/addwriter":
		c.handleWriterCommand(ctx, ev, text, "add", lang)
		return true

	case "/removewriter":
		c.handleWriterCommand(ctx, ev, text, "remove", lang)
		return true

	case "/writers":
		c.handleListWriters(ctx, ev, lang)
		return true

	case "/tasks":
		c.handleTasksList(ctx, ev, lang)
		return true

	case "/task_detail":
		c.handleTaskDetail(ctx, ev, text, lang)
		return true

	case "/subagents":
		c.handleSubagentsList(ctx, ev, lang)
		return true

	case "/subagent":
		c.handleSubagentDetail(ctx, ev, text, lang)
		return true

	default:
		// Unknown command: pass through to the agent rather than swallow it.
		return false
	}
}

// publishCommand emits a channel-agnostic command InboundMessage on the bus.
// The field mapping mirrors BaseChannel.HandleMessage so the consumer derives an
// identical scoped session key: Channel=c.Name(), AgentID=c.AgentID(),
// TenantID=c.TenantID(), PeerKind=peerKind ("direct"|"group"), ChatID=chatID,
// SenderID/UserID from the event. Metadata[tools.MetaCommand] selects the
// consumer handler (reset/stop/stopall). LINE WORKS has no forum/topic
// threading, so the is_forum / message_thread_id metadata keys are intentionally
// omitted (the consumer treats their absence as non-forum).
func (c *Channel) publishCommand(ev callbackEvent, chatID, peerKind, senderID, command, content string) {
	c.Bus().PublishInbound(bus.InboundMessage{
		Channel:  c.Name(),
		SenderID: senderID,
		ChatID:   chatID,
		Content:  content,
		PeerKind: peerKind,
		AgentID:  c.AgentID(),
		UserID:   ev.Source.UserID,
		TenantID: c.TenantID(),
		Metadata: map[string]string{
			tools.MetaCommand: command,
		},
	})
}

// replyCommand sends a channel-local command reply to the originating peer. The
// (userID, channelID) pair from the event selects the endpoint: a group reply
// goes to the channel/room endpoint, a 1:1 reply to the user endpoint (SendText
// routes on channelID presence). No-op without a client (unit tests).
func (c *Channel) replyCommand(ctx context.Context, ev callbackEvent, text string) {
	if c.client == nil {
		return
	}
	if err := c.SendText(ctx, ev.Source.UserID, ev.Source.ChannelID, text); err != nil {
		slog.Warn("LINEWORKS: command reply send failed",
			"userID", ev.Source.UserID,
			"channelID", ev.Source.ChannelID,
			"err", err,
		)
	}
}

// lineWorksHelpText lists the Tier 1 commands available on this channel, in the
// canonical (English) language. The localized variant is selected at call time
// via localize(lang, keyHelp); this wrapper is retained for tests and any
// language-agnostic caller.
func lineWorksHelpText() string {
	return localize(langEN, keyHelp)
}

// statusText reports the channel's runtime status without invoking the agent:
// channel name, running state, and the resolved bot name(s) used for group
// mention gating (or a note that gating is disabled when unresolved). lang
// selects the language for the fixed labels (the channel name and bot names are
// data, not translated).
func (c *Channel) statusText(lang string) string {
	running := localize(lang, keyStatusStopped)
	if c.IsRunning() {
		running = localize(lang, keyStatusRunning)
	}
	names := localize(lang, keyStatusNamesUnresolved)
	if bn := c.botNamesSnapshot(); len(bn) > 0 {
		names = strings.Join(bn, ", ")
	}
	return localize(lang, keyStatusLine, running, c.Name(), names)
}
