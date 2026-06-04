package lineworksworkflow

import (
	"context"
	"time"

	channel "github.com/nextlevelbuilder/goclaw/internal/channels/lineworks"
)

// DirectoryResolver looks up a LINE WORKS user's external key via the
// Directory API (GET /users/{userId} → userExternalKey). The lineworks SDK
// Client (internal/lineworks) satisfies this; declaring it as an interface
// here keeps the plugin decoupled from the concrete client and trivially
// mockable in tests.
type DirectoryResolver interface {
	// UserExternalKey returns the userExternalKey for the given LINE WORKS
	// resource userId. Requires directory.read (or user.read) scope.
	UserExternalKey(ctx context.Context, userID string) (string, error)
}

// Config holds everything the LINE WORKS workflow plugin needs. All tunables
// are explicit struct fields (not env reads buried in helpers) so tests can
// pass a literal Config. Production wiring in cmd/ fills it at startup.
type Config struct {
	// Sender is the LINE WORKS reply channel. Production passes the
	// *lineworks.Channel which satisfies channel.Sender.
	Sender channel.Sender

	// Directory resolves LINE WORKS userId → userExternalKey. Production
	// passes the internal/lineworks SDK Client. May be nil in tests that
	// stub resolution at a higher layer.
	Directory DirectoryResolver

	// MCPURL is the Odoo MCP HTTP endpoint (JSON-RPC tools/call).
	MCPURL string

	// MCPToken is the bearer token for MCPURL.
	MCPToken string

	// OdooBaseURL is the optional Odoo web base URL for deep links. If
	// empty, callers that need it derive it from MCPURL.
	OdooBaseURL string

	// ExternalKeyPrefix is the prefix stamped on each LINE WORKS user's
	// externalKey by directory sync. The employee id is the suffix:
	// "<prefix>{hr_employee.id}". Zero value falls back to the default
	// "odoo-emp-".
	ExternalKeyPrefix string

	// TodoProjectID is the project.project the todo flow files tasks into.
	// create_my_todo requires a project_id; this is the default used when
	// the user does not specify one. Zero means the todo flow replies with
	// a configuration hint instead of guessing.
	TodoProjectID int

	// IdentityCacheTTL is how long a resolved (userId → odoo uid) mapping is
	// cached in memory. Zero means use default (30m).
	IdentityCacheTTL time.Duration
}

// Default values used by WithDefaults. Unexported package vars so tests can
// reference them without duplicating literals.
var (
	defaultExternalKeyPrefix = "odoo-emp-"
	defaultIdentityCacheTTL  = 30 * time.Minute
)

// WithDefaults returns a copy of c with zero-value optional fields filled in.
// Required fields (Sender, MCPURL, MCPToken, Directory) are NOT filled — the
// caller validates those at startup.
func (c Config) WithDefaults() Config {
	if c.ExternalKeyPrefix == "" {
		c.ExternalKeyPrefix = defaultExternalKeyPrefix
	}
	if c.IdentityCacheTTL == 0 {
		c.IdentityCacheTTL = defaultIdentityCacheTTL
	}
	return c
}
