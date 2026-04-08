package line

// conversation.go implements the LINE Flex Message conversation flow that
// follows km-meeting-pipeline.sh `cmd_check`. The bash side writes a draft
// JSON under /data/km/meetings/drafts/ in state `awaiting_project`; this
// file picks up that draft and walks the user through 4 picker bubbles to
// fill the remaining required fields, finally calling `publish-odoo finalize`
// which creates the `job.meeting.minutes` record on stage35.
//
// Architecture choice: file-first state, polling watcher.
//
//	bash               file system                 goclaw
//	-----              -----------                 ------
//	cmd_check  ---->   drafts/<ref>.json     <---  draft watcher (polling, 10s)
//	                   .pushed marker        --->  pushFlex project_picker
//	                                               -- user taps button --
//	                   <- LINE PostbackEvent ---   handlePostback()
//	                                               exec publish-odoo update
//	                                               replyFlex next picker
//	                                               ...
//	                                               exec publish-odoo finalize
//	                                               replyText id confirmation
//
// Polling > push trigger because it requires no bash changes and survives
// goclaw restarts (drafts on disk get re-discovered). The 10s interval is
// the upper bound on user-perceived latency from "audio uploaded" to "first
// picker bubble"; cmd_check itself takes minutes so this is fine.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/line/line-bot-sdk-go/v7/linebot"
)

const (
	defaultDraftsDir       = "/data/km/meetings/drafts"
	defaultPublishedDir    = "/data/km/meetings/drafts/published"
	draftPushedSuffix      = ".pushed"
	draftPollIntervalDef   = 10 * time.Second
	maxProjectsPerPicker   = 10
	maxAttendeesPerPicker  = 10
	mcpRequestTimeout      = 15 * time.Second
	pipelineExecTimeout    = 30 * time.Second
)

// finalizeIDRegex extracts the new id from `finalize OK ... id=<N>` lines
// emitted by km-meeting-pipeline.sh publish-odoo finalize.
var finalizeIDRegex = regexp.MustCompile(`id=(\d+)`)

// updateOKRegex confirms an `update` call succeeded and reports the next
// state. Format: `update OK <ref>.<field>=<value> (state=<next>)`.
var updateOKRegex = regexp.MustCompile(`update OK\s+\S+\s+\(state=(\w+)\)`)

// draftJSON is the subset of the file-first draft state that goclaw needs.
// The full schema lives in km-meeting-pipeline.sh; do not write fields here
// that the bash side does not understand.
type draftJSON struct {
	SourceRef          string  `json:"source_ref"`
	State              string  `json:"state"`
	LineChatID         *string `json:"line_chat_id"`
	LineUserID         *string `json:"line_user_id"`
	ResolvedUserID     *int    `json:"resolved_user_id"`
	Subject            string  `json:"subject"`
	MeetingDate        string  `json:"meeting_date"`
	Answers            struct {
		ProjectID        *int    `json:"project_id"`
		MeetingLocation  *string `json:"meeting_location"`
		PartnerIDs       []int   `json:"partner_ids"`
		JobTypeID        *int    `json:"job_type_id"`
		JobWorkingPlanID *int    `json:"job_working_plan_id"`
	} `json:"answers"`
}

// conversationState is the per-channel runtime state for the meeting writeback
// flow. It is independent from the file-first draft (which is the source of
// truth for cross-process state) — this struct only tracks transient
// goclaw-side concerns: the in-progress attendee multi-select set and a
// projectName cache so the confirm bubble can show a friendly label.
type conversationState struct {
	mu sync.Mutex

	// attendeesSelection tracks which partner ids the user has toggled on
	// for each draft ref. The bash side stores partner_ids only after the
	// user taps the "submit_attendees" button; up to that moment toggles
	// only update this map and re-render the picker.
	attendeesSelection map[string]map[int]bool

	// projectNameCache lets the confirm bubble show a project name even
	// though the bash draft only stores the integer id. Populated when
	// the user picks a project from the project_picker bubble.
	projectNameCache map[string]string // ref → project name

	// attendeesPoolCache stores the candidate partners shown in the
	// attendees_picker so re-renders after toggle don't re-fetch from MCP.
	attendeesPoolCache map[string][]partner // ref → partner pool
}

func newConversationState() *conversationState {
	return &conversationState{
		attendeesSelection: make(map[string]map[int]bool),
		projectNameCache:   make(map[string]string),
		attendeesPoolCache: make(map[string][]partner),
	}
}

