package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/channels/discord"
	"github.com/nextlevelbuilder/goclaw/internal/channels/feishu"
	linechannel "github.com/nextlevelbuilder/goclaw/internal/channels/line"
	lineworkschannel "github.com/nextlevelbuilder/goclaw/internal/channels/lineworks"
	slackchannel "github.com/nextlevelbuilder/goclaw/internal/channels/slack"
	"github.com/nextlevelbuilder/goclaw/internal/channels/telegram"
	"github.com/nextlevelbuilder/goclaw/internal/channels/whatsapp"
	"github.com/nextlevelbuilder/goclaw/internal/channels/zalo"
	zalopersonal "github.com/nextlevelbuilder/goclaw/internal/channels/zalo/personal"
	"github.com/nextlevelbuilder/goclaw/internal/channels/zalo/personal/zalomethods"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/gateway"
	"github.com/nextlevelbuilder/goclaw/internal/gateway/methods"
	lw "github.com/nextlevelbuilder/goclaw/internal/lineworks"
	esmithkm "github.com/nextlevelbuilder/goclaw/internal/plugins/esmith-km"
	lineworksworkflow "github.com/nextlevelbuilder/goclaw/internal/plugins/lineworks-workflow"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// registerEsmithKmHook reads esmith-km env vars and, if present, constructs
// an esmithkm.Hook and registers it on the given LINE channel. Returns the
// constructed hook (or nil) so the caller can wire auxiliary surfaces such
// as the LIFF HTTP handler against the same channel instance.
//
// Three cases:
//
//  1. Both ODOO_STAGE35_MCP_URL and ODOO_STAGE35_MCP_TOKEN set → hook
//     registered.
//  2. Both unset → hook NOT registered, log at Info level. This is the
//     backwards-compatible path for any goclaw deployment of this fork
//     that does not need km-meeting writeback.
//  3. One set but not the other → hook NOT registered, log at ERROR
//     level. Partial config indicates operator intent to use esmith-km
//     that failed at env-var wiring; silently skipping would be a
//     silent functional regression for an e-smith deployment.
//
// This helper does not return an error because LINE channel startup
// should not block on plugin config — but the error log in case 3 is
// loud enough that any deployment monitoring slog output will catch it.
func registerEsmithKmHook(ch *linechannel.Channel) *esmithkm.Hook {
	mcpURL := os.Getenv("ODOO_STAGE35_MCP_URL")
	mcpToken := os.Getenv("ODOO_STAGE35_MCP_TOKEN")
	liffURL := os.Getenv("ESMITH_KM_ATTENDEES_LIFF_URL")

	switch {
	case mcpURL == "" && mcpToken == "":
		slog.Info("esmith-km: MCP env not set, skipping hook registration (non-esmith deployment)")
		return nil
	case mcpURL == "" || mcpToken == "":
		slog.Error("esmith-km: partial MCP config detected, HOOK WILL NOT BE REGISTERED",
			"url_set", mcpURL != "",
			"token_set", mcpToken != "",
			"action", "Set both ODOO_STAGE35_MCP_URL and ODOO_STAGE35_MCP_TOKEN, or neither")
		return nil
	}
	if liffURL == "" {
		slog.Error("esmith-km: ESMITH_KM_ATTENDEES_LIFF_URL not set, attendees bubble will degrade to configuration-hint",
			"action", "Set ESMITH_KM_ATTENDEES_LIFF_URL=https://liff.line.me/<liff_id> to enable the LIFF attendees picker")
	}

	hook := esmithkm.New(esmithkm.Config{
		Sender:           ch,
		MCPURL:           mcpURL,
		MCPToken:         mcpToken,
		OdooBaseURL:      os.Getenv("ODOO_STAGE35_BASE_URL"),
		LIFFAttendeesURL: liffURL,
	})
	ch.RegisterHook(hook)
	slog.Info("esmith-km: hook registered on LINE channel",
		"liff_attendees_url_set", liffURL != "")
	return hook
}

