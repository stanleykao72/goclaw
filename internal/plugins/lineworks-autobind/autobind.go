// Package lineworksautobind provisions per-user Odoo MCP credentials for LINE
// WORKS senders automatically, so the agent's MCP tool calls run as the
// individual employee (enabling Odoo `self` row-scope) instead of a shared
// gateway identity.
//
// Mechanism (no user interaction, no /bind command):
//
//	inbound LINE WORKS message
//	 → resolve sender userId → directory userExternalKey ("odoo-emp-{id}")
//	 → Odoo MCP search res.users(employee_id) → login
//	 → POST {odoo}/api/openclaw/user/bind(login)      → 6-digit code (returned to gateway)
//	 → POST {odoo}/api/openclaw/user/verify(code)     → per-user api_key
//	 → store mcp_user_credentials[serverID, "lineworks:<uid>"] = api_key
//
// Consumption is already wired in internal/mcp/manager.go: when a per-user
// credential exists it overrides the server-level Authorization header. This
// hook only POPULATES that row.
//
// The identity source is the externalKey stamped by the directory sync — an
// org-managed, deterministic mapping — NOT a self-reported email, so a sender
// cannot bind to another employee's identity.
//
// All work is best-effort and silent: provisioning failures are logged but
// never surface to the chat (the bot still answers via the shared identity).
// Results are cached in memory (success and negative) so a steady-state
// conversation does no extra Odoo round-trips.
package lineworksautobind

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	channel "github.com/nextlevelbuilder/goclaw/internal/channels/lineworks"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

const (
	httpTimeout        = 15 * time.Second
	defaultPrefix      = "odoo-emp-"
	defaultSuccessTTL  = 12 * time.Hour // re-verify the store occasionally in case a cred is revoked
	defaultNegativeTTL = 30 * time.Minute
	senderPrefix       = "lineworks:" // matches the lineworks channel sender id namespace

	groupRecheckTTL = 2 * time.Minute // group-unbound: recheck soon so a just-DM-bound user is admitted quickly

	// defaultHint: replied to a sender we cannot map to any staff Odoo identity.
	defaultHint = "您好,本助理僅限已綁定的承暉同仁使用。若您是同仁,請聯絡系統管理員協助綁定您的 LINE WORKS 帳號。"
	// defaultGroupUnbound: replied in a GROUP to a staff member not yet bound —
	// directs them to do a 1:1 bind first.
	defaultGroupUnbound = "您好,您尚未完成綁定。請先「私訊」我(1:1 聊天)傳一則訊息完成身分綁定,綁定後即可在群組中使用本助理。"
)

// DirUser is the LINE WORKS directory identity the hook consumes (one API
// call). PrivateEmail (個人電子郵件地址) equals the Odoo res.users login and is
// the primary bind key; ExternalKey ("odoo-emp-{id}") is the fallback. Name and
// Email are carried so the identity context file can record the LINE WORKS side
// of the binding too.
type DirUser struct {
	UserID       string
	Name         string // LINE WORKS display name (e.g. 高玉明)
	Email        string // LINE WORKS account id ("<local>@<domain>")
	PrivateEmail string // 個人電子郵件地址 == Odoo login
	ExternalKey  string // "odoo-emp-{id}" (sync'd accounts only)
}

// DirectoryResolver resolves a LINE WORKS resource userId to its DirUser via a
// single Directory API call. The internal/lineworks SDK (wrapped by an adapter
// in cmd) satisfies this.
type DirectoryResolver interface {
	ResolveUser(ctx context.Context, userID string) (*DirUser, error)
}

// AgentContextStore writes the per-user identity context file the agent loads
// each turn (so the LLM knows who "我" is). store.AgentStore satisfies it.
type AgentContextStore interface {
	SetUserContextFile(ctx context.Context, agentID uuid.UUID, userID, fileName, content string) error
}

// CredentialStore is the subset of store.MCPServerStore the hook needs to read
// and write per-user MCP credentials.
type CredentialStore interface {
	GetUserCredentials(ctx context.Context, serverID uuid.UUID, userID string) (*store.MCPUserCredentials, error)
	SetUserCredentials(ctx context.Context, serverID uuid.UUID, userID string, creds store.MCPUserCredentials) error
}