func (s *conversationState) toggleAttendee(ref string, partnerID int) (selected map[int]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.attendeesSelection[ref]
	if !ok {
		cur = map[int]bool{}
		s.attendeesSelection[ref] = cur
	}
	if cur[partnerID] {
		delete(cur, partnerID)
	} else {
		cur[partnerID] = true
	}
	return copyBoolMap(cur)
}

func (s *conversationState) getAttendees(ref string) map[int]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyBoolMap(s.attendeesSelection[ref])
}

func (s *conversationState) clearRef(ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.attendeesSelection, ref)
	delete(s.projectNameCache, ref)
	delete(s.attendeesPoolCache, ref)
}

func (s *conversationState) cacheProjectName(ref, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectNameCache[ref] = name
}

func (s *conversationState) getProjectName(ref string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projectNameCache[ref]
}

func (s *conversationState) cacheAttendeesPool(ref string, pool []partner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attendeesPoolCache[ref] = pool
}

func (s *conversationState) getAttendeesPool(ref string) []partner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attendeesPoolCache[ref]
}

func copyBoolMap(in map[int]bool) map[int]bool {
	out := make(map[int]bool, len(in))
	for k, v := range in {
		if v {
			out[k] = true
		}
	}
	return out
}

// --- environment ------------------------------------------------------------

func getDraftsDir() string {
	if v := os.Getenv("KM_MEETING_DRAFTS_DIR"); v != "" {
		return v
	}
	return defaultDraftsDir
}

func getPublishedDir() string {
	if v := os.Getenv("KM_MEETING_PUBLISHED_DIR"); v != "" {
		return v
	}
	return defaultPublishedDir
}

func getDraftPollInterval() time.Duration {
	if v := os.Getenv("KM_MEETING_DRAFT_POLL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return draftPollIntervalDef
}

// --- draft I/O --------------------------------------------------------------

func readDraft(path string) (*draftJSON, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read draft: %w", err)
	}
	var d draftJSON
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parse draft: %w", err)
	}
	return &d, nil
}

func draftPath(ref string) string {
	return filepath.Join(getDraftsDir(), ref+".json")
}

func draftPublishedPath(ref string) string {
	return filepath.Join(getPublishedDir(), ref+".json")
}

func draftPushedMarker(ref string) string {
	return filepath.Join(getDraftsDir(), ref+draftPushedSuffix)
}

// --- draft watcher ---------------------------------------------------------

// startDraftWatcher polls drafts/ for new awaiting_project drafts that need
// the project_picker bubble pushed. Idempotent via .pushed marker file —
// even if goclaw restarts, an already-pushed draft is not pushed again.
//
// The watcher exits when ctx is cancelled (Channel.Stop).
func (c *Channel) startDraftWatcher(ctx context.Context) {
	interval := getDraftPollInterval()
	slog.Info("LINE meeting: draft watcher started", "dir", getDraftsDir(), "interval", interval)

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("LINE meeting: draft watcher stopped")
			return
		case <-t.C:
			c.scanDraftsOnce()
		}
	}
}

func (c *Channel) scanDraftsOnce() {
	dir := getDraftsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("LINE meeting: scan drafts failed", "err", err, "dir", dir)
		}
		return
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		ref := strings.TrimSuffix(name, ".json")

		if _, err := os.Stat(draftPushedMarker(ref)); err == nil {
			continue // already pushed
		}

		path := filepath.Join(dir, name)
		d, err := readDraft(path)
		if err != nil {
			slog.Warn("LINE meeting: skip unreadable draft", "ref", ref, "err", err)
			continue
		}
		if d.State != "awaiting_project" {
			// Not in initial state — either already in progress or
			// finalized. Mark pushed so we stop scanning it.
			_ = touchPushedMarker(ref)
			continue
		}
		if d.LineChatID == nil || *d.LineChatID == "" {
			slog.Debug("LINE meeting: draft has no chat id, skipping", "ref", ref)
			_ = touchPushedMarker(ref)
			continue
		}

		if err := c.pushProjectPickerForDraft(d); err != nil {
			slog.Error("LINE meeting: push project_picker failed",
				"ref", ref, "err", err)
			continue
		}
		if err := touchPushedMarker(ref); err != nil {
			slog.Warn("LINE meeting: failed to write .pushed marker",
				"ref", ref, "err", err)
		}
	}
}

func touchPushedMarker(ref string) error {
	f, err := os.Create(draftPushedMarker(ref))
	if err != nil {
		return err
	}
	return f.Close()
}

