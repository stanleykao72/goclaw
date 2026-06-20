package lineworks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Tier 2 admin commands for the LINE WORKS channel. These mirror the Telegram
// admin commands (commands_writers.go / commands_tasks.go /
// commands_subagents.go) but route every reply through c.replyCommand (which
// uses c.SendText) instead of telego, and reuse the same store queries.
//
// Target selection differs from Telegram by necessity: a LINE WORKS callback
// event carries no reply-to message and no mention list, so /addwriter and
// /removewriter take the target user id as an explicit text argument:
//
//	/addwriter <userId>
//	/removewriter <userId>
//
// All handlers are nil-safe: when the backing store is not wired the command
// replies with an "unavailable" message rather than panicking. None of them
// reach the agent path — handleBotCommand returns true after dispatching.

const (
	lineWorksMaxTasksInList     = 30
	lineWorksMaxSubagentsInList = 30
)

// resolveAgentUUID converts the channel's configured agent key (a raw UUID or
// an agent_key string) into the canonical UUID the stores expect. Required by
// the writer and task commands. When no agent context is available the caller
// disables the command with a clear message rather than crashing.
func (c *Channel) resolveAgentUUID(ctx context.Context) (uuid.UUID, error) {
	key := c.AgentID()
	if key == "" {
		return uuid.Nil, fmt.Errorf("no agent key configured")
	}
	// Try direct UUID parse first (future-proofing).
	if id, err := uuid.Parse(key); err == nil {
		return id, nil
	}
	if c.agentStore == nil {
		return uuid.Nil, fmt.Errorf("agent store unavailable")
	}
	ctx = store.WithTenantID(ctx, c.TenantID())
	agent, err := c.agentStore.GetByKey(ctx, key)
	if err != nil {
		return uuid.Nil, fmt.Errorf("agent %q not found: %w", key, err)
	}
	return agent.ID, nil
}

// --- /addwriter, /removewriter, /writers ---

