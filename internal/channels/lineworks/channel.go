package lineworks

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	lw "github.com/nextlevelbuilder/goclaw/internal/lineworks"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Config holds the non-secret runtime settings for a LINE WORKS channel
// instance. Policy fields mirror the line channel; botSecret is the HMAC key
// for callback signature verification (distinct from the OAuth client secret
// used by the token source).
type Config struct {
	// BotSecret is the bot's own secret — the HMAC-SHA256 key used to verify
	// the X-WORKS-Signature callback header. NOT the OAuth client_secret.
	BotSecret string
	// AllowFrom is the sender allowlist (entries are "lineworks:<userId>" or a
	// bare userId; senderPrefix is applied before matching).
	AllowFrom []string
	// DMPolicy / GroupPolicy follow the channels package semantics
	// ("open"|"allowlist"|"disabled"|"pairing").
	DMPolicy    string
	GroupPolicy string
}

// Channel implements the LINE WORKS Bot channel. It is the structural twin of
// internal/channels/line.Channel but shares no types with it: a separate
// platform type ("lineworks"), a separate webhook path, and its own
// JWT/REST SDK (internal/lineworks). It coexists with the consumer LINE channel
// without touching its behavior.
type Channel struct {
	*channels.BaseChannel

	client    botClient // outbound sender (wraps *lineworks.Client); nil in unit tests
	botSecret string    // HMAC key for X-WORKS-Signature verification
	cfg       Config

	// Admin-command stores (Tier 2). All optional: a nil store makes the
	// corresponding command reply "unavailable" instead of panicking. Wired at
	// construction by makeLineWorksFactory; left nil in unit tests unless a
	// fake is injected via the setters below.
	agentStore        store.AgentStore            // agent key → UUID for /addwriter /removewriter /writers /tasks /task_detail
	configPermStore   store.ConfigPermissionStore // group file-writer ACL for /addwriter /removewriter /writers
	teamStore         store.TeamStore             // team task lookup for /tasks /task_detail
	subagentTaskStore store.SubagentTaskStore     // subagent task lookup for /subagents /subagent

	// pendingStore backs the group PendingHistory with DB persistence. nil →
	// RAM-only history (unit tests / send-disabled instances). Threaded from
	// FactoryWithPendingStore through New so Start can build the history with
	// the right tenant id.
	pendingStore store.PendingMessageStore

	// botNames is the set of the bot's display names (default + i18n variants)
	// used for group @-mention gating. Resolved once at Start from GetBot. Empty
	// → mention gating disabled (fail-safe), so the bot never goes silent in a
	// group when its name cannot be resolved. Guarded by botNamesMu: written once
	// at Start (possibly from the startup goroutine on the Reload path) and read
	// from concurrent webhook handler goroutines.
	botNames   []string
	botNamesMu sync.RWMutex

	// hooks are MessageHook plugins registered via RegisterHook. Events fan out
	// to every hook in registration order. Populated before Start(); not mutated
	// thereafter (Register/Start happen at process init, no lock needed).
	hooks []MessageHook

	// hookCtx is the parent context for every fan-out goroutine. Created in
	// Start, cancelled in Stop. Nil before Start — hookContext() falls back to
	// context.Background() so unit tests that exercise fan-out without Start work.
	hookCtx    context.Context
	hookCancel context.CancelFunc

	// hookWG tracks in-flight fan-out goroutines so Stop can wait briefly for
	// them to drain (bounded by a 3s timeout).
	hookWG sync.WaitGroup

	// gate, when set via SetGate, runs synchronously before hook fan-out and the
	// agent path; a deny result blocks the message (the agent never sees it). At
	// most one gate per channel.
	gate MessageGate

	// groupChats remembers which chat ids are group/room conversations (chatID →
	// struct{}), recorded on every inbound group event. The agent's outbound
	// reply arrives via the bus without the inbound peer metadata, so routePeer
	// consults this set to route group replies to the channel endpoint instead
	// of mis-sending the channelId to the 1:1 user endpoint.
	groupChats sync.Map

	// groupLastSender remembers the most recent asker per group chat (chatID →
	// LINE WORKS userId). The agent's group reply is prefixed with a mention of
	// that user (<m userId="...">) so the reply notifies + addresses whoever
	// asked. Best-effort: in a busy group the "last sender" may differ from the
	// exact triggering message, but conversations are typically sequential.
	groupLastSender sync.Map

	// --- Command-reply localization (i18n) ---
	//
	// credsStore reads the per-user MCP credential row autobind populates; its
	// Env["odoo_lang"] selects the language for the sender's fixed command
	// replies. All four fields are optional: a nil credsStore (or zero
	// mcpServerID) makes resolveUserLang fall back to "en" — the channel never
	// fails because lang resolution is unavailable. Wired by makeLineWorksFactory
	// from the same odoo MCP server row the autobind hook uses.
	credsStore  credentialLangStore
	mcpServerID uuid.UUID
	mcpURL      string
	mcpToken    string

	// langCache memoizes lwUserID → resolved lang to avoid a credential read on
	// every command. Guarded by langCacheMu with a TTL (langCacheTTL).
	langCache   map[string]langCacheEntry
	langCacheMu sync.Mutex

	// ackPending tracks chats awaiting an agent reply (chatID → generation int64).
	// LINE WORKS has no streaming / typing indicator and bot messages cannot be
	// edited, so for a slow turn we send a one-off "processing" ack only if the
	// real reply has not arrived within ackDelay. A newer inbound (higher gen)
	// or the reply (Send clears the entry) cancels a pending ack.
	ackPending sync.Map
	ackCounter int64 // monotonic generation source (atomic)
}

