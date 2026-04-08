package esmithkm

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/channels/line"
)

// Compile-time interface checks — if Hook ever drifts from the contract
// the build breaks loudly.
var (
	_ line.MessageHook = (*Hook)(nil)
	_ line.Lifecycle   = (*Hook)(nil)
)

// Hook is the e-smith km-meeting plugin's line.MessageHook implementation.
// It owns the full ingest → conversation → Odoo writeback pipeline.
type Hook struct {
	cfg   Config
	dedup *dedupCache
	conv  *conversationState

	// watcher lifecycle — Start records the cancel func, Stop calls it
	// and waits briefly for the watcher goroutine to exit.
	watcherMu     sync.Mutex
	watcherCancel context.CancelFunc
	watcherDone   chan struct{}
}

// New constructs a Hook with the given Config. Missing optional fields
// get filled in by Config.WithDefaults. Required fields (Sender, MCPURL,
// MCPToken) are the caller's responsibility — this constructor does not
// validate them so tests can pass zero values for fields they don't care
// about.
func New(cfg Config) *Hook {
	filled := cfg.WithDefaults()
	return &Hook{
		cfg:   filled,
		dedup: newDedupCache(filled.DedupTTL),
		conv:  newConversationState(),
	}
}

// OnAudio handles a LINE AudioMessage event. The channel has already
// downloaded the content to ev.TempPath. OnAudio takes ownership: moves
// the file to the inbox with the km-meeting-pipeline naming convention
// and writes a sidecar JSON with LINE provenance.
//
// Webhook resends (same MessageID within the dedup TTL) are suppressed:
// the tmp file is deleted and a "已收到" reply is pushed so the user
// knows their original send is still in flight.
func (h *Hook) OnAudio(_ context.Context, ev line.AudioEvent) error {
	if h.dedup != nil && h.dedup.SeenOrMark(audioMessageKey(ev.MessageID)) {
		slog.Info("LINE: audio message resend detected, skipping ingest",
			"message_id", ev.MessageID, "chat", ev.ChatID)
		if ev.TempPath != "" {
			_ = os.Remove(ev.TempPath)
		}
		if h.cfg.Sender != nil {
			_ = h.cfg.Sender.SendChunks(ev.ChatID, []string{
				"⏳ 已收到此會議錄音，正在處理中。完成後會自動傳送選單請你補欄位。",
			})
		}
		return nil
	}
	return h.ingestAudio(ev.TempPath, ev.ContentType, ev.MessageID, ev.UserID, ev.ChatID)
}

// OnText handles a LINE TextMessage event. Scans the body for GDrive
// shared-file URLs and ingests each one via km-meeting-pipeline.sh.
// Non-URL messages are ignored (the agent path handles them separately).
func (h *Hook) OnText(_ context.Context, ev line.TextEvent) error {
	h.ingestGdriveLinks(ev.Text, ev.UserID, ev.ChatID)
	return nil
}

// OnPostback dispatches a LINE postback event through the km-meeting
// conversation state machine. Routes on the `action` query-string field
// to update / toggle / submit_attendees / finalize / cancel handlers.
func (h *Hook) OnPostback(_ context.Context, ev line.PostbackEvent) error {
	h.handlePostback(ev)
	return nil
}

// Start launches the draft watcher goroutine. Called by the LINE channel's
// Start after its own init completes. Idempotent — a second Start with a
// watcher already running is a no-op.
func (h *Hook) Start(ctx context.Context) error {
	h.watcherMu.Lock()
	defer h.watcherMu.Unlock()
	if h.watcherCancel != nil {
		return nil // already started
	}
	watcherCtx, cancel := context.WithCancel(ctx)
	h.watcherCancel = cancel
	h.watcherDone = make(chan struct{})
	go func() {
		defer close(h.watcherDone)
		h.startDraftWatcher(watcherCtx)
	}()
	return nil
}

// Stop cancels the draft watcher context and waits briefly for the
// goroutine to exit. The timeout is short because the watcher select
// loop responds to ctx.Done within one tick; if it hangs longer than
// that, we log and return anyway rather than blocking Channel.Stop.
func (h *Hook) Stop() error {
	h.watcherMu.Lock()
	cancel := h.watcherCancel
	done := h.watcherDone
	h.watcherCancel = nil
	h.watcherDone = nil
	h.watcherMu.Unlock()

	if cancel == nil {
		return nil
	}
	cancel()
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			slog.Warn("LINE meeting: watcher did not exit within 2s of Stop")
		}
	}
	return nil
}