// handleWriterCommand implements /addwriter and /removewriter. action is "add"
// or "remove". The target user id is taken from the command argument.
func (c *Channel) handleWriterCommand(ctx context.Context, ev callbackEvent, text, action, lang string) {
	_, peerKind := peerOf(ev.Source)
	if peerKind != peerGroup {
		c.replyCommand(ctx, ev, localize(lang, keyWriterGroupOnly))
		return
	}
	if c.configPermStore == nil {
		c.replyCommand(ctx, ev, localize(lang, keyWriterUnavailable))
		return
	}

	agentID, err := c.resolveAgentUUID(ctx)
	if err != nil {
		slog.Debug("lineworks.writer_cmd.agent_resolve_failed", "error", err)
		c.replyCommand(ctx, ev, localize(lang, keyWriterUnavailableAgent))
		return
	}

	groupID := fmt.Sprintf("group:%s:%s", c.Name(), ev.Source.ChannelID)
	// Canonical writer id. It MUST equal the id the file-write ACL checks
	// (store.CheckFilePermission keys on SenderIDFromContext, which for LINE
	// WORKS is senderPrefix+<user id>). The allowlist grant, this auth gate, and
	// the /reset gate must all use this same format — otherwise a grant never
	// matches the write-time check and writes stay denied even after a
	// "successful" /addwriter. (Before: the grant stored the raw command
	// argument and the gate compared the bare user id — three mismatched
	// formats, so file writes were never actually authorized.)
	selfID := senderPrefix + ev.Source.UserID

	existingWriters, _ := c.configPermStore.ListFileWriters(ctx, agentID, groupID)

	// Authorization gate: only existing writers manage the allowlist. An empty
	// list lets the first /addwriter caller bootstrap it (self-add);
	// /removewriter on an empty list is rejected.
	if len(existingWriters) > 0 {
		isWriter := false
		for _, w := range existingWriters {
			if w.UserID == selfID {
				isWriter = true
				break
			}
		}
		if !isWriter {
			c.replyCommand(ctx, ev, localize(lang, keyWriterOnlyWriters))
			return
		}
	} else if action == "remove" {
		c.replyCommand(ctx, ev, localize(lang, keyWriterNoneYet))
		return
	}

	// Resolve the target in the canonical (senderPrefix-qualified) format.
	// Bare "/addwriter" self-adds the sender — the common "give me write access"
	// flow and the only form guaranteed to match the file-write ACL. An explicit
	// argument is treated as a LINE WORKS user id and prefixed if it is not
	// already. /removewriter still requires an explicit target.
	arg := writerCommandArg(text)
	var targetID, displayName string
	switch {
	case arg == "" && action == "add":
		targetID = selfID
		displayName = ev.Source.UserID
	case arg == "":
		c.replyCommand(ctx, ev, localize(lang, keyWriterUsage, "remove"))
		return
	default:
		displayName = arg
		if strings.HasPrefix(arg, senderPrefix) {
			targetID = arg
		} else {
			targetID = senderPrefix + arg
		}
	}

	switch action {
	case "add":
		meta, _ := json.Marshal(map[string]string{"displayName": displayName})
		if err := c.configPermStore.Grant(ctx, &store.ConfigPermission{
			AgentID:    agentID,
			Scope:      groupID,
			ConfigType: store.ConfigTypeFileWriter,
			UserID:     targetID,
			Permission: "allow",
			Metadata:   meta,
		}); err != nil {
			slog.Warn("lineworks.writer_cmd.add_failed", "error", err, "target", targetID)
			c.replyCommand(ctx, ev, localize(lang, keyWriterAddFailed))
			return
		}
		c.replyCommand(ctx, ev, localize(lang, keyWriterAdded, displayName))

	case "remove":
		if len(existingWriters) <= 1 {
			c.replyCommand(ctx, ev, localize(lang, keyWriterRemoveLast))
			return
		}
		if err := c.configPermStore.Revoke(ctx, agentID, groupID, store.ConfigTypeFileWriter, targetID); err != nil {
			slog.Warn("lineworks.writer_cmd.remove_failed", "error", err, "target", targetID)
			c.replyCommand(ctx, ev, localize(lang, keyWriterRemoveFailed))
			return
		}
		c.replyCommand(ctx, ev, localize(lang, keyWriterRemoved, displayName))
	}
}

// handleListWriters implements /writers.
func (c *Channel) handleListWriters(ctx context.Context, ev callbackEvent, lang string) {
	_, peerKind := peerOf(ev.Source)
	if peerKind != peerGroup {
		c.replyCommand(ctx, ev, localize(lang, keyWriterGroupOnly))
		return
	}
	if c.configPermStore == nil {
		c.replyCommand(ctx, ev, localize(lang, keyWriterUnavailable))
		return
	}

	agentID, err := c.resolveAgentUUID(ctx)
	if err != nil {
		slog.Debug("lineworks.writer_cmd.agent_resolve_failed", "error", err)
		c.replyCommand(ctx, ev, localize(lang, keyWriterUnavailableAgent))
		return
	}

	groupID := fmt.Sprintf("group:%s:%s", c.Name(), ev.Source.ChannelID)
	writers, err := c.configPermStore.List(ctx, agentID, store.ConfigTypeFileWriter, groupID)
	if err != nil {
		slog.Warn("lineworks.writer_cmd.list_failed", "error", err)
		c.replyCommand(ctx, ev, localize(lang, keyWriterListFailed))
		return
	}
	if len(writers) == 0 {
		c.replyCommand(ctx, ev, localize(lang, keyWriterListEmpty))
		return
	}

	var sb strings.Builder
	sb.WriteString(localize(lang, keyWriterListHeader, len(writers)))
	for i, w := range writers {
		sb.WriteString(localize(lang, keyWriterListRow, i+1, channels.WriterLabel(w.Metadata, w.UserID), w.UserID))
	}
	c.replyCommand(ctx, ev, sb.String())
}

