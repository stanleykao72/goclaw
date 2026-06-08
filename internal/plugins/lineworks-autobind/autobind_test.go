package lineworksautobind

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	channel "github.com/nextlevelbuilder/goclaw/internal/channels/lineworks"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// gate is a test helper: run the gate for a LINE WORKS userId.
func gate(h *Hook, userID string) (allow bool, reply string) {
	return h.Gate(context.Background(), channel.TextEvent{UserID: userID})
}

func TestParseEmployeeID(t *testing.T) {
	cases := []struct {
		key, prefix string
		want        int
		wantErr     bool
	}{
		{"odoo-emp-42", "odoo-emp-", 42, false},
		{"odoo-emp-1", "odoo-emp-", 1, false},
		{"42", "", 42, false},
		{"odoo-emp-0", "odoo-emp-", 0, true},
		{"odoo-emp-x", "odoo-emp-", 0, true},
		{"foreign-9", "odoo-emp-", 0, true},
		{"", "odoo-emp-", 0, true},
		{"  odoo-emp-7 ", "odoo-emp-", 7, false},
	}
	for _, c := range cases {
		got, err := parseEmployeeID(c.key, c.prefix)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseEmployeeID(%q,%q): want error, got %d", c.key, c.prefix, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("parseEmployeeID(%q,%q) = %d,%v; want %d,nil", c.key, c.prefix, got, err, c.want)
		}
	}
}

func TestDeriveBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://odoo-esmith.odoo.com/mcp/v1/message": "https://odoo-esmith.odoo.com",
		"https://h.example.com/mcp/v2/x":              "https://h.example.com",
		"https://h.example.com/":                      "https://h.example.com",
		"https://h.example.com":                       "https://h.example.com",
	}
	for in, want := range cases {
		if got := deriveBaseURL(in); got != want {
			t.Errorf("deriveBaseURL(%q) = %q; want %q", in, got, want)
		}
	}
}

// fakeCredStore is an in-memory CredentialStore.
type fakeCredStore struct {
	mu sync.Mutex
	m  map[string]store.MCPUserCredentials
}

func newFakeCredStore() *fakeCredStore {
	return &fakeCredStore{m: map[string]store.MCPUserCredentials{}}
}

func (f *fakeCredStore) key(serverID uuid.UUID, userID string) string {
	return serverID.String() + "|" + userID
}

func (f *fakeCredStore) GetUserCredentials(_ context.Context, serverID uuid.UUID, userID string) (*store.MCPUserCredentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.m[f.key(serverID, userID)]; ok {
		cp := c
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeCredStore) SetUserCredentials(_ context.Context, serverID uuid.UUID, userID string, creds store.MCPUserCredentials) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[f.key(serverID, userID)] = creds
	return nil
}

type stubDir struct {
	privateEmail string
	externalKey  string
	err          error
}

func (s stubDir) ResolveUser(context.Context, string) (*DirUser, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &DirUser{
		UserID:       "lw-uid",
		Name:         "高玉明",
		Email:        "stanleykao72@esmith",
		PrivateEmail: s.privateEmail,
		ExternalKey:  s.externalKey,
	}, nil
}

// fakeAgentStore captures SetUserContextFile calls.
type fakeAgentStore struct {
	mu    sync.Mutex
	files map[string]string // "agentID|userID|fileName" -> content
}

func newFakeAgentStore() *fakeAgentStore { return &fakeAgentStore{files: map[string]string{}} }

func (f *fakeAgentStore) SetUserContextFile(_ context.Context, agentID uuid.UUID, userID, fileName, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[agentID.String()+"|"+userID+"|"+fileName] = content
	return nil
}

// odooStub serves the MCP search and the openclaw bind/verify endpoints.
func odooStub(t *testing.T, login, code, apiKey string) *httptest.Server {
	t.Helper()
	return odooStubLang(t, login, "", code, apiKey)
}

