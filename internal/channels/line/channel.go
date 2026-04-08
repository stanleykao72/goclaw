package line

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/line/line-bot-sdk-go/v7/linebot"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// replyTokenEntry caches a reply token with its receive time.
type replyTokenEntry struct {
	token      string
	receivedAt time.Time
}

// Channel implements the LINE Messaging API channel.
type Channel struct {
	*channels.BaseChannel
	bot            *linebot.Client
	cfg            config.LineConfig
	pairingService store.PairingStore
	replyTokens    sync.Map // chatID → replyTokenEntry

	// hooks are MessageHook plugins registered via RegisterHook.
	// Events fan out to every hook in registration order. Populated
	// before Start() is called; not mutated thereafter (no lock needed
	// post-start because Register/Start happen in cmd/main.go init).
	hooks []MessageHook
}

// New creates a new LINE channel.
func New(cfg config.LineConfig, msgBus *bus.MessageBus, pairingSvc store.PairingStore) (*Channel, error) {
	bot, err := linebot.New(cfg.ChannelSecret, cfg.ChannelAccessToken)
	if err != nil {
		return nil, err
	}

	base := channels.NewBaseChannel("line", msgBus, cfg.AllowFrom)
	base.ValidatePolicy(cfg.DMPolicy, cfg.GroupPolicy)

	return &Channel{
		BaseChannel:    base,
		bot:            bot,
		cfg:            cfg,
		pairingService: pairingSvc,
	}, nil
}

// Type returns the channel type.
func (c *Channel) Type() string { return "line" }

// RegisterHook appends a MessageHook to the channel's fan-out list. Must be
// called before Start(). Not safe for concurrent use — intended for cmd/main.go
// wiring at process init.
func (c *Channel) RegisterHook(h MessageHook) {
	if h == nil {
		return
	}
	c.hooks = append(c.hooks, h)
}

// Start begins listening (webhook mode). After its own init, Start calls
// Lifecycle.Start on every registered hook that implements it.
func (c *Channel) Start(ctx context.Context) error {
	c.SetRunning(true)

	// Lifecycle scan: start any hook that implements Lifecycle.
	for _, h := range c.hooks {
		if lc, ok := h.(Lifecycle); ok {
			if err := lc.Start(ctx); err != nil {
				slog.Error("LINE: hook Start failed", "err", err)
			}
		}
	}
	slog.Info("LINE channel started (webhook mode)", "hooks", len(c.hooks))
	return nil
}

// Stop shuts down the channel. Lifecycle hooks are stopped in reverse
// registration order before the channel itself stops.
func (c *Channel) Stop(_ context.Context) error {
	// Stop hooks in reverse registration order.
	for i := len(c.hooks) - 1; i >= 0; i-- {
		if lc, ok := c.hooks[i].(Lifecycle); ok {
			if err := lc.Stop(); err != nil {
				slog.Error("LINE: hook Stop failed", "err", err)
			}
		}
	}
	c.SetRunning(false)
	slog.Info("LINE channel stopped")
	return nil
}

// SendChunks is the LineSender-interface wrapper around sendChunks. Exported
// so plugin packages can call it through the LineSender interface without
// importing the concrete *Channel type.
func (c *Channel) SendChunks(chatID string, chunks []string) error {
	return c.sendChunks(chatID, chunks)
}

// PushFlex is the LineSender-interface wrapper around pushFlex.
func (c *Channel) PushFlex(chatID, altText string, flexJSON []byte) error {
	return c.pushFlex(chatID, altText, flexJSON)
}

// ReplyFlex is the LineSender-interface wrapper around replyFlex.
func (c *Channel) ReplyFlex(chatID, altText string, flexJSON []byte) error {
	return c.replyFlex(chatID, altText, flexJSON)
}

// WebhookHandler returns the HTTP path and handler for LINE webhook callbacks.
func (c *Channel) WebhookHandler() (string, http.Handler) {
	return "/webhook/line", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		events, err := c.bot.ParseRequest(r)
		if err != nil {
			if err == linebot.ErrInvalidSignature {
				slog.Warn("LINE webhook: invalid signature")
				w.WriteHeader(http.StatusBadRequest)
			} else {
				slog.Error("LINE webhook: parse error", "err", err)
				w.WriteHeader(http.StatusInternalServerError)
			}
			return
		}

		for _, event := range events {
			go c.handleEvent(event)
		}

		w.WriteHeader(http.StatusOK)
	})
}
