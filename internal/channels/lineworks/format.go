package lineworks

import "strings"

// splitMessage splits text into chunks of at most maxLen bytes, preferring to
// break at newline boundaries. LINE WORKS text messages cap at 2000 chars
// (maxTextLength); longer agent replies are chunked into multiple sends.
func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}

	var parts []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			parts = append(parts, text)
			break
		}
		// Try to split at the last newline within maxLen so we don't cut a
		// line mid-sentence. Fall back to a hard cut at maxLen.
		cut := maxLen
		if idx := strings.LastIndex(text[:maxLen], "\n"); idx > 0 {
			cut = idx + 1
		}
		parts = append(parts, text[:cut])
		text = text[cut:]
	}
	return parts
}

// formatForLineWorks strips markdown markers that LINE WORKS text messages do
// not render. Mirrors line/format.go formatForLINE: LINE WORKS plain-text
// content has no markdown support, so bold/inline-code markers would otherwise
// leak through as literal asterisks/backticks.
func formatForLineWorks(text string) string {
	// Remove bold markers: **text** → text
	text = strings.ReplaceAll(text, "**", "")
	// Remove inline code markers: `text` → text
	text = strings.ReplaceAll(text, "`", "")
	return text
}
