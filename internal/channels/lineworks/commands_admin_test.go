package lineworks

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// capturingClient records the text of every send so tests can assert on the
// channel-local reply that admin commands produce via c.SendText. It satisfies
// botClient (so c.client != nil, meaning replyCommand actually sends).
type capturingClient struct {
	mu   sync.Mutex
	sent []string
}

func (cc *capturingClient) SendTextToUser(_ context.Context, _, text string) error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.sent = append(cc.sent, text)
	return nil
}
func (cc *capturingClient) SendTextToChannel(_ context.Context, _, text string) error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.sent = append(cc.sent, text)
	return nil
}
func (cc *capturingClient) InvalidateToken() {}

func (cc *capturingClient) all() string {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return strings.Join(cc.sent, "\n")
}

// newAdminChannel builds a Channel wired with a capturing client (so replies are
// observable), an agent key, and a tenant id. Stores are left nil by default;
// individual tests inject fakes via the Set*Store setters.
func newAdminChannel(t *testing.T) (*Channel, *capturingClient) {
	t.Helper()
	mb := bus.New()
	t.Cleanup(mb.Close)

	cc := &capturingClient{}
	c := New(cc, Config{DMPolicy: "open", GroupPolicy: "open"}, mb)
	c.SetName("lineworks")
	c.SetAgentID("agent-key-1")
	c.SetTenantID(uuid.New())
	return c, cc
}

// --- Fakes (embed the interface so only the called methods need impls) ---

type fakeAgentStore struct {
	store.AgentStore
	id uuid.UUID
}

func (f fakeAgentStore) GetByKey(_ context.Context, _ string) (*store.AgentData, error) {
	return &store.AgentData{BaseModel: store.BaseModel{ID: f.id}}, nil
}

type fakeConfigPermStore struct {
	store.ConfigPermissionStore
	writers []store.ConfigPermission
}

func (f fakeConfigPermStore) ListFileWriters(_ context.Context, _ uuid.UUID, _ string) ([]store.ConfigPermission, error) {
	return f.writers, nil
}
func (f fakeConfigPermStore) List(_ context.Context, _ uuid.UUID, _, _ string) ([]store.ConfigPermission, error) {
	return f.writers, nil
}

type fakeTeamStore struct {
	store.TeamStore
	team  *store.TeamData
	tasks []store.TeamTaskData
}

func (f fakeTeamStore) GetTeamForAgent(_ context.Context, _ uuid.UUID) (*store.TeamData, error) {
	return f.team, nil
}
func (f fakeTeamStore) ListTasks(_ context.Context, _ uuid.UUID, _, _, _, _, _ string, _, _ int) ([]store.TeamTaskData, error) {
	return f.tasks, nil
}

// --- store-not-wired: graceful "unavailable", no panic ---

func TestAdmin_StoresNotWired_RepliesUnavailable(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		group   bool
		wantSub string
	}{
		{"writers", "/writers", true, "not available"},
		{"addwriter", "/addwriter userX", true, "not available"},
		{"removewriter", "/removewriter userX", true, "not available"},
		{"tasks", "/tasks", false, "not available"},
		{"task_detail", "/task_detail abc", false, "not available"},
		{"subagents", "/subagents", false, "not available"},
		{"subagent", "/subagent " + uuid.NewString(), false, "not available"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cc := newAdminChannel(t)
			var ev callbackEvent
			if tc.group {
				ev = groupEvent("grp1", "userA", tc.text)
			} else {
				ev = directEvent("userA", tc.text)
			}
			if handled := c.handleBotCommand(context.Background(), ev); !handled {
				t.Fatalf("%s: expected command to be handled", tc.text)
			}
			if got := cc.all(); !strings.Contains(strings.ToLower(got), tc.wantSub) {
				t.Fatalf("%s: expected reply containing %q, got %q", tc.text, tc.wantSub, got)
			}
		})
	}
}

// --- /writers lists writers from a wired fake store ---

func TestAdmin_WritersListsFromStore(t *testing.T) {
	c, cc := newAdminChannel(t)
	agentID := uuid.New()
	c.SetAgentStore(fakeAgentStore{id: agentID})
	c.SetConfigPermStore(fakeConfigPermStore{writers: []store.ConfigPermission{
		{UserID: "userA"},
		{UserID: "userB"},
	}})

	if handled := c.handleBotCommand(context.Background(), groupEvent("grp1", "userA", "/writers")); !handled {
		t.Fatal("expected /writers to be handled")
	}
	got := cc.all()
	if !strings.Contains(got, "userA") || !strings.Contains(got, "userB") {
		t.Fatalf("expected both writers listed, got %q", got)
	}
	if !strings.Contains(got, "(2)") {
		t.Fatalf("expected count (2) in header, got %q", got)
	}
}

// /writers only works in group chats.
func TestAdmin_WritersDirectChatRejected(t *testing.T) {
	c, cc := newAdminChannel(t)
	c.SetAgentStore(fakeAgentStore{id: uuid.New()})
	c.SetConfigPermStore(fakeConfigPermStore{})

	if handled := c.handleBotCommand(context.Background(), directEvent("userA", "/writers")); !handled {
		t.Fatal("expected /writers to be handled")
	}
	if got := cc.all(); !strings.Contains(got, "group chats") {
		t.Fatalf("expected group-only rejection, got %q", got)
	}
}

// --- /tasks lists tasks from a wired fake store ---

func TestAdmin_TasksListsFromStore(t *testing.T) {
	c, cc := newAdminChannel(t)
	agentID := uuid.New()
	c.SetAgentStore(fakeAgentStore{id: agentID})
	c.SetTeamStore(fakeTeamStore{
		team: &store.TeamData{BaseModel: store.BaseModel{ID: uuid.New()}, Name: "Builders"},
		tasks: []store.TeamTaskData{
			{BaseModel: store.BaseModel{ID: uuid.New()}, Subject: "Wire LIFF", Status: "in_progress"},
			{BaseModel: store.BaseModel{ID: uuid.New()}, Subject: "Ship release", Status: "pending"},
		},
	})

	if handled := c.handleBotCommand(context.Background(), groupEvent("grp1", "userA", "/tasks")); !handled {
		t.Fatal("expected /tasks to be handled")
	}
	got := cc.all()
	if !strings.Contains(got, "Builders") {
		t.Fatalf("expected team name in reply, got %q", got)
	}
	if !strings.Contains(got, "Wire LIFF") || !strings.Contains(got, "Ship release") {
		t.Fatalf("expected both task subjects listed, got %q", got)
	}
}

// /tasks with no team replies clearly.
func TestAdmin_TasksNoTeam(t *testing.T) {
	c, cc := newAdminChannel(t)
	c.SetAgentStore(fakeAgentStore{id: uuid.New()})
	c.SetTeamStore(fakeTeamStore{team: nil})

	if handled := c.handleBotCommand(context.Background(), groupEvent("grp1", "userA", "/tasks")); !handled {
		t.Fatal("expected /tasks to be handled")
	}
	if got := cc.all(); !strings.Contains(got, "not part of any team") {
		t.Fatalf("expected no-team reply, got %q", got)
	}
}

// /help advertises the Tier 2 admin commands.
func TestAdmin_HelpListsTier2(t *testing.T) {
	c, cc := newAdminChannel(t)
	if handled := c.handleBotCommand(context.Background(), directEvent("userA", "/help")); !handled {
		t.Fatal("expected /help to be handled")
	}
	got := cc.all()
	for _, want := range []string{"/addwriter", "/writers", "/tasks", "/task_detail", "/subagents", "/subagent"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected help to mention %q, got %q", want, got)
		}
	}
}
