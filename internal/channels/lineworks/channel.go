package lineworks

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	lw "github.com/nextlevelbuilder/goclaw/internal/lineworks"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

// Config holds the non-secret runtime settings for a LINE WORKS channel
// instance. Policy fields mirror the line channel; botSecret is the HMAC key
// for callback signature verification (distinct from the OAuth client secret
// used by the token source).
type Config struct {
	// BotSecret is the bot's own secret — the HMAC-SHA256 key used to verify
	// the X-WORKS-Signature callback header. NOT the OAuth client_secret.
	BotSecret string
	// AllowFrom is the sender allowlist (entries are "lineworks:<userId>" or a
	// bare userId; senderPrefix is applied before matching).
	AllowFrom []string
	// DMPolicy / GroupPolicy follow the channels package semantics
	// ("open"|"allowlist"|"disabled"|"pairing").
	DMPolicy    string
	GroupPolicy string
}

// Channel implements the LINE WORKS Bot channel. It is the structural twin of
// internal/channels/line.Channel but shares no types with it: a separate
// platform type ("lineworks"), a separate webhook path, and its own
// JWT/REST SDK (internal/lineworks). It coexists with the consumer LINE channel
// without touching its behavior.
type Channel struct {
	*channels.BaseChannel

	client    botClient // outbound sender (wraps *lineworks.Client); nil in unit tests
	botSecret string    // HMAC key for X-WORKS-Signature verification
	cfg       Config

	// hooks are MessageHook plugins registered via RegisterHook. Events fan out
	// to every hook in registration order. Populated before Start(); not mutated
	// thereafter (Register/Start happen at process init, no lock needed).
	hooks []MessageHook

	// hookCtx is the parent context for every fan-out goroutine. Created in
	// Start, cancelled in Stop. Nil before Start — hookContext() falls back to
	// context.Background() so unit tests that exercise fan-out without Start work.
	hookCtx    context.Context
	hookCancel context.CancelFunc

	// hookWG tracks in-flight fan-out goroutines so Stop can wait briefly for
	// them to drain (bounded by a 3s timeout).
	hookWG sync.WaitGroup

	// gate, when set via SetGate, runs synchronously before hook fan-out and the
	// agent path; a deny result blocks the message (the agent never sees it). At
	// most one gate per channel.
	gate MessageGate

	// groupChats remembers which chat ids are group/room conversations (chatID →
	// struct{}), recorded on every inbound group event. The agent's outbound
	// reply arrives via the bus without the inbound peer metadata, so routePeer
	// consults this set to route group replies to the channel endpoint instead
	// of mis-sending the channelId to the 1:1 user endpoint.
	groupChats sync.Map

	// groupLastSender remembers the most recent asker per group chat (chatID →
	// LINE WORKS userId). The agent's group reply is prefixed with a mention of
	// that user (<m userId="...">) so the reply notifies + addresses whoever
	// asked. Best-effort: in a busy group the "last sender" may differ from the
	// exact triggering message, but conversations are typically sequential.
	groupLastSender sync.Map

	// ackPending tracks chats awaiting an agent reply (chatID → generation int64).
	// LINE WORKS has no streaming / typing indicator and bot messages cannot be
	// edited, so for a slow turn we send a one-off "processing" ack only if the
	// real reply has not arrived within ackDelay. A newer inbound (higher gen)
	// or the reply (Send clears the entry) cancels a pending ack.
	ackPending sync.Map
	ackCounter int64 // monotonic generation source (atomic)
}

const (
	// defaultAckDelay is how long to wait for the agent's reply before sending a
	// "processing" ack. Tuned so fast replies never trigger an ack.
	defaultAckDelay = 4 * time.Second
	// defaultAckMessage is the one-off ack sent for a slow turn.
	defaultAckMessage = "⏳ 收到,正在為你處理中,請稍候…"
)

// compile-time assertions: Channel satisfies the channel + webhook + sender
// contracts.
var (
	_ channels.Channel        = (*Channel)(nil)
	_ channels.WebhookChannel = (*Channel)(nil)
	_ Sender                  = (*Channel)(nil)
)

// New builds a LINE WORKS channel from a ready outbound client and config.
// client may be nil (unit tests / send-disabled instances); the webhook path
// and hook fan-out still function. msgBus and the allowlist are wired into the
// embedded BaseChannel exactly as the line channel does.
func New(client botClient, cfg Config, msgBus *bus.MessageBus) *Channel {
	base := channels.NewBaseChannel(ChannelType, msgBus, cfg.AllowFrom)
	base.SetType(ChannelType)
	base.ValidatePolicy(cfg.DMPolicy, cfg.GroupPolicy)

	return &Channel{
		BaseChannel: base,
		client:      client,
		botSecret:   cfg.BotSecret,
		cfg:         cfg,
	}
}

// Type returns the platform type. Always "lineworks", independent from "line".
func (c *Channel) Type() string { return ChannelType }

