// Package esmithkm is the e-smith km-meeting writeback plugin for goclaw.
//
// It implements the line.MessageHook interface to own the entire km-meeting
// ingest → conversation → Odoo writeback workflow that used to live inside
// internal/channels/line/. Moving it out lets the channel adapter stay
// generic (upstream-able to nextlevelbuilder/goclaw) while the e-smith
// vertical continues to live in fork-only code.
//
// # One-way import rule
//
// This package imports internal/channels/line ONLY for interface types
// (MessageHook, LineSender, AudioEvent, TextEvent, PostbackEvent, Lifecycle).
// It MUST NOT reference the concrete *line.Channel type. The dependency
// arrow is always esmith-km → channels/line, never the other way around.
// The channel package has zero knowledge of this plugin.
//
// # Wiring
//
// Plugin registration happens in cmd/ at channel factory time. When the
// LINE channel is constructed, the cmd layer checks for the required
// esmith env vars (MCPURL, MCPToken); if present, it builds a Config,
// constructs a *Hook via New, and calls lineCh.RegisterHook(hook).
// If the env vars are missing, no hook is registered and the channel
// behaves like a generic LINE adapter — this preserves backwards
// compatibility for non-esmith deployments that might ever use this fork.
//
// # Lifecycle
//
// Hook implements line.Lifecycle. Start launches the draft watcher
// goroutine that polls /data/km/meetings/drafts. Stop cancels it. The
// watcher pushes Flex picker bubbles to LINE chats when new drafts
// appear with line_chat_id set.
//
// # See also
//
// The design decisions are documented in the change proposal
// goclaw-line-channel-extract-esmith (design.md Decision 1-8).
package esmithkm
