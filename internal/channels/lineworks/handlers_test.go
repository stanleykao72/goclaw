package lineworks

import (
	"context"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

// fakeBotClient is a no-op botClient used so scheduleAck (which short-circuits
// when client==nil) actually arms its pending entry on the processed path.
type fakeBotClient struct{}

func (fakeBotClient) SendTextToUser(ctx context.Context, userID, text string) error       { return nil }
func (fakeBotClient) SendTextToChannel(ctx context.Context, channelID, text string) error { return nil }
func (fakeBotClient) InvalidateToken()                                                     {}

// newGatingChannel builds a Channel wired for mention-gating tests: a real bus
// (so HandleMessage publishes observably), a RAM-only group history, mention
// required, and the provided bot names. A nil-name slice leaves gating disabled.
func newGatingChannel(t *testing.T, botNames []string, withClient bool) (*Channel, *bus.MessageBus) {
	t.Helper()
	mb := bus.New()
	t.Cleanup(mb.Close)

	var client botClient
	if withClient {
		client = fakeBotClient{}
	}
	c := New(client, Config{DMPolicy: "open", GroupPolicy: "open"}, mb)
	c.SetGroupHistory(channels.NewPendingHistory())
	c.SetHistoryLimit(channels.DefaultGroupHistoryLimit)
	c.SetRequireMention(true)
	c.SetBotNames(botNames)
	return c, mb
}

// consume drains one inbound message with a short timeout; ok=false on timeout.
func consume(t *testing.T, mb *bus.MessageBus) (bus.InboundMessage, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	return mb.ConsumeInbound(ctx)
}

func groupEvent(chatID, userID, text string) callbackEvent {
	return callbackEvent{
		Type:    eventTypeMessage,
		Source:  callbackSource{UserID: userID, ChannelID: chatID},
		Content: callbackContent{Type: contentTypeText, Text: text},
	}
}

func directEvent(userID, text string) callbackEvent {
	return callbackEvent{
		Type:    eventTypeMessage,
		Source:  callbackSource{UserID: userID},
		Content: callbackContent{Type: contentTypeText, Text: text},
	}
}

func ackPendingLen(c *Channel) int {
	n := 0
	c.ackPending.Range(func(_, _ any) bool { n++; return true })
	return n
}

// Group message without a mention: NOT forwarded to the agent, NO ack armed,
// but recorded into group history.
func TestGroup_NoMention_RecordsAndStops(t *testing.T) {
	c, mb := newGatingChannel(t, []string{"goclaw"}, true)

	c.handleMessageEvent(groupEvent("grp1", "userA", "hello team, lunch?"))

	if _, ok := consume(t, mb); ok {
		t.Fatal("expected NO inbound publish for a non-mention group message")
	}
	if n := ackPendingLen(c); n != 0 {
		t.Fatalf("expected no pending ack, got %d", n)
	}
	entries := c.GroupHistory().GetEntries("grp1")
	if len(entries) != 1 {
		t.Fatalf("expected 1 recorded history entry, got %d", len(entries))
	}
	if entries[0].Body != "hello team, lunch?" {
		t.Fatalf("unexpected recorded body: %q", entries[0].Body)
	}
	if entries[0].SenderID != senderPrefix+"userA" {
		t.Fatalf("unexpected recorded senderID: %q", entries[0].SenderID)
	}
}

// Group message WITH a mention: forwarded to the agent, the leading "@bot" is
// stripped, prior non-mention history is folded in via BuildContext, and an ack
// is armed.
func TestGroup_Mention_ProcessedAndStripped(t *testing.T) {
	c, mb := newGatingChannel(t, []string{"goclaw"}, true)

	// Seed a prior non-mention message so BuildContext has context to fold in.
	c.handleMessageEvent(groupEvent("grp1", "userA", "the door is broken"))
	if _, ok := consume(t, mb); ok {
		t.Fatal("seed message should not have been forwarded")
	}

	c.handleMessageEvent(groupEvent("grp1", "userB", "@goclaw please open a ticket"))

	msg, ok := consume(t, mb)
	if !ok {
		t.Fatal("expected the mention message to be forwarded to the agent")
	}
	if msg.PeerKind != peerGroup {
		t.Fatalf("expected peerKind=%q, got %q", peerGroup, msg.PeerKind)
	}
	// Leading "@goclaw" must be stripped from the user portion.
	if contains(msg.Content, "@goclaw") {
		t.Fatalf("expected @goclaw to be stripped, content=%q", msg.Content)
	}
	if !contains(msg.Content, "please open a ticket") {
		t.Fatalf("expected stripped text retained, content=%q", msg.Content)
	}
	// BuildContext should have folded in the prior non-mention message.
	if !contains(msg.Content, "the door is broken") {
		t.Fatalf("expected prior history folded in, content=%q", msg.Content)
	}
	if !contains(msg.Content, "[From: userB]") {
		t.Fatalf("expected sender annotation, content=%q", msg.Content)
	}
	if n := ackPendingLen(c); n != 1 {
		t.Fatalf("expected 1 pending ack on processed path, got %d", n)
	}
}

// 1:1 chats are never gated: processed without any mention, text unchanged.
func TestDirect_ProcessedWithoutMention(t *testing.T) {
	c, mb := newGatingChannel(t, []string{"goclaw"}, true)

	c.handleMessageEvent(directEvent("userA", "what is my schedule today?"))

	msg, ok := consume(t, mb)
	if !ok {
		t.Fatal("expected the 1:1 message to be forwarded")
	}
	if msg.PeerKind != peerDirect {
		t.Fatalf("expected peerKind=%q, got %q", peerDirect, msg.PeerKind)
	}
	if msg.Content != "what is my schedule today?" {
		t.Fatalf("expected raw text for 1:1, got %q", msg.Content)
	}
}

// An i18n bot-name variant must also match an @-mention.
func TestGroup_Mention_I18nVariantMatches(t *testing.T) {
	c, mb := newGatingChannel(t, []string{"goclaw", "ゴークロウ"}, true)

	c.handleMessageEvent(groupEvent("grp1", "userA", "@ゴークロウ おはよう"))

	msg, ok := consume(t, mb)
	if !ok {
		t.Fatal("expected the i18n-name mention to be forwarded")
	}
	if contains(msg.Content, "@ゴークロウ") {
		t.Fatalf("expected i18n mention stripped, content=%q", msg.Content)
	}
	if !contains(msg.Content, "おはよう") {
		t.Fatalf("expected remainder retained, content=%q", msg.Content)
	}
}

// Fail-safe: when no bot name is resolved, gating is DISABLED — group messages
// are processed normally instead of being silently dropped.
func TestGroup_UnresolvedBotName_FailSafeProcesses(t *testing.T) {
	c, mb := newGatingChannel(t, nil, true) // no bot names → gating disabled

	c.handleMessageEvent(groupEvent("grp1", "userA", "no mention here"))

	msg, ok := consume(t, mb)
	if !ok {
		t.Fatal("expected fail-safe processing (bot must not go silent) when bot names unresolved")
	}
	if msg.Content != "no mention here" {
		t.Fatalf("expected raw text passthrough on fail-safe path, got %q", msg.Content)
	}
	// Nothing should have been recorded to history on the fail-safe path.
	if entries := c.GroupHistory().GetEntries("grp1"); len(entries) != 0 {
		t.Fatalf("expected no history recording on fail-safe path, got %d", len(entries))
	}
}

// contains is a tiny strings.Contains shim kept local to avoid an import just
// for assertions.
func contains(haystack, needle string) bool {
	return len(needle) == 0 || indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
