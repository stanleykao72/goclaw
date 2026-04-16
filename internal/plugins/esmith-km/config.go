package esmithkm

import (
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/channels/line"
)

// Config holds everything the e-smith km-meeting plugin needs to run. All
// paths, URLs and tunables are explicit struct fields (not env-var reads
// buried inside helper functions) so tests can pass a literal Config and
// skip env entirely. Production wiring in cmd/ reads env vars into this
// struct at startup.
type Config struct {
	// Sender is the LINE reply channel. Production wiring passes the
	// *line.Channel which satisfies line.LineSender.
	Sender line.LineSender

	// InboxDir is where downloaded audio files + GDrive ingest output
	// land. km-meeting-pipeline.sh upload cron picks up from here.
	// Production: /data/km/meetings/inbox
	InboxDir string

	// DraftsDir is where the upload cron writes per-meeting draft state
	// files that the plugin's watcher polls. Production: /data/km/meetings/drafts
	DraftsDir string

	// PublishedDir is where finalized drafts get moved after successful
	// Odoo writeback. Production: /data/km/meetings/drafts/published
	PublishedDir string

	// PipelineScript is the absolute path to km-meeting-pipeline.sh. The
	// plugin shells out to it for ingest-gdrive, publish-odoo, etc.
	PipelineScript string

	// MCPURL is the stage35 Odoo MCP HTTP endpoint for job.meeting.minutes
	// reads + MCP tool calls.
	MCPURL string

	// MCPToken is the bearer token for MCPURL.
	MCPToken string

	// OdooBaseURL is the optional Odoo web base URL for deep links. If
	// empty, the plugin derives it from MCPURL.
	OdooBaseURL string

	// LIFFAttendeesURL is the liff.line.me URL the attendees picker
	// bubble sends users to. Empty means the plugin is running without
	// the LIFF path configured and the bubble degrades to a
	// configuration-hint message; production must set it.
	LIFFAttendeesURL string

	// DraftPollInterval is how often the draft watcher ticks. Zero means
	// use default (10s).
	DraftPollInterval time.Duration

	// DraftStaleTTL is the age after which an unresolved draft gets the
	// stale-cleanup notification. Zero means use default (24h).
	DraftStaleTTL time.Duration

	// DedupTTL is the LINE webhook resend dedup cache TTL. Zero means
	// use default (1h).
	DedupTTL time.Duration
}

// Default values used by WithDefaults. Kept as unexported package vars so
// tests can reference them without duplicating the constants.
var (
	defaultInboxDir          = "/data/km/meetings/inbox"
	defaultDraftsDir         = "/data/km/meetings/drafts"
	defaultPublishedDir      = "/data/km/meetings/drafts/published"
	defaultPipelineScript    = "/home/ubuntu/odoo_dev/esmith-specs/scripts/km-meeting-pipeline.sh"
	defaultDraftPollInterval = 10 * time.Second
	defaultDraftStaleTTL     = 24 * time.Hour
	defaultDedupTTL          = 1 * time.Hour
)

// WithDefaults returns a copy of c with any zero-value fields filled in
// with production defaults. Required fields (Sender, MCPURL, MCPToken)
// are NOT filled — the caller must validate those at startup.
func (c Config) WithDefaults() Config {
	if c.InboxDir == "" {
		c.InboxDir = defaultInboxDir
	}
	if c.DraftsDir == "" {
		c.DraftsDir = defaultDraftsDir
	}
	if c.PublishedDir == "" {
		c.PublishedDir = defaultPublishedDir
	}
	if c.PipelineScript == "" {
		c.PipelineScript = defaultPipelineScript
	}
	if c.DraftPollInterval == 0 {
		c.DraftPollInterval = defaultDraftPollInterval
	}
	if c.DraftStaleTTL == 0 {
		c.DraftStaleTTL = defaultDraftStaleTTL
	}
	if c.DedupTTL == 0 {
		c.DedupTTL = defaultDedupTTL
	}
	return c
}