func (c *Channel) pushProjectPickerForDraft(d *draftJSON) error {
	chatID := *d.LineChatID

	if d.ResolvedUserID == nil {
		// LINE user not bound to a res.users — instruct user to /bind.
		msg := "👋 已收到語音檔，但找不到對應的 Odoo 使用者。\n\n請先傳送 `/bind <你的 e-smith email>` 完成綁定，之後再傳一次語音檔。"
		return c.sendChunks(chatID, []string{msg})
	}

	projects, err := fetchUserProjects(context.Background(), *d.ResolvedUserID)
	if err != nil {
		return fmt.Errorf("fetch projects: %w", err)
	}
	if len(projects) == 0 {
		msg := "已收到會議錄音，但找不到你最近活躍的專案。請至 Odoo 確認你的負責專案後再試。"
		return c.sendChunks(chatID, []string{msg})
	}

	bubble, err := buildProjectPicker(d.SourceRef, d.Subject, projects)
	if err != nil {
		return fmt.Errorf("build project picker: %w", err)
	}
	return c.pushFlex(chatID, "請選擇會議專案", bubble)
}

// --- postback handler ------------------------------------------------------

// handlePostback parses the postback `data` query string and routes to the
// matching state-machine action. Called from handleEvent on EventTypePostback.
func (c *Channel) handlePostback(event *linebot.Event) {
	chatID := chatIDFromSource(event.Source)
	if chatID == "" {
		return
	}

	// Cache the reply token for any subsequent send within 30s.
	c.replyTokens.Store(chatID, replyTokenEntry{
		token:      event.ReplyToken,
		receivedAt: time.Now(),
	})

	if event.Postback == nil {
		return
	}
	values, err := url.ParseQuery(event.Postback.Data)
	if err != nil {
		slog.Warn("LINE meeting: bad postback data", "data", event.Postback.Data, "err", err)
		return
	}
	action := values.Get("action")
	ref := values.Get("ref")
	if action == "" || ref == "" {
		return
	}

	switch action {
	case "update":
		c.handleUpdatePostback(chatID, ref, values.Get("field"), values.Get("value"))
	case "toggle":
		c.handleTogglePostback(chatID, ref, values.Get("value"))
	case "submit_attendees":
		c.handleSubmitAttendees(chatID, ref)
	case "finalize":
		c.handleFinalize(chatID, ref)
	case "cancel":
		c.handleCancel(chatID, ref)
	default:
		slog.Warn("LINE meeting: unknown postback action", "action", action)
	}
}

// chatIDFromSource extracts a stable chat id from any LINE event source.
// Mirrors the logic in handleEvent so postback events use the same key as
// the original message events that opened the conversation.
func chatIDFromSource(src *linebot.EventSource) string {
	if src == nil {
		return ""
	}
	switch src.Type {
	case linebot.EventSourceTypeUser:
		return src.UserID
	case linebot.EventSourceTypeGroup:
		return src.GroupID
	case linebot.EventSourceTypeRoom:
		return src.RoomID
	}
	return ""
}

func (c *Channel) handleUpdatePostback(chatID, ref, field, value string) {
	if field == "" || value == "" {
		return
	}
	nextState, err := runPublishUpdate(ref, field, value)
	if err != nil {
		slog.Error("LINE meeting: publish-odoo update failed",
			"ref", ref, "field", field, "err", err)
		_ = c.sendChunks(chatID, []string{"⚠️ 更新會議草稿失敗，請稍後再試"})
		return
	}

	// Cache the project name on first project pick so the confirm bubble
	// can show a label instead of just an id.
	if field == "project_id" {
		if name := lookupProjectName(value); name != "" {
			c.conv.cacheProjectName(ref, name)
		}
	}

	switch nextState {
	case "awaiting_location":
		bubble, _ := buildLocationPicker(ref)
		_ = c.replyFlex(chatID, "選擇會議地點", bubble)
	case "awaiting_attendees":
		c.sendAttendeesPicker(chatID, ref)
	case "awaiting_confirm":
		c.sendConfirmBubble(chatID, ref)
	default:
		slog.Warn("LINE meeting: unknown next state", "ref", ref, "state", nextState)
	}
}

func (c *Channel) handleTogglePostback(chatID, ref, value string) {
	pid, err := strconv.Atoi(value)
	if err != nil {
		return
	}
	c.conv.toggleAttendee(ref, pid)
	c.sendAttendeesPicker(chatID, ref)
}

