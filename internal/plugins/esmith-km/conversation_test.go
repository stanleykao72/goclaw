package esmithkm

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/channels/line"
)

// newTestHook builds a Hook with a tmp drafts dir and the given sender.
// Config.WithDefaults fills the rest. Tests that override runPipeline or
// mcpToolCall must also restore them via deferred assignment.
func newTestHook(t *testing.T, draftsDir string) *Hook {
	t.Helper()
	return New(Config{
		DraftsDir:    draftsDir,
		PublishedDir: filepath.Join(draftsDir, "published"),
	})
}

// --- postback parsing -------------------------------------------------------

func TestBuildPostbackData_RoundTripsThroughURLValues(t *testing.T) {
	in := map[string]string{
		"action": "update",
		"ref":    "20260408_094612_line_a1b2",
		"field":  "project_id",
		"value":  "224",
	}
	encoded := buildPostbackData(in)
	parsed, err := url.ParseQuery(encoded)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for k, v := range in {
		if got := parsed.Get(k); got != v {
			t.Errorf("key %q: want %q, got %q", k, v, got)
		}
	}
}

// --- attendee selection state ----------------------------------------------

func TestAttendeeSelection_TogglesAreIdempotentInPairs(t *testing.T) {
	s := newConversationState()
	const ref = "ref1"

	s.toggleAttendee(ref, 522)
	s.toggleAttendee(ref, 610)
	got := s.getAttendees(ref)
	if len(got) != 2 || !got[522] || !got[610] {
		t.Fatalf("expected {522,610}, got %v", got)
	}

	// Toggling the same partner removes it.
	s.toggleAttendee(ref, 522)
	got = s.getAttendees(ref)
	if len(got) != 1 || !got[610] {
		t.Errorf("expected {610}, got %v", got)
	}
}

func TestAttendeeSelection_ClearRefRemovesAllStateForThatRef(t *testing.T) {
	s := newConversationState()
	s.toggleAttendee("ref-a", 1)
	s.toggleAttendee("ref-b", 2)
	s.cacheProjectName("ref-a", "Project A")
	s.cacheAttendeesPool("ref-a", []partner{{ID: 1, Name: "Alice"}})

	s.clearRef("ref-a")

	if got := s.getAttendees("ref-a"); len(got) != 0 {
		t.Errorf("expected attendees cleared, got %v", got)
	}
	if got := s.getProjectName("ref-a"); got != "" {
		t.Errorf("expected project name cleared, got %q", got)
	}
	if got := s.getAttendeesPool("ref-a"); got != nil {
		t.Errorf("expected pool cleared, got %v", got)
	}
	// ref-b untouched.
	if got := s.getAttendees("ref-b"); len(got) != 1 || !got[2] {
		t.Errorf("expected ref-b unchanged, got %v", got)
	}
}

// --- exec wrapper output parsing -------------------------------------------

func TestRunPublishUpdate_ParsesNextStateFromUpdateOK(t *testing.T) {
	original := runPipeline
	defer func() { runPipeline = original }()
	runPipeline = func(scriptPath string, args ...string) (string, error) {
		return "[publish-odoo] update OK 20260408_094612 (state=awaiting_location)\n", nil
	}

	h := newTestHook(t, t.TempDir())
	state, err := h.runPublishUpdate("20260408_094612", "project_id", "224")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != "awaiting_location" {
		t.Errorf("want awaiting_location, got %q", state)
	}
}

func TestRunPublishFinalize_ExtractsNewID(t *testing.T) {
	original := runPipeline
	defer func() { runPipeline = original }()
	runPipeline = func(scriptPath string, args ...string) (string, error) {
		return "[publish-odoo] finalize OK 20260408_094612 → job.meeting.minutes id=490\n", nil
	}

	h := newTestHook(t, t.TempDir())
	id, err := h.runPublishFinalize("20260408_094612")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != 490 {
		t.Errorf("want 490, got %d", id)
	}
}

