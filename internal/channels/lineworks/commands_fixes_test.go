package lineworks

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// recordingPermStore is a pointer-based ConfigPermissionStore fake that answers
// CheckPermission/ListFileWriters from a preset writer set and records
// Grant/Revoke calls, so the writer-gate and writer-management policy branches
// can be asserted (the value-based fakeConfigPermStore cannot record mutations).
type recordingPermStore struct {
	store.ConfigPermissionStore
	writers  []store.ConfigPermission
	granted  []store.ConfigPermission
	revoked  []string
	checkErr error
}

func (f *recordingPermStore) ListFileWriters(_ context.Context, _ uuid.UUID, _ string) ([]store.ConfigPermission, error) {
	return f.writers, nil
}

func (f *recordingPermStore) CheckPermission(_ context.Context, _ uuid.UUID, _, _, userID string) (bool, error) {
	if f.checkErr != nil {
		return false, f.checkErr
	}
	for _, w := range f.writers {
		if w.UserID == userID {
			return true, nil
		}
	}
	return false, nil
}

func (f *recordingPermStore) Grant(_ context.Context, p *store.ConfigPermission) error {
	f.granted = append(f.granted, *p)
	return nil
}

func (f *recordingPermStore) Revoke(_ context.Context, _ uuid.UUID, _, _, userID string) error {
	f.revoked = append(f.revoked, userID)
	return nil
}

// FIX 2 regression: a bare "/reset" (no @mention) in a group with mention-gating
// ENABLED must still be dispatched as a command (command-first ordering), not
// recorded as discussion and dropped. The pre-fix code ran the mention gate
// before dispatch, so a bare command was silently swallowed.
func TestGate_BareGroupCommand_DispatchesWithGatingEnabled(t *testing.T) {
	c, mb := newGatingChannel(t, []string{"goclaw"}, true) // gating ENABLED (names resolved)
	c.SetName("lineworks")
	c.SetAgentID("agent-key-1")
	c.SetTenantID(uuid.New())

	c.handleMessageEvent(groupEvent("grp1", "userA", "/reset")) // no @goclaw

	msg, ok := consume(t, mb)
	if !ok {
		t.Fatal("expected bare /reset to dispatch even with mention-gating enabled")
	}
	if msg.Metadata[tools.MetaCommand] != "reset" {
		t.Fatalf("expected command=reset, got %q", msg.Metadata[tools.MetaCommand])
	}
	if entries := c.GroupHistory().GetEntries("grp1"); len(entries) != 0 {
		t.Fatalf("a command must not be recorded as discussion history, got %d", len(entries))
	}
}

// FIX 1: group /reset is writer-gated — only file writers may wipe the shared
// group session. A non-writer is denied (no publish); a writer succeeds. 1:1
// reset is never gated.
func TestGate_GroupReset_WriterGated(t *testing.T) {
	c, mb := newGatingChannel(t, nil, true) // names nil → mention gate off; isolate the writer gate
	c.SetName("lineworks")
	c.SetAgentID("agent-key-1")
	c.SetTenantID(uuid.New())
	c.SetAgentStore(fakeAgentStore{id: uuid.New()})
	c.SetConfigPermStore(&recordingPermStore{writers: []store.ConfigPermission{{UserID: "boss"}}})

	// Non-writer → denied, nothing published.
	c.handleMessageEvent(groupEvent("grp1", "userA", "/reset"))
	if _, ok := consume(t, mb); ok {
		t.Fatal("expected non-writer group /reset to be denied (no publish)")
	}

	// Writer → reset published.
	c.handleMessageEvent(groupEvent("grp1", "boss", "/reset"))
	msg, ok := consume(t, mb)
	if !ok {
		t.Fatal("expected writer group /reset to publish a reset command")
	}
	if msg.Metadata[tools.MetaCommand] != "reset" {
		t.Fatalf("expected command=reset, got %q", msg.Metadata[tools.MetaCommand])
	}

	// 1:1 reset is never writer-gated.
	c.handleMessageEvent(directEvent("randomUser", "/reset"))
	if _, ok := consume(t, mb); !ok {
		t.Fatal("expected 1:1 /reset to publish regardless of writer status")
	}
}