const (
	// defaultAckDelay is how long to wait for the agent's reply before sending a
	// "processing" ack. Tuned so fast replies never trigger an ack.
	defaultAckDelay = 4 * time.Second
	// defaultAckMessage is the one-off ack sent for a slow turn.
	defaultAckMessage = "⏳ 收到,正在為你處理中,請稍候…"
)

// compile-time assertions: Channel satisfies the channel + webhook + sender
// contracts.
var (
	_ channels.Channel        = (*Channel)(nil)
	_ channels.WebhookChannel = (*Channel)(nil)
	_ Sender                  = (*Channel)(nil)
)

// New builds a LINE WORKS channel from a ready outbound client and config.
// client may be nil (unit tests / send-disabled instances); the webhook path
// and hook fan-out still function. msgBus and the allowlist are wired into the
// embedded BaseChannel exactly as the line channel does.
func New(client botClient, cfg Config, msgBus *bus.MessageBus) *Channel {
	base := channels.NewBaseChannel(ChannelType, msgBus, cfg.AllowFrom)
	base.SetType(ChannelType)
	base.ValidatePolicy(cfg.DMPolicy, cfg.GroupPolicy)

	return &Channel{
		BaseChannel: base,
		client:      client,
		botSecret:   cfg.BotSecret,
		cfg:         cfg,
	}
}

// SetPendingStore wires the DB-backed pending-message store used to persist
// group history. Must be called before Start() (Start builds the PendingHistory
// from it). A nil store leaves history RAM-only.
func (c *Channel) SetPendingStore(s store.PendingMessageStore) {
	c.pendingStore = s
}

// SetAgentStore wires the agent store used to resolve the channel's agent key
// to a UUID for writer / task admin commands. Optional: when nil, commands that
// need a resolved agent reply "unavailable".
func (c *Channel) SetAgentStore(s store.AgentStore) { c.agentStore = s }

// SetConfigPermStore wires the group file-writer ACL store backing
// /addwriter, /removewriter and /writers. Optional: nil → those commands reply
// "unavailable".
func (c *Channel) SetConfigPermStore(s store.ConfigPermissionStore) { c.configPermStore = s }

// SetTeamStore wires the team store backing /tasks and /task_detail. Optional:
// nil → those commands reply "unavailable".
func (c *Channel) SetTeamStore(s store.TeamStore) { c.teamStore = s }

// SetSubagentTaskStore wires the subagent task store backing /subagents and
// /subagent. Optional: nil → those commands reply "unavailable".
func (c *Channel) SetSubagentTaskStore(s store.SubagentTaskStore) { c.subagentTaskStore = s }

// credentialLangStore is the minimal credential-store surface the channel needs
// to resolve (and lazily backfill) a sender's preferred command-reply language.
// store.MCPServerStore satisfies it; a fake implements it in tests. Kept narrow
// so command-reply localization does not couple the channel to the full MCP
// store contract.
type credentialLangStore interface {
	GetUserCredentials(ctx context.Context, serverID uuid.UUID, userID string) (*store.MCPUserCredentials, error)
	SetUserCredentials(ctx context.Context, serverID uuid.UUID, userID string, creds store.MCPUserCredentials) error
}

// langCacheEntry is a memoized lang resolution with its expiry.
type langCacheEntry struct {
	lang   string
	expiry time.Time
}

