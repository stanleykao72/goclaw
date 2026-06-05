package lineworks

import (
	"testing"

	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// newCommandChannel builds a Channel wired for command-dispatch tests: a real
// bus, a fake client (so replies don't no-op and the processed path arms its
// ack), gating disabled (commands must work with or without resolved bot names),
// plus an agent key and tenant id so published commands carry routing fields.
func newCommandChannel(t *testing.T) (*Channel, *bus.MessageBus, uuid.UUID) {
	t.Helper()
	mb := bus.New()
	t.Cleanup(mb.Close)

	c := New(fakeBotClient{}, Config{DMPolicy: "open", GroupPolicy: "open"}, mb)
	c.SetName("lineworks")
	c.SetAgentID("agent-key-1")
	tenant := uuid.New()
	c.SetTenantID(tenant)
	return c, mb, tenant
}

// /new is an alias of /reset: both publish a command InboundMessage with
// Metadata[command]="reset" and do NOT reach the agent path.
func TestCommand_NewIsResetAlias(t *testing.T) {
	for _, cmd := range []string{"/reset", "/new"} {
		c, mb, _ := newCommandChannel(t)
		c.handleMessageEvent(directEvent("userA", cmd))

		msg, ok := consume(t, mb)
		if !ok {
			t.Fatalf("%s: expected a command publish", cmd)
		}
		if got := msg.Metadata[tools.MetaCommand]; got != "reset" {
			t.Fatalf("%s: expected command=reset, got %q", cmd, got)
		}
		// A command must not arm an agent ack.
		if n := ackPendingLen(c); n != 0 {
			t.Fatalf("%s: expected no pending ack for a command, got %d", cmd, n)
		}
	}
}

// A published command carries the routing fields the consumer needs to build a
// scoped session key (AgentID, PeerKind, ChatID, TenantID, SenderID/UserID).
func TestCommand_PublishCarriesRoutingFields(t *testing.T) {
	c, mb, tenant := newCommandChannel(t)

	c.handleMessageEvent(groupEvent("grp1", "userA", "/reset"))

	msg, ok := consume(t, mb)
	if !ok {
		t.Fatal("expected a command publish for a group /reset")
	}
	if msg.AgentID != "agent-key-1" {
		t.Fatalf("expected AgentID=agent-key-1, got %q", msg.AgentID)
	}
	if msg.PeerKind != peerGroup {
		t.Fatalf("expected PeerKind=%q, got %q", peerGroup, msg.PeerKind)
	}
	if msg.ChatID != "grp1" {
		t.Fatalf("expected ChatID=grp1, got %q", msg.ChatID)
	}
	if msg.TenantID != tenant {
		t.Fatalf("expected TenantID=%v, got %v", tenant, msg.TenantID)
	}
	if msg.SenderID != senderPrefix+"userA" {
		t.Fatalf("expected SenderID=%q, got %q", senderPrefix+"userA", msg.SenderID)
	}
	if msg.UserID != "userA" {
		t.Fatalf("expected UserID=userA, got %q", msg.UserID)
	}
	if msg.Channel != "lineworks" {
		t.Fatalf("expected Channel=lineworks, got %q", msg.Channel)
	}
}

// /stop publishes command="stop"; /stopall publishes command="stopall".
func TestCommand_StopAndStopAll(t *testing.T) {
	cases := []struct {
		text    string
		command string
	}{
		{"/stop", "stop"},
		{"/stopall", "stopall"},
	}
	for _, tc := range cases {
		c, mb, _ := newCommandChannel(t)
		c.handleMessageEvent(directEvent("userA", tc.text))

		msg, ok := consume(t, mb)
		if !ok {
			t.Fatalf("%s: expected a command publish", tc.text)
		}
		if got := msg.Metadata[tools.MetaCommand]; got != tc.command {
			t.Fatalf("%s: expected command=%q, got %q", tc.text, tc.command, got)
		}
	}
}

// /help is a channel-local reply: it must NOT publish to the bus, and its text
// must not advertise commands that are out of scope (/reactions, /start).
func TestCommand_HelpIsLocalAndExcludesOutOfScope(t *testing.T) {
	c, mb, _ := newCommandChannel(t)

	if !c.handleBotCommand(c.hookContext(), directEvent("userA", "/help")) {
		t.Fatal("expected /help to be handled")
	}
	if _, ok := consume(t, mb); ok {
		t.Fatal("expected /help to NOT publish to the bus")
	}
	help := lineWorksHelpText()
	if contains(help, "/reactions") {
		t.Fatalf("/help must not list /reactions: %q", help)
	}
	if contains(help, "/start") {
		t.Fatalf("/help must not list /start: %q", help)
	}
	// Sanity: the in-scope commands are listed.
	for _, want := range []string{"/new", "/reset", "/stop", "/stopall", "/help", "/status"} {
		if !contains(help, want) {
			t.Fatalf("/help should list %q: %q", want, help)
		}
	}
}

// /status is channel-local: it must NOT publish to the bus (no agent path).
func TestCommand_StatusDoesNotInvokeAgent(t *testing.T) {
	c, mb, _ := newCommandChannel(t)

	if !c.handleBotCommand(c.hookContext(), directEvent("userA", "/status")) {
		t.Fatal("expected /status to be handled")
	}
	if _, ok := consume(t, mb); ok {
		t.Fatal("expected /status to NOT publish to the bus")
	}
	if n := ackPendingLen(c); n != 0 {
		t.Fatalf("expected no pending ack for /status, got %d", n)
	}
}

// An unknown "/foo" command is not swallowed: handleBotCommand returns false so
// it falls through to the agent path.
func TestCommand_UnknownPassesThrough(t *testing.T) {
	c, _, _ := newCommandChannel(t)

	if c.handleBotCommand(c.hookContext(), directEvent("userA", "/foo bar")) {
		t.Fatal("expected unknown /foo to NOT be handled (must pass through to agent)")
	}
	// And via the full handler it should reach the agent path (be published).
	c2, mb2, _ := newCommandChannel(t)
	c2.handleMessageEvent(directEvent("userA", "/foo bar"))
	msg, ok := consume(t, mb2)
	if !ok {
		t.Fatal("expected unknown command to fall through to the agent path")
	}
	if msg.Metadata[tools.MetaCommand] != "" {
		t.Fatalf("expected no command metadata for an unknown command, got %q", msg.Metadata[tools.MetaCommand])
	}
	if msg.Content != "/foo bar" {
		t.Fatalf("expected raw content passthrough, got %q", msg.Content)
	}
}

// A command addressed by name ("/help@goclaw") and one prefixed by a leading
// mention ("@goclaw /reset") are both recognized.
func TestCommand_NameSuffixAndLeadingMention(t *testing.T) {
	// "/help@goclaw" → handled locally, no publish.
	c, mb, _ := newCommandChannel(t)
	c.SetBotNames([]string{"goclaw"})
	if !c.handleBotCommand(c.hookContext(), directEvent("userA", "/help@goclaw")) {
		t.Fatal("expected /help@goclaw to be handled")
	}
	if _, ok := consume(t, mb); ok {
		t.Fatal("expected /help@goclaw to NOT publish")
	}

	// "@goclaw /reset" → leading mention stripped, recognized as /reset.
	c2, mb2, _ := newCommandChannel(t)
	c2.SetBotNames([]string{"goclaw"})
	if !c2.handleBotCommand(c2.hookContext(), directEvent("userA", "@goclaw /reset")) {
		t.Fatal("expected @goclaw /reset to be handled")
	}
	msg, ok := consume(t, mb2)
	if !ok {
		t.Fatal("expected @goclaw /reset to publish a reset command")
	}
	if msg.Metadata[tools.MetaCommand] != "reset" {
		t.Fatalf("expected command=reset, got %q", msg.Metadata[tools.MetaCommand])
	}
}
