package nlmdoc

import (
	"strings"
)

// idPrefixLen is how many leading characters of a scope id are kept in a Doc
// name. Long ids (lineworks user ids, chat ids) are truncated for readability;
// the pointer table's UNIQUE(tenant,kind,scope_id) — not the Doc name — is the
// real key, so a truncated, possibly-colliding name is harmless.
const idPrefixLen = 8

// DocName builds the Drive Doc display name for a scope:
//
//	"<scopeKind>-<id前8碼>-<displayName>"   (when displayName != "")
//	"<scopeKind>-<id前8碼>"                 (when displayName == "")
//	"<scopeKind>"                           (when scopeID == "", e.g. shared)
//
// displayName is best-effort (the caller resolves a Chinese name later; "" is
// valid → id-only / kind-only name). Unsafe path characters are sanitized so
// the name is safe as a Drive file name and as a path segment.
func DocName(scopeKind, scopeID, displayName string) string {
	kind := SanitizePathSegment(scopeKind)

	parts := []string{kind}
	if id := strings.TrimSpace(scopeID); id != "" {
		parts = append(parts, idPrefix(SanitizePathSegment(id)))
	}
	if dn := SanitizePathSegment(strings.TrimSpace(displayName)); dn != "" {
		parts = append(parts, dn)
	}
	return strings.Join(parts, "-")
}

// idPrefix returns the first idPrefixLen runes of s (rune-safe so multibyte
// ids are not split mid-character), trimming a trailing separator left by the
// cut so names like "agent-e-smith-hub" → "agent-e-smith" (not "e-smith-").
func idPrefix(s string) string {
	r := []rune(s)
	if len(r) <= idPrefixLen {
		return s
	}
	return strings.TrimRight(string(r[:idPrefixLen]), "-_")
}

// SanitizePathSegment replaces characters that are unsafe in Drive file names
// or path segments with '_'. It preserves Unicode letters/digits (so Chinese
// display names survive) and a small safe punctuation set, collapses runs of
// '_' , and trims leading/trailing separators. An all-unsafe input returns "".
func SanitizePathSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|':
			b.WriteByte('_')
		case r == 0:
			// drop NUL
		case r < 0x20 || r == 0x7f:
			// drop other control characters
		default:
			b.WriteRune(r)
		}
	}
	out := collapseUnderscores(b.String())
	return strings.Trim(out, "_")
}

// collapseUnderscores reduces runs of '_' to a single '_'.
func collapseUnderscores(s string) string {
	if !strings.Contains(s, "__") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prevUnderscore := false
	for _, r := range s {
		if r == '_' {
			if prevUnderscore {
				continue
			}
			prevUnderscore = true
		} else {
			prevUnderscore = false
		}
		b.WriteRune(r)
	}
	return b.String()
}
