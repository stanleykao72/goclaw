package esmithkm

// conversation.go implements the LINE Flex Message conversation flow that
// follows km-meeting-pipeline.sh `cmd_check`. The bash side writes a draft
// JSON under /data/km/meetings/drafts/ in state `awaiting_project`; this
// file picks up that draft and walks the user through 4 picker bubbles to
// fill the remaining required fields, finally calling `publish-odoo finalize`
// which creates the `job.meeting.minutes` record on stage35.
//
// Architecture choice: file-first state, polling watcher.
//
//	bash               file system                 esmith-km plugin
//	-----              -----------                 ----------------
//	cmd_check  ---->   drafts/<ref>.json     <---  draft watcher (polling, 10s)
//	                   .pushed marker        --->  Sender.PushFlex project_picker
//	                                               -- user taps button --
//	                   <- LINE PostbackEvent ---   Hook.OnPostback → handlePostback
//	                                               exec publish-odoo update
//	                                               Sender.ReplyFlex next picker
//	                                               ...
//	                                               exec publish-odoo finalize
//	                                               Sender.SendChunks id confirmation
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

	"github.com/nextlevelbuilder/goclaw/internal/channels/line"
)

const (
	draftPushedSuffix      = ".pushed"
	draftBindPendingSuffix = ".bind_pending"
	maxProjectsPerPicker   = 15
	maxAttendeesPerPicker  = 30
	mcpRequestTimeout      = 15 * time.Second
	pipelineExecTimeout    = 30 * time.Second

	// draftStaleCheckEvery runs as part of the draft watcher, but at a slower
	// cadence than the picker scan so it does not race with the picker push
	// pass. The TTL itself is read from Hook.cfg.DraftStaleTTL.
	draftStaleCheckEvery = 5 * time.Minute
)

// errBindPending is the sentinel returned by pushProjectPickerForDraft when
// the LINE user is not bound and we want the scan loop to leave the draft
// in "bind_pending" state (no .pushed marker, so the next scan will retry
// resolve via tryRecoverBindPending).
var errBindPending = errors.New("bind pending")

// finalizeIDRegex extracts the new id from `finalize OK ... id=<N>` lines
// emitted by km-meeting-pipeline.sh publish-odoo finalize.
var finalizeIDRegex = regexp.MustCompile(`id=(\d+)`)

// updateOKRegex confirms an `update` call succeeded and reports the next
// state. Format: `update OK <ref>.<field>=<value> (state=<next>)`.
var updateOKRegex = regexp.MustCompile(`update OK\s+\S+\s+\(state=(\w+)\)`)

// draftJSON is the subset of the file-first draft state that the plugin
// needs. The full schema lives in km-meeting-pipeline.sh; do not write
// fields here that the bash side does not understand.
type draftJSON struct {
	SourceRef      string  `json:"source_ref"`
	State          string  `json:"state"`
	LineChatID     *string `json:"line_chat_id"`
	LineUserID     *string `json:"line_user_id"`
	ResolvedUserID *int    `json:"resolved_user_id"`
	Subject        string  `json:"subject"`
	MeetingDate    string  `json:"meeting_date"`
	Answers        struct {
		ProjectID        *int    `json:"project_id"`
		MeetingLocation  *string `json:"meeting_location"`
		PartnerIDs       []int   `json:"partner_ids"`
		JobTypeID        *int    `json:"job_type_id"`
		JobWorkingPlanID *int    `json:"job_working_plan_id"`
	} `json:"answers"`
}

// conversationState is the per-hook runtime state for the meeting writeback
// flow. It is independent from the file-first draft (which is the source of
// truth for cross-process state) — this struct only tracks transient
// in-memory concerns: the in-progress attendee multi-select set and a
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

func (h *Hook) draftPath(ref string) string {
	return filepath.Join(h.cfg.DraftsDir, ref+".json")
}

func (h *Hook) draftPublishedPath(ref string) string {
	return filepath.Join(h.cfg.PublishedDir, ref+".json")
}

func (h *Hook) draftPushedMarker(ref string) string {
	return filepath.Join(h.cfg.DraftsDir, ref+draftPushedSuffix)
}

func (h *Hook) draftBindPendingMarker(ref string) string {
	return filepath.Join(h.cfg.DraftsDir, ref+draftBindPendingSuffix)
}

// --- draft watcher ---------------------------------------------------------

