package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/nlmdoc"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// NLM provisioning config. The Drive root folder is overridable so tests /
// alternate deployments do not collide; the tenant slug names the per-tenant
// subtree under it.
const (
	// envNLMDriveRoot overrides the top-level Drive folder. Default below.
	envNLMDriveRoot = "GOCLAW_NLM_DRIVE_ROOT"
	// defaultNLMDriveRoot is the top-level Drive folder for all memory Docs.
	defaultNLMDriveRoot = "goclaw-memory"
)

// nlmDriveRoot resolves the Drive root folder name (env → default).
func nlmDriveRoot() string {
	if v := strings.TrimSpace(os.Getenv(envNLMDriveRoot)); v != "" {
		return v
	}
	return defaultNLMDriveRoot
}

// NLMNotebookRunner creates/manages NotebookLM notebooks via the nlm CLI. It
// mirrors notebook_recall's nlmRunner abstraction (argv slice, no shell
// interpolation) but exposes the higher-level operations the provisioner needs,
// so unit tests inject a fake and never touch real nlm.
type NLMNotebookRunner interface {
	// CreateNotebook creates a notebook with the given title and returns its id.
	CreateNotebook(ctx context.Context, title string) (notebookID string, err error)
	// AddDriveSource adds a Google Doc (by Drive file id) as a --type doc source.
	AddDriveSource(ctx context.Context, notebookID, driveDocID string) error
	// DeleteNotebook removes a notebook (used to clean up a lost race).
	DeleteNotebook(ctx context.Context, notebookID string) error
	// SourceSync refreshes a notebook from its (just-updated) Drive source(s).
	// The ingest worker (sub-phase 2.3) calls this after appending a drained
	// window to the scope's Doc, so the notebook re-reads the new content.
	SourceSync(ctx context.Context, notebookID string) error
}

// execNLMNotebookRunner shells out to the nlm CLI. The binary is resolved the
// same way notebook_recall resolves it (configured → env → "nlm").
type execNLMNotebookRunner struct {
	binary string
	run    nlmRunner // reuse notebook_recall.go's injectable runner type
}

// NewExecNLMNotebookRunner constructs the real nlm-backed runner. An empty
// binary resolves via GOCLAW_NLM_BINARY → PATH "nlm".
func NewExecNLMNotebookRunner(binary string) *execNLMNotebookRunner {
	return &execNLMNotebookRunner{binary: binary, run: defaultNLMRunner}
}

func (r *execNLMNotebookRunner) resolveBinary() string {
	if r.binary != "" {
		return r.binary
	}
	if v := strings.TrimSpace(os.Getenv(nlmEnvBinary)); v != "" {
		return v
	}
	return defaultNLMBinary
}

func (r *execNLMNotebookRunner) CreateNotebook(ctx context.Context, title string) (string, error) {
	out, err := r.run(ctx, r.resolveBinary(), []string{"notebook", "create", title})
	if err != nil {
		return "", fmt.Errorf("nlm notebook create: %w", err)
	}
	// `nlm notebook create` prints human-readable text, e.g.
	//   ✓ Created notebook: <title>
	//     ID: <uuid>
	// Extract the notebook UUID — NOT the whole output (a stray full string here
	// would be passed as the notebook id to source add / delete and fail).
	id := extractNotebookID(string(out))
	if id == "" {
		return "", fmt.Errorf("nlm notebook create: no notebook id in output: %q", strings.TrimSpace(string(out)))
	}
	return id, nil
}

// notebookIDRe matches a canonical UUID (the notebook id nlm prints after "ID:").
// The scope-derived title only carries an 8-hex fragment (e.g. user-2e56b7af-…),
// which cannot match this full 8-4-4-4-12 pattern, so the first match is the id.
var notebookIDRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// extractNotebookID pulls the notebook UUID from nlm's create output, tolerating
// either the human-readable banner or a bare id.
func extractNotebookID(out string) string {
	return notebookIDRe.FindString(out)
}

func (r *execNLMNotebookRunner) AddDriveSource(ctx context.Context, notebookID, driveDocID string) error {
	_, err := r.run(ctx, r.resolveBinary(),
		[]string{"source", "add", notebookID, "--drive", driveDocID, "--type", "doc", "--wait"})
	if err != nil {
		return fmt.Errorf("nlm source add: %w", err)
	}
	return nil
}

func (r *execNLMNotebookRunner) DeleteNotebook(ctx context.Context, notebookID string) error {
	_, err := r.run(ctx, r.resolveBinary(), []string{"notebook", "delete", notebookID, "--force"})
	if err != nil {
		return fmt.Errorf("nlm notebook delete: %w", err)
	}
	return nil
}

func (r *execNLMNotebookRunner) SourceSync(ctx context.Context, notebookID string) error {
	_, err := r.run(ctx, r.resolveBinary(), []string{"source", "sync", notebookID})
	if err != nil {
		return fmt.Errorf("nlm source sync: %w", err)
	}
	return nil
}

// NotebookProvisioner performs lazy first-write notebook creation for a scope.
// Every dependency is injected so unit tests exercise the create/race logic
// with fakes (no real Drive / nlm / DB).
type NotebookProvisioner struct {
	store    store.NotebookPointerStore
	docs     nlmdoc.DocLibrary
	notebook NLMNotebookRunner
	root     string // Drive root folder name
}

