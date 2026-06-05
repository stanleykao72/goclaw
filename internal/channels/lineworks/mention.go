package lineworks

import "strings"

// SetBotNames installs the set of bot display names used for group @-mention
// gating. Resolved at Start() from GetBot (default botName + every
// i18nBotNames variant). Empty/whitespace names are dropped. When the resulting
// set is empty the channel treats mention gating as DISABLED (fail-safe: the
// bot never goes silent in groups) — see handleMessageEvent.
//
// Guarded by botNamesMu: it is written here (once at Start, possibly from the
// Reload startup goroutine) and read concurrently by webhook handler goroutines
// via botNamesSnapshot / botNamesResolved / mentionsBot.
func (c *Channel) SetBotNames(names []string) {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		key := strings.ToLower(n)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, n)
	}
	c.botNamesMu.Lock()
	c.botNames = out
	c.botNamesMu.Unlock()
}

// botNamesSnapshot returns the current bot-name set under a read lock. The
// returned slice is never mutated in place (SetBotNames replaces it wholesale),
// so callers may range over it safely.
func (c *Channel) botNamesSnapshot() []string {
	c.botNamesMu.RLock()
	defer c.botNamesMu.RUnlock()
	return c.botNames
}

// botNamesResolved reports whether at least one bot name is known. When false,
// mention gating is disabled (fail-safe) so group messages are processed
// normally instead of being silently dropped.
func (c *Channel) botNamesResolved() bool {
	c.botNamesMu.RLock()
	defer c.botNamesMu.RUnlock()
	return len(c.botNames) > 0
}

// mentionsBot reports whether text contains an "@<botName>" for any resolved
// bot name (case-insensitive), bounded so a short ASCII name does not match
// inside a longer word (e.g. name "bot" must not match "@robotics"). Returns
// false when no names are resolved — the caller treats an unresolved set as
// "gating disabled" rather than "no match".
func (c *Channel) mentionsBot(text string) bool {
	names := c.botNamesSnapshot()
	if len(names) == 0 {
		return false
	}
	lower := strings.ToLower(text)
	for _, name := range names {
		token := "@" + strings.ToLower(strings.TrimSpace(name))
		if token == "@" {
			continue
		}
		// Scan every occurrence; accept the first one followed by a mention
		// boundary so "@bot" matches "@bot ..." / "@bot，" / end-of-string but
		// not "@robotics".
		from := 0
		for {
			idx := strings.Index(lower[from:], token)
			if idx < 0 {
				break
			}
			end := from + idx + len(token)
			if end >= len(lower) || isMentionBoundaryByte(lower[end]) {
				return true
			}
			from = from + idx + 1
		}
	}
	return false
}

// isMentionBoundaryByte reports whether b terminates a bot-name mention. ASCII
// letters/digits, '_' and '-' continue a name (so "@bot" does not match inside
// "@bot-jp" or "@robotics"); any other byte — whitespace, punctuation, or the
// leading byte of a multibyte (e.g. CJK) rune — is a boundary, so a CJK name
// like "@小幫手" is still recognized when followed immediately by CJK text.
func isMentionBoundaryByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z':
		return false
	case b >= 'A' && b <= 'Z':
		return false
	case b >= '0' && b <= '9':
		return false
	case b == '_' || b == '-':
		return false
	default:
		return true
	}
}

// stripBotMention removes a single leading "@<botName>" (any resolved variant)
// plus the whitespace that follows it, returning the remainder. Only a leading
// mention is stripped; mentions elsewhere in the text are left intact. The
// operation is idempotent for text that has no leading mention, and matching is
// case-insensitive (the original casing of the remainder is preserved).
func stripBotMention(text string, botNames []string) string {
	trimmed := strings.TrimLeft(text, " \t")
	lower := strings.ToLower(trimmed)
	// Prefer the longest matching name so "@bot-jp" is not partially stripped
	// when "@bot" is also a registered variant.
	bestLen := -1
	for _, name := range botNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		token := "@" + strings.ToLower(name)
		if strings.HasPrefix(lower, token) && len(token) > bestLen {
			bestLen = len(token)
		}
	}
	if bestLen < 0 {
		return text
	}
	rest := trimmed[bestLen:]
	return strings.TrimLeft(rest, " \t")
}