func (c *Channel) handleSubmitAttendees(chatID, ref string) {
	selected := c.conv.getAttendees(ref)
	if len(selected) == 0 {
		_ = c.sendChunks(chatID, []string{"請至少選擇 1 位出席者"})
		return
	}
	csv := joinIntsCSV(sortedKeys(selected))
	c.handleUpdatePostback(chatID, ref, "partner_ids", csv)
}

func (c *Channel) handleFinalize(chatID, ref string) {
	id, err := runPublishFinalize(ref)
	if err != nil {
		slog.Error("LINE meeting: publish-odoo finalize failed",
			"ref", ref, "err", err)
		_ = c.sendChunks(chatID, []string{
			"⚠️ 建立會議記錄時發生錯誤，請稍後再試或聯絡系統管理員",
		})
		return
	}
	msg := "✅ 會議記錄已建立"
	if id > 0 {
		msg = fmt.Sprintf("%s（id=%d）", msg, id)
		if link := buildOdooDeepLink("job.meeting.minutes", id); link != "" {
			msg = msg + "\n\n📎 點此查看：\n" + link
		}
	}
	c.conv.clearRef(ref)
	_ = c.sendChunks(chatID, []string{msg})
}

// buildOdooDeepLink returns a clickable web URL to a job.meeting.minutes
// (or any model) record on stage35. Empty string when the base URL cannot
// be determined — caller should fall back to a no-link message.
//
// Resolution order:
//  1. ODOO_STAGE35_BASE_URL env (explicit)
//  2. Strip "/mcp/v1" suffix from ODOO_STAGE35_MCP_URL
func buildOdooDeepLink(model string, id int) string {
	base := os.Getenv("ODOO_STAGE35_BASE_URL")
	if base == "" {
		mcp := os.Getenv("ODOO_STAGE35_MCP_URL")
		if mcp == "" {
			return ""
		}
		// Strip path suffix — accept both /mcp/v1 and /mcp/v1/.
		base = strings.TrimSuffix(strings.TrimSuffix(mcp, "/"), "/mcp/v1")
	}
	return fmt.Sprintf("%s/web#id=%d&model=%s&view_type=form",
		strings.TrimRight(base, "/"), id, model)
}

func (c *Channel) handleCancel(chatID, ref string) {
	c.conv.clearRef(ref)
	_ = c.sendChunks(chatID, []string{"已取消。下次錄音時可重新填寫。"})
}

// sendAttendeesPicker fetches partners (cached after first call), renders
// the bubble with current selection, and replies / pushes.
func (c *Channel) sendAttendeesPicker(chatID, ref string) {
	pool := c.conv.getAttendeesPool(ref)
	if pool == nil {
		// First time — fetch partner candidates from MCP. We use the
		// project's team members (project.user_ids → res.users → partner_id)
		// as the pool. Falls back to top-N res.partner if empty.
		fetched, err := fetchProjectAttendees(context.Background(), ref)
		if err != nil {
			slog.Warn("LINE meeting: fetch attendees failed", "ref", ref, "err", err)
		}
		pool = fetched
		c.conv.cacheAttendeesPool(ref, pool)
	}
	if len(pool) == 0 {
		_ = c.sendChunks(chatID, []string{"找不到候選出席者，請至 Odoo 手動建立會議記錄。"})
		return
	}

	selected := c.conv.getAttendees(ref)
	bubble, err := buildAttendeesPicker(ref, pool, selected)
	if err != nil {
		slog.Error("LINE meeting: build attendees picker", "err", err)
		return
	}
	_ = c.replyFlex(chatID, "選擇出席者", bubble)
}

func (c *Channel) sendConfirmBubble(chatID, ref string) {
	d, err := readDraft(draftPath(ref))
	if err != nil {
		slog.Error("LINE meeting: re-read draft for confirm", "ref", ref, "err", err)
		return
	}
	projectName := c.conv.getProjectName(ref)
	if projectName == "" && d.Answers.ProjectID != nil {
		projectName = "#" + strconv.Itoa(*d.Answers.ProjectID)
	}
	location := ""
	if d.Answers.MeetingLocation != nil {
		location = *d.Answers.MeetingLocation
	}
	bubble, err := buildConfirmBubble(ref, d.Subject, projectName, location, len(d.Answers.PartnerIDs))
	if err != nil {
		slog.Error("LINE meeting: build confirm bubble", "ref", ref, "err", err)
		return
	}
	_ = c.replyFlex(chatID, "確認會議記錄", bubble)
}

