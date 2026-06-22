package nlmingest

import (
	"regexp"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// senderPrefix matches the lineworks sender_id namespace ("lineworks:<uid>").
// Kept local (the channel package owns the authoritative constant) so the worker
// can strip it for DM classification without importing the channel package.
const senderPrefix = "lineworks:"

// classifyScope derives the WRITE scope for a buffered key+window from VERIFIED
// row fields ONLY — never from message body (FINAL spec §10 isolation rule).
//
// A LINE WORKS 1:1 message is buffered with history_key == userId and
// sender_id == "lineworks:<userId>" (see the channel's
// recordDirectMessageForIngest), so it classifies as a USER scope precisely when
// the key equals the prefix-stripped sender id of its messages. A group message
// is buffered with history_key == chatId (a LINE WORKS channelId, never equal to
// a sender's userId), so it classifies as a GROUP scope.
//
// The DM test requires EVERY message in the window to be from the keyed user
// (all senders == lineworks:<historyKey>); a group room never satisfies this
// (multiple senders, none equal to the room id). This makes the heuristic robust
// against the lone documented ambiguity (a key that coincidentally equals a
// sender id) — a real group would have to be a single-sender room whose id also
// equals that sender's user id, which the LINE WORKS id space does not produce.
// ok=false when the window is empty (nothing to scope).
func classifyScope(historyKey string, msgs []store.PendingMessage) (scopeKind, scopeID string, ok bool) {
	if historyKey == "" || len(msgs) == 0 {
		return "", "", false
	}
	if isDirectWindow(historyKey, msgs) {
		return store.ScopeKindUser, historyKey, true
	}
	return store.ScopeKindGroup, historyKey, true
}

// isDirectWindow reports whether the whole window is a 1:1 conversation keyed by
// the sender's own user id (history_key == every message's prefix-stripped
// sender_id).
func isDirectWindow(historyKey string, msgs []store.PendingMessage) bool {
	for _, m := range msgs {
		if strings.TrimPrefix(m.SenderID, senderPrefix) != historyKey {
			return false
		}
	}
	return true
}

// buildBatchBlob renders ONE append blob for a drained window: a sender +
// timestamp + body line per message, with obvious leaked secrets redacted. The
// blob ends with a trailing newline (the Drive AppendText caller contract: it
// concatenates verbatim and adds no separator), so successive drains stay line-
// separated in the Doc.
func buildBatchBlob(msgs []store.PendingMessage) string {
	var sb strings.Builder
	for _, m := range msgs {
		sender := m.Sender
		if sender == "" {
			sender = strings.TrimPrefix(m.SenderID, senderPrefix)
		}
		ts := ""
		if !m.CreatedAt.IsZero() {
			ts = " [" + m.CreatedAt.UTC().Format("2006-01-02 15:04") + "]"
		}
		body := redactSecrets(strings.TrimSpace(m.Body))
		if body == "" {
			continue
		}
		sb.WriteString(sender)
		sb.WriteString(ts)
		sb.WriteString(": ")
		sb.WriteString(body)
		sb.WriteString("\n")
	}
	return sb.String()
}

// secretPatterns redacts only HIGH-CONFIDENCE leaked credentials. It deliberately
// does NOT touch phone/email/payment (legitimate personal-memory content for a
// user Doc — over-redacting would gut the value of the memory). Curation tools
// (sub-phase 2.5) may apply heavier PII redaction before writing shared/agent
// Docs; raw per-user/group ingest keeps the conversation intact bar obvious keys.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(api[_-]?key|token|secret|password|passwd|pwd)\b\s*[:=]\s*['"]?[^'"\s]+`),
	regexp.MustCompile(`(?i)\b(sk-[a-z0-9_-]{12,}|ghp_[a-z0-9_]+|xox[baprs]-[a-z0-9-]+)\b`),
	regexp.MustCompile(`(?i)\b(postgres|postgresql|mysql|redis|mongodb)://[^\s]+`),
}

// redactSecrets replaces obvious leaked credentials with a [REDACTED] marker.
func redactSecrets(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}