// startDraftWatcher polls drafts/ for new awaiting_project drafts that need
// the project_picker bubble pushed. Idempotent via .pushed marker file —
// even if the plugin restarts, an already-pushed draft is not pushed again.
//
// A second slower ticker runs the stale-draft cleanup pass which pushes a
// LINE expiry notification and removes drafts whose mtime is past TTL.
//
// The watcher exits when ctx is cancelled (Hook.Stop via watcherCancel).
func (h *Hook) startDraftWatcher(ctx context.Context) {
	interval := h.cfg.DraftPollInterval
	slog.Info("LINE meeting: draft watcher started",
		"dir", h.cfg.DraftsDir,
		"interval", interval,
		"stale_check_every", draftStaleCheckEvery,
		"stale_ttl", h.cfg.DraftStaleTTL,
	)

	pickerTick := time.NewTicker(interval)
	defer pickerTick.Stop()
	staleTick := time.NewTicker(draftStaleCheckEvery)
	defer staleTick.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("LINE meeting: draft watcher stopped")
			return
		case <-pickerTick.C:
			h.scanDraftsOnce()
		case <-staleTick.C:
			h.cleanupStaleDraftsOnce()
		}
	}
}

// cleanupStaleDraftsOnce removes drafts whose mtime is past DraftStaleTTL,
// pushing a LINE expiry message to the original chat first when chat_id
// is known. Idempotent: a draft removed in a previous pass simply isn't
// there next time. Safe to call from a single goroutine.
//
// Spec scenario: "使用者放棄 draft → 24h TTL cleanup".
func (h *Hook) cleanupStaleDraftsOnce() {
	dir := h.cfg.DraftsDir
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("LINE meeting: stale-cleanup readdir failed", "err", err, "dir", dir)
		}
		return
	}

	cutoff := time.Now().Add(-h.cfg.DraftStaleTTL)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		ref := strings.TrimSuffix(name, ".json")
		path := filepath.Join(dir, name)

		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue // not stale
		}

		// Best-effort notification before deletion. If the draft is
		// missing chat_id (gdrive ingest with no LINE provenance), the
		// notification step is silently skipped — we still delete.
		d, derr := readDraft(path)
		if derr == nil && d.LineChatID != nil && *d.LineChatID != "" {
			msg := fmt.Sprintf(
				"⏰ 會議草稿「%s」已超過 %d 小時未完成補欄位，已自動清除。\n\n下次傳新的語音檔或 GDrive 連結時可以重新開始。",
				line.Truncate(d.Subject, 40),
				int(h.cfg.DraftStaleTTL/time.Hour),
			)
			if h.cfg.Sender != nil {
				if perr := h.cfg.Sender.SendChunks(*d.LineChatID, []string{msg}); perr != nil {
					slog.Warn("LINE meeting: stale-cleanup notify failed",
						"ref", ref, "err", perr)
					// Push failure does not block deletion — file age is
					// the source of truth, not whether LINE took the message.
				}
			}
		}

		if rerr := os.Remove(path); rerr != nil {
			slog.Warn("LINE meeting: stale-cleanup remove failed",
				"ref", ref, "err", rerr)
			continue
		}
		// Also drop the .pushed marker so the dir stays clean.
		_ = os.Remove(h.draftPushedMarker(ref))

		// Forget any per-ref in-memory state.
		if h.conv != nil {
			h.conv.clearRef(ref)
		}

		slog.Info("LINE meeting: stale draft removed",
			"ref", ref,
			"age_hours", time.Since(info.ModTime()).Hours(),
		)
	}
}

func (h *Hook) scanDraftsOnce() {
	dir := h.cfg.DraftsDir
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

		if _, err := os.Stat(h.draftPushedMarker(ref)); err == nil {
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
			_ = h.touchPushedMarker(ref)
			continue
		}
		if d.LineChatID == nil || *d.LineChatID == "" {
			slog.Debug("LINE meeting: draft has no chat id, skipping", "ref", ref)
			_ = h.touchPushedMarker(ref)
			continue
		}

		// Bind-pending recovery: a previous scan pushed the /bind hint
		// because the LINE user was not bound. Re-attempt resolve via
		// the bash sync_line helper — if the user has bound since,
		// patch the draft + clear the bind_pending marker so we can
		// fall through into the normal picker push.
		if _, err := os.Stat(h.draftBindPendingMarker(ref)); err == nil {
			if h.tryRecoverBindPending(ref, d, path) {
				// recovery succeeded, draft now has resolved_user_id
			} else {
				// still unbound — leave bind_pending marker so the
				// next scan retries; do NOT re-push the hint to avoid
				// LINE notification spam
				continue
			}
		}

		if err := h.pushProjectPickerForDraft(d); err != nil {
			if errors.Is(err, errBindPending) {
				// expected — scan again next tick to recover
				continue
			}
			slog.Error("LINE meeting: push project_picker failed",
				"ref", ref, "err", err)
			continue
		}
		if err := h.touchPushedMarker(ref); err != nil {
			slog.Warn("LINE meeting: failed to write .pushed marker",
				"ref", ref, "err", err)
		}
		// Clear bind-pending now that we've successfully pushed the picker.
		_ = os.Remove(h.draftBindPendingMarker(ref))
	}
}

