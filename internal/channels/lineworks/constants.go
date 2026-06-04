package lineworks

// Channel type / wiring constants for the LINE WORKS Bot channel.
//
// These are intentionally independent from the consumer-grade LINE channel
// (internal/channels/line) — LINE WORKS is a separate platform with its own
// webhook path, authentication model, and message API. The two channels must
// coexist in the same gateway process without sharing any state.
const (
	// ChannelType is the platform type registered with the channel manager and
	// the InstanceLoader factory map. Distinct from "line".
	ChannelType = "lineworks"

	// webhookPath is mounted on the main gateway mux by the WebhookChannel
	// auto-wiring (cmd/gateway_lifecycle.go WebhookHandlers()). Independent from
	// "/webhook/line".
	webhookPath = "/webhook/lineworks"

	// signatureHeader is the callback signature header LINE WORKS sends. Its
	// value = base64(HMAC-SHA256(botSecret, rawBody)). NOTE: the HMAC key is the
	// Bot Secret, NOT the OAuth client_secret.
	signatureHeader = "X-WORKS-Signature"

	// senderPrefix namespaces LINE WORKS sender IDs on the bus / allowlist so
	// "lineworks:<userId>" entries can never collide with "line:<userId>".
	senderPrefix = "lineworks:"

	// maxTextLength is the LINE WORKS Bot per-message text cap (2000 chars).
	// Longer agent replies are chunked into multiple sends.
	maxTextLength = 2000

	// directoryCacheTTLSeconds is how long a resolved (userId -> externalKey)
	// mapping may be cached before re-querying the LINE WORKS Directory.
	directoryCacheTTLSeconds = 600
)

// peerDirect / peerGroup are the peer-kind discriminators used across the
// channel. They map onto goclaw's "direct"/"group" bus peer kinds. A callback
// whose source.channelId is set is a group/room conversation; otherwise it is a
// 1:1 chat.
const (
	peerDirect = "direct"
	peerGroup  = "group"
)

// Outbound routing metadata keys. handleMessageEvent stamps these onto the
// InboundMessage so Send() can route the agent's reply back to the correct Bot
// endpoint (POST /bots/{botId}/users/{userId}/messages vs
// /bots/{botId}/channels/{channelId}/messages) without re-deriving the peer.
const (
	metaPeerKind  = "lw_peer_kind"
	metaUserID    = "lw_user_id"
	metaChannelID = "lw_channel_id"
)

// content.type values inside a message callback event. Only text is in v1
// scope; other content types are acknowledged-and-ignored.
const (
	contentTypeText = "text"
)
