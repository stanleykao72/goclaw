package esmithkm

import (
	"context"

	"github.com/nextlevelbuilder/goclaw/internal/channels/line"
)

// Compile-time interface checks — if Hook ever drifts from the contract
// the build breaks loudly.
var (
	_ line.MessageHook = (*Hook)(nil)
	_ line.Lifecycle   = (*Hook)(nil)
)

// Hook is the e-smith km-meeting plugin's line.MessageHook implementation.
// It owns the full ingest → conversation → Odoo writeback pipeline.
//
// Phase 2: skeleton only. OnAudio / OnText / OnPostback are no-ops.
// Phases 3-4 move the real logic from internal/channels/line/ into this
// package and flesh out the methods.
type Hook struct {
	cfg Config
}

// New constructs a Hook with the given Config. Missing optional fields
// get filled in by Config.WithDefaults. Required fields (Sender, MCPURL,
// MCPToken) are the caller's responsibility — this constructor does not
// validate them so tests can pass zero values for fields they don't care
// about.
func New(cfg Config) *Hook {
	return &Hook{cfg: cfg.WithDefaults()}
}

// OnAudio is called by the LINE channel for every AudioMessage event.
// Phase 2: no-op. Phase 3 moves ingestLineAudio here.
func (h *Hook) OnAudio(_ context.Context, _ line.AudioEvent) error {
	return nil
}

// OnText is called by the LINE channel for every TextMessage event.
// Phase 2: no-op. Phase 3 moves ingestGdriveLinks here.
func (h *Hook) OnText(_ context.Context, _ line.TextEvent) error {
	return nil
}

// OnPostback is called by the LINE channel for every Postback event.
// Phase 2: no-op. Phase 4 moves handlePostback here.
func (h *Hook) OnPostback(_ context.Context, _ line.PostbackEvent) error {
	return nil
}

// Start launches any background goroutines the hook needs (the draft
// watcher in phase 4). Phase 2: no-op.
func (h *Hook) Start(_ context.Context) error {
	return nil
}

// Stop tears down background work started in Start. Phase 2: no-op.
func (h *Hook) Stop() error {
	return nil
}