// buildEsmithKmLiffHandler constructs the km-meeting attendees LIFF HTTP
// handler. Returns nil when the plugin is not configured so the caller can
// skip registration on the gateway mux.
//
// ID token verification: LIFF tokens are signed with the LINE LOGIN channel
// secret (the one that owns the LIFF App), not the messaging channel secret
// that the goclaw bot uses. They are different channels with different
// secrets. Rather than surface a second secret via env vars, we call LINE's
// online verify endpoint with just the LIFF client id (public — the prefix of
// the LIFF ID before the dash). See internal/plugins/esmith-km/liff_verifier.go.
func buildEsmithKmLiffHandler(ch *linechannel.Channel) *esmithkm.LiffHandler {
	mcpURL := os.Getenv("ODOO_STAGE35_MCP_URL")
	mcpToken := os.Getenv("ODOO_STAGE35_MCP_TOKEN")
	liffURL := os.Getenv("ESMITH_KM_ATTENDEES_LIFF_URL")
	if mcpURL == "" || mcpToken == "" || ch == nil {
		return nil
	}

	// Derive LIFF client id. Path ends in `<channel_id>-<app_token>` — e.g.
	// `https://liff.line.me/2009610420-ClGgYLB1` → client id `2009610420`.
	clientID := ""
	if liffURL != "" {
		if idx := strings.LastIndex(liffURL, "/"); idx >= 0 && idx+1 < len(liffURL) {
			tail := liffURL[idx+1:]
			if dash := strings.Index(tail, "-"); dash > 0 {
				clientID = tail[:dash]
			}
		}
	}
	if clientID == "" {
		slog.Error("esmith-km: cannot derive LIFF client id from ESMITH_KM_ATTENDEES_LIFF_URL; LIFF handler will reject every token",
			"liff_url", liffURL,
			"action", "Set ESMITH_KM_ATTENDEES_LIFF_URL=https://liff.line.me/<channel_id>-<app_token>")
	}
	verifier := esmithkm.NewLINELiffVerifier(clientID)

	draftsDir := os.Getenv("ESMITH_KM_DRAFTS_DIR")
	if draftsDir == "" {
		// Production default; mirrors esmithkm.Config.WithDefaults.
		draftsDir = "/data/km/meetings/drafts"
	}
	origins := strings.Split(os.Getenv("ESMITH_KM_LIFF_ALLOW_ORIGINS"), ",")
	for i, o := range origins {
		origins[i] = strings.TrimSpace(o)
	}
	// Strip empties so a missing env doesn't produce a single empty-string
	// entry that every origin string trivially compares against.
	cleaned := origins[:0]
	for _, o := range origins {
		if o != "" {
			cleaned = append(cleaned, o)
		}
	}
	if len(cleaned) == 0 {
		// Safe defaults: allow the known staging + production Odoo hosts.
		cleaned = []string{
			"https://odoo-esmith*.odoo.com",
			"https://odoo-esmith*.dev.odoo.com",
		}
	}
	return esmithkm.NewLiffHandler(verifier, draftsDir, mcpURL, mcpToken, cleaned)
}

// liffHandlerOnce guarantees the km-meeting LIFF HTTP handler is registered
// on the gateway mux at most once even if multiple LINE channel instances are
// loaded. The handler is wired to the first LINE channel it sees — e-smith
// runs a single LINE channel, so this is the expected shape. A multi-channel
// deployment would need a per-tenant dispatch front-end in front of
// NewLiffHandler, which is out of scope for T-057.
var liffHandlerOnce sync.Once

func registerEsmithKmLiffOnGateway(srv *gateway.Server, lc *linechannel.Channel) {
	if srv == nil || lc == nil {
		return
	}
	handler := buildEsmithKmLiffHandler(lc)
	if handler == nil {
		return
	}
	liffHandlerOnce.Do(func() {
		srv.RegisterPluginHandler(handler)
		slog.Info("esmith-km: LIFF HTTP handler registered on gateway",
			"routes", []string{
				"GET /km/meeting/attendees/bootstrap",
				"POST /km/meeting/attendees/submit",
			})
	})
}