// langCacheTTL bounds how long a resolved lang is reused before re-reading the
// credential row (so a user who changes their Odoo language is picked up within
// the window without a per-message store round-trip).
const langCacheTTL = 6 * time.Hour

// SetCredsStore wires the credential store backing command-reply localization
// (autobind writes Env["odoo_lang"] into the same rows). Optional: nil →
// resolveUserLang returns "en". Mirrors SetAgentStore.
func (c *Channel) SetCredsStore(s credentialLangStore) { c.credsStore = s }

// SetMCPServerID sets the odoo MCP server row id the per-user credential is
// stored under (the same id the autobind hook uses). Zero → resolveUserLang
// returns "en".
func (c *Channel) SetMCPServerID(id uuid.UUID) { c.mcpServerID = id }

// SetMCPEndpoint wires the Odoo MCP JSON-RPC URL + bearer token used for the
// lazy res.users.lang backfill. Both optional: an empty pair disables backfill
// (resolveUserLang then relies solely on the lang autobind already stored).
func (c *Channel) SetMCPEndpoint(url, token string) {
	c.mcpURL = url
	c.mcpToken = token
}

// resolveUserLang returns the command-reply language for a LINE WORKS sender,
// normalized to one of the supported codes. Resolution order:
//
//  1. in-memory cache (langCacheTTL) — avoids a credential read per message;
//  2. the per-user MCP credential's Env["odoo_lang"] (written by autobind);
//  3. "en" fallback.
//
// It never panics and never blocks the command: any missing wiring, a store
// error, or an unrecognized value all collapse to "en". The result is cached
// (including the "en" fallback) so a steady-state conversation does at most one
// credential read per TTL.
func (c *Channel) resolveUserLang(ctx context.Context, lwUserID string) string {
	if lwUserID == "" {
		return langEN
	}

	// 1. Cache.
	now := time.Now()
	c.langCacheMu.Lock()
	if c.langCache != nil {
		if e, ok := c.langCache[lwUserID]; ok && now.Before(e.expiry) {
			c.langCacheMu.Unlock()
			return e.lang
		}
	}
	c.langCacheMu.Unlock()

	lang := c.resolveUserLangUncached(ctx, lwUserID)

	// Cache the result (fallback included) to bound store round-trips.
	c.langCacheMu.Lock()
	if c.langCache == nil {
		c.langCache = make(map[string]langCacheEntry)
	}
	c.langCache[lwUserID] = langCacheEntry{lang: lang, expiry: now.Add(langCacheTTL)}
	c.langCacheMu.Unlock()
	return lang
}

// resolveUserLangUncached performs the actual credential lookup without touching
// the cache. Returns a normalized lang ("en" on any miss/error).
func (c *Channel) resolveUserLangUncached(_ context.Context, lwUserID string) string {
	if c.credsStore == nil || c.mcpServerID == uuid.Nil {
		return langEN
	}
	// userKey matches the namespace autobind binds under ("lineworks:<uid>").
	userKey := senderPrefix + lwUserID
	// Tenant alignment: autobind writes the per-user credential under
	// MasterTenantID (its gate runs on a tenant-less context, so
	// tenantIDForInsert falls back to Master). The command path may have
	// overridden the context with the channel's own TenantID (for the admin ACL
	// stores), which on a non-master deployment filters to a different partition
	// and misses the row — making every reply fall back to "en". Read under a
	// tenant-less context so it resolves to the SAME Master partition the write
	// used, independent of the caller's tenant override.
	readCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	uc, err := c.credsStore.GetUserCredentials(readCtx, c.mcpServerID, userKey)
	if err != nil || uc == nil {
		return langEN
	}
	if l := uc.Env["odoo_lang"]; l != "" {
		return normalizeLang(l)
	}
	// No stored lang yet → English. The channel does not backfill here (it lacks
	// the sender's Odoo uid at command time); instead the autobind hook backfills
	// Env["odoo_lang"] on the user's next message via its already-bound path
	// (it has the login to query res.users.lang). So an already-bound user
	// converges to their real language within one message, without re-binding.
	return langEN
}

// SetPendingCompaction configures LLM-based auto-compaction for the group
// pending history, so discussion that exceeds the history limit is summarized
// (not dropped) and remains available as context on the next mention. Mirrors
// the telegram/zalo/feishu/discord channels; the gateway InstanceLoader calls
// this for any channel implementing channels.PendingCompactable.
func (c *Channel) SetPendingCompaction(cfg *channels.CompactionConfig) {
	if gh := c.GroupHistory(); gh != nil {
		gh.SetCompactionConfig(cfg)
	}
}