// NewNotebookProvisioner constructs a provisioner. An empty root resolves via
// GOCLAW_NLM_DRIVE_ROOT → default.
func NewNotebookProvisioner(ptrStore store.NotebookPointerStore, docs nlmdoc.DocLibrary, notebook NLMNotebookRunner, root string) *NotebookProvisioner {
	if root == "" {
		root = nlmDriveRoot()
	}
	return &NotebookProvisioner{store: ptrStore, docs: docs, notebook: notebook, root: root}
}

// GetOrCreateScopeNotebook returns the (notebookID, driveDocID) for a scope,
// lazily creating the Drive Doc + NotebookLM notebook + pointer on first write.
//
// LAZY-CREATE ORDER (on a pointer miss):
//  1. EnsureFolder  goclaw-memory/<tenantSlug>/<scopeKind>/   (idempotent)
//  2. CreateDoc     "<scopeKind>-<id8>-<displayName>"          (Drive Doc)
//  3. nlm notebook create  → notebookID
//  4. nlm source add --drive <docID> --type doc
//  5. store.Create  (INSERT ... ON CONFLICT DO NOTHING + re-SELECT)
//
// RACE SAFETY: a fast-path Get short-circuits when the pointer already exists
// (no creation). On a genuine miss, two concurrent callers may each run steps
// 1-4, but step 5 is arbitrated by the UNIQUE(tenant,kind,scope_id) constraint:
// store.Create returns the single winning row. If the returned row's
// notebook_id differs from the one WE just created, a racer won — we delete our
// orphaned notebook (and leave the Doc; folder/Doc creation is idempotent and a
// stray Doc is harmless and human-inspectable) and return the winner's pair.
// This keeps the operation idempotent without needing a DB advisory lock,
// preserving the db-less injectable test design.
//
// tenant / scopeKind / scopeID MUST come from the verified injected identity
// (see scopeKeyForCtx) — never from LLM args or message content.
func (p *NotebookProvisioner) GetOrCreateScopeNotebook(ctx context.Context, tenant uuid.UUID, scopeKind, scopeID, displayName string) (notebookID, driveDocID string, err error) {
	// Fast path: existing pointer → reuse, no creation.
	if existing, ok, gerr := p.store.Get(ctx, tenant, scopeKind, scopeID); gerr != nil {
		return "", "", gerr
	} else if ok {
		return existing.NotebookID, existing.DriveDocID, nil
	}

	tenantSlug := store.TenantSlugFromContext(ctx)
	if tenantSlug == "" {
		tenantSlug = tenant.String()
	}

	// 1. Folder tree.
	folderID, err := p.docs.EnsureFolder(ctx, []string{
		p.root,
		nlmdoc.SanitizePathSegment(tenantSlug),
		nlmdoc.SanitizePathSegment(scopeKind),
	})
	if err != nil {
		return "", "", fmt.Errorf("ensure folder: %w", err)
	}

	// 2. Drive Doc.
	title := nlmdoc.DocName(scopeKind, scopeID, displayName)
	docID, err := p.docs.CreateDoc(ctx, folderID, title, "")
	if err != nil {
		return "", "", fmt.Errorf("create doc: %w", err)
	}

	// 3. NotebookLM notebook.
	nbID, err := p.notebook.CreateNotebook(ctx, title)
	if err != nil {
		return "", "", fmt.Errorf("create notebook: %w", err)
	}

	// 4. Add the Doc as a --drive source.
	if err := p.notebook.AddDriveSource(ctx, nbID, docID); err != nil {
		// Our notebook is unusable without its source; clean it up before
		// bailing so we do not leak an empty notebook.
		p.cleanupNotebook(ctx, nbID)
		return "", "", fmt.Errorf("add drive source: %w", err)
	}

	// 5. Race-safe pointer insert.
	winner, err := p.store.Create(ctx, store.NotebookPointer{
		TenantID:    tenant,
		ScopeKind:   scopeKind,
		ScopeID:     scopeID,
		NotebookID:  nbID,
		DriveDocID:  docID,
		DisplayName: displayName,
	})
	if err != nil {
		// Pointer insert failed entirely — clean up our notebook to avoid leak.
		p.cleanupNotebook(ctx, nbID)
		return "", "", fmt.Errorf("create pointer: %w", err)
	}

	// Lost the race: a concurrent caller's pointer won. Drop our orphaned
	// notebook and return the winner's pair.
	if winner.NotebookID != nbID {
		slog.Info("nlm_provision.lost_race",
			"tenant", tenant, "scope_kind", scopeKind, "scope_id", scopeID,
			"our_notebook", nbID, "winner_notebook", winner.NotebookID)
		p.cleanupNotebook(ctx, nbID)
	}

	return winner.NotebookID, winner.DriveDocID, nil
}

// cleanupNotebook best-effort deletes a notebook we created but no longer own
// (lost race or failed pointer insert). Failures are logged, not returned —
// the caller already has the authoritative result.
func (p *NotebookProvisioner) cleanupNotebook(ctx context.Context, notebookID string) {
	if notebookID == "" {
		return
	}
	if err := p.notebook.DeleteNotebook(ctx, notebookID); err != nil {
		slog.Warn("nlm_provision.cleanup_failed", "notebook_id", notebookID, "error", err)
	}
}
