package lineworksworkflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// errNoIdentity is returned by resolveOdooUID when the LINE WORKS user could
// not be mapped to an Odoo res.users id. Callers turn it into a
// user-facing configuration hint rather than failing silently.
var errNoIdentity = errors.New("lineworks-workflow: no Odoo identity for LINE WORKS user")

// resolvedIdentity is the result of mapping a LINE WORKS resource userId to
// Odoo. EmployeeID is hr.employee.id (parsed from the externalKey); UID is
// res.users.id.
type resolvedIdentity struct {
	EmployeeID int
	UID        int
}

// identityCache is an in-memory userId → resolvedIdentity map with per-entry
// TTL. Concurrency-safe; the plugin shares one instance.
type identityCache struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]identityCacheEntry
}

type identityCacheEntry struct {
	id     resolvedIdentity
	expiry time.Time
}

func newIdentityCache(ttl time.Duration) *identityCache {
	return &identityCache{ttl: ttl, m: make(map[string]identityCacheEntry)}
}

func (c *identityCache) get(userID string) (resolvedIdentity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[userID]
	if !ok || time.Now().After(e.expiry) {
		return resolvedIdentity{}, false
	}
	return e.id, true
}

func (c *identityCache) put(userID string, id resolvedIdentity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[userID] = identityCacheEntry{id: id, expiry: time.Now().Add(c.ttl)}
}

// resolveOdooUID maps a LINE WORKS resource userId to an Odoo res.users id.
//
// Source-of-truth path (per the implementation contract):
//
//  1. Directory: GET /users/{userId} → userExternalKey == "<prefix>{empId}".
//  2. Parse empId out of the externalKey using cfg.ExternalKeyPrefix.
//  3. Odoo MCP search_records res.users where employee_id = empId → uid.
//
// Results are cached in memory with cfg.IdentityCacheTTL. Returns
// errNoIdentity (wrapped) when any step yields nothing usable, so the hook
// layer can reply with a binding hint.
func (h *Hook) resolveOdooUID(ctx context.Context, userID string) (resolvedIdentity, error) {
	if userID == "" {
		return resolvedIdentity{}, errNoIdentity
	}
	if id, ok := h.identity.get(userID); ok {
		return id, nil
	}

	if h.cfg.Directory == nil {
		return resolvedIdentity{}, fmt.Errorf("%w: directory resolver not configured", errNoIdentity)
	}
	externalKey, err := h.cfg.Directory.UserExternalKey(ctx, userID)
	if err != nil {
		return resolvedIdentity{}, fmt.Errorf("directory lookup for %q: %w", userID, err)
	}

	empID, err := parseEmployeeID(externalKey, h.cfg.ExternalKeyPrefix)
	if err != nil {
		return resolvedIdentity{}, fmt.Errorf("%w: %v (externalKey=%q)", errNoIdentity, err, externalKey)
	}

	uid, err := h.lookupOdooUID(ctx, empID)
	if err != nil {
		return resolvedIdentity{}, err
	}
	if uid == 0 {
		return resolvedIdentity{}, fmt.Errorf("%w: no res.users for employee_id=%d", errNoIdentity, empID)
	}

	id := resolvedIdentity{EmployeeID: empID, UID: uid}
	h.identity.put(userID, id)
	return id, nil
}

// parseEmployeeID extracts the hr.employee.id from an externalKey of the form
// "<prefix>{id}". An empty prefix is tolerated (whole key must be the id).
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
	empID, err := strconv.Atoi(externalKey)
	if err != nil || empID <= 0 {
		return 0, fmt.Errorf("externalKey suffix %q is not a positive employee id", externalKey)
	}
	return empID, nil
}

// lookupOdooUID runs an Odoo MCP search_records on res.users filtered by
// employee_id, returning the first matching user id (0 if none).
func (h *Hook) lookupOdooUID(ctx context.Context, empID int) (int, error) {
	var rows []struct {
		ID int `json:"id"`
	}
	err := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "search_records", map[string]any{
		"model":  "res.users",
		"domain": [][]any{{"employee_id", "=", empID}},
		"fields": []string{"id"},
		"limit":  1,
	}, &rows)
	if err != nil {
		return 0, fmt.Errorf("search res.users employee_id=%d: %w", empID, err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].ID, nil
}