// RegisterHook appends a MessageHook to the channel's fan-out list. Must be
// called before Start(); not safe for concurrent use (intended for process
// init wiring, identical to line.Channel.RegisterHook).
func (c *Channel) RegisterHook(h MessageHook) {
	if h == nil {
		return
	}
	c.hooks = append(c.hooks, h)
}

// SetGate installs the channel's message gate (replacing any previous one).
// Must be called before Start(); not safe for concurrent use. A nil gate means
// no gating — every policy-passing message proceeds to the agent.
func (c *Channel) SetGate(g MessageGate) {
	c.gate = g
}

// Start begins listening (webhook mode). It creates the fan-out parent context
// that hook goroutines inherit, then calls Lifecycle.Start on every hook that
// implements it. The webhook handler is auto-mounted by the gateway via
// WebhookHandler(), so Start has no HTTP server of its own.
func (c *Channel) Start(ctx context.Context) error {
	c.SetRunning(true)

	// Derive from Background (not the caller ctx) so channel lifetime is owned
	// by Stop, mirroring line.Channel.Start.
	c.hookCtx, c.hookCancel = context.WithCancel(context.Background())

	for _, h := range c.hooks {
		if lc, ok := h.(Lifecycle); ok {
			if err := lc.Start(ctx); err != nil {
				slog.Error("LINEWORKS: hook Start failed", "err", err)
			}
		}
	}
	slog.Info("LINE WORKS channel started (webhook mode)", "hooks", len(c.hooks))
	return nil
}

// Stop shuts down the channel: cancel the hook context, Stop Lifecycle hooks in
// reverse registration order, then wait up to 3s for fan-out goroutines to
// drain. Identical shutdown ordering to the line channel.
func (c *Channel) Stop(_ context.Context) error {
	if c.hookCancel != nil {
		c.hookCancel()
	}

	for i := len(c.hooks) - 1; i >= 0; i-- {
		if lc, ok := c.hooks[i].(Lifecycle); ok {
			if err := lc.Stop(); err != nil {
				slog.Error("LINEWORKS: hook Stop failed", "err", err)
			}
		}
	}

	drained := make(chan struct{})
	go func() {
		c.hookWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		slog.Warn("LINEWORKS: hook fan-out did not drain within 3s of Stop")
	}

	c.SetRunning(false)
	slog.Info("LINE WORKS channel stopped")
	return nil
}

// SendText sends a plain-text message to a peer, satisfying the Sender
// interface used by workflow-plugin hooks. The (userID, channelID) pair selects
// the endpoint: channelID set → group/room, otherwise 1:1 user. Long text is
// chunked to the platform per-message cap.
func (c *Channel) SendText(ctx context.Context, userID, channelID, text string) error {
	text = formatForLineWorks(text)
	if text == "" {
		return nil
	}
	for _, chunk := range splitMessage(text, maxTextLength) {
		if err := c.sendText(ctx, userID, channelID, chunk); err != nil {
			return err
		}
	}
	return nil
}

// scheduleAck arms a delayed "processing" ack for an agent-bound message. If
// the agent's reply has not reached the chat within defaultAckDelay (Send
// clears the pending entry) and no newer inbound has superseded it, a single
// ack is sent so the user knows a slow turn (e.g. an Odoo query) is in flight.
// No-op without a client (unit tests).
func (c *Channel) scheduleAck(userID, channelID, chatID string) {
	if c.client == nil {
		return
	}
	gen := atomic.AddInt64(&c.ackCounter, 1)
	c.ackPending.Store(chatID, gen)
	c.hookWG.Add(1)
	go func() {
		defer c.hookWG.Done()
		ctx := c.hookContext()
		t := time.NewTimer(defaultAckDelay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
		// Send the ack only if this is still the latest pending turn for the
		// chat (no reply cleared it, no newer inbound replaced the generation).
		if v, ok := c.ackPending.Load(chatID); ok && v.(int64) == gen {
			c.ackPending.Delete(chatID)
			if err := c.SendText(ctx, userID, channelID, defaultAckMessage); err != nil {
				slog.Warn("LINEWORKS: processing-ack send failed", "chatID", chatID, "err", err)
			}
		}
	}()
}

// WebhookHandler returns the HTTP path and handler for LINE WORKS callbacks.
// The handler verifies the X-WORKS-Signature HMAC over the RAW request body
// using the bot secret BEFORE parsing, then dispatches asynchronously and
// always replies 200 so LINE WORKS does not retry-storm. Auto-mounted by the
// gateway because Channel implements channels.WebhookChannel.
func (c *Channel) WebhookHandler() (string, http.Handler) {
	return webhookPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Error("LINEWORKS webhook: read body failed", "err", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		sig := r.Header.Get(signatureHeader)
		if !lw.VerifySignature(c.botSecret, rawBody, sig) {
			slog.Warn("LINEWORKS webhook: invalid signature")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Body is trusted past this point. Dispatch off the request goroutine so
		// the 200 is not blocked by hook fan-out or agent processing.
		go c.handleCallback(rawBody)

		w.WriteHeader(http.StatusOK)
	})
}