// botInfoLookup is the inbound-side capability the channel needs to resolve its
// own display name(s) for mention gating. The SDK's *lineworks.Client satisfies
// it (via the clientAdapter's embedded client); a fake implements it in tests.
type botInfoLookup interface {
	GetBot(ctx context.Context) (*lw.BotInfo, error)
}

// Type returns the platform type. Always "lineworks", independent from "line".
func (c *Channel) Type() string { return ChannelType }

// RegisterHook appends a MessageHook to the channel's fan-out list. Must be
// called before Start(); not safe for concurrent use (intended for process
// init wiring, identical to line.Channel.RegisterHook).
func (c *Channel) RegisterHook(h MessageHook) {
	if h == nil {
		return
	}
	c.hooks = append(c.hooks, h)
}

// SetGate installs the channel's message gate (replacing any previous one).
// Must be called before Start(); not safe for concurrent use. A nil gate means
// no gating — every policy-passing message proceeds to the agent.
func (c *Channel) SetGate(g MessageGate) {
	c.gate = g
}

// Start begins listening (webhook mode). It creates the fan-out parent context
// that hook goroutines inherit, then calls Lifecycle.Start on every hook that
// implements it. The webhook handler is auto-mounted by the gateway via
// WebhookHandler(), so Start has no HTTP server of its own.
func (c *Channel) Start(ctx context.Context) error {
	c.SetRunning(true)

	// Derive from Background (not the caller ctx) so channel lifetime is owned
	// by Stop, mirroring line.Channel.Start.
	c.hookCtx, c.hookCancel = context.WithCancel(context.Background())

	// Group history: accumulate non-mention group messages so that, when the bot
	// IS mentioned, the recent conversation is folded into the agent prompt. The
	// store may be nil (RAM-only). Idempotent: skip if already wired (tests may
	// preset a history via SetGroupHistory).
	if c.GroupHistory() == nil {
		c.SetGroupHistory(channels.MakeHistory(channels.TypeLineWorks, c.pendingStore, c.TenantID()))
	}
	if c.HistoryLimit() <= 0 {
		c.SetHistoryLimit(channels.DefaultGroupHistoryLimit)
	}
	if gh := c.GroupHistory(); gh != nil {
		gh.StartFlusher()
	}
	// Require an explicit @-mention in groups by default. The whole point of the
	// gate is to stop the bot from replying to every group message; 1:1 chats are
	// never gated. (No per-instance config field today — default true.)
	c.SetRequireMention(true)

	// Resolve the bot's display name(s) for mention matching. On error we leave
	// botNames empty, which DISABLES gating (fail-safe) so the bot still answers
	// in groups rather than going silent.
	if bl, ok := c.client.(botInfoLookup); ok && bl != nil {
		if info, err := bl.GetBot(ctx); err != nil {
			slog.Warn("LINEWORKS: GetBot failed; group mention gating disabled (fail-safe)", "err", err)
		} else if info != nil {
			names := []string{info.BotName}
			for _, n := range info.I18nBotNames {
				names = append(names, n.BotName)
			}
			c.SetBotNames(names)
			slog.Info("LINEWORKS: resolved bot names for mention gating", "count", len(names))
		}
	} else {
		slog.Warn("LINEWORKS: client does not support GetBot; group mention gating disabled (fail-safe)")
	}

	for _, h := range c.hooks {
		if lc, ok := h.(Lifecycle); ok {
			if err := lc.Start(ctx); err != nil {
				slog.Error("LINEWORKS: hook Start failed", "err", err)
			}
		}
	}
	slog.Info("LINE WORKS channel started (webhook mode)", "hooks", len(c.hooks))
	return nil
}

// Stop shuts down the channel: cancel the hook context, Stop Lifecycle hooks in
// reverse registration order, then wait up to 3s for fan-out goroutines to
// drain. Identical shutdown ordering to the line channel.
func (c *Channel) Stop(_ context.Context) error {
	if c.hookCancel != nil {
		c.hookCancel()
	}

	for i := len(c.hooks) - 1; i >= 0; i-- {
		if lc, ok := c.hooks[i].(Lifecycle); ok {
			if err := lc.Stop(); err != nil {
				slog.Error("LINEWORKS: hook Stop failed", "err", err)
			}
		}
	}

	// Drain the group-history flusher (no-op for RAM-only history), matching the
	// telegram channel's shutdown handling.
	if gh := c.GroupHistory(); gh != nil {
		gh.StopFlusher()
	}

	drained := make(chan struct{})
	go func() {
		c.hookWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		slog.Warn("LINEWORKS: hook fan-out did not drain within 3s of Stop")
	}

	c.SetRunning(false)
	slog.Info("LINE WORKS channel stopped")
	return nil
}

