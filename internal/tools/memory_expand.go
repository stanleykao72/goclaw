package tools

import (
	"context"
	"fmt"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// MemoryExpandTool provides L2 deep retrieval of episodic memory entries.
// Returns full summary for a given episodic ID.
type MemoryExpandTool struct {
	episodicStore store.EpisodicStore
}

// NewMemoryExpandTool creates a memory_expand tool.
func NewMemoryExpandTool() *MemoryExpandTool {
	return &MemoryExpandTool{}
}

// SetEpisodicStore configures the episodic store for full content retrieval.
func (t *MemoryExpandTool) SetEpisodicStore(es store.EpisodicStore) {
	t.episodicStore = es
}

func (t *MemoryExpandTool) Name() string { return "memory_expand" }
func (t *MemoryExpandTool) Description() string {
	return "Load full content for a memory entry by ID. Returns the complete episodic summary for deep context."
}

func (t *MemoryExpandTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "Episodic memory ID to expand (from memory_search results)",
			},
		},
		"required": []string{"id"},
	}
}

// Execute retrieves full episodic summary by ID.
func (t *MemoryExpandTool) Execute(ctx context.Context, args map[string]any) *Result {
	id, _ := args["id"].(string)

	// Memory mode gate: a "notebook"-only agent does not use the vault/episodic
	// store. No-op with a graceful "not found" result (never an error) so the turn
	// is unbroken — placed BEFORE the episodicStore-nil check so a notebook-mode
	// agent stays graceful even on a deployment where episodic is not wired.
	if store.MemoryModeFromCtx(ctx) == store.MemoryModeNotebook {
		return &Result{ForLLM: "memory entry not found: " + id}
	}

	if t.episodicStore == nil {
		return ErrorResult("memory_expand requires v3 episodic memory (not available)")
	}
	if id == "" {
		return ErrorResult("id parameter is required")
	}

	ep, err := t.episodicStore.Get(ctx, id)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to retrieve memory: %v", err))
	}
	if ep == nil {
		return ErrorResult("memory entry not found: " + id)
	}

	// Format full summary with metadata
	result := fmt.Sprintf("## Memory: %s\n\n**Session:** %s\n**Created:** %s\n**Turns:** %d\n\n%s",
		ep.L0Abstract, ep.SessionKey, ep.CreatedAt.Format("2006-01-02 15:04"),
		ep.TurnCount, ep.Summary)

	return &Result{ForLLM: result}
}