func (h *Hook) touchPushedMarker(ref string) error {
	f, err := os.Create(h.draftPushedMarker(ref))
	if err != nil {
		return err
	}
	return f.Close()
}

func (h *Hook) touchBindPendingMarker(ref string) error {
	f, err := os.Create(h.draftBindPendingMarker(ref))
	if err != nil {
		return err
	}
	return f.Close()
}

// tryRecoverBindPending re-attempts to resolve the LINE user via bash
// sync_line_resolve helper. On success, patches the draft file in place
// with the new resolved_user_id and updates the in-memory draftJSON,
// then notifies the user that resolution succeeded so they understand
// why a fresh picker bubble is about to land. Returns true on recovery.
func (h *Hook) tryRecoverBindPending(ref string, d *draftJSON, path string) bool {
	if d.LineUserID == nil || *d.LineUserID == "" {
		return false
	}
	out, err := runPipeline(h.cfg.PipelineScript, "publish-odoo", "resolve", *d.LineUserID)
	if err != nil {
		slog.Debug("LINE meeting: bind recovery resolve still failing",
			"ref", ref, "err", err)
		return false
	}
	uid := parseResolveStdout(out)
	if uid <= 0 {
		return false
	}

	// Patch the draft JSON with the new resolved_user_id.
	if perr := patchDraftResolvedUserID(path, uid); perr != nil {
		slog.Warn("LINE meeting: bind recovery patch draft failed",
			"ref", ref, "err", perr)
		return false
	}
	d.ResolvedUserID = &uid

	if d.LineChatID != nil && h.cfg.Sender != nil {
		_ = h.cfg.Sender.SendChunks(*d.LineChatID, []string{
			"✅ 已偵測到你完成綁定，正在重新傳送會議補欄位選單…",
		})
	}
	slog.Info("LINE meeting: bind recovery succeeded",
		"ref", ref, "resolved_user_id", uid)
	return true
}

// parseResolveStdout extracts a numeric user id from the bash
// `publish-odoo resolve <uid>` stdout. The bash side prints just the
// integer (or empty) on success.
func parseResolveStdout(out string) int {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "[") {
			continue // skip log prefix lines
		}
		if id, err := strconv.Atoi(line); err == nil {
			return id
		}
	}
	return 0
}