// makeLineFactoryWithEsmithKm closes over the gateway server so each LINE
// channel instance can wire both the plugin hook and (once) the LIFF HTTP
// handler. Passing srv via closure avoids a package-level global while still
// honoring the channels.ChannelFactory signature instanceLoader expects.
func makeLineFactoryWithEsmithKm(srv *gateway.Server) channels.ChannelFactory {
	return func(name string, creds json.RawMessage, cfg json.RawMessage,
		msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {
		ch, err := linechannel.Factory(name, creds, cfg, msgBus, pairingSvc)
		if err != nil {
			return nil, err
		}
		if lc, ok := ch.(*linechannel.Channel); ok {
			if hook := registerEsmithKmHook(lc); hook != nil {
				registerEsmithKmLiffOnGateway(srv, lc)
			}
		}
		return ch, nil
	}
}

// lineWorksFactoryCreds is the cmd-local view of the decrypted LINE WORKS
// credentials JSON. It mirrors the channel package's (unexported) cred struct
// just enough to build the Directory resolver's own SDK client — the channel
// factory parses the same JSON independently for the outbound/send path. Two
// distinct secrets exist: client_secret authenticates the OAuth token
// exchange, bot_secret keys the X-WORKS-Signature HMAC.
type lineWorksFactoryCreds struct {
	BotID          string   `json:"bot_id"`
	ServiceAccount string   `json:"service_account"`
	ClientID       string   `json:"client_id"`
	ClientSecret   string   `json:"client_secret"`
	PrivateKey     string   `json:"private_key"`
	Scopes         []string `json:"scopes,omitempty"`
}

// lineWorksFactoryConfig is the cmd-local view of the non-secret config JSONB,
// limited to the fields the workflow plugin needs at wiring time.
type lineWorksFactoryConfig struct {
	DirectoryExternalKeyPrefix string   `json:"directory_external_key_prefix,omitempty"`
	Scopes                     []string `json:"scopes,omitempty"`
}

// lineWorksDirectoryAdapter bridges the lineworks SDK Client.GetUser to the
// workflow plugin's DirectoryResolver (UserExternalKey). The plugin stays
// decoupled from the concrete SDK; this adapter lives in cmd wiring.
type lineWorksDirectoryAdapter struct {
	client *lw.Client
}

func (d lineWorksDirectoryAdapter) UserExternalKey(ctx context.Context, userID string) (string, error) {
	u, err := d.client.GetUser(ctx, userID)
	if err != nil {
		return "", err
	}
	return u.UserExternalKey, nil
}

// registerLineWorksWorkflowHook reads the lineworks-workflow env vars and, if
// present, constructs a lineworksworkflow.Hook and registers it on the given
// LINE WORKS channel. Mirrors registerEsmithKmHook's three-case env handling
// (both set → register; both unset → skip at Info; partial → skip at Error).
//
// The hook's Sender is the channel itself (it satisfies the channel's Sender
// interface). The DirectoryResolver wraps a dedicated SDK client built from the
// same per-instance credentials, scoped with directory.read so externalKey
// lookups succeed regardless of the channel's send-only scopes.
//
// creds/cfg are the raw per-instance JSON the factory received; they carry the
// service-account key material needed to mint a directory-scoped token. A nil
// returned hook means the plugin was not wired (env missing or creds invalid)
// — channel startup proceeds regardless, exactly like esmith-km on LINE.
func registerLineWorksWorkflowHook(ch *lineworkschannel.Channel, creds json.RawMessage, cfg json.RawMessage) *lineworksworkflow.Hook {
	mcpURL := os.Getenv("ODOO_STAGE38_MCP_URL")
	mcpToken := os.Getenv("ODOO_STAGE38_MCP_TOKEN")

	switch {
	case mcpURL == "" && mcpToken == "":
		slog.Info("lineworks-workflow: MCP env not set, skipping hook registration (non-lineworks deployment)")
		return nil
	case mcpURL == "" || mcpToken == "":
		slog.Error("lineworks-workflow: partial MCP config detected, HOOK WILL NOT BE REGISTERED",
			"url_set", mcpURL != "",
			"token_set", mcpToken != "",
			"action", "Set both ODOO_STAGE38_MCP_URL and ODOO_STAGE38_MCP_TOKEN, or neither")
		return nil
	}

	var cr lineWorksFactoryCreds
	if len(creds) > 0 {
		if err := json.Unmarshal(creds, &cr); err != nil {
			slog.Error("lineworks-workflow: cannot decode credentials, HOOK WILL NOT BE REGISTERED", "error", err)
			return nil
		}
	}
	var ic lineWorksFactoryConfig
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &ic); err != nil {
			slog.Error("lineworks-workflow: cannot decode config, HOOK WILL NOT BE REGISTERED", "error", err)
			return nil
		}
	}

	// Directory lookups need directory.read; ensure it is present on the
	// token the resolver's client mints, without mutating the channel's own
	// send scopes.
	scopes := cr.Scopes
	if len(scopes) == 0 {
		scopes = ic.Scopes
	}
	scopes = ensureScope(scopes, "directory.read")

	ts, err := lw.NewTokenSource(lw.AuthConfig{
		ClientID:       cr.ClientID,
		ClientSecret:   cr.ClientSecret,
		ServiceAccount: cr.ServiceAccount,
		PrivateKeyPEM:  cr.PrivateKey,
		Scopes:         scopes,
	})
	if err != nil {
		slog.Error("lineworks-workflow: directory token source failed, HOOK WILL NOT BE REGISTERED", "error", err)
		return nil
	}
	dirClient, err := lw.NewClient(lw.ClientConfig{BotID: cr.BotID, Tokens: ts})
	if err != nil {
		slog.Error("lineworks-workflow: directory client failed, HOOK WILL NOT BE REGISTERED", "error", err)
		return nil
	}

	prefix := ic.DirectoryExternalKeyPrefix // empty → plugin default "odoo-emp-"
	hook := lineworksworkflow.New(lineworksworkflow.Config{
		Sender:            ch,
		Directory:         lineWorksDirectoryAdapter{client: dirClient},
		MCPURL:            mcpURL,
		MCPToken:          mcpToken,
		OdooBaseURL:       os.Getenv("ODOO_STAGE38_BASE_URL"),
		ExternalKeyPrefix: prefix,
		TodoProjectID:     atoiOrZero(os.Getenv("LINEWORKS_WORKFLOW_TODO_PROJECT_ID")),
	})
	ch.RegisterHook(hook)
	slog.Info("lineworks-workflow: hook registered on LINE WORKS channel",
		"external_key_prefix_set", prefix != "")
	return hook
}

