package lineworksworkflow

import (
	"context"
	"log/slog"

	channel "github.com/nextlevelbuilder/goclaw/internal/channels/lineworks"
)

// Compile-time interface check — if Hook drifts from the channel's MessageHook
// contract the build breaks loudly. This plugin needs no background work, so
// it deliberately does NOT implement channel.Lifecycle.
var _ channel.MessageHook = (*Hook)(nil)

// Hook is the LINE WORKS workflow plugin's channel.MessageHook implementation.
// It routes inbound text/postback events to the todo and daily-log flows,
// resolving the sender's Odoo identity first.
type Hook struct {
	cfg      Config
	identity *identityCache
}

// New constructs a Hook with the given Config. Missing optional fields are
// filled by Config.WithDefaults. Required fields (Sender, Directory, MCPURL,
// MCPToken) are the caller's responsibility — this constructor does not
// validate them so tests can pass zero values for fields they don't exercise.
func New(cfg Config) *Hook {
	filled := cfg.WithDefaults()
	return &Hook{
		cfg:      filled,
		identity: newIdentityCache(filled.IdentityCacheTTL),
	}
}

// OnText routes a LINE WORKS text message to the todo or daily-log flow.
//
// Messages that match no flow keyword are ignored (returns nil) so this
// plugin coexists with any agent path on the same channel. Identity is
// resolved before running a flow; an unresolvable sender gets a binding hint
// instead of a silent drop.
func (h *Hook) OnText(ctx context.Context, ev channel.TextEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	flow, arg := classifyText(ev.Text)
	if flow == flowNone {
		return nil
	}

	if _, err := h.resolveOdooUID(ctx, ev.UserID); err != nil {
		return h.replyIdentityHint(ctx, ev.UserID, ev.ChannelID, err)
	}

	var (
		reply string
		err   error
	)
	switch flow {
	case flowTodo:
		reply, err = h.runTodoFlow(ctx, arg)
	case flowDailyLog:
		reply, err = h.runDailyLogFlow(ctx, arg)
	}
	if err != nil {
		slog.Error("lineworks-workflow: flow failed",
			"flow", flow, "user", ev.UserID, "channel", ev.ChannelID, "err", err)
		return h.reply(ctx, ev.UserID, ev.ChannelID, "處理時發生錯誤，請稍後再試。")
	}
	if reply == "" {
		return nil
	}
	return h.reply(ctx, ev.UserID, ev.ChannelID, reply)
}

// OnPostback routes a LINE WORKS postback. v1 flows are text-driven (no
// quick-reply buttons yet), so postbacks that carry no recognized action are
// logged and ignored. The `data` field is top-level per the callback
// contract.
func (h *Hook) OnPostback(ctx context.Context, ev channel.PostbackEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	flow, arg := classifyPostback(ev.Data)
	if flow == flowNone {
		slog.Debug("lineworks-workflow: unhandled postback", "data", ev.Data, "user", ev.UserID)
		return nil
	}

	if _, err := h.resolveOdooUID(ctx, ev.UserID); err != nil {
		return h.replyIdentityHint(ctx, ev.UserID, ev.ChannelID, err)
	}

	var (
		reply string
		err   error
	)
	switch flow {
	case flowTodo:
		reply, err = h.runTodoFlow(ctx, arg)
	case flowDailyLog:
		reply, err = h.runDailyLogFlow(ctx, arg)
	}
	if err != nil {
		slog.Error("lineworks-workflow: postback flow failed",
			"flow", flow, "user", ev.UserID, "err", err)
		return h.reply(ctx, ev.UserID, ev.ChannelID, "處理時發生錯誤，請稍後再試。")
	}
	if reply == "" {
		return nil
	}
	return h.reply(ctx, ev.UserID, ev.ChannelID, reply)
}

// classifyPostback maps a postback data string to a flow. Postback payloads
// use the same "action=..." style the channel emits for quick replies;
// unknown payloads return flowNone. The arg is whatever follows the action.
func classifyPostback(data string) (flowKind, string) {
	switch {
	case data == "action=todo":
		return flowTodo, ""
	case data == "action=daily_log":
		return flowDailyLog, ""
	default:
		return flowNone, ""
	}
}

// reply sends a plain-text reply via the configured Sender, routing to the
// group channel when channelID is set, otherwise 1:1 to the user. A nil
// Sender (test config) is a no-op.
func (h *Hook) reply(ctx context.Context, userID, channelID, text string) error {
	if h.cfg.Sender == nil || text == "" {
		return nil
	}
	return h.cfg.Sender.SendText(ctx, userID, channelID, text)
}

// replyIdentityHint logs the resolution failure and sends a binding hint.
func (h *Hook) replyIdentityHint(ctx context.Context, userID, channelID string, cause error) error {
	slog.Warn("lineworks-workflow: identity resolution failed",
		"user", userID, "err", cause)
	return h.reply(ctx, userID, channelID,
		"無法對應你的 Odoo 帳號，請確認 LINE WORKS 目錄已綁定員工編號（externalKey），或聯絡系統管理員。")
}
