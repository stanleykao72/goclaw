// Package lineworks' hooks.go defines the MessageHook plugin interface that
// lets goclaw deployments subscribe to LINE WORKS bot callback events without
// modifying the channel adapter itself.
//
// This mirrors internal/channels/line/hooks.go but is an INDEPENDENT
// declaration — the lineworks channel deliberately does NOT share the line
// package's hook types. That keeps the consumer-LINE channel and the LINE
// WORKS channel fully decoupled (different event shapes, different SDKs,
// independent evolution).
//
// Design intent:
//
//   - Hooks receive flat, typed event structs (TextEvent, PostbackEvent).
//     They must NOT import the lineworks SDK callback types.
//   - Hooks use a Sender interface (separate from MessageHook) to push
//     replies back to LINE WORKS. The channel implements Sender.
//   - Fan-out: every registered hook receives every event. Hooks that don't
//     care return nil immediately. Hooks MUST be independent — one hook's
//     error must not block another.
//   - Optional Lifecycle interface for hooks that need Start/Stop (e.g.
//     background goroutines / identity-cache warmup). Channel.Start calls
//     Start after its own init; Channel.Stop calls Stop in reverse order.
package lineworks

import "context"

// TextEvent is delivered when a LINE WORKS `message` callback with a text
// content arrives.
//
// UserID is the source resource ID (source.userId). ChannelID is the
// room/group id (source.channelId) — empty for a 1:1 conversation. ChatID is
// the normalized conversation key the channel uses on the bus: it equals
// ChannelID for a group, or UserID for a 1:1 chat. For a 1:1 message replies
// go to the user; for a group message replies go to the channel.
type TextEvent struct {
	UserID    string
	ChatID    string
	ChannelID string
	Text      string
}

// PostbackEvent is delivered when a LINE WORKS `postback` callback arrives.
// Per the callback contract the postback payload is the TOP-LEVEL `data`
// string (NOT content.postback). ChatID is the normalized conversation key
// (ChannelID for a group, UserID for a 1:1) so hooks can reply via Sender
// without re-deriving the peer.
type PostbackEvent struct {
	UserID    string
	ChatID    string
	ChannelID string
	Data      string
}

// MessageHook is implemented by plugins that want to observe LINE WORKS bot
// events.
//
// Hooks MUST be safe to call concurrently with other hook invocations. Hooks
// MUST NOT depend on side effects from other hooks within the same event
// delivery. Returning an error is logged by the channel but does not block
// other hooks or retry the event.
type MessageHook interface {
	OnText(ctx context.Context, ev TextEvent) error
	OnPostback(ctx context.Context, ev PostbackEvent) error
}

// Sender is the reply-capable interface that hooks use to push messages back
// to LINE WORKS. The Channel implements this — hooks receive it via their
// Config at construction time, not via the MessageHook method signatures, so
// the interface stays focused on delivery.
//
// Routing rule: when channelID is non-empty the message targets the
// room/group (POST /bots/{botId}/channels/{channelId}/messages); when
// channelID is empty it targets the user 1:1 (POST
// /bots/{botId}/users/{userId}/messages).
type Sender interface {
	// SendText sends one plain-text message (<=2000 chars; longer is the
	// caller's responsibility to chunk). userID is always required;
	// channelID is empty for 1:1.
	SendText(ctx context.Context, userID, channelID, text string) error
}

// Lifecycle is optionally implemented by hooks that need Start/Stop around
// the channel's own lifecycle. Hooks that need no background work skip it.
type Lifecycle interface {
	Start(ctx context.Context) error
	Stop() error
}

// MessageGate decides, synchronously and BEFORE the agent path runs, whether an
// inbound text message may proceed. Unlike MessageHook (parallel, observe-only)
// a gate can BLOCK the message: returning allow=false stops the channel from
// forwarding the message to the agent, and the channel sends `reply` (when
// non-empty) back to the sender instead.
//
// At most one gate is installed per channel (via SetGate). It runs after the
// policy + content-type checks and before hook fan-out and HandleMessage, so a
// blocked sender reaches neither plugins nor the agent. Gates should fail OPEN
// (allow=true) on transient/internal errors so an outage does not lock out
// legitimate users.
type MessageGate interface {
	Gate(ctx context.Context, ev TextEvent) (allow bool, reply string)
}