// odooStubLang is odooStub with an explicit res.users.lang returned from the
// MCP search (so lookupLang sees a value). Each MCP search row carries both
// login and lang; lookupLogin/lookupLang each read only their own field.
func odooStubLang(t *testing.T, login, lang, code, apiKey string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/v1/message", func(w http.ResponseWriter, r *http.Request) {
		// search_records res.users → [{login, lang}]
		rows := []map[string]any{{"login": login, "lang": lang}}
		structured, _ := json.Marshal(rows)
		resp := map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"structuredContent": json.RawMessage(structured)},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/api/openclaw/user/bind", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "code": code})
	})
	mux.HandleFunc("/api/openclaw/user/verify", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "api_key": apiKey,
			"user": map[string]any{"id": 6, "login": "stanleykao72@gmail.com", "name": "高玉明"},
		})
	})
	return httptest.NewServer(mux)
}

func TestProvision_PrivateEmailPrimary(t *testing.T) {
	srv := odooStub(t, "", "123456", "deadbeefkey") // MCP not consulted on this path
	defer srv.Close()

	creds := newFakeCredStore()
	agents := newFakeAgentStore()
	serverID := uuid.New()
	agentID := uuid.New()
	h := New(Config{
		Directory: stubDir{privateEmail: "stanleykao72@gmail.com"}, // no externalKey
		Creds:     creds,
		ServerID:  serverID,
		MCPURL:    srv.URL + "/mcp/v1/message",
		MCPToken:  "gateway-tok",
		Agents:    agents,
		AgentID:   agentID,
	})

	allow, reply := gate(h, "lwUID-1") // DM (ChannelID empty)
	if !allow {
		t.Fatalf("expected allow for staff via privateEmail, got block reply=%q", reply)
	}
	if !strings.Contains(reply, "已完成身分綁定") {
		t.Errorf("expected bind-success confirmation reply, got %q", reply)
	}

	got, _ := creds.GetUserCredentials(context.Background(), serverID, "lineworks:lwUID-1")
	if got == nil || got.APIKey != "deadbeefkey" {
		t.Fatalf("expected stored api_key 'deadbeefkey' via privateEmail, got %+v", got)
	}
	// identity context file written for the agent + user
	doc := agents.files[agentID.String()+"|lineworks:lwUID-1|identity.md"]
	if doc == "" {
		t.Fatal("expected identity.md context file to be written")
	}
	for _, want := range []string{"高玉明", "stanleykao72@gmail.com", "res.users id：6"} {
		if !strings.Contains(doc, want) {
			t.Errorf("identity doc missing %q; got:\n%s", want, doc)
		}
	}
}

func TestProvision_StoresOdooLang(t *testing.T) {
	// verify returns user id=6; lookupLang searches res.users(id=6) → lang.
	srv := odooStubLang(t, "stanleykao72@gmail.com", "zh_TW", "123456", "langkey")
	defer srv.Close()

	creds := newFakeCredStore()
	serverID := uuid.New()
	h := New(Config{
		Directory: stubDir{privateEmail: "stanleykao72@gmail.com"},
		Creds:     creds,
		ServerID:  serverID,
		MCPURL:    srv.URL + "/mcp/v1/message",
		MCPToken:  "gateway-tok",
	})

	if allow, _ := gate(h, "lwUID-lang"); !allow {
		t.Fatal("expected allow for staff with lang lookup")
	}

	got, _ := creds.GetUserCredentials(context.Background(), serverID, "lineworks:lwUID-lang")
	if got == nil || got.APIKey != "langkey" {
		t.Fatalf("expected stored api_key 'langkey', got %+v", got)
	}
	if got.Env == nil || got.Env["odoo_lang"] != "zh_TW" {
		t.Fatalf("expected Env[odoo_lang]=zh_TW, got Env=%+v", got.Env)
	}
}