// writerCommandArg extracts the first argument after the command token, e.g.
// "/addwriter userA" → "userA"; "/addwriter@bot userA" → "userA". Returns ""
// when no argument is present.
func writerCommandArg(text string) string {
	parts := strings.SplitN(strings.TrimSpace(text), " ", 2)
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// --- /tasks, /task_detail ---

// handleTasksList implements /tasks — lists team tasks.
func (c *Channel) handleTasksList(ctx context.Context, ev callbackEvent, lang string) {
	if c.teamStore == nil {
		c.replyCommand(ctx, ev, localize(lang, keyTeamUnavailable))
		return
	}

	agentID, err := c.resolveAgentUUID(ctx)
	if err != nil {
		slog.Debug("lineworks.tasks_cmd.agent_resolve_failed", "error", err)
		c.replyCommand(ctx, ev, localize(lang, keyTeamUnavailableAgent))
		return
	}

	team, err := c.teamStore.GetTeamForAgent(ctx, agentID)
	if err != nil {
		slog.Warn("lineworks.tasks_cmd.get_team_failed", "error", err)
		c.replyCommand(ctx, ev, localize(lang, keyTeamLookupFailed))
		return
	}
	if team == nil {
		c.replyCommand(ctx, ev, localize(lang, keyTeamNotInTeam))
		return
	}

	chatID, peerKind := peerOf(ev.Source)
	tasks, err := c.teamStore.ListTasks(ctx, team.ID, "newest", store.TeamTaskFilterAll, taskUserID(c.Name(), chatID, peerKind), "", "", 0, 0)
	if err != nil {
		slog.Warn("lineworks.tasks_cmd.list_failed", "error", err)
		c.replyCommand(ctx, ev, localize(lang, keyTaskListFailed))
		return
	}
	if len(tasks) == 0 {
		c.replyCommand(ctx, ev, localize(lang, keyTaskNoneForTeam, team.Name))
		return
	}

	total := len(tasks)
	if total > lineWorksMaxTasksInList {
		tasks = tasks[:lineWorksMaxTasksInList]
	}

	var sb strings.Builder
	if total > lineWorksMaxTasksInList {
		sb.WriteString(localize(lang, keyTaskListHeaderTrunc, team.Name, lineWorksMaxTasksInList, total))
	} else {
		sb.WriteString(localize(lang, keyTaskListHeader, team.Name, total))
	}
	for i, t := range tasks {
		owner := ""
		if t.OwnerAgentKey != "" {
			owner = " — @" + t.OwnerAgentKey
		}
		fmt.Fprintf(&sb, "%d. %s %s%s\n   id: %s\n", i+1, taskStatusIcon(t.Status), t.Subject, owner, t.ID.String())
	}
	sb.WriteString(localize(lang, keyTaskListFooter))
	c.replyCommand(ctx, ev, sb.String())
}

// handleTaskDetail implements /task_detail <id> — shows detail for a task.
func (c *Channel) handleTaskDetail(ctx context.Context, ev callbackEvent, text, lang string) {
	idArg := writerCommandArg(text)
	if idArg == "" {
		c.replyCommand(ctx, ev, localize(lang, keyTaskDetailUsage))
		return
	}

	if c.teamStore == nil {
		c.replyCommand(ctx, ev, localize(lang, keyTeamUnavailable))
		return
	}

	agentID, err := c.resolveAgentUUID(ctx)
	if err != nil {
		slog.Debug("lineworks.task_detail_cmd.agent_resolve_failed", "error", err)
		c.replyCommand(ctx, ev, localize(lang, keyTeamUnavailableAgent))
		return
	}

	team, err := c.teamStore.GetTeamForAgent(ctx, agentID)
	if err != nil {
		slog.Warn("lineworks.task_detail_cmd.get_team_failed", "error", err)
		c.replyCommand(ctx, ev, localize(lang, keyTeamLookupFailed))
		return
	}
	if team == nil {
		c.replyCommand(ctx, ev, localize(lang, keyTeamNotInTeam))
		return
	}

	chatID, peerKind := peerOf(ev.Source)
	tasks, err := c.teamStore.ListTasks(ctx, team.ID, "newest", store.TeamTaskFilterAll, taskUserID(c.Name(), chatID, peerKind), "", "", 0, 0)
	if err != nil {
		slog.Warn("lineworks.task_detail_cmd.list_failed", "error", err)
		c.replyCommand(ctx, ev, localize(lang, keyTaskListFailed))
		return
	}

	// Match by full UUID or prefix.
	idLower := strings.ToLower(idArg)
	for i := range tasks {
		tid := tasks[i].ID.String()
		if tid == idLower || strings.HasPrefix(tid, idLower) {
			c.replyCommand(ctx, ev, formatTaskDetail(&tasks[i]))
			return
		}
	}
	c.replyCommand(ctx, ev, localize(lang, keyTaskNotFound, idArg))
}

// taskUserID composes the scoped user id for task filtering. Groups use
// "group:{channel}:{chatID}"; direct chats use the chat id directly. Mirrors
// the Telegram channel's taskUserID semantics.
func taskUserID(channelName, chatID, peerKind string) string {
	if peerKind == peerGroup {
		return fmt.Sprintf("group:%s:%s", channelName, chatID)
	}
	return chatID
}

// taskStatusIcon returns a short icon for each task status.
func taskStatusIcon(status string) string {
	switch status {
	case "completed":
		return "✅"
	case "in_progress":
		return "🔄"
	case "blocked":
		return "⛔"
	default: // pending
		return "⏳"
	}
}

// formatTaskDetail formats a single team task for display.
func formatTaskDetail(t *store.TeamTaskData) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Task: %s\n", t.Subject)
	fmt.Fprintf(&sb, "ID: %s\n", t.ID.String())
	fmt.Fprintf(&sb, "Status: %s %s\n", taskStatusIcon(t.Status), t.Status)
	if t.OwnerAgentKey != "" {
		fmt.Fprintf(&sb, "Owner: @%s\n", t.OwnerAgentKey)
	}
	fmt.Fprintf(&sb, "Priority: %d\n", t.Priority)
	if !t.CreatedAt.IsZero() {
		fmt.Fprintf(&sb, "Created: %s\n", t.CreatedAt.Format("2006-01-02 15:04"))
	}
	if t.Description != "" {
		fmt.Fprintf(&sb, "\nDescription:\n%s\n", t.Description)
	}
	if t.Result != nil && *t.Result != "" {
		fmt.Fprintf(&sb, "\nResult:\n%s\n", *t.Result)
	}
	if len(t.BlockedBy) > 0 {
		ids := make([]string, len(t.BlockedBy))
		for j, bid := range t.BlockedBy {
			ids[j] = bid.String()[:8]
		}
		fmt.Fprintf(&sb, "\nBlocked by: %s\n", strings.Join(ids, ", "))
	}
	return sb.String()
}