// patchDraftResolvedUserID is an atomic JSON merge that updates exactly
// one field on the bash-owned draft. We do not invoke jq here — keep
// the dependency surface small and stay in Go.
func patchDraftResolvedUserID(path string, uid int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	m["resolved_user_id"] = uid
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (h *Hook) pushProjectPickerForDraft(d *draftJSON) error {
	chatID := *d.LineChatID

	if d.ResolvedUserID == nil {
		// LINE user not bound to a res.users — instruct user to /bind
		// AND mark this draft as bind_pending so the watcher knows to
		// keep retrying resolve (instead of giving up after the first
		// hint via the .pushed marker). The bind_pending marker is
		// idempotent — it gets cleared once recovery succeeds.
		msg := "👋 已收到語音檔，但找不到對應的 Odoo 使用者。\n\n請先傳送 `/bind <你的 e-smith email>` 完成綁定，綁定成功後系統會自動繼續對話。"
		if err := h.touchBindPendingMarker(d.SourceRef); err != nil {
			slog.Warn("LINE meeting: failed to write .bind_pending marker",
				"ref", d.SourceRef, "err", err)
		}
		// Return a sentinel error so the caller does NOT touch the
		// .pushed marker — bind-pending state is the authoritative one.
		if h.cfg.Sender != nil {
			if perr := h.cfg.Sender.SendChunks(chatID, []string{msg}); perr != nil {
				return perr
			}
		}
		return errBindPending
	}

	var lineUID string
	if d.LineUserID != nil {
		lineUID = *d.LineUserID
	}
	projects, err := h.fetchUserProjects(context.Background(), lineUID, *d.ResolvedUserID)
	if err != nil {
		return fmt.Errorf("fetch projects: %w", err)
	}
	if len(projects) == 0 {
		msg := "已收到會議錄音，但找不到你最近活躍的專案。請至 Odoo 確認你的負責專案後再試。"
		if h.cfg.Sender != nil {
			return h.cfg.Sender.SendChunks(chatID, []string{msg})
		}
		return nil
	}

	bubble, err := buildProjectPicker(d.SourceRef, d.Subject, projects)
	if err != nil {
		return fmt.Errorf("build project picker: %w", err)
	}
	if h.cfg.Sender != nil {
		return h.cfg.Sender.PushFlex(chatID, "請選擇會議專案", bubble)
	}
	return nil
}

// --- postback handler ------------------------------------------------------

// handlePostback parses the postback `data` query string and routes to the
// matching state-machine action. Called from Hook.OnPostback.
func (h *Hook) handlePostback(ev line.PostbackEvent) {
	chatID := ev.ChatID
	if chatID == "" {
		return
	}

	values, err := url.ParseQuery(ev.Data)
	if err != nil {
		slog.Warn("LINE meeting: bad postback data", "data", ev.Data, "err", err)
		return
	}
	action := values.Get("action")
	ref := values.Get("ref")
	if action == "" || ref == "" {
		return
	}

	switch action {
	case "update":
		h.handleUpdatePostback(chatID, ref, values.Get("field"), values.Get("value"))
	case "toggle":
		h.handleTogglePostback(chatID, ref, values.Get("value"))
	case "submit_attendees":
		h.handleSubmitAttendees(chatID, ref)
	case "finalize":
		h.handleFinalize(chatID, ref)
	case "cancel":
		h.handleCancel(chatID, ref)
	default:
		slog.Warn("LINE meeting: unknown postback action", "action", action)
	}
}

// chatIDFromSource extracts a stable chat id from any LINE event source.
// Kept in the plugin (instead of exported from channels/line) because it
// is an e-smith-specific helper used only when a LINE-typed source leaks
// through a test fixture.
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

func (h *Hook) handleUpdatePostback(chatID, ref, field, value string) {
	if field == "" || value == "" {
		return
	}
	nextState, err := h.runPublishUpdate(ref, field, value)
	if err != nil {
		slog.Error("LINE meeting: publish-odoo update failed",
			"ref", ref, "field", field, "err", err)
		if h.cfg.Sender != nil {
			_ = h.cfg.Sender.SendChunks(chatID, []string{"⚠️ 更新會議草稿失敗，請稍後再試"})
		}
		return
	}

	// Cache the project name on first project pick so the confirm bubble
	// can show a label instead of just an id.
	if field == "project_id" {
		if name := lookupProjectName(value); name != "" {
			h.conv.cacheProjectName(ref, name)
		}
	}

	switch nextState {
	case "awaiting_location":
		bubble, _ := buildLocationPicker(ref)
		if h.cfg.Sender != nil {
			_ = h.cfg.Sender.ReplyFlex(chatID, "選擇會議地點", bubble)
		}
	case "awaiting_attendees":
		h.sendAttendeesPicker(chatID, ref)
	case "awaiting_confirm":
		h.sendConfirmBubble(chatID, ref)
	default:
		slog.Warn("LINE meeting: unknown next state", "ref", ref, "state", nextState)
	}
}

func (h *Hook) handleTogglePostback(chatID, ref, value string) {
	pid, err := strconv.Atoi(value)
	if err != nil {
		return
	}
	h.conv.toggleAttendee(ref, pid)
	h.sendAttendeesPicker(chatID, ref)
}

func (h *Hook) handleSubmitAttendees(chatID, ref string) {
	selected := h.conv.getAttendees(ref)
	if len(selected) == 0 {
		if h.cfg.Sender != nil {
			_ = h.cfg.Sender.SendChunks(chatID, []string{"請至少選擇 1 位出席者"})
		}
		return
	}
	csv := joinIntsCSV(sortedKeys(selected))
	h.handleUpdatePostback(chatID, ref, "partner_ids", csv)
}

func (h *Hook) handleFinalize(chatID, ref string) {
	id, err := h.runPublishFinalize(ref)
	if err != nil {
		slog.Error("LINE meeting: publish-odoo finalize failed",
			"ref", ref, "err", err)
		if h.cfg.Sender != nil {
			_ = h.cfg.Sender.SendChunks(chatID, []string{
				"⚠️ 建立會議記錄時發生錯誤，請稍後再試或聯絡系統管理員",
			})
		}
		return
	}
	msg := "✅ 會議記錄已建立"
	if id > 0 {
		msg = fmt.Sprintf("%s（id=%d）", msg, id)
		if link := h.buildOdooDeepLink("job.meeting.minutes", id); link != "" {
			msg = msg + "\n\n📎 點此查看：\n" + link
		}
	}
	h.conv.clearRef(ref)
	if h.cfg.Sender != nil {
		_ = h.cfg.Sender.SendChunks(chatID, []string{msg})
	}
}

func (h *Hook) handleCancel(chatID, ref string) {
	h.conv.clearRef(ref)
	if h.cfg.Sender != nil {
		_ = h.cfg.Sender.SendChunks(chatID, []string{"已取消。下次錄音時可重新填寫。"})
	}
}

// sendAttendeesPicker fetches partners (cached after first call), renders
// the bubble with current selection, and replies / pushes.
func (h *Hook) sendAttendeesPicker(chatID, ref string) {
	pool := h.conv.getAttendeesPool(ref)
	if pool == nil {
		// First time — fetch partner candidates from MCP. We use the
		// project's team members (project.user_ids → res.users → partner_id)
		// as the pool. Falls back to top-N res.partner if empty.
		fetched, err := h.fetchProjectAttendees(context.Background(), ref)
		if err != nil {
			slog.Warn("LINE meeting: fetch attendees failed", "ref", ref, "err", err)
		}
		pool = fetched
		h.conv.cacheAttendeesPool(ref, pool)
	}
	if len(pool) == 0 {
		if h.cfg.Sender != nil {
			_ = h.cfg.Sender.SendChunks(chatID, []string{"找不到候選出席者，請至 Odoo 手動建立會議記錄。"})
		}
		return
	}

	selected := h.conv.getAttendees(ref)
	bubble, err := buildAttendeesPicker(ref, pool, selected)
	if err != nil {
		slog.Error("LINE meeting: build attendees picker", "err", err)
		return
	}
	if h.cfg.Sender != nil {
		_ = h.cfg.Sender.ReplyFlex(chatID, "選擇出席者", bubble)
	}
}

func (h *Hook) sendConfirmBubble(chatID, ref string) {
	d, err := readDraft(h.draftPath(ref))
	if err != nil {
		slog.Error("LINE meeting: re-read draft for confirm", "ref", ref, "err", err)
		return
	}
	projectName := h.conv.getProjectName(ref)
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
	if h.cfg.Sender != nil {
		_ = h.cfg.Sender.ReplyFlex(chatID, "確認會議記錄", bubble)
	}
}

// --- exec wrappers ----------------------------------------------------------

// runPublishUpdate calls `km-meeting-pipeline.sh publish-odoo update <ref> <field> <value>`
// and parses the next state from stdout.
func (h *Hook) runPublishUpdate(ref, field, value string) (string, error) {
	out, err := runPipeline(h.cfg.PipelineScript, "publish-odoo", "update", ref, field, value)
	if err != nil {
		return "", err
	}
	if m := updateOKRegex.FindStringSubmatch(out); len(m) >= 2 {
		return m[1], nil
	}
	// Field is one of the optional ones (job_type_id / job_working_plan_id) —
	// the bash side does not advance state. Re-read draft for current state.
	d, derr := readDraft(h.draftPath(ref))
	if derr != nil {
		return "", fmt.Errorf("update succeeded but state read failed: %w", derr)
	}
	return d.State, nil
}

// runPublishFinalize calls `km-meeting-pipeline.sh publish-odoo finalize <ref>`
// and parses the new record id from stdout.
func (h *Hook) runPublishFinalize(ref string) (int, error) {
	out, err := runPipeline(h.cfg.PipelineScript, "publish-odoo", "finalize", ref)
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
// Package-level var so tests can stub it — the `var runPipeline = func…`
// form is deliberate. Tests in this package save the original and restore
// it via a deferred assignment.
var runPipeline = func(scriptPath string, args ...string) (string, error) {
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
//
// Package-level var so tests can stub it (see conversation_test.go's
// `original := mcpToolCall; mcpToolCall = func(...)` pattern). Endpoint
// and token are passed in by the Hook's callers from h.cfg — this keeps
// the function free of env-var reads and makes Hook.cfg the single
// source of truth for MCP configuration.
var mcpToolCall = func(ctx context.Context, endpoint, token, tool string, args map[string]any, dst any) error {
	if endpoint == "" || token == "" {
		return errors.New("esmith-km: MCP endpoint/token not configured (h.cfg.MCPURL / h.cfg.MCPToken)")
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

// fetchUserProjects returns up to maxProjectsPerPicker projects to show
// in the picker bubble.
//
// Primary path: call the e-smith job_field_recorder LIFF endpoint
// `/liff/field_recorder/projects` with the LINE user ID. That endpoint
// applies the FR module's "my projects" logic: favorites → recent →
// others, scoped via Odoo record rules under the employee's user
// context. This is the same three-tier ordering the FR SPA shows,
// which is what operators expect.
//
// Fallback path (when LIFF is unreachable or returns zero): the older
// MCP-based query against project.project filtered by user_id. That
// path is known to be incomplete (see the 2026-04-08 dual-review
// findings — it queries project.project instead of job.project, so
// it may show projects without active working plans), but it is kept
// as a safety net so the picker never silently fails.
func (h *Hook) fetchUserProjects(ctx context.Context, lineUserID string, fallbackUserID int) ([]project, error) {
	if lineUserID != "" {
		projects, err := h.fetchProjectsViaLIFF(ctx, lineUserID)
		if err == nil && len(projects) > 0 {
			rememberProjectNames(projects)
			return projects, nil
		}
		if err != nil {
			slog.Warn("esmith-km: LIFF project fetch failed, falling back to MCP",
				"line_user_id", lineUserID, "err", err)
		}
	}
	return h.fetchUserProjectsViaMCP(ctx, fallbackUserID)
}

// fetchProjectsViaLIFF calls the e-smith job_field_recorder LIFF endpoint
// `/liff/field_recorder/projects`. The endpoint uses `type='json'` which
// means it follows the Odoo JSON-RPC 2.0 envelope:
//
//	{"jsonrpc":"2.0","method":"call","params":{...},"id":1}
//
// and returns:
//
//	{"jsonrpc":"2.0","id":1,"result":{"success":true,"projects":[...]}}
//
// On Odoo-side errors the `result` object has `success: false` and an
// `error` field — we treat that as a non-nil error so the caller can
// fall back to the MCP path.
func (h *Hook) fetchProjectsViaLIFF(ctx context.Context, lineUserID string) ([]project, error) {
	base := parseBaseURL(h.cfg.MCPURL)
	if base == "" {
		base = h.cfg.OdooBaseURL
	}
	if base == "" {
		return nil, errors.New("esmith-km: LIFF base URL not configured")
	}
	endpoint := strings.TrimRight(base, "/") + "/liff/field_recorder/projects"

	envelope := map[string]any{
		"jsonrpc": "2.0",
		"method":  "call",
		"id":      1,
		"params": map[string]any{
			"line_user_id": lineUserID,
		},
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, mcpRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("liff http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("liff http %d", resp.StatusCode)
	}

	var decoded struct {
		Result struct {
			Success  bool   `json:"success"`
			Error    string `json:"error,omitempty"`
			Projects []struct {
				ID         int    `json:"id"` // job.project.id
				Name       string `json:"name"`
				IsFavorite bool   `json:"is_favorite"`
			} `json:"projects"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("liff decode: %w", err)
	}
	if !decoded.Result.Success {
		return nil, fmt.Errorf("liff error: %s", decoded.Result.Error)
	}

	// Collect ONLY the favorites. Per user feedback on 2026-04-08 E2E:
	// the picker should show "我的收藏" only, not the full FR three-tier
	// list (favorites + recent + others). Users curate their meeting
	// picker explicitly via the FR "我的專案" screen star button; if
	// they haven't starred anything, fall back to MCP further down.
	type staged struct {
		jobProjectID int
		name         string
	}
	stagedRows := make([]staged, 0, maxProjectsPerPicker)
	for _, p := range decoded.Result.Projects {
		if !p.IsFavorite {
			continue
		}
		stagedRows = append(stagedRows, staged{
			jobProjectID: p.ID,
			name:         p.Name,
		})
		if len(stagedRows) >= maxProjectsPerPicker {
			break
		}
	}
	if len(stagedRows) == 0 {
		return nil, nil
	}

	// Resolve job.project.id → project.project.id via the m2o. Required
	// because job.meeting.minutes.project_id is a FK to project.project,
	// not job.project — the LIFF endpoint exposes job.project rows keyed
	// by their own PK so we cannot use those ids directly at finalize
	// time (see the 2026-04-08 phase 6 E2E error: FK constraint
	// job_meeting_minutes_project_id_fkey violated when goclaw passed
	// job.project.id as the meeting's project_id).
	jobIDs := make([]int, len(stagedRows))
	for i, r := range stagedRows {
		jobIDs[i] = r.jobProjectID
	}
	var jpRows []struct {
		ID        int   `json:"id"`
		ProjectID []any `json:"project_id"` // [pp_id, pp_name] in m2o read format
	}
	if err := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "search_records", map[string]any{
		"model":  "job.project",
		"domain": [][]any{{"id", "in", jobIDs}},
		"fields": []string{"id", "project_id"},
		"limit":  len(jobIDs),
	}, &jpRows); err != nil {
		return nil, fmt.Errorf("resolve job.project.project_id: %w", err)
	}
	ppByJobID := make(map[int]int, len(jpRows))
	for _, r := range jpRows {
		if len(r.ProjectID) >= 2 {
			if f, ok := r.ProjectID[0].(float64); ok && int(f) > 0 {
				ppByJobID[r.ID] = int(f)
			}
		}
	}

	// Build the final picker slice with project.project.id as the key.
	// Preserves the LIFF favorites order (by field.recorder.favorite.sequence).
	// Every entry here is a favorite by construction — we filtered above —
	// so IsFavorite is hardcoded true.
	out := make([]project, 0, len(stagedRows))
	for _, s := range stagedRows {
		ppID, ok := ppByJobID[s.jobProjectID]
		if !ok {
			slog.Warn("esmith-km: job.project missing project_id, skipping from picker",
				"job_project_id", s.jobProjectID)
			continue
		}
		out = append(out, project{
			ID:         ppID,
			Name:       s.name,
			IsFavorite: true,
		})
	}
	return out, nil
}

// fetchUserProjectsViaMCP is the legacy MCP-based query kept as a
// fallback for when the LIFF endpoint is unreachable. See fetchUserProjects
// for the primary path.
func (h *Hook) fetchUserProjectsViaMCP(ctx context.Context, userID int) ([]project, error) {
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
	if err := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "search_records", args, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		// Fallback: any active project the bearer-token user can see.
		args["domain"] = [][]any{{"active", "=", true}}
		if err := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "search_records", args, &rows); err != nil {
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
func (h *Hook) fetchProjectAttendees(ctx context.Context, ref string) ([]partner, error) {
	d, err := readDraft(h.draftPath(ref))
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
	if err := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "search_records", map[string]any{
		"model":  "project.project",
		"domain": [][]any{{"id", "=", *d.Answers.ProjectID}},
		"fields": []string{"id", "user_ids"},
		"limit":  1,
	}, &projectRows); err != nil {
		return nil, err
	}
	if len(projectRows) == 0 || len(projectRows[0].UserIDs) == 0 {
		return h.fetchPartnersFallback(ctx)
	}

	// Resolve user_ids → partner_id.
	var userRows []struct {
		ID        int   `json:"id"`
		PartnerID []any `json:"partner_id"` // [id, name] in Odoo many2one read format
	}
	if err := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "search_records", map[string]any{
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
		return h.fetchPartnersFallback(ctx)
	}
	return out, nil
}

// fetchPartnersFallback returns partners that correspond to internal company
// users (res.users with share=false). This is the canonical "company employee
// picker list" — we deliberately do NOT show generic res.partner customer
// contacts here. Per design D4, attendees are e-smith employees.
func (h *Hook) fetchPartnersFallback(ctx context.Context) ([]partner, error) {
	var rows []struct {
		ID        int   `json:"id"`
		PartnerID []any `json:"partner_id"` // [id, name] many2one read format
	}
	if err := mcpToolCall(ctx, h.cfg.MCPURL, h.cfg.MCPToken, "search_records", map[string]any{
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
