package lineworks

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// captureFakePending records AppendBatch calls and signals on a channel so a
// test can wait for the async (non-blocking) DM capture goroutine to finish.
// Only AppendBatch is exercised; the other interface methods are no-op stubs.
type captureFakePending struct {
	mu     sync.Mutex
	rows   []store.PendingMessage
	gotRow chan struct{}
}

func newCaptureFakePending() *captureFakePending {
	return &captureFakePending{gotRow: make(chan struct{}, 8)}
}

func (f *captureFakePending) AppendBatch(ctx context.Context, msgs []store.PendingMessage) error {
	f.mu.Lock()
	f.rows = append(f.rows, msgs...)
	f.mu.Unlock()
	for range msgs {
		select {
		case f.gotRow <- struct{}{}:
		default:
		}
	}
	return nil
}

func (f *captureFakePending) rowCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func (f *captureFakePending) lastRow() (store.PendingMessage, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rows) == 0 {
		return store.PendingMessage{}, false
	}
	return f.rows[len(f.rows)-1], true
}

func (f *captureFakePending) ListByKey(ctx context.Context, channelName, historyKey string) ([]store.PendingMessage, error) {
	return nil, nil
}
func (f *captureFakePending) ListSince(ctx context.Context, channelName, historyKey string, afterCreatedAt time.Time, afterID uuid.UUID, limit int) ([]store.PendingMessage, error) {
	return nil, nil
}
func (f *captureFakePending) DeleteByKey(ctx context.Context, channelName, historyKey string) error {
	return nil
}
func (f *captureFakePending) Compact(ctx context.Context, deleteIDs []uuid.UUID, summary *store.PendingMessage) error {
	return nil
}
func (f *captureFakePending) DeleteByIDs(ctx context.Context, ids []uuid.UUID) error { return nil }
func (f *captureFakePending) DeleteStale(ctx context.Context, olderThan time.Duration) (int64, error) {
	return 0, nil
}
func (f *captureFakePending) ListGroups(ctx context.Context) ([]store.PendingMessageGroup, error) {
	return nil, nil
}
func (f *captureFakePending) CountAll(ctx context.Context) (int64, error) { return 0, nil }
func (f *captureFakePending) CountByKey(ctx context.Context, channelName, historyKey string) (int, error) {
	return 0, nil
}
func (f *captureFakePending) ResolveGroupTitles(ctx context.Context, groups []store.PendingMessageGroup) (map[string]string, error) {
	return nil, nil
}

var _ store.PendingMessageStore = (*captureFakePending)(nil)

// waitForRow waits up to 2s for the async capture goroutine to enqueue a row.
func waitForRow(t *testing.T, f *captureFakePending) {
	t.Helper()
	select {
	case <-f.gotRow:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for async DM capture row")
	}
}

// TestDMCapture_EnabledBuffersUserRow verifies that when ingest is enabled a 1:1
// message is captured into the pending buffer keyed by the userId, with the
// lineworks-prefixed sender id (so the worker classifies it as a user scope).
func TestDMCapture_EnabledBuffersUserRow(t *testing.T) {
	t.Setenv(envNLMIngestEnabled, "true")
	c, mb := newGatingChannel(t, nil, true)
	pending := newCaptureFakePending()
	c.SetPendingStore(pending)
	c.SetTenantID(uuid.New())

	c.handleMessageEvent(directEvent("user-42", "remember my coffee order"))

	// The agent path still publishes (capture is in addition, non-blocking).
	if _, ok := consume(t, mb); !ok {
		t.Fatal("DM should still reach the agent path")
	}

	waitForRow(t, pending)
	row, ok := pending.lastRow()
	if !ok {
		t.Fatal("expected a captured DM row")
	}
	if row.HistoryKey != "user-42" {
		t.Errorf("DM row must be keyed by userId, got history_key=%q", row.HistoryKey)
	}
	if row.SenderID != senderPrefix+"user-42" {
		t.Errorf("DM row sender_id must be lineworks-prefixed, got %q", row.SenderID)
	}
	if row.Body != "remember my coffee order" {
		t.Errorf("DM row body mismatch: %q", row.Body)
	}
	if row.IsSummary {
		t.Error("DM row must not be a summary")
	}
	// history_key == strip(sender_id) is the worker's DM classification invariant.
	if row.HistoryKey != row.SenderID[len(senderPrefix):] {
		t.Error("DM invariant broken: history_key must equal prefix-stripped sender_id")
	}
}

// TestDMCapture_DisabledBuffersNothing verifies the privacy property: with the
// flag OFF, no DM rows are buffered at all.
func TestDMCapture_DisabledBuffersNothing(t *testing.T) {
	t.Setenv(envNLMIngestEnabled, "false")
	c, mb := newGatingChannel(t, nil, true)
	pending := newCaptureFakePending()
	c.SetPendingStore(pending)
	c.SetTenantID(uuid.New())

	c.handleMessageEvent(directEvent("user-42", "secret diary entry"))

	// Agent path still works.
	if _, ok := consume(t, mb); !ok {
		t.Fatal("DM should still reach the agent path even with ingest off")
	}
	// Give any (erroneous) goroutine a moment.
	time.Sleep(50 * time.Millisecond)
	if n := pending.rowCount(); n != 0 {
		t.Errorf("ingest OFF must buffer no DM rows, got %d", n)
	}
}

// TestDMCapture_GroupMessageNotDuplicated verifies the DM capture path does NOT
// fire for group messages (those are captured by GroupHistory.Record). A
// mentionless group message must produce no AppendBatch from the DM path.
func TestDMCapture_GroupMessageNotDuplicated(t *testing.T) {
	t.Setenv(envNLMIngestEnabled, "true")
	c, _ := newGatingChannel(t, []string{"goclaw"}, true)
	pending := newCaptureFakePending()
	// Group history is RAM-only here (newGatingChannel sets a RAM PendingHistory),
	// so GroupHistory.Record will NOT hit this fake — only the DM path would. We
	// assert the DM path stays silent for a group event.
	c.SetPendingStore(pending)
	c.SetTenantID(uuid.New())

	c.handleMessageEvent(groupEvent("grp1", "userA", "team lunch?")) // no mention

	time.Sleep(50 * time.Millisecond)
	if n := pending.rowCount(); n != 0 {
		t.Errorf("DM capture path must not fire for group messages, got %d rows", n)
	}
}