func TestProvision_NoLangNoEnv(t *testing.T) {
	// res.users.lang empty → credential stored without an Env map (unchanged
	// behavior on lookup miss).
	srv := odooStubLang(t, "stanleykao72@gmail.com", "", "123456", "nolangkey")
	defer srv.Close()

	creds := newFakeCredStore()
	serverID := uuid.New()
	h := New(Config{
		Directory: stubDir{privateEmail: "stanleykao72@gmail.com"},
		Creds:     creds,
		ServerID:  serverID,
		MCPURL:    srv.URL + "/mcp/v1/message",
		MCPToken:  "gateway-tok",
	})

	if allow, _ := gate(h, "lwUID-nolang"); !allow {
		t.Fatal("expected allow for staff without lang")
	}

	got, _ := creds.GetUserCredentials(context.Background(), serverID, "lineworks:lwUID-nolang")
	if got == nil || got.APIKey != "nolangkey" {
		t.Fatalf("expected stored api_key 'nolangkey', got %+v", got)
	}
	if got.Env != nil {
		t.Fatalf("expected no Env on empty lang, got Env=%+v", got.Env)
	}
}

// A user bound before lang capture existed (credential has APIKey but no Env)
// gets odoo_lang backfilled on their next message via the already-bound path,
// WITHOUT re-binding, preserving the existing api_key.
func TestBackfill_AlreadyBoundUserGetsLang(t *testing.T) {
	srv := odooStubLang(t, "stanleykao72@gmail.com", "zh_TW", "123456", "ignored")
	defer srv.Close()

	creds := newFakeCredStore()
	serverID := uuid.New()
	// Pre-existing binding: api_key present, no Env (no stored lang).
	_ = creds.SetUserCredentials(context.Background(), serverID, "lineworks:lwUID-old",
		store.MCPUserCredentials{APIKey: "preexisting"})

	h := New(Config{
		Directory: stubDir{privateEmail: "stanleykao72@gmail.com"},
		Creds:     creds,
		ServerID:  serverID,
		MCPURL:    srv.URL + "/mcp/v1/message",
		MCPToken:  "gateway-tok",
	})

	if allow, _ := gate(h, "lwUID-old"); !allow {
		t.Fatal("expected already-bound user to be allowed")
	}

	got, _ := creds.GetUserCredentials(context.Background(), serverID, "lineworks:lwUID-old")
	if got == nil || got.APIKey != "preexisting" {
		t.Fatalf("backfill must preserve the existing api_key, got %+v", got)
	}
	if got.Env == nil || got.Env["odoo_lang"] != "zh_TW" {
		t.Fatalf("expected backfilled Env[odoo_lang]=zh_TW, got Env=%+v", got.Env)
	}
}

func TestProvision_ExternalKeyFallback(t *testing.T) {
	srv := odooStub(t, "suci@esmith.com", "123456", "fallbackkey")
	defer srv.Close()

	creds := newFakeCredStore()
	serverID := uuid.New()
	h := New(Config{
		Directory: stubDir{externalKey: "odoo-emp-42"}, // no privateEmail → MCP lookup
		Creds:     creds,
		ServerID:  serverID,
		MCPURL:    srv.URL + "/mcp/v1/message",
		MCPToken:  "gateway-tok",
	})

	if allow, _ := gate(h, "lwUID-1"); !allow {
		t.Fatal("expected allow for staff via externalKey fallback")
	}

	got, _ := creds.GetUserCredentials(context.Background(), serverID, "lineworks:lwUID-1")
	if got == nil || got.APIKey != "fallbackkey" {
		t.Fatalf("expected stored api_key 'fallbackkey' via externalKey fallback, got %+v", got)
	}
}

func TestProvision_ExistingCredentialNotReminted(t *testing.T) {
	creds := newFakeCredStore()
	agents := newFakeAgentStore()
	serverID := uuid.New()
	agentID := uuid.New()
	// Pre-seed a credential (simulates an already-bound user).
	_ = creds.SetUserCredentials(context.Background(), serverID, "lineworks:lwUID-2", store.MCPUserCredentials{APIKey: "existing"})

	h := New(Config{
		// privateEmail set → bindID resolves without any MCP/openclaw call.
		Directory: stubDir{privateEmail: "stanleykao72@gmail.com"},
		Creds:     creds,
		ServerID:  serverID,
		// bind/verify point nowhere; with an existing credential they must NOT
		// be hit (no re-mint of an already-bound uid).
		OdooBaseURL: "http://127.0.0.1:0",
		MCPURL:      "http://127.0.0.1:0/mcp/v1/message",
		MCPToken:    "t",
		Agents:      agents,
		AgentID:     agentID,
	})

	if allow, _ := gate(h, "lwUID-2"); !allow {
		t.Fatal("expected allow for already-bound staff")
	}

	// Existing credential is preserved (not clobbered by a re-mint).
	got, _ := creds.GetUserCredentials(context.Background(), serverID, "lineworks:lwUID-2")
	if got == nil || got.APIKey != "existing" {
		t.Fatalf("existing credential was clobbered: %+v", got)
	}
	// Identity context is still (re)written even though we did not re-mint.
	if agents.files[agentID.String()+"|lineworks:lwUID-2|identity.md"] == "" {
		t.Error("expected identity.md to be backfilled for an already-bound user")
	}
}