func TestRunPublishFinalize_IdempotentSuccessReturnsZeroNoError(t *testing.T) {
	original := runPipeline
	defer func() { runPipeline = original }()
	runPipeline = func(scriptPath string, args ...string) (string, error) {
		return "[publish-odoo] finalize: km_source_ref already exists in Odoo (idempotent success)\n", nil
	}

	h := newTestHook(t, t.TempDir())
	id, err := h.runPublishFinalize("20260408_094612")
	if err != nil {
		t.Fatalf("idempotent path should not error: %v", err)
	}
	if id != 0 {
		t.Errorf("want 0, got %d", id)
	}
}

// --- draft scanner ---------------------------------------------------------

func TestScanDraftsOnce_SkipsAlreadyPushedRefs(t *testing.T) {
	dir := t.TempDir()

	chatID := "Cabc"
	d := draftJSON{
		SourceRef:  "ref1",
		State:      "awaiting_project",
		LineChatID: &chatID,
	}
	writeDraftFile(t, dir, d)
	// Pre-existing .pushed marker means we should skip without dialing LINE.
	if err := os.WriteFile(filepath.Join(dir, "ref1.pushed"), nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	// We pass a Hook with nil Sender — if scanDraftsOnce tries to push, it
	// will panic on Sender.PushFlex. The test passes iff no panic.
	h := newTestHook(t, dir)
	h.scanDraftsOnce()
}

func TestScanDraftsOnce_MarksNonInitialStateAsPushedAndDoesNotPush(t *testing.T) {
	dir := t.TempDir()

	chatID := "Cabc"
	d := draftJSON{
		SourceRef:  "ref2",
		State:      "awaiting_location",
		LineChatID: &chatID,
	}
	writeDraftFile(t, dir, d)

	h := newTestHook(t, dir)
	h.scanDraftsOnce()

	if _, err := os.Stat(filepath.Join(dir, "ref2.pushed")); err != nil {
		t.Errorf("expected .pushed marker after non-initial state scan: %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

func TestSortedKeysAndJoinIntsCSV(t *testing.T) {
	in := map[int]bool{610: true, 522: true, 701: true}
	keys := sortedKeys(in)
	if got := joinIntsCSV(keys); got != "522,610,701" {
		t.Errorf("want 522,610,701, got %q", got)
	}
}

// --- mcpToolCall override pattern -----------------------------------------

func TestFetchUserProjects_UsesMCPCallAndFallsBackOnEmpty(t *testing.T) {
	original := mcpToolCall
	defer func() { mcpToolCall = original }()

	calls := 0
	mcpToolCall = func(ctx context.Context, endpoint, token, tool string, args map[string]any, dst any) error {
		calls++
		if calls == 1 {
			// First call: user's projects empty.
			return json.Unmarshal([]byte(`[]`), dst)
		}
		// Second call: fallback yields one row.
		return json.Unmarshal([]byte(`[{"id":7,"name":"Fallback Project"}]`), dst)
	}

	// Use a Hook with stub endpoint/token — the stub ignores them anyway.
	// Pass empty lineUserID so fetchUserProjects skips the LIFF path and
	// exercises the MCP fallback we're stubbing here.
	h := New(Config{MCPURL: "http://stub", MCPToken: "stub-token"})
	got, err := h.fetchUserProjects(context.Background(), "", 42)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0].ID != 7 {
		t.Errorf("want fallback project id=7, got %+v", got)
	}
	if calls != 2 {
		t.Errorf("expected 2 mcp calls, got %d", calls)
	}
	// rememberProjectNames should have populated the cache.
	if name := lookupProjectName("7"); name != "Fallback Project" {
		t.Errorf("name cache miss, got %q", name)
	}
}

// writeDraftFile writes a draftJSON to dir/<ref>.json. Failure aborts the
// test — no caller should have to handle JSON marshal errors on a literal.
func writeDraftFile(t *testing.T, dir string, d draftJSON) {
	t.Helper()
	raw, err := json.Marshal(&d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, d.SourceRef+".json"), raw, 0o644); err != nil {
		t.Fatalf("write draft: %v", err)
	}
}

// --- parseResolveStdout ----------------------------------------------------

func TestParseResolveStdout_ExtractsIDFromBareLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"bare integer", "6\n", 6},
		{"with log prefix above", "[2026-04-08 06:29:18] [publish-odoo] resolved Ufoo → res.users.id=6\n6\n", 6},
		{"empty stdout", "", 0},
		{"only log prefix lines", "[publish-odoo] not found\n", 0},
		{"multiline whitespace", "  42  \n", 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseResolveStdout(tc.in)
			if got != tc.want {
				t.Errorf("want %d, got %d", tc.want, got)
			}
		})
	}
}

