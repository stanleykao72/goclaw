package agent

import (
	"context"

	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/eventbus"
	"github.com/nextlevelbuilder/goclaw/internal/memory"
	"github.com/nextlevelbuilder/goclaw/internal/pipeline"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tokencount"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// runViaPipeline delegates a run to the v3 pipeline.
func (l *Loop) runViaPipeline(ctx context.Context, req RunRequest) (*RunResult, error) {
	input := convertRunInput(&req)
	// Bridge runState shares loop detection state between pipeline and agent.
	bridgeRS := &runState{}
	deps := l.buildPipelineDeps(&req, bridgeRS)

	model := l.model
	if req.ModelOverride != "" {
		model = req.ModelOverride
	}
	provider := l.provider
	if req.ProviderOverride != nil {
		provider = req.ProviderOverride
	} else if req.ModelOverride != "" {
		if fallback, ok := provider.(interface{ PrimaryProvider() providers.Provider }); ok {
			provider = fallback.PrimaryProvider()
		}
	}

	p := pipeline.NewDefaultPipeline(deps)
	state := pipeline.NewRunState(input, nil, model, provider)

	pResult, err := p.Run(ctx, state)
	if err != nil {
		return nil, err
	}
	return convertRunResult(pResult), nil
}

// buildPipelineDeps maps Loop fields + methods to PipelineDeps callbacks.
func (l *Loop) buildPipelineDeps(req *RunRequest, bridgeRS *runState) pipeline.PipelineDeps {
	maxIter := l.maxIterations
	if req.MaxIterations > 0 && req.MaxIterations < maxIter {
		maxIter = req.MaxIterations
	}

	cb := l.pipelineCallbacks(req, bridgeRS)
	emitBlockReply := func(content, source string) {
		sanitized := SanitizeAssistantContent(content)
		if sanitized == "" || IsSilentReply(sanitized) {
			return
		}
		payload := map[string]string{"content": sanitized}
		if source != "" {
			payload["source"] = source
		}
		cb.emitRun(AgentEvent{
			Type:    protocol.AgentEventBlockReply,
			AgentID: l.id,
			RunID:   req.RunID,
			Payload: payload,
		})
	}

	return pipeline.PipelineDeps{
		TokenCounter: tokencount.NewTiktokenCounter(),
		EventBus:     l.domainBus,
		Hooks:        l.hookDispatcher,
		Config: pipeline.PipelineConfig{
			MaxIterations:      maxIter,
			MaxToolCalls:       l.maxToolCalls,
			CheckpointInterval: 5,
			ContextWindow:      l.contextWindow,
			MaxTokens:          l.effectiveMaxTokens(),
			ReserveTokens:      l.resolveReserveTokens(),
			Compaction:         l.compactionCfg,
			// V3 memory/retrieval flags removed — always true at runtime.
		},
		// Resolve per-model context window once per run. Falls back to
		// Config.ContextWindow when registry/model is unknown (existing
		// behaviour unchanged for tests and lite edition).
		ResolveContextWindow: func(provider, model string) int {
			if l.modelRegistry == nil || model == "" {
				return 0
			}
			spec := l.modelRegistry.Resolve(provider, model)
			if spec == nil {
				return 0
			}
			return spec.ContextWindow
		},
		EmitEvent: func(event any) {
			if ae, ok := event.(AgentEvent); ok {
				l.emit(ae)
			}
		},

		// Auto-inject: memory L0 injection into system prompt. Vault-backend agents
		// inject their bounded LONGTERM index from disk; db agents use the episodic
		// vector injector.
		AutoInject: l.autoInjectCallback(req),

		// Context injection + session history
		InjectContext:      cb.injectContext,
		LoadSessionHistory: cb.loadSessionHistory,

		// Context callbacks
		ResolveWorkspace: cb.resolveWorkspace,
		LoadContextFiles: cb.loadContextFiles,
		BuildMessages:    cb.buildMessages,
		EnrichMedia:      cb.enrichMedia,
		InjectReminders:  cb.injectReminders,

		// Think callbacks
		BuildFilteredTools: cb.buildFilteredTools,
		CallLLM:            cb.callLLM,
		UniqueToolCallIDs:  uniquifyToolCallIDs,
		EmitBlockReply: func(content string) {
			emitBlockReply(content, "")
		},
		EmitBlockReplyWithSource: emitBlockReply,

		// Prune callbacks
		PruneMessages:   cb.pruneMessages,
		SanitizeHistory: cb.sanitizeHistory,
		CompactMessages: cb.compactMessages,

		// Cache-TTL gate callbacks (Phase 06)
		GetProviderCaps: func() providers.ProviderCapabilities {
			if ca, ok := l.provider.(providers.CapabilitiesAware); ok {
				return ca.Capabilities()
			}
			return providers.ProviderCapabilities{}
		},
		GetPruningConfig: func() *config.ContextPruningConfig {
			return l.contextPruningCfg
		},
		GetCacheTouch:    l.cacheTouchAt,
		MarkCacheTouched: l.markCacheTouched,

		// Memory flush
		RunMemoryFlush: cb.runMemoryFlush,

		// Tool callbacks
		ExecuteToolCall:   cb.executeToolCall,
		ExecuteToolRaw:    cb.executeToolRaw,
		ProcessToolResult: cb.processToolResult,
		AuthorizeToolCall: cb.authorizeToolCall,
		SequentialToolCall: func(tc providers.ToolCall) bool {
			return l.resolveToolCallName(tc.Name) == "wait"
		},
		ParallelEligibleToolCall: l.parallelEligibleToolCall,
		CheckReadOnly:            cb.checkReadOnly,

		// Observe: drain InjectCh
		DrainInjectCh: func() []providers.Message {
			if req.InjectCh == nil {
				return nil
			}
			var msgs []providers.Message
			for {
				select {
				case injected := <-req.InjectCh:
					msgs = append(msgs, providers.Message{
						Role:    "user",
						Content: injected.Content,
					})
				default:
					return msgs
				}
			}
		},

		// Checkpoint + Finalize
		FlushMessages:          cb.flushMessages,
		PersistAssistantImages: persistAssistantImages,
		SkillPostscript:        l.makeSkillPostscript(),
		SanitizeContent:        cb.sanitizeContent,
		StripMessageDirectives: StripMessageDirectives,
		DeduplicateMediaSuffix: deduplicateMediaSuffix,
		IsSilentReply:          IsSilentReply,
		EmitSessionCompleted: func(ctx context.Context, sessionKey string, msgCount, tokensUsed, compactionCount int) {
			if l.domainBus != nil {
				// Include existing session summary (from previous compaction cycles).
				// Current cycle's compaction runs async so its summary isn't ready yet,
				// but previous summaries are available and useful for episodic creation.
				var summary string
				if compactionCount > 0 {
					summary = l.sessions.GetSummary(ctx, sessionKey)
				}
				l.domainBus.Publish(eventbus.DomainEvent{
					Type:     eventbus.EventSessionCompleted,
					TenantID: l.tenantID.String(),
					AgentID:  l.agentUUID.String(),
					UserID:   req.UserID,
					SourceID: sessionKey,
					Payload: &eventbus.SessionCompletedPayload{
						SessionKey:      sessionKey,
						MessageCount:    msgCount,
						TokensUsed:      tokensUsed,
						CompactionCount: compactionCount,
						Summary:         summary,
					},
				})
			}
		},
		UpdateMetadata:   cb.updateMetadata,
		BootstrapCleanup: cb.bootstrapCleanup,
		MaybeSummarize:   cb.maybeSummarize,
	}
}

// convertRunInput converts agent.RunRequest to pipeline.RunInput.
func convertRunInput(req *RunRequest) *pipeline.RunInput {
	return &pipeline.RunInput{
		SessionKey:         req.SessionKey,
		Message:            req.Message,
		Media:              req.Media,
		ForwardMedia:       req.ForwardMedia,
		Channel:            req.Channel,
		ChannelType:        req.ChannelType,
		BitrixPortalDomain: req.BitrixPortalDomain,
		ChatTitle:          req.ChatTitle,
		ChatID:             req.ChatID,
		PeerKind:           req.PeerKind,
		RunID:              req.RunID,
		UserID:             req.UserID,
		SenderID:           req.SenderID,
		SenderName:         req.SenderName,
		Stream:             req.Stream,
		ExtraSystemPrompt:  req.ExtraSystemPrompt,
		SkillFilter:        req.SkillFilter,
		HistoryLimit:       req.HistoryLimit,
		ToolAllow:          req.ToolAllow,
		LightContext:       req.LightContext,
		RunKind:            req.RunKind,
		DelegationID:       req.DelegationID,
		TeamID:             req.TeamID,
		TeamTaskID:         req.TeamTaskID,
		ParentAgentID:      req.ParentAgentID,
		MaxIterations:      req.MaxIterations,
		ModelOverride:      req.ModelOverride,
		HideInput:          req.HideInput,
		ContentSuffix:      req.ContentSuffix,
		LeaderAgentID:      req.LeaderAgentID,
		WorkspaceChannel:   req.WorkspaceChannel,
		WorkspaceChatID:    req.WorkspaceChatID,
		TeamWorkspace:      req.TeamWorkspace,
	}
}

// convertRunResult converts pipeline.RunResult to agent.RunResult.
func convertRunResult(pr *pipeline.RunResult) *RunResult {
	if pr == nil {
		return nil
	}
	media := make([]MediaResult, len(pr.MediaResults))
	for i, m := range pr.MediaResults {
		media[i] = MediaResult{
			Path:        m.Path,
			ContentType: m.ContentType,
			Size:        m.Size,
			AsVoice:     m.AsVoice,
			Prompt:      m.Prompt,
		}
	}
	return &RunResult{
		Content:        pr.Content,
		Thinking:       pr.Thinking,
		RunID:          pr.RunID,
		Iterations:     pr.Iterations,
		Usage:          &pr.TotalUsage,
		Media:          media,
		Deliverables:   pr.Deliverables,
		BlockReplies:   pr.BlockReplies,
		LastBlockReply: pr.LastBlockReply,
		LoopKilled:     pr.LoopKilled,
	}
}

// makeAutoInjectCallback creates the AutoInject callback that captures agent/tenant context.
// Returns nil if autoInjector is not configured (v3 retrieval disabled or no episodic store).
// Phase 9: plumbs recentContext through to enrich vector search queries for
// context-aware recall.
func (l *Loop) makeAutoInjectCallback(req *RunRequest) func(ctx context.Context, userMessage, userID, recentContext string) (string, error) {
	if l.autoInjector == nil {
		return nil
	}
	return func(ctx context.Context, userMessage, userID, recentContext string) (string, error) {
		result, err := l.autoInjector.Inject(ctx, memory.InjectParams{
			AgentID:       l.agentUUID.String(),
			UserID:        store.MemoryUserID(ctx),
			TenantID:      store.TenantIDFromContext(ctx).String(),
			UserMessage:   userMessage,
			RecentContext: recentContext,
		})
		if err != nil || result == nil {
			return "", err
		}
		return result.Section, nil
	}
}

// Auto-inject budget for the vault index (LONGTERM.md). Bounded so the per-turn
// injection cost stays controlled regardless of how large the file grows.
const (
	vaultIndexMaxLines = 200
	vaultIndexMaxBytes = 8192
)

// autoInjectCallback selects the per-turn memory auto-injector by backend: the
// file-based vault index injector for vault agents (when a vault dir is set), else
// the episodic vector injector (db agents — behavior unchanged; also the fail-safe
// path when a vault agent has no vault dir configured).
func (l *Loop) autoInjectCallback(req *RunRequest) func(ctx context.Context, userMessage, userID, recentContext string) (string, error) {
	if l.memoryBackend == "vault" && l.memoryVaultDir != "" {
		return l.makeVaultAutoInjectCallback(req)
	}
	return l.makeAutoInjectCallback(req)
}

// makeVaultAutoInjectCallback injects the scope's bounded LONGTERM index from the
// vault each turn, wrapped as untrusted content.
//
// The scope is computed from the REQUEST (channel/peerKind/chatID/userID), not from
// ctx: this callback runs in the pipeline context, which — unlike the tool-dispatch
// context — does not carry the tool peerKind/chatID keys. Deriving the scope from
// ctx here would land in the user branch for group chats and inject the wrong
// scope's index. MemoryVaultSubdirFor reproduces exactly the scope a write produces.
func (l *Loop) makeVaultAutoInjectCallback(req *RunRequest) func(ctx context.Context, userMessage, userID, recentContext string) (string, error) {
	scope := tools.MemoryVaultSubdirFor(req.ChannelType, req.PeerKind, req.ChatID, req.UserID)
	return func(ctx context.Context, userMessage, userID, recentContext string) (string, error) {
		index, _, err := tools.ReadMemoryVaultIndexForScope(l.memoryVaultDir, scope, vaultIndexMaxLines, vaultIndexMaxBytes)
		if err != nil || index == "" {
			return "", err
		}
		return "## Memory Context\n\nRelevant long-term memory (your curated LONGTERM index):\n" +
			tools.WrapUntrustedMemory(index) + "\n", nil
	}
}