// ensureScope appends scope to scopes if not already present. A nil/empty
// input yields a single-element slice — the SDK then uses an explicit scope set
// instead of its send-only default, which directory lookups require.
func ensureScope(scopes []string, scope string) []string {
	for _, s := range scopes {
		if s == scope {
			return scopes
		}
	}
	return append(append([]string{}, scopes...), scope)
}

// atoiOrZero parses a base-10 int from s, returning 0 on any error / empty
// input. Used for optional numeric env vars whose absence means "unset".
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// makeLineWorksFactory closes over the gateway server (kept for signature
// parity with makeLineFactoryWithEsmithKm and future LIFF/HTTP surfaces) and
// returns a ChannelFactory that builds the LINE WORKS channel via the channel
// package's own Factory, then registers the lineworks-workflow plugin hook on
// the resulting channel instance. The workflow plugin needs no HTTP surface in
// v1, so srv is not used to mount any handler here.
func makeLineWorksFactory(_ *gateway.Server) channels.ChannelFactory {
	return func(name string, creds json.RawMessage, cfg json.RawMessage,
		msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {
		ch, err := lineworkschannel.Factory(name, creds, cfg, msgBus, pairingSvc)
		if err != nil {
			return nil, err
		}
		if lc, ok := ch.(*lineworkschannel.Channel); ok {
			registerLineWorksWorkflowHook(lc, creds, cfg)
		}
		return ch, nil
	}
}

// registerConfigChannels registers config-based channels as fallback when no DB instances are loaded.
func registerConfigChannels(cfg *config.Config, channelMgr *channels.Manager, msgBus *bus.MessageBus, pgStores *store.Stores, instanceLoader *channels.InstanceLoader, srv *gateway.Server) {
	if instanceLoader != nil {
		return
	}

	recordMissingConfig := func(name, detail string) {
		channelMgr.RecordHealth(name, channels.NewChannelHealthForType(
			name,
			channels.ChannelHealthStateFailed,
			"Missing credentials",
			detail,
			channels.ChannelFailureKindConfig,
			false,
		))
	}

	if cfg.Channels.Telegram.Enabled {
		if cfg.Channels.Telegram.Token == "" {
			recordMissingConfig(channels.TypeTelegram, "Set channels.telegram.token in config.")
		} else if tg, err := telegram.New(cfg.Channels.Telegram, msgBus, pgStores.Pairing); err != nil {
			channelMgr.RecordFailure(channels.TypeTelegram, "", err)
			slog.Error("failed to initialize telegram channel", "error", err)
		} else {
			channelMgr.RegisterChannel(channels.TypeTelegram, tg)
			slog.Info("telegram channel enabled (config)")
		}
	}

	if cfg.Channels.Discord.Enabled {
		if cfg.Channels.Discord.Token == "" {
			recordMissingConfig(channels.TypeDiscord, "Set channels.discord.token in config.")
		} else if dc, err := discord.New(cfg.Channels.Discord, msgBus, nil, nil, nil, nil); err != nil {
			channelMgr.RecordFailure(channels.TypeDiscord, "", err)
			slog.Error("failed to initialize discord channel", "error", err)
		} else {
			channelMgr.RegisterChannel(channels.TypeDiscord, dc)
			slog.Info("discord channel enabled (config)")
		}
	}

	if cfg.Channels.WhatsApp.Enabled {
		waDialect := "pgx"
		if strings.Contains(fmt.Sprintf("%T", pgStores.DB.Driver()), "sqlite") {
			waDialect = "sqlite3"
		}
		wa, err := whatsapp.New(cfg.Channels.WhatsApp, msgBus, pgStores.Pairing, pgStores.DB, pgStores.PendingMessages, waDialect)
		if err != nil {
			channelMgr.RecordFailure(channels.TypeWhatsApp, "", err)
			slog.Error("failed to initialize whatsapp channel", "error", err)
		} else {
			channelMgr.RegisterChannel(channels.TypeWhatsApp, wa)
			slog.Info("whatsapp channel enabled (config)")
		}
	}

	if cfg.Channels.Zalo.Enabled {
		if cfg.Channels.Zalo.Token == "" {
			recordMissingConfig(channels.TypeZaloOA, "Set channels.zalo.token in config.")
		} else if z, err := zalo.New(cfg.Channels.Zalo, msgBus, pgStores.Pairing); err != nil {
			channelMgr.RecordFailure(channels.TypeZaloOA, "", err)
			slog.Error("failed to initialize zalo channel", "error", err)
		} else {
			channelMgr.RegisterChannel(channels.TypeZaloOA, z)
			slog.Info("zalo channel enabled (config)")
		}
	}

	if cfg.Channels.ZaloPersonal.Enabled {
		zp, err := zalopersonal.New(cfg.Channels.ZaloPersonal, msgBus, pgStores.Pairing, nil)
		if err != nil {
			channelMgr.RecordFailure(channels.TypeZaloPersonal, "", err)
			slog.Error("failed to initialize zca channel", "error", err)
		} else {
			channelMgr.RegisterChannel(channels.TypeZaloPersonal, zp)
			slog.Info("zca (zalo personal) channel enabled (config)")
		}
	}

	if cfg.Channels.Slack.Enabled {
		switch {
		case cfg.Channels.Slack.BotToken == "":
			recordMissingConfig(channels.TypeSlack, "Set channels.slack.bot_token in config.")
		case cfg.Channels.Slack.AppToken == "":
			recordMissingConfig(channels.TypeSlack, "Set channels.slack.app_token in config.")
		default:
			sl, err := slackchannel.New(cfg.Channels.Slack, msgBus, nil, nil)
			if err != nil {
				channelMgr.RecordFailure(channels.TypeSlack, "", err)
				slog.Error("failed to initialize slack channel", "error", err)
			} else {
				channelMgr.RegisterChannel(channels.TypeSlack, sl)
				slog.Info("slack channel enabled (config)")
			}
		}
	}

	if cfg.Channels.Feishu.Enabled {
		if cfg.Channels.Feishu.AppID == "" {
			recordMissingConfig(channels.TypeFeishu, "Set channels.feishu.app_id in config.")
		} else {
			feishuOpts := []feishu.Option{
				feishu.WithAgentStore(pgStores.Agents),
				feishu.WithConfigPermStore(pgStores.ConfigPermissions),
			}
			if f, err := feishu.New(cfg.Channels.Feishu, msgBus, pgStores.Pairing, nil, feishuOpts...); err != nil {
				channelMgr.RecordFailure(channels.TypeFeishu, "", err)
				slog.Error("failed to initialize feishu channel", "error", err)
			} else {
				channelMgr.RegisterChannel(channels.TypeFeishu, f)
				slog.Info("feishu/lark channel enabled (config)")
			}
		}
	}

	if cfg.Channels.Line.Enabled && cfg.Channels.Line.ChannelAccessToken != "" && instanceLoader == nil {
		l, err := linechannel.New(cfg.Channels.Line, msgBus, pgStores.Pairing)
		if err != nil {
			slog.Error("failed to initialize line channel", "error", err)
		} else {
			if hook := registerEsmithKmHook(l); hook != nil {
				registerEsmithKmLiffOnGateway(srv, l)
			}
			channelMgr.RegisterChannel(channels.TypeLine, l)
			slog.Info("line channel enabled (config)")
		}
	}
}

// wireChannelRPCMethods registers WS RPC methods for channels, instances, agent links, and teams.
func wireChannelRPCMethods(server *gateway.Server, pgStores *store.Stores, channelMgr *channels.Manager, agentRouter *agent.Router, msgBus *bus.MessageBus, dataDir string) {
	// Register channels RPC methods (after channelMgr is initialized with all channels)
	methods.NewChannelsMethods(channelMgr).Register(server.Router())

	// Register channel instances WS RPC methods
	if pgStores.ChannelInstances != nil {
		methods.NewChannelInstancesMethods(pgStores.ChannelInstances, pgStores.Agents, msgBus, msgBus).Register(server.Router())
		zalomethods.NewQRMethods(pgStores.ChannelInstances, msgBus).Register(server.Router())
		zalomethods.NewContactsMethods(pgStores.ChannelInstances).Register(server.Router())
		whatsapp.NewQRMethods(pgStores.ChannelInstances, channelMgr).Register(server.Router())
	}

	// Register agent links WS RPC methods
	if pgStores.AgentLinks != nil && pgStores.Agents != nil {
		methods.NewAgentLinksMethods(pgStores.AgentLinks, pgStores.Agents, agentRouter, msgBus, msgBus).Register(server.Router())
	}

	// Register agent teams WS RPC methods
	if pgStores.Teams != nil {
		methods.NewTeamsMethods(pgStores.Teams, pgStores.Agents, pgStores.AgentLinks, agentRouter, msgBus, msgBus, dataDir).Register(server.Router())
	}
}

// wireChannelEventSubscribers sets up event subscribers for channel instance cache invalidation,
// pairing approval/revocation, and agent cascade disable.
func wireChannelEventSubscribers(
	msgBus *bus.MessageBus,
	server *gateway.Server,
	pgStores *store.Stores,
	channelMgr *channels.Manager,
	instanceLoader *channels.InstanceLoader,
	pairingMethods *methods.PairingMethods,
	cfg *config.Config,
) {
	// Cache invalidation: reload channel instances on changes.
	if instanceLoader != nil {
		msgBus.Subscribe(bus.TopicCacheChannelInstances, func(event bus.Event) {
			if event.Name != protocol.EventCacheInvalidate {
				return
			}
			payload, ok := event.Payload.(bus.CacheInvalidatePayload)
			if !ok || payload.Kind != bus.CacheKindChannelInstances {
				return
			}
			go instanceLoader.Reload(context.Background())
		})
	}

	// Wire pairing approval notification → channel (matching TS notifyPairingApproved).
	botName := cfg.ResolveDisplayName("default")
	pairingMethods.SetOnApprove(func(ctx context.Context, channel, chatID, senderID string) {
		// Browser/internal channels use WebSocket — UI polls approval status directly.
		if channels.IsInternalChannel(channel) {
			slog.Debug("pairing approved for internal channel, skipping notification", "channel", channel)
			return
		}
		msg := fmt.Sprintf("✅ %s access approved. Send a message to start chatting.", botName)
		// Group pairings need group_id metadata so channels (e.g. Zalo) route to group API.
		if strings.HasPrefix(senderID, "group:") {
			msgBus.PublishOutbound(bus.OutboundMessage{
				Channel:  channel,
				ChatID:   chatID,
				Content:  msg,
				Metadata: map[string]string{"group_id": chatID},
			})
		} else if err := channelMgr.SendToChannel(ctx, channel, chatID, msg); err != nil {
			slog.Warn("failed to send pairing approval notification", "channel", channel, "chatID", chatID, "error", err)
		}
	})

	// Wire pairing revocation → force disconnect active WebSocket sessions.
	msgBus.Subscribe(bus.TopicPairingRevoked, func(event bus.Event) {
		if event.Name != bus.EventPairingRevoked {
			return
		}
		payload, ok := event.Payload.(bus.PairingRevokedPayload)
		if !ok {
			return
		}
		go server.DisconnectByPairing(payload.SenderID, payload.Channel)
	})

	// Cascade: when an agent becomes inactive, disable its linked channel instances.
	if pgStores.ChannelInstances != nil {
		ciStore := pgStores.ChannelInstances
		msgBus.Subscribe(bus.TopicAgentStatusChanged, func(event bus.Event) {
			if event.Name != bus.EventAgentStatusChanged {
				return
			}
			payload, ok := event.Payload.(bus.AgentStatusChangedPayload)
			if !ok || payload.NewStatus != store.AgentStatusInactive {
				return
			}
			go func() {
				agentID, err := uuid.Parse(payload.AgentID)
				if err != nil {
					return
				}
				all, err := ciStore.ListAllInstances(context.Background())
				if err != nil {
					slog.Warn("cascade disable: failed to list channel instances", "error", err)
					return
				}
				disabled := 0
				for _, inst := range all {
					if inst.AgentID == agentID && inst.Enabled {
						if err := ciStore.Update(store.WithTenantID(context.Background(), inst.TenantID), inst.ID, map[string]any{"enabled": false}); err != nil {
							slog.Warn("cascade disable: failed to disable channel instance", "name", inst.Name, "error", err)
						} else {
							disabled++
						}
					}
				}
				if disabled > 0 {
					slog.Info("cascade disabled channel instances for inactive agent", "agent_id", payload.AgentID, "count", disabled)
					// Trigger channel reload so disabled instances are stopped.
					msgBus.Broadcast(bus.Event{
						Name:    protocol.EventCacheInvalidate,
						Payload: bus.CacheInvalidatePayload{Kind: bus.CacheKindChannelInstances},
					})
				}
			}()
		})
	}
}