// --- bind pending recovery -------------------------------------------------

func TestScanDraftsOnce_BindPendingDoesNotPushTwice(t *testing.T) {
	dir := t.TempDir()

	chatID := "Cabc"
	uid := "Ufake_unbound_user"
	d := draftJSON{
		SourceRef:      "bind_test_ref",
		State:          "awaiting_project",
		LineChatID:     &chatID,
		LineUserID:     &uid,
		ResolvedUserID: nil, // unbound
	}
	writeDraftFile(t, dir, d)

	// Stub the bash CLI so resolve always returns empty (still unbound).
	original := runPipeline
	defer func() { runPipeline = original }()
	runPipeline = func(scriptPath string, args ...string) (string, error) {
		return "", nil
	}

	h := newTestHook(t, dir)
	// First scan: pushes hint, sets bind_pending marker.
	h.scanDraftsOnce()
	if _, err := os.Stat(filepath.Join(dir, "bind_test_ref"+draftBindPendingSuffix)); err != nil {
		t.Errorf("expected bind_pending marker after first scan, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bind_test_ref"+draftPushedSuffix)); err == nil {
		t.Errorf("did NOT expect .pushed marker for unbound draft")
	}

	// Second scan: still unbound → must NOT re-push or set .pushed.
	h.scanDraftsOnce()
	if _, err := os.Stat(filepath.Join(dir, "bind_test_ref"+draftPushedSuffix)); err == nil {
		t.Errorf("did NOT expect .pushed marker after second unbound scan")
	}
}

// --- stale draft cleanup ---------------------------------------------------

func TestCleanupStaleDraftsOnce_RemovesOldDraftsAndPreservesYoungOnes(t *testing.T) {
	dir := t.TempDir()

	chatID := "Cabc"
	old := draftJSON{
		SourceRef:  "old_ref",
		State:      "awaiting_attendees",
		LineChatID: &chatID,
		Subject:    "stale meeting",
	}
	young := draftJSON{
		SourceRef:  "young_ref",
		State:      "awaiting_project",
		LineChatID: &chatID,
		Subject:    "fresh meeting",
	}
	writeDraftFile(t, dir, old)
	writeDraftFile(t, dir, young)

	h := newTestHook(t, dir)
	// Force the old one to look ancient.
	oldPath := filepath.Join(dir, "old_ref.json")
	ancient := time.Now().Add(-h.cfg.DraftStaleTTL - time.Hour)
	if err := os.Chtimes(oldPath, ancient, ancient); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	h.cleanupStaleDraftsOnce()

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("expected old draft removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "young_ref.json")); err != nil {
		t.Errorf("expected young draft preserved, err=%v", err)
	}
}

func TestCleanupStaleDraftsOnce_HandlesMissingChatIDGracefully(t *testing.T) {
	dir := t.TempDir()

	d := draftJSON{
		SourceRef: "no_chat_ref",
		State:     "awaiting_project",
		// LineChatID is nil — gdrive draft without LINE provenance
	}
	writeDraftFile(t, dir, d)

	h := newTestHook(t, dir)
	path := filepath.Join(dir, "no_chat_ref.json")
	ancient := time.Now().Add(-h.cfg.DraftStaleTTL - time.Hour)
	_ = os.Chtimes(path, ancient, ancient)

	// nil-sender Hook — if cleanup tries to push, Sender nil-check elides it.
	// The chat-id-missing branch should silently skip the push regardless.
	h.cleanupStaleDraftsOnce()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected draft removed even without chat_id, err=%v", err)
	}
}

// --- truncate --------------------------------------------------------------

func TestTruncateRespectsRuneCount(t *testing.T) {
	// line.Truncate is the canonical implementation; the plugin uses it via
	// the line package. Verify here so any change in channels/line gets
	// caught by the plugin test suite too.
	in := strings.Repeat("中", 80)
	out := line.Truncate(in, 10)
	if r := []rune(out); len(r) != 10 {
		t.Errorf("want 10 runes, got %d (%q)", len(r), out)
	}
}