func TestGate_NonStaffBlocked(t *testing.T) {
	creds := newFakeCredStore()
	serverID := uuid.New()
	h := New(Config{
		Directory: stubDir{externalKey: "foreign-key"}, // no privateEmail, no odoo-emp- prefix
		Creds:     creds,
		ServerID:  serverID,
		MCPURL:    "http://127.0.0.1:0/mcp/v1/message",
		MCPToken:  "t",
	})

	allow, reply := gate(h, "lwUID-3")
	if allow {
		t.Fatal("expected non-staff sender to be blocked")
	}
	if !strings.Contains(reply, "僅限") {
		t.Errorf("expected non-staff hint reply, got %q", reply)
	}

	if got, _ := creds.GetUserCredentials(context.Background(), serverID, "lineworks:lwUID-3"); got != nil {
		t.Fatalf("non-employee sender should not get a credential, got %+v", got)
	}
}

func TestGate_GroupUnboundPromptsDM(t *testing.T) {
	creds := newFakeCredStore()
	serverID := uuid.New()
	h := New(Config{
		Directory:   stubDir{privateEmail: "stanleykao72@gmail.com"}, // staff, but not bound yet
		Creds:       creds,
		ServerID:    serverID,
		OdooBaseURL: "http://127.0.0.1:0", // bind/verify must NOT be hit in a group
		MCPURL:      "http://127.0.0.1:0/mcp/v1/message",
		MCPToken:    "t",
	})

	// ChannelID set → group message.
	allow, reply := h.Gate(context.Background(), channel.TextEvent{UserID: "lwUID-4", ChannelID: "grp-1"})
	if allow {
		t.Fatal("expected unbound staff in a group to be blocked")
	}
	if !strings.Contains(reply, "私訊") {
		t.Errorf("expected a DM-bind prompt, got %q", reply)
	}
	// No credential minted in the group path.
	if got, _ := creds.GetUserCredentials(context.Background(), serverID, "lineworks:lwUID-4"); got != nil {
		t.Fatalf("group path must not mint a credential, got %+v", got)
	}
}

func TestGate_GroupAdmittedAfterBind(t *testing.T) {
	creds := newFakeCredStore()
	serverID := uuid.New()
	h := New(Config{
		Directory:   stubDir{privateEmail: "stanleykao72@gmail.com"},
		Creds:       creds,
		ServerID:    serverID,
		OdooBaseURL: "http://127.0.0.1:0",
		MCPURL:      "http://127.0.0.1:0/mcp/v1/message",
		MCPToken:    "t",
	})
	groupEv := channel.TextEvent{UserID: "lwUID-5", ChannelID: "grp-1"}

	// 1. Group message while unbound → blocked with DM prompt (cached).
	if allow, reply := h.Gate(context.Background(), groupEv); allow || reply == "" {
		t.Fatalf("expected unbound group block with prompt, got allow=%v reply=%q", allow, reply)
	}

	// 2. User binds in a DM (simulated: credential now exists).
	_ = creds.SetUserCredentials(context.Background(), serverID, "lineworks:lwUID-5", store.MCPUserCredentials{APIKey: "k"})

	// 3. Next group message must be admitted immediately (cached block busted by
	//    the live credential re-check), not wait out the cache TTL.
	if allow, _ := h.Gate(context.Background(), groupEv); !allow {
		t.Fatal("expected group message to be admitted right after DM bind")
	}
}
