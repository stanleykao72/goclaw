package lineworks

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// envNLMIngestEnabled is the opt-in flag for the whole NotebookLM ingest feature
// (sub-phase 2.3). It gates BOTH the background drain worker AND this DM capture
// path, so when OFF no raw DM text is buffered (a privacy property — rows must
// not accumulate before the feature is live). Mirrored verbatim in
// internal/nlmingest so capture and drain share one switch; kept as a local
// constant here to avoid the channel importing the worker package.
const envNLMIngestEnabled = "GOCLAW_NLM_INGEST_ENABLED"

// nlmIngestEnabled reports whether NotebookLM ingest is opted in. Default false:
// any unset / unparseable / falsey value disables capture (and the worker).
func nlmIngestEnabled() bool {
	v := strings.TrimSpace(os.Getenv(envNLMIngestEnabled))
	if v == "" {
		return false
	}
	enabled, err := strconv.ParseBool(v)
	return err == nil && enabled
}

// recordDirectMessageForIngest buffers a verified 1:1 message into
// channel_pending_messages so the NotebookLM ingest worker can land it in the
// user-<userId> Doc. NON-BLOCKING: the actual store write runs on a detached
// goroutine, so the webhook path (and HandleMessage that follows) is never
// delayed by DB latency — identical discipline to the group GroupHistory.Record
// flusher and HandleMessage's bus publish.
//
// ROUTING / ISOLATION: the row is keyed by history_key = userID, which for a 1:1
// chat IS the verified sender's userId (peerOf returns src.UserID for direct).
// sender_id carries the "lineworks:" prefix. The worker classifies a row as DM
// (user scope) precisely when history_key == TrimPrefix(sender_id, "lineworks:")
// — true here because userID == the prefix-stripped senderID. The scope is
// therefore derived from the verified identity, never from body text.
//
// GATING: a no-op unless GOCLAW_NLM_INGEST_ENABLED is set, so OFF buffers no DM
// rows. Also a no-op without a pending store (RAM-only / send-disabled
// instances) or an empty userId/body (nothing to ingest, and an empty userId
// must never be written — it would mis-key the user Doc).
func (c *Channel) recordDirectMessageForIngest(userID, senderID, senderLabel, body string) {
	if !nlmIngestEnabled() {
		return
	}
	if c.pendingStore == nil {
		return
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(body) == "" {
		return
	}

	tenantID := c.TenantID()
	now := time.Now()
	pending := c.pendingStore

	go func() {
		ctx := context.Background()
		if tenantID != uuid.Nil {
			ctx = store.WithTenantID(ctx, tenantID)
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		if err := pending.AppendBatch(ctx, []store.PendingMessage{{
			ChannelName: channels.TypeLineWorks,
			HistoryKey:  userID,
			Sender:      senderLabel,
			SenderID:    senderID,
			Body:        body,
			IsSummary:   false,
			CreatedAt:   now,
		}}); err != nil {
			slog.Warn("LINEWORKS: nlm DM ingest capture failed (non-blocking)",
				"user", userID, "err", err)
		}
	}()
}