// --- exec wrappers ----------------------------------------------------------

// runPublishUpdate calls `km-meeting-pipeline.sh publish-odoo update <ref> <field> <value>`
// and parses the next state from stdout.
func runPublishUpdate(ref, field, value string) (string, error) {
	out, err := runPipeline("publish-odoo", "update", ref, field, value)
	if err != nil {
		return "", err
	}
	if m := updateOKRegex.FindStringSubmatch(out); len(m) >= 2 {
		return m[1], nil
	}
	// Field is one of the optional ones (job_type_id / job_working_plan_id) —
	// the bash side does not advance state. Re-read draft for current state.
	d, derr := readDraft(draftPath(ref))
	if derr != nil {
		return "", fmt.Errorf("update succeeded but state read failed: %w", derr)
	}
	return d.State, nil
}

// runPublishFinalize calls `km-meeting-pipeline.sh publish-odoo finalize <ref>`
// and parses the new record id from stdout.
func runPublishFinalize(ref string) (int, error) {
	out, err := runPipeline("publish-odoo", "finalize", ref)
	if err != nil {
		return 0, err
	}
	if m := finalizeIDRegex.FindStringSubmatch(out); len(m) >= 2 {
		id, _ := strconv.Atoi(m[1])
		return id, nil
	}
	// Idempotent success path — no id but exit 0 still means OK.
	return 0, nil
}

// runPipeline is a thin wrapper around exec.Command for the bash CLI. It
// merges stdout/stderr (the script logs to both) so callers can grep for
// the marker patterns regardless of where the line was emitted.
//
// Override the script path with KM_MEETING_PIPELINE_SCRIPT for tests; the
// constant lives in constants.go.
var runPipeline = func(args ...string) (string, error) {
	scriptPath := getMeetingPipelineScript()
	ctx, cancel := context.WithTimeout(context.Background(), pipelineExecTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", append([]string{scriptPath}, args...)...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), fmt.Errorf("pipeline %v: %w (output: %s)", args, err, buf.String())
	}
	return buf.String(), nil
}

// --- stage35 MCP HTTP client ------------------------------------------------

type mcpRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int            `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *mcpError) Error() string {
	return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message)
}

// mcpToolCall posts a JSON-RPC tools/call request to the stage35 MCP server.
// The MCP server's `tools/call` returns a structuredContent envelope; we
// unmarshal directly into the caller-provided dst pointer.
var mcpToolCall = func(ctx context.Context, tool string, args map[string]any, dst any) error {
	endpoint := os.Getenv("ODOO_STAGE35_MCP_URL")
	token := os.Getenv("ODOO_STAGE35_MCP_TOKEN")
	if endpoint == "" || token == "" {
		return errors.New("ODOO_STAGE35_MCP_URL / ODOO_STAGE35_MCP_TOKEN not set")
	}

	body := mcpRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      tool,
			"arguments": args,
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	reqCtx, cancel := context.WithTimeout(ctx, mcpRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("mcp http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mcp http %d", resp.StatusCode)
	}

	var envelope mcpResponse
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("mcp decode: %w", err)
	}
	if envelope.Error != nil {
		return envelope.Error
	}

	// MCP tools/call result is { content: [...], structuredContent: <X> }.
	// Prefer structuredContent for typed extraction.
	var wrapper struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(envelope.Result, &wrapper); err != nil {
		return fmt.Errorf("mcp result envelope: %w", err)
	}
	if len(wrapper.StructuredContent) > 0 && string(wrapper.StructuredContent) != "null" {
		return json.Unmarshal(wrapper.StructuredContent, dst)
	}
	if len(wrapper.Content) > 0 && wrapper.Content[0].Type == "text" {
		return json.Unmarshal([]byte(wrapper.Content[0].Text), dst)
	}
	return errors.New("mcp result has no usable payload")
}

// fetchUserProjects returns up to 10 most-recently-active project.project
// rows for the given Odoo user_id. Falls back to all projects on miss.
func fetchUserProjects(ctx context.Context, userID int) ([]project, error) {
	args := map[string]any{
		"model":  "project.project",
		"domain": [][]any{{"user_id", "=", userID}},
		"fields": []string{"id", "name"},
		"order":  "write_date desc",
		"limit":  maxProjectsPerPicker,
	}

	var rows []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := mcpToolCall(ctx, "search_records", args, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		// Fallback: any active project the bearer-token user can see.
		args["domain"] = [][]any{{"active", "=", true}}
		if err := mcpToolCall(ctx, "search_records", args, &rows); err != nil {
			return nil, err
		}
	}

	out := make([]project, 0, len(rows))
	for _, r := range rows {
		out = append(out, project{ID: r.ID, Name: r.Name})
	}
	rememberProjectNames(out)
	return out, nil
}

// fetchProjectAttendees returns the partner pool used by the attendees
// picker. Strategy: read draft.answers.project_id, fetch its team members,
// then resolve them to res.partner. Falls back to top contacts if empty.
func fetchProjectAttendees(ctx context.Context, ref string) ([]partner, error) {
	d, err := readDraft(draftPath(ref))
	if err != nil {
		return nil, err
	}
	if d.Answers.ProjectID == nil {
		return nil, errors.New("project_id not yet set on draft")
	}

	// Get the project's user_ids.
	var projectRows []struct {
		ID      int   `json:"id"`
		UserIDs []int `json:"user_ids"`
	}
	if err := mcpToolCall(ctx, "search_records", map[string]any{
		"model":  "project.project",
		"domain": [][]any{{"id", "=", *d.Answers.ProjectID}},
		"fields": []string{"id", "user_ids"},
		"limit":  1,
	}, &projectRows); err != nil {
		return nil, err
	}
	if len(projectRows) == 0 || len(projectRows[0].UserIDs) == 0 {
		return fetchPartnersFallback(ctx)
	}

	// Resolve user_ids → partner_id.
	var userRows []struct {
		ID        int   `json:"id"`
		PartnerID []any `json:"partner_id"` // [id, name] in Odoo many2one read format
	}
	if err := mcpToolCall(ctx, "search_records", map[string]any{
		"model":  "res.users",
		"domain": [][]any{{"id", "in", projectRows[0].UserIDs}},
		"fields": []string{"id", "partner_id"},
		"limit":  maxAttendeesPerPicker,
	}, &userRows); err != nil {
		return nil, err
	}

	out := make([]partner, 0, len(userRows))
	for _, u := range userRows {
		if len(u.PartnerID) >= 2 {
			id, _ := u.PartnerID[0].(float64)
			name, _ := u.PartnerID[1].(string)
			if int(id) > 0 {
				out = append(out, partner{ID: int(id), Name: name})
			}
		}
	}
	if len(out) == 0 {
		return fetchPartnersFallback(ctx)
	}
	return out, nil
}

// fetchPartnersFallback returns partners that correspond to internal company
// users (res.users with share=false). This is the canonical "company employee
// picker list" — we deliberately do NOT show generic res.partner customer
// contacts here. Per design D4, attendees are e-smith employees.
func fetchPartnersFallback(ctx context.Context) ([]partner, error) {
	var rows []struct {
		ID        int   `json:"id"`
		PartnerID []any `json:"partner_id"` // [id, name] many2one read format
	}
	if err := mcpToolCall(ctx, "search_records", map[string]any{
		"model":  "res.users",
		"domain": [][]any{{"active", "=", true}, {"share", "=", false}},
		"fields": []string{"id", "partner_id"},
		"order":  "name asc",
		"limit":  maxAttendeesPerPicker,
	}, &rows); err != nil {
		return nil, err
	}
	out := make([]partner, 0, len(rows))
	for _, r := range rows {
		if len(r.PartnerID) >= 2 {
			id, _ := r.PartnerID[0].(float64)
			name, _ := r.PartnerID[1].(string)
			if int(id) > 0 {
				out = append(out, partner{ID: int(id), Name: name})
			}
		}
	}
	return out, nil
}

// --- project name cache ----------------------------------------------------

// projectNameMemo is a global cache populated whenever fetchUserProjects
// returns rows; lookupProjectName uses it to label the confirm bubble
// without re-hitting MCP.
var (
	projectNameMemoMu sync.RWMutex
	projectNameMemo   = map[string]string{}
)

func rememberProjectNames(ps []project) {
	projectNameMemoMu.Lock()
	defer projectNameMemoMu.Unlock()
	for _, p := range ps {
		projectNameMemo[strconv.Itoa(p.ID)] = p.Name
	}
}

func lookupProjectName(idStr string) string {
	projectNameMemoMu.RLock()
	defer projectNameMemoMu.RUnlock()
	return projectNameMemo[idStr]
}

// --- helpers ---------------------------------------------------------------

func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Insertion sort — n is small (≤10).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func joinIntsCSV(ints []int) string {
	parts := make([]string, len(ints))
	for i, n := range ints {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}