// --- /subagents, /subagent ---

// handleSubagentsList implements /subagents — lists subagent tasks from DB.
func (c *Channel) handleSubagentsList(ctx context.Context, ev callbackEvent, lang string) {
	if c.subagentTaskStore == nil {
		c.replyCommand(ctx, ev, localize(lang, keySubUnavailable))
		return
	}

	agentKey := c.AgentID()
	if agentKey == "" {
		c.replyCommand(ctx, ev, localize(lang, keySubUnavailableAgent))
		return
	}

	tasks, err := c.subagentTaskStore.ListByParent(ctx, agentKey, "")
	if err != nil {
		slog.Warn("lineworks.subagents_cmd.list_failed", "error", err)
		c.replyCommand(ctx, ev, localize(lang, keySubListFailed))
		return
	}
	if len(tasks) == 0 {
		c.replyCommand(ctx, ev, localize(lang, keySubNone))
		return
	}

	total := len(tasks)
	if total > lineWorksMaxSubagentsInList {
		tasks = tasks[:lineWorksMaxSubagentsInList]
	}

	var sb strings.Builder
	if total > lineWorksMaxSubagentsInList {
		sb.WriteString(localize(lang, keySubListHeaderTrunc, lineWorksMaxSubagentsInList, total))
	} else {
		sb.WriteString(localize(lang, keySubListHeader, total))
	}
	for i, t := range tasks {
		tokens := fmt.Sprintf("%s/%s tokens", formatTokenCount(t.InputTokens), formatTokenCount(t.OutputTokens))
		model := ""
		if t.Model != nil && *t.Model != "" {
			model = *t.Model
		}
		if model != "" {
			fmt.Fprintf(&sb, "%d. %s %s (%s, %s)\n   id: %s\n", i+1, subagentStatusIcon(t.Status), t.Subject, model, tokens, t.ID.String())
		} else {
			fmt.Fprintf(&sb, "%d. %s %s (%s)\n   id: %s\n", i+1, subagentStatusIcon(t.Status), t.Subject, tokens, t.ID.String())
		}
	}
	sb.WriteString(localize(lang, keySubListFooter))
	c.replyCommand(ctx, ev, sb.String())
}

