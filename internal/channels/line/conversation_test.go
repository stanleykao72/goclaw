package line

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	runPipeline = func(args ...string) (string, error) {
		return "[publish-odoo] update OK 20260408_094612 (state=awaiting_location)\n", nil
	}

	state, err := runPublishUpdate("20260408_094612", "project_id", "224")
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
	runPipeline = func(args ...string) (string, error) {
		return "[publish-odoo] finalize OK 20260408_094612 → job.meeting.minutes id=490\n", nil
	}

	id, err := runPublishFinalize("20260408_094612")
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
	runPipeline = func(args ...string) (string, error) {
		return "[publish-odoo] finalize: km_source_ref already exists in Odoo (idempotent success)\n", nil
	}

	id, err := runPublishFinalize("20260408_094612")
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
	t.Setenv("KM_MEETING_DRAFTS_DIR", dir)

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

	// We pass a nil-bot Channel — if scanDraftsOnce tries to push, it
	// will panic on bot.PushMessage. The test passes iff no panic.
	c := &Channel{conv: newConversationState()}
	c.scanDraftsOnce()
}

func TestScanDraftsOnce_MarksNonInitialStateAsPushedAndDoesNotPush(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KM_MEETING_DRAFTS_DIR", dir)

	chatID := "Cabc"
	d := draftJSON{
		SourceRef:  "ref2",
		State:      "awaiting_location",
		LineChatID: &chatID,
	}
	writeDraftFile(t, dir, d)

	c := &Channel{conv: newConversationState()}
	c.scanDraftsOnce()

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

func TestTruncateRespectsRuneCount(t *testing.T) {
	in := strings.Repeat("中", 80)
	out := truncate(in, 10)
	// 9 runes + ellipsis
	if r := []rune(out); len(r) != 10 {
		t.Errorf("want 10 runes, got %d (%q)", len(r), out)
	}
}

// --- mcpToolCall override pattern -----------------------------------------

func TestFetchUserProjects_UsesMCPCallAndFallsBackOnEmpty(t *testing.T) {
	original := mcpToolCall
	defer func() { mcpToolCall = original }()

	calls := 0
	mcpToolCall = func(ctx context.Context, tool string, args map[string]any, dst any) error {
		calls++
		if calls == 1 {
			// First call: user's projects empty.
			return json.Unmarshal([]byte(`[]`), dst)
		}
		// Second call: fallback yields one row.
		return json.Unmarshal([]byte(`[{"id":7,"name":"Fallback Project"}]`), dst)
	}

	got, err := fetchUserProjects(context.Background(), 42)
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