// Config holds everything the autobind hook needs. Required: Directory, Creds,
// ServerID, MCPURL, MCPToken, OdooBaseURL. Optional fields fall back to
// package defaults.
type Config struct {
	// Directory resolves LINE WORKS userId → userExternalKey.
	Directory DirectoryResolver
	// Creds reads/writes mcp_user_credentials.
	Creds CredentialStore
	// ServerID is the odoo MCP server row the credential is stored under (the
	// same server the agent calls — typically "odoo-prod").
	ServerID uuid.UUID
	// MCPURL is the Odoo MCP JSON-RPC endpoint, used for the res.users lookup.
	MCPURL string
	// MCPToken is the gateway bearer token for MCPURL and the openclaw API.
	MCPToken string
	// OdooBaseURL is the Odoo web origin (e.g. https://odoo-esmith.odoo.com)
	// the openclaw bind/verify endpoints live under. If empty it is derived
	// from MCPURL by stripping the "/mcp/..." suffix.
	OdooBaseURL string
	// ExternalKeyPrefix is the directory externalKey prefix ("odoo-emp-").
	ExternalKeyPrefix string
	// Agents writes the per-user identity context file. Optional: when nil (or
	// AgentID is Nil) the hook still provisions the credential but skips the
	// identity-context injection.
	Agents AgentContextStore
	// AgentID is the agent that serves this channel — the owner of the per-user
	// context file written via Agents.
	AgentID uuid.UUID
	// HintMessage is replied to senders the gate blocks because they cannot be
	// mapped to any staff Odoo identity. Empty falls back to defaultHint.
	HintMessage string
	// GroupUnboundMessage is replied in a group to a staff member who has not
	// bound yet (directs them to 1:1 bind first). Empty → defaultGroupUnbound.
	GroupUnboundMessage string
	// SuccessTTL / NegativeTTL tune the in-memory decision cache.
	SuccessTTL  time.Duration
	NegativeTTL time.Duration
}

// withDefaults returns a copy of c with zero-value optional fields filled.
func (c Config) withDefaults() Config {
	if c.ExternalKeyPrefix == "" {
		c.ExternalKeyPrefix = defaultPrefix
	}
	if c.OdooBaseURL == "" {
		c.OdooBaseURL = deriveBaseURL(c.MCPURL)
	}
	if c.SuccessTTL == 0 {
		c.SuccessTTL = defaultSuccessTTL
	}
	if c.NegativeTTL == 0 {
		c.NegativeTTL = defaultNegativeTTL
	}
	if c.HintMessage == "" {
		c.HintMessage = defaultHint
	}
	if c.GroupUnboundMessage == "" {
		c.GroupUnboundMessage = defaultGroupUnbound
	}
	return c
}

// Hook implements channel.MessageGate. On each inbound message it resolves the
// sender to an Odoo identity, provisions a per-user credential + identity
// context (side effects), and decides whether the message may reach the agent:
// staff are allowed; senders that cannot be mapped to a staff Odoo identity are
// blocked with a hint. Decisions are cached per user.
type Hook struct {
	cfg Config

	mu        sync.Mutex
	decisions map[string]gateDecision // userKey → cached gate decision
	inflight  map[string]bool         // userKey → evaluation in progress
}

// gateDecision is a cached allow/deny result with its expiry.
type gateDecision struct {
	allow  bool
	reply  string
	expiry time.Time
}

var _ channel.MessageGate = (*Hook)(nil)

// New constructs a Hook. The caller is responsible for providing the required
// Config fields; New does not validate them so tests can pass partial configs.
func New(cfg Config) *Hook {
	return &Hook{
		cfg:       cfg.withDefaults(),
		decisions: make(map[string]gateDecision),
		inflight:  make(map[string]bool),
	}
}

