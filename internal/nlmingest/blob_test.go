package nlmingest

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

func TestClassifyScope_DM(t *testing.T) {
	msgs := []store.PendingMessage{
		{HistoryKey: "userA", SenderID: "lineworks:userA", Body: "hi"},
		{HistoryKey: "userA", SenderID: "lineworks:userA", Body: "again"},
	}
	kind, id, ok := classifyScope("userA", msgs)
	if !ok || kind != store.ScopeKindUser || id != "userA" {
		t.Fatalf("DM should classify as (user, userA), got %s/%s ok=%v", kind, id, ok)
	}
}

func TestClassifyScope_Group(t *testing.T) {
	// Multiple distinct senders, none equal to the room id → group scope.
	msgs := []store.PendingMessage{
		{HistoryKey: "chat123", SenderID: "lineworks:userA", Body: "hi"},
		{HistoryKey: "chat123", SenderID: "lineworks:userB", Body: "yo"},
	}
	kind, id, ok := classifyScope("chat123", msgs)
	if !ok || kind != store.ScopeKindGroup || id != "chat123" {
		t.Fatalf("group should classify as (group, chat123), got %s/%s ok=%v", kind, id, ok)
	}
}

func TestClassifyScope_GroupSingleSenderNotEqualKey(t *testing.T) {
	// A single-sender group room whose id != the sender → still group (the DM test
	// requires history_key == sender uid, which a real room id never satisfies).
	msgs := []store.PendingMessage{
		{HistoryKey: "roomXYZ", SenderID: "lineworks:userA", Body: "alone in room"},
	}
	kind, _, ok := classifyScope("roomXYZ", msgs)
	if !ok || kind != store.ScopeKindGroup {
		t.Fatalf("room keyed != sender must be group, got %s ok=%v", kind, ok)
	}
}

func TestClassifyScope_EmptyWindow(t *testing.T) {
	if _, _, ok := classifyScope("userA", nil); ok {
		t.Error("empty window must not classify")
	}
	if _, _, ok := classifyScope("", []store.PendingMessage{{}}); ok {
		t.Error("empty key must not classify")
	}
}

func TestBuildBatchBlob_Format(t *testing.T) {
	t0 := time.Date(2026, 6, 22, 8, 30, 0, 0, time.UTC)
	msgs := []store.PendingMessage{
		{ID: uuid.New(), Sender: "Alice", SenderID: "lineworks:u1", Body: " hello ", CreatedAt: t0},
		{ID: uuid.New(), Sender: "", SenderID: "lineworks:u2", Body: "no name", CreatedAt: t0.Add(time.Minute)},
	}
	blob := buildBatchBlob(msgs)
	if !strings.Contains(blob, "Alice [2026-06-22 08:30]: hello\n") {
		t.Errorf("blob missing first line: %q", blob)
	}
	// Empty Sender falls back to the prefix-stripped sender id.
	if !strings.Contains(blob, "u2 [2026-06-22 08:31]: no name\n") {
		t.Errorf("blob missing fallback-sender line: %q", blob)
	}
	if !strings.HasSuffix(blob, "\n") {
		t.Error("blob must end with newline (Drive AppendText separator contract)")
	}
}

func TestBuildBatchBlob_RedactsSecrets(t *testing.T) {
	t0 := time.Now().UTC()
	msgs := []store.PendingMessage{
		{Sender: "Bob", SenderID: "lineworks:u1", Body: "my api_key=sk-abcdef0123456789 ok", CreatedAt: t0},
		{Sender: "Bob", SenderID: "lineworks:u1", Body: "db postgres://user:pw@host/db", CreatedAt: t0},
	}
	blob := buildBatchBlob(msgs)
	if strings.Contains(blob, "sk-abcdef0123456789") {
		t.Error("api key must be redacted")
	}
	if strings.Contains(blob, "postgres://user:pw@host/db") {
		t.Error("connection string must be redacted")
	}
	if !strings.Contains(blob, "[REDACTED]") {
		t.Error("redaction marker missing")
	}
}

func TestBuildBatchBlob_KeepsPersonalContent(t *testing.T) {
	// PII like phone/email is legitimate user-memory content — NOT redacted.
	t0 := time.Now().UTC()
	msgs := []store.PendingMessage{
		{Sender: "Carol", SenderID: "lineworks:u1", Body: "call me at 0912-345-678 or carol@x.com", CreatedAt: t0},
	}
	blob := buildBatchBlob(msgs)
	if !strings.Contains(blob, "0912-345-678") || !strings.Contains(blob, "carol@x.com") {
		t.Errorf("personal content must be preserved, got %q", blob)
	}
}

func TestBuildBatchBlob_SkipsEmptyBodies(t *testing.T) {
	t0 := time.Now().UTC()
	msgs := []store.PendingMessage{
		{Sender: "A", SenderID: "lineworks:u1", Body: "   ", CreatedAt: t0},
		{Sender: "A", SenderID: "lineworks:u1", Body: "real", CreatedAt: t0},
	}
	blob := buildBatchBlob(msgs)
	if strings.Count(blob, "\n") != 1 {
		t.Errorf("blank body should be skipped, got %q", blob)
	}
}