// FIX (weak coverage): the file-writer ACL mutation policy in handleWriterCommand
// — bootstrap, authorization gate, last-writer protection, usage, and the
// Grant/Revoke success paths.
func TestWriterCommand_PolicyBranches(t *testing.T) {
	newWriterChan := func(t *testing.T, ps *recordingPermStore) (*Channel, *capturingClient) {
		t.Helper()
		c, cc := newAdminChannel(t)
		c.SetAgentStore(fakeAgentStore{id: uuid.New()})
		c.SetConfigPermStore(ps)
		return c, cc
	}

	t.Run("empty list bootstrap add", func(t *testing.T) {
		ps := &recordingPermStore{} // no writers
		c, cc := newWriterChan(t, ps)
		if !c.handleBotCommand(context.Background(), groupEvent("grp1", "userA", "/addwriter userX")) {
			t.Fatal("expected /addwriter handled")
		}
		if len(ps.granted) != 1 || ps.granted[0].UserID != "userX" {
			t.Fatalf("expected Grant for userX, got %+v", ps.granted)
		}
		if !strings.Contains(cc.all(), "Added") {
			t.Fatalf("expected success reply, got %q", cc.all())
		}
	})

	t.Run("non-writer add rejected", func(t *testing.T) {
		ps := &recordingPermStore{writers: []store.ConfigPermission{{UserID: "userA"}}}
		c, cc := newWriterChan(t, ps)
		if !c.handleBotCommand(context.Background(), groupEvent("grp1", "userB", "/addwriter userY")) {
			t.Fatal("expected /addwriter handled")
		}
		if len(ps.granted) != 0 {
			t.Fatalf("expected no Grant by a non-writer, got %+v", ps.granted)
		}
		if !strings.Contains(cc.all(), "Only existing file writers") {
			t.Fatalf("expected authorization rejection, got %q", cc.all())
		}
	})

	t.Run("remove on empty list rejected", func(t *testing.T) {
		ps := &recordingPermStore{}
		c, cc := newWriterChan(t, ps)
		if !c.handleBotCommand(context.Background(), groupEvent("grp1", "userA", "/removewriter userY")) {
			t.Fatal("expected /removewriter handled")
		}
		if len(ps.revoked) != 0 {
			t.Fatalf("expected no Revoke, got %+v", ps.revoked)
		}
		if !strings.Contains(strings.ToLower(cc.all()), "no file writers") {
			t.Fatalf("expected empty-list rejection, got %q", cc.all())
		}
	})

	t.Run("cannot remove last writer", func(t *testing.T) {
		ps := &recordingPermStore{writers: []store.ConfigPermission{{UserID: "userA"}}}
		c, cc := newWriterChan(t, ps)
		if !c.handleBotCommand(context.Background(), groupEvent("grp1", "userA", "/removewriter userZ")) {
			t.Fatal("expected /removewriter handled")
		}
		if len(ps.revoked) != 0 {
			t.Fatalf("expected no Revoke for last writer, got %+v", ps.revoked)
		}
		if !strings.Contains(cc.all(), "Cannot remove the last") {
			t.Fatalf("expected last-writer protection, got %q", cc.all())
		}
	})

	t.Run("missing arg usage", func(t *testing.T) {
		ps := &recordingPermStore{writers: []store.ConfigPermission{{UserID: "userA"}}}
		c, cc := newWriterChan(t, ps)
		if !c.handleBotCommand(context.Background(), groupEvent("grp1", "userA", "/addwriter")) {
			t.Fatal("expected /addwriter handled")
		}
		if !strings.Contains(cc.all(), "Usage:") {
			t.Fatalf("expected usage reply, got %q", cc.all())
		}
	})
}