// Gate decides whether an inbound text may proceed to the agent. Allowed staff
// also get their credential + identity context provisioned as a side effect.
// Fails OPEN (allow) on misconfiguration / transient errors so an outage never
// locks out legitimate users.
func (h *Hook) Gate(ctx context.Context, ev channel.TextEvent) (bool, string) {
	lwUserID := ev.UserID
	if lwUserID == "" || h.cfg.Creds == nil || h.cfg.Directory == nil {
		return true, "" // not configured to gate → allow
	}
	isDM := ev.ChannelID == ""
	userKey := senderPrefix + lwUserID
	// Cache key is scoped by peer kind: a DM evaluation must not be short-
	// circuited by a cached group "go DM to bind" block (and vice-versa).
	cacheKey := userKey
	if isDM {
		cacheKey += "|dm"
	} else {
		cacheKey += "|grp"
	}

	// Cache lookup (read then release — no lock held across the credential I/O).
	h.mu.Lock()
	d, ok := h.decisions[cacheKey]
	h.mu.Unlock()
	if ok && time.Now().Before(d.expiry) {
		if d.allow {
			return d.allow, d.reply // cached allow → fast path
		}
		// Cached BLOCK: re-check the live credential cheaply. A user who just
		// bound in a DM must be admitted on their very next group message
		// instead of waiting out the block TTL — so if a credential now exists,
		// bust the cache and re-evaluate (which will allow). Still-unbound →
		// keep the cached (blanked) block so we don't re-spam the prompt.
		if uc, _ := h.cfg.Creds.GetUserCredentials(ctx, h.cfg.ServerID, userKey); uc != nil && uc.APIKey != "" {
			h.mu.Lock()
			delete(h.decisions, cacheKey)
			h.mu.Unlock()
		} else {
			return d.allow, d.reply
		}
	}

	// Single-flight.
	h.mu.Lock()
	if h.inflight[cacheKey] {
		h.mu.Unlock()
		return true, "" // another eval in flight → allow this turn (avoid double work / deadlock)
	}
	h.inflight[cacheKey] = true
	h.mu.Unlock()

	allow, reply, ttl := h.evaluate(ctx, lwUserID, userKey, isDM)

	// Cache the decision but blank the reply so the hint / prompt / success
	// message is sent at most once per TTL (no group spam on repeated messages).
	h.mu.Lock()
	delete(h.inflight, cacheKey)
	h.decisions[cacheKey] = gateDecision{allow: allow, reply: "", expiry: time.Now().Add(ttl)}
	h.mu.Unlock()
	return allow, reply
}

