package lineworks

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// These tests exercise the end-to-end command-reply localization path: a real
// handleBotCommand dispatch (via a capturingClient so the reply is observable)
// with the sender's preferred language resolved from a wired fake credsStore.
// They prove that /help and /status replies are localized to the requesting
// user's Odoo language, and that an absent creds store falls back to English.

// newI18nReplyChannel builds a Channel with a capturing client (replies are
// observable) and, when langCode != "", a wired fake credsStore returning that
// odoo_lang plus a non-zero MCP server id so resolveUserLang reads it. Passing
// langCode == "" leaves the creds store unwired (English fallback path).
func newI18nReplyChannel(t *testing.T, langCode string) (*Channel, *capturingClient) {
	t.Helper()
	c, cc := newAdminChannel(t)
	if langCode != "" {
		c.SetCredsStore(&fakeCredsStore{
			creds: &store.MCPUserCredentials{
				APIKey: "k",
				Env:    map[string]string{"odoo_lang": langCode},
			},
		})
		c.SetMCPServerID(uuid.New())
	}
	return c, cc
}

// TestReply_HelpLocalized asserts the /help reply is rendered in the requesting
// user's language (en / zh_TW / vi_VN), keyed off Env["odoo_lang"].
func TestReply_HelpLocalized(t *testing.T) {
	cases := []struct {
		name     string
		langCode string
		want     string // a substring unique to the expected language's /help body
	}{
		{"english_no_creds", "", "Available commands:"},
		{"english_explicit", "en_US", "Available commands:"},
		{"traditional_chinese", "zh_TW", "可用指令"},
		{"vietnamese", "vi_VN", "Các lệnh khả dụng"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cc := newI18nReplyChannel(t, tc.langCode)
			if !c.handleBotCommand(context.Background(), directEvent("userA", "/help")) {
				t.Fatal("expected /help to be handled")
			}
			got := cc.all()
			if !strings.Contains(got, tc.want) {
				t.Fatalf("/help (%s): expected reply containing %q, got %q", tc.langCode, tc.want, got)
			}
			// The in-scope command list is language-agnostic (slash tokens stay
			// literal), so it must always be present regardless of language.
			for _, cmd := range []string{"/new", "/reset", "/stop", "/stopall", "/help", "/status"} {
				if !strings.Contains(got, cmd) {
					t.Fatalf("/help (%s): expected command %q listed, got %q", tc.langCode, cmd, got)
				}
			}
		})
	}
}

// TestReply_StatusLocalized asserts the /status reply localizes the fixed labels
// (Bot status / Channel / Bot name(s)) and the running-state word, while the
// channel name remains untranslated data.
func TestReply_StatusLocalized(t *testing.T) {
	cases := []struct {
		name     string
		langCode string
		// wantLabel is a substring of the localized status template; wantState is
		// the localized "stopped" word (the test channel is not Started).
		wantLabel string
		wantState string
	}{
		{"english_no_creds", "", "Bot status:", "stopped"},
		{"traditional_chinese", "zh_TW", "機器人狀態", "已停止"},
		{"vietnamese", "vi_VN", "Trạng thái bot", "đã dừng"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cc := newI18nReplyChannel(t, tc.langCode)
			if !c.handleBotCommand(context.Background(), directEvent("userA", "/status")) {
				t.Fatal("expected /status to be handled")
			}
			got := cc.all()
			if !strings.Contains(got, tc.wantLabel) {
				t.Fatalf("/status (%s): expected label %q, got %q", tc.langCode, tc.wantLabel, got)
			}
			if !strings.Contains(got, tc.wantState) {
				t.Fatalf("/status (%s): expected running-state word %q, got %q", tc.langCode, tc.wantState, got)
			}
			// Channel name is data, not translated: it must appear verbatim.
			if !strings.Contains(got, "lineworks") {
				t.Fatalf("/status (%s): expected untranslated channel name 'lineworks', got %q", tc.langCode, got)
			}
		})
	}
}

// TestReply_ResetAckLocalized asserts a state-changing command's ack (here
// /reset) is localized to the sender. /reset is open in a 1:1 chat (the writer
// gate only applies to groups), so the ack path runs unconditionally.
func TestReply_ResetAckLocalized(t *testing.T) {
	cases := []struct {
		name     string
		langCode string
		want     string
	}{
		{"english_no_creds", "", "Conversation history has been reset."},
		{"traditional_chinese", "zh_TW", "已重設對話紀錄。"},
		{"vietnamese", "vi_VN", "Lịch sử hội thoại đã được đặt lại."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cc := newI18nReplyChannel(t, tc.langCode)
			if !c.handleBotCommand(context.Background(), directEvent("userA", "/reset")) {
				t.Fatal("expected /reset to be handled")
			}
			if got := cc.all(); !strings.Contains(got, tc.want) {
				t.Fatalf("/reset ack (%s): expected %q, got %q", tc.langCode, tc.want, got)
			}
		})
	}
}

// TestReply_AdminUnavailableLocalized proves the admin-handler reply path is also
// localized: with no stores wired, /tasks replies the "team unavailable" message
// in the sender's language.
func TestReply_AdminUnavailableLocalized(t *testing.T) {
	cases := []struct {
		name     string
		langCode string
		want     string
	}{
		{"english_no_creds", "", "Team features are not available."},
		{"traditional_chinese", "zh_TW", "團隊功能無法使用。"},
		{"vietnamese", "vi_VN", "Tính năng nhóm không khả dụng."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cc := newI18nReplyChannel(t, tc.langCode)
			// teamStore left nil → "unavailable" path.
			if !c.handleBotCommand(context.Background(), directEvent("userA", "/tasks")) {
				t.Fatal("expected /tasks to be handled")
			}
			if got := cc.all(); !strings.Contains(got, tc.want) {
				t.Fatalf("/tasks unavailable (%s): expected %q, got %q", tc.langCode, tc.want, got)
			}
		})
	}
}
