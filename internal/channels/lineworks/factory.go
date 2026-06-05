package lineworks

import (
	"encoding/json"
	"fmt"

	lw "github.com/nextlevelbuilder/goclaw/internal/lineworks"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// lineWorksCreds maps the decrypted credentials JSON from the channel_instances
// table. Two distinct secrets are stored: client_secret authenticates the OAuth
// token exchange, while bot_secret is the HMAC key for callback signature
// verification — they are NOT interchangeable, so both must be present.
type lineWorksCreds struct {
	BotID          string `json:"bot_id"`
	BotSecret      string `json:"bot_secret"`
	ServiceAccount string `json:"service_account"`
	ClientID       string `json:"client_id"`
	ClientSecret   string `json:"client_secret"`
	PrivateKey     string `json:"private_key"` // RSA private key, PEM
	DomainID       string `json:"domain_id"`
}

// lineWorksInstanceConfig maps the non-secret config JSONB from the
// channel_instances table.
type lineWorksInstanceConfig struct {
	AllowFrom                  []string `json:"allow_from,omitempty"`
	DMPolicy                   string   `json:"dm_policy,omitempty"`
	GroupPolicy                string   `json:"group_policy,omitempty"`
	DirectoryExternalKeyPrefix string   `json:"directory_external_key_prefix,omitempty"`
	Scopes                     []string `json:"scopes,omitempty"`
}

// Factory creates a LINE WORKS channel from DB instance data. It satisfies the
// channels.ChannelFactory signature and is registered for the "lineworks"
// channel type by the gateway's InstanceLoader wiring.
//
// pairingSvc is accepted for signature compatibility and wired into the channel
// via the embedded BaseChannel; LINE WORKS uses the same DM/group policy gate
// as the other channels. pendingStore (when non-nil) backs the group history
// with DB persistence — see FactoryWithPendingStore.
func Factory(name string, creds json.RawMessage, cfg json.RawMessage,
	msgBus *bus.MessageBus, pairingSvc store.PairingStore,
	pendingStore store.PendingMessageStore) (channels.Channel, error) {

	var cr lineWorksCreds
	if len(creds) > 0 {
		if err := json.Unmarshal(creds, &cr); err != nil {
			return nil, fmt.Errorf("decode lineworks credentials: %w", err)
		}
	}
	if cr.BotID == "" || cr.BotSecret == "" {
		return nil, fmt.Errorf("lineworks bot_id and bot_secret are required")
	}
	if cr.ClientID == "" || cr.ClientSecret == "" || cr.ServiceAccount == "" || cr.PrivateKey == "" {
		return nil, fmt.Errorf("lineworks client_id, client_secret, service_account and private_key are required")
	}

	var ic lineWorksInstanceConfig
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &ic); err != nil {
			return nil, fmt.Errorf("decode lineworks config: %w", err)
		}
	}

	// Token source (JWT RS256 → bearer, cached/auto-refreshed by the SDK).
	ts, err := lw.NewTokenSource(lw.AuthConfig{
		ClientID:       cr.ClientID,
		ClientSecret:   cr.ClientSecret,
		ServiceAccount: cr.ServiceAccount,
		PrivateKeyPEM:  cr.PrivateKey,
		Scopes:         ic.Scopes, // empty → SDK default {bot, bot.message}
	})
	if err != nil {
		return nil, fmt.Errorf("lineworks token source: %w", err)
	}

	sdkClient, err := lw.NewClient(lw.ClientConfig{
		BotID:  cr.BotID,
		Tokens: ts,
	})
	if err != nil {
		return nil, fmt.Errorf("lineworks client: %w", err)
	}

	channelCfg := Config{
		BotSecret:   cr.BotSecret,
		AllowFrom:   ic.AllowFrom,
		DMPolicy:    ic.DMPolicy,
		GroupPolicy: ic.GroupPolicy,
	}

	ch := New(&clientAdapter{Client: sdkClient}, channelCfg, msgBus)
	ch.SetName(name)
	if pairingSvc != nil {
		ch.SetPairingService(pairingSvc)
	}
	// Wire the pending-message store so Start builds a DB-backed group history.
	if pendingStore != nil {
		ch.SetPendingStore(pendingStore)
	}
	return ch, nil
}

// FactoryWithPendingStore returns a ChannelFactory, matching the line channel's
// helper shape so the gateway can register lineworks uniformly. The pending
// store is threaded into the channel so group-history is DB-backed (used for
// @-mention gating context accumulation).
func FactoryWithPendingStore(pendingStore store.PendingMessageStore) channels.ChannelFactory {
	return func(name string, creds json.RawMessage, cfg json.RawMessage,
		msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {
		return Factory(name, creds, cfg, msgBus, pairingSvc, pendingStore)
	}
}

// clientAdapter bridges the SDK's *lineworks.Client to the channel's botClient
// interface. The SDK already exposes the two send methods botClient needs
// (SendTextToUser / SendTextToChannel) via the embedded *lw.Client; the adapter
// only adds the InvalidateToken method the channel's auth-error retry expects.
type clientAdapter struct {
	*lw.Client
}

var _ botClient = (*clientAdapter)(nil)

// InvalidateToken is a best-effort no-op: the SDK TokenSource auto-refreshes on
// expiry (with a skew window), so the channel's one-shot 401 retry simply
// re-issues the request; if the cached token had genuinely expired the SDK
// mints a fresh one transparently on the retry. The hook exists so a future SDK
// that exposes explicit token invalidation can be wired here without touching
// the channel's send path.
func (a *clientAdapter) InvalidateToken() {}