// evaluate resolves the sender, provisions credential + identity for staff, and
// returns the gate decision (allow, reply) plus the cache TTL.
//
// Block (allow=false, hint) only when the sender is DEFINITIVELY not staff —
// the directory returned a user with neither a privateEmail nor a resolvable
// employee Odoo login. Every other outcome — resolved staff, or a transient
// directory/bind error — fails OPEN (allow=true) so glitches never lock out
// legitimate users.
func (h *Hook) evaluate(ctx context.Context, lwUserID, userKey string, isDM bool) (allow bool, reply string, ttl time.Duration) {
	// 1. Resolve the directory identity (LINE WORKS side).
	du, err := h.cfg.Directory.ResolveUser(ctx, lwUserID)
	if err != nil || du == nil {
		slog.Warn("lineworks-autobind: directory lookup failed (fail-open)", "user", lwUserID, "err", err)
		return true, "", h.cfg.NegativeTTL
	}

	// 2. Determine the login_or_email to bind by. Primary: privateEmail (the
	// e-smith login email == Odoo res.users login). Fallback: externalKey
	// "odoo-emp-{id}" → Odoo MCP res.users lookup → login.
	bindID := strings.TrimSpace(du.PrivateEmail)
	if bindID == "" {
		empID, perr := parseEmployeeID(du.ExternalKey, h.cfg.ExternalKeyPrefix)
		if perr != nil {
			// No privateEmail and no employee externalKey — definitively not a
			// staff account. BLOCK with the hint (cached long).
			slog.Info("lineworks-autobind: blocking non-staff sender",
				"user", lwUserID, "externalKey", du.ExternalKey)
			return false, h.cfg.HintMessage, h.cfg.SuccessTTL
		}
		login, lerr := h.lookupLogin(ctx, empID)
		if lerr != nil {
			slog.Warn("lineworks-autobind: res.users lookup failed (fail-open)", "user", lwUserID, "employee_id", empID, "err", lerr)
			return true, "", h.cfg.NegativeTTL
		}
		if login == "" {
			// Directory employee but no Odoo user — cannot operate. Block, retry soon.
			slog.Info("lineworks-autobind: employee has no Odoo user, blocking", "user", lwUserID, "employee_id", empID)
			return false, h.cfg.HintMessage, h.cfg.NegativeTTL
		}
		bindID = login
	}

	// 3. Staff. If already bound, allow everywhere (and backfill identity).
	if uc, gerr := h.cfg.Creds.GetUserCredentials(ctx, h.cfg.ServerID, userKey); gerr == nil && uc != nil && uc.APIKey != "" {
		// Backfill the Odoo language for users bound before lang capture existed
		// (their credential row has no Env["odoo_lang"]). One MCP lookup by login,
		// guarded so it runs at most once — once odoo_lang is stored this branch
		// is skipped. Preserves APIKey + Headers (SetUserCredentials is a full
		// replace). Best-effort: a failure does not block the message.
		if uc.Env["odoo_lang"] == "" {
			if lang, lerr := h.lookupLangByLogin(ctx, bindID); lerr != nil {
				slog.Warn("lineworks-autobind: lang backfill lookup failed", "user", lwUserID, "err", lerr)
			} else if lang != "" {
				env := uc.Env
				if env == nil {
					env = map[string]string{}
				}
				env["odoo_lang"] = lang
				if serr := h.cfg.Creds.SetUserCredentials(ctx, h.cfg.ServerID, userKey,
					store.MCPUserCredentials{APIKey: uc.APIKey, Headers: uc.Headers, Env: env}); serr != nil {
					slog.Warn("lineworks-autobind: lang backfill write failed", "user", lwUserID, "err", serr)
				} else {
					slog.Info("lineworks-autobind: backfilled odoo_lang for bound user", "user", lwUserID, "lang", lang)
				}
			}
		}
		h.writeIdentityContext(ctx, userKey, du, bindID, 0)
		return true, "", h.cfg.SuccessTTL
	}

	// 4. Staff, not yet bound. Binding (and its confirmation) happens in a 1:1
	// DM, so the first bind is private. In a GROUP, block with a "go DM to bind"
	// prompt and recheck soon (admits them shortly after they DM-bind).
	if !isDM {
		slog.Info("lineworks-autobind: unbound staff in group, prompting DM bind", "user", lwUserID)
		return false, h.cfg.GroupUnboundMessage, groupRecheckTTL
	}

	// 5. DM + staff + unbound → provision now. A provisioning failure does NOT
	// block (staff falls back to the shared identity); it retries after NegativeTTL.
	code, bindErr := h.openclawBind(ctx, lwUserID, bindID)
	if bindErr != nil {
		slog.Warn("lineworks-autobind: openclaw bind failed (allow, fall back to shared identity)", "user", lwUserID, "bind_id", bindID, "err", bindErr)
		return true, "", h.cfg.NegativeTTL
	}
	apiKey, odoo, vErr := h.openclawVerify(ctx, lwUserID, code)
	if vErr != nil {
		slog.Warn("lineworks-autobind: openclaw verify failed (allow)", "user", lwUserID, "err", vErr)
		return true, "", h.cfg.NegativeTTL
	}
	// Fetch the bound user's Odoo res.users.lang so command replies can be
	// localized to their preferred language. Fail-soft: a lookup error never
	// blocks binding — we just store the credential without a lang.
	lang, langErr := h.lookupLang(ctx, odoo.ID)
	if langErr != nil {
		slog.Warn("lineworks-autobind: res.users.lang lookup failed (provisioning without lang)", "user", lwUserID, "odoo_uid", odoo.ID, "err", langErr)
		lang = ""
	}
	creds := store.MCPUserCredentials{APIKey: apiKey}
	if lang != "" {
		creds.Env = map[string]string{"odoo_lang": lang}
	}
	if serr := h.cfg.Creds.SetUserCredentials(ctx, h.cfg.ServerID, userKey, creds); serr != nil {
		slog.Error("lineworks-autobind: SetUserCredentials failed (allow)", "user", lwUserID, "err", serr)
		return true, "", h.cfg.NegativeTTL
	}
	slog.Info("lineworks-autobind: provisioned per-user Odoo credential",
		"user", lwUserID, "bind_id", bindID, "odoo_uid", odoo.ID)

	// 6. Inject identity context + return a one-time bind-success confirmation
	// (allow=true → the message still proceeds to the agent).
	h.writeIdentityContext(ctx, userKey, du, bindID, odoo.ID)
	name := du.Name
	if name == "" {
		name = odoo.Name
	}
	success := fmt.Sprintf("✅ 已完成身分綁定。你是 %s(Odoo:%s)。之後查個人待辦/日報/員工等資料,我會直接以你的身分處理,不需再確認。", name, bindID)
	return true, success, h.cfg.SuccessTTL
}