// handleSubagentDetail implements /subagent <id> — shows detail for a subagent
// task.
func (c *Channel) handleSubagentDetail(ctx context.Context, ev callbackEvent, text, lang string) {
	idArg := writerCommandArg(text)
	if idArg == "" {
		c.replyCommand(ctx, ev, localize(lang, keySubDetailUsage))
		return
	}

	if c.subagentTaskStore == nil {
		c.replyCommand(ctx, ev, localize(lang, keySubUnavailable))
		return
	}

	taskID, err := uuid.Parse(idArg)
	if err != nil {
		c.replyCommand(ctx, ev, localize(lang, keySubInvalidID, idArg))
		return
	}

	task, err := c.subagentTaskStore.Get(ctx, taskID)
	if err != nil {
		slog.Warn("lineworks.subagent_cmd.get_failed", "id", idArg, "error", err)
		c.replyCommand(ctx, ev, localize(lang, keySubLoadFailed))
		return
	}
	if task == nil {
		c.replyCommand(ctx, ev, localize(lang, keySubNotFound, idArg))
		return
	}

	c.replyCommand(ctx, ev, formatSubagentDetail(task))
}

// subagentStatusIcon returns an icon for each subagent task status.
func subagentStatusIcon(status string) string {
	switch status {
	case "completed":
		return "✅"
	case "failed":
		return "❌"
	case "cancelled":
		return "⏹"
	default: // running
		return "🔄"
	}
}

// formatTokenCount formats token counts as "1.2k" for readability.
func formatTokenCount(n int64) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

// truncateStr truncates a string to maxLen runes, appending "…" if truncated.
func truncateStr(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "…"
}

// formatSubagentDetail formats a single subagent task for display.
func formatSubagentDetail(t *store.SubagentTaskData) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Subagent: %s\n", t.Subject)
	fmt.Fprintf(&sb, "ID: %s\n", t.ID.String())
	fmt.Fprintf(&sb, "Status: %s %s\n", subagentStatusIcon(t.Status), t.Status)
	if t.Model != nil && *t.Model != "" {
		fmt.Fprintf(&sb, "Model: %s\n", *t.Model)
	}
	fmt.Fprintf(&sb, "Depth: %d\n", t.Depth)
	fmt.Fprintf(&sb, "Iterations: %d\n", t.Iterations)
	fmt.Fprintf(&sb, "Tokens: %s in / %s out\n", formatTokenCount(t.InputTokens), formatTokenCount(t.OutputTokens))
	if !t.CreatedAt.IsZero() {
		fmt.Fprintf(&sb, "Created: %s\n", t.CreatedAt.Format("2006-01-02 15:04"))
	}
	if t.Description != "" {
		fmt.Fprintf(&sb, "\nPrompt:\n%s\n", truncateStr(t.Description, 500))
	}
	if t.Result != nil && *t.Result != "" {
		fmt.Fprintf(&sb, "\nResult:\n%s\n", truncateStr(*t.Result, 1000))
	}
	return sb.String()
}
