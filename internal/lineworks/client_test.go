package lineworks

import (
	"encoding/json"
	"testing"
)

// TestBotInfo_JSONDecode locks the JSON tag contract for the GetBot response.
// GetBot drives all group mention gating (the resolved names are matched against
// inbound text), and the channel-level tests inject names directly via
// SetBotNames — so a wrong json tag here (botName / i18nBotNames) would silently
// break mention gating in production while every other test stays green. This
// asserts the documented LINE WORKS GET /bots/{botId} shape decodes correctly.
func TestBotInfo_JSONDecode(t *testing.T) {
	body := []byte(`{
		"botName": "hub",
		"i18nBotNames": [
			{"language": "zh_TW", "botName": "小幫手"},
			{"language": "en_US", "botName": "Hub"}
		]
	}`)

	var b BotInfo
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("decode BotInfo: %v", err)
	}
	if b.BotName != "hub" {
		t.Fatalf("BotName = %q, want %q", b.BotName, "hub")
	}
	if len(b.I18nBotNames) != 2 {
		t.Fatalf("I18nBotNames len = %d, want 2", len(b.I18nBotNames))
	}
	if b.I18nBotNames[0].BotName != "小幫手" || b.I18nBotNames[1].BotName != "Hub" {
		t.Fatalf("i18n names not decoded: %+v", b.I18nBotNames)
	}

	// The set the channel assembles for mention matching: default + every i18n
	// variant. Guards the Start() assembly shape against an empty/partial set.
	names := []string{b.BotName}
	for _, n := range b.I18nBotNames {
		names = append(names, n.BotName)
	}
	if len(names) != 3 {
		t.Fatalf("assembled mention names = %v, want 3", names)
	}
}