// writeIdentityContext writes/updates the per-user "identity.md" context file
// so the agent loads the sender's LINE WORKS + Odoo identity every turn. No-op
// when the agent store / id are not configured.
func (h *Hook) writeIdentityContext(ctx context.Context, userKey string, du *DirUser, login string, odooUID int) {
	if h.cfg.Agents == nil || h.cfg.AgentID == uuid.Nil {
		return
	}
	empID, _ := parseEmployeeID(du.ExternalKey, h.cfg.ExternalKeyPrefix)
	var b strings.Builder
	b.WriteString("# 對話者身分(自動綁定)\n\n")
	fmt.Fprintf(&b, "你正在與 **%s** 對話。當對方說「我 / 我的」即指此人。\n\n", du.Name)
	b.WriteString("**Odoo 身分**(你的 Odoo 工具呼叫已自動以此身分執行,self row-scope):\n")
	fmt.Fprintf(&b, "- login：%s\n", login)
	if odooUID > 0 {
		fmt.Fprintf(&b, "- res.users id：%d\n", odooUID)
	}
	if empID > 0 {
		fmt.Fprintf(&b, "- 員工編號(hr.employee.id)：%d\n", empID)
	}
	b.WriteString("\n**LINE WORKS 身分**：\n")
	fmt.Fprintf(&b, "- 顯示名稱：%s\n", du.Name)
	fmt.Fprintf(&b, "- 帳號：%s\n", du.Email)
	b.WriteString("\n查個人待辦 / 日報 / 員工資料時直接以上述身分查詢,不需再向對方詢問是誰。\n")

	if err := h.cfg.Agents.SetUserContextFile(ctx, h.cfg.AgentID, userKey, "identity.md", b.String()); err != nil {
		slog.Warn("lineworks-autobind: SetUserContextFile failed", "user", userKey, "err", err)
		return
	}
	slog.Info("lineworks-autobind: identity context injected", "user", userKey, "name", du.Name, "odoo_uid", odooUID)
}

// parseEmployeeID extracts the hr.employee.id from "<prefix>{id}".
func parseEmployeeID(externalKey, prefix string) (int, error) {
	externalKey = strings.TrimSpace(externalKey)
	if externalKey == "" {
		return 0, errors.New("empty externalKey")
	}
	if prefix != "" {
		if !strings.HasPrefix(externalKey, prefix) {
			return 0, fmt.Errorf("externalKey missing prefix %q", prefix)
		}
		externalKey = strings.TrimPrefix(externalKey, prefix)
	}
	id, err := strconv.Atoi(externalKey)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("externalKey suffix %q is not a positive employee id", externalKey)
	}
	return id, nil
}

// lookupLogin runs an Odoo MCP search_records on res.users filtered by
// employee_id, returning the first matching login ("" if none).
func (h *Hook) lookupLogin(ctx context.Context, empID int) (string, error) {
	var rows []struct {
		Login string `json:"login"`
	}
	err := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "search_records", map[string]any{
		"model":  "res.users",
		"domain": [][]any{{"employee_id", "=", empID}},
		"fields": []string{"login"},
		"limit":  1,
	}, &rows)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rows[0].Login, nil
}

// lookupLang runs an Odoo MCP search_records on res.users filtered by id,
// returning the user's lang ("" if none). Mirrors lookupLogin.
func (h *Hook) lookupLang(ctx context.Context, odooUID int) (string, error) {
	var rows []struct {
		Lang string `json:"lang"`
	}
	err := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "search_records", map[string]any{
		"model":  "res.users",
		"domain": [][]any{{"id", "=", odooUID}},
		"fields": []string{"lang"},
		"limit":  1,
	}, &rows)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rows[0].Lang, nil
}

// lookupLangByLogin returns the Odoo res.users.lang for a given login/email,
// used to backfill the language of users bound before lang capture existed (the
// already-bound path has the login but not the uid). Mirrors lookupLang but
// filters by login. Returns "" when no user matches.
func (h *Hook) lookupLangByLogin(ctx context.Context, login string) (string, error) {
	if login == "" {
		return "", nil
	}
	var rows []struct {
		Lang string `json:"lang"`
	}
	err := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "search_records", map[string]any{
		"model":  "res.users",
		"domain": [][]any{{"login", "=", login}},
		"fields": []string{"lang"},
		"limit":  1,
	}, &rows)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rows[0].Lang, nil
}

// deriveBaseURL strips the MCP path suffix from a full MCP endpoint, yielding
// the Odoo web origin the openclaw API lives under. "https://h/mcp/v1/message"
// → "https://h". Falls back to the input unchanged when no "/mcp" is present.
func deriveBaseURL(mcpURL string) string {
	if i := strings.Index(mcpURL, "/mcp/"); i >= 0 {
		return mcpURL[:i]
	}
	return strings.TrimRight(mcpURL, "/")
}