// SendText sends a plain-text message to a peer, satisfying the Sender
// interface used by workflow-plugin hooks. The (userID, channelID) pair selects
// the endpoint: channelID set → group/room, otherwise 1:1 user. Long text is
// chunked to the platform per-message cap.
func (c *Channel) SendText(ctx context.Context, userID, channelID, text string) error {
	text = formatForLineWorks(text)
	if text == "" {
		return nil
	}
	for _, chunk := range splitMessage(text, maxTextLength) {
		if err := c.sendText(ctx, userID, channelID, chunk); err != nil {
			return err
		}
	}
	return nil
}

// scheduleAck arms a delayed "processing" ack for an agent-bound message. If
// the agent's reply has not reached the chat within defaultAckDelay (Send
// clears the pending entry) and no newer inbound has superseded it, a single
// ack is sent so the user knows a slow turn (e.g. an Odoo query) is in flight.
// No-op without a client (unit tests).
func (c *Channel) scheduleAck(userID, channelID, chatID string) {
	if c.client == nil {
		return
	}
	gen := atomic.AddInt64(&c.ackCounter, 1)
	c.ackPending.Store(chatID, gen)
	c.hookWG.Add(1)
	go func() {
		defer c.hookWG.Done()
		ctx := c.hookContext()
		t := time.NewTimer(defaultAckDelay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
		// Send the ack only if this is still the latest pending turn for the
		// chat (no reply cleared it, no newer inbound replaced the generation).
		if v, ok := c.ackPending.Load(chatID); ok && v.(int64) == gen {
			c.ackPending.Delete(chatID)
			if err := c.SendText(ctx, userID, channelID, defaultAckMessage); err != nil {
				slog.Warn("LINEWORKS: processing-ack send failed", "chatID", chatID, "err", err)
			}
		}
	}()
}

// VerifyAndHandle is the per-channel verify+route hook used by the gateway's
// single LINE WORKS webhook dispatcher (channels.Manager.LineWorksWebhookDispatcher).
// Because the LINE WORKS callback body carries no bot id, a process hosting
// multiple LINE WORKS bots demuxes purely by "which bot secret verifies the
// X-WORKS-Signature HMAC over the raw body". This method answers exactly that
// for one channel: it returns true iff c.botSecret verifies sig over rawBody,
// and — only then — kicks off the asynchronous callback dispatch (so the HTTP
// 200 is not blocked by hook fan-out or agent processing, preserving the
// existing retry-storm-avoidance behavior). On a non-match it returns false
// with NO side effects, so the dispatcher can try the next candidate bot.
//
// Keeping botSecret private: the dispatcher never sees the secret; it only asks
// each channel "is this yours?" via this method.
func (c *Channel) VerifyAndHandle(rawBody []byte, sig string) bool {
	if !lw.VerifySignature(c.botSecret, rawBody, sig) {
		return false
	}
	// Body is trusted past this point for THIS bot. Dispatch off the request
	// goroutine so the 200 is not blocked by hook fan-out or agent processing.
	go c.handleCallback(rawBody)
	return true
}

// WebhookHandler returns the HTTP path and handler for LINE WORKS callbacks.
// The handler verifies the X-WORKS-Signature HMAC over the RAW request body
// using the bot secret BEFORE parsing, then dispatches asynchronously and
// always replies 200 so LINE WORKS does not retry-storm.
//
// NOTE: As of the multi-bot dispatcher, this per-instance handler is NO LONGER
// mounted on the gateway mux: channels.Manager.WebhookHandlers() deliberately
// skips lineworks channels and the gateway mounts ONE shared dispatcher at
// webhookPath that demuxes across all registered bots via VerifyAndHandle.
// The method is retained to satisfy the channels.WebhookChannel contract (the
// compile-time assertion above) and for direct single-bot use in tests.
func (c *Channel) WebhookHandler() (string, http.Handler) {
	return webhookPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Error("LINEWORKS webhook: read body failed", "err", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		sig := r.Header.Get(signatureHeader)
		if !lw.VerifySignature(c.botSecret, rawBody, sig) {
			slog.Warn("LINEWORKS webhook: invalid signature")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Body is trusted past this point. Dispatch off the request goroutine so
		// the 200 is not blocked by hook fan-out or agent processing.
		go c.handleCallback(rawBody)

		w.WriteHeader(http.StatusOK)
	})
}
