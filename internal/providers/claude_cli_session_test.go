package providers

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// TestBuildStreamJSONInput_MimeRouting verifies that buildStreamJSONInput
// picks the correct Anthropic content block type based on MIME:
//   - application/pdf → "document"
//   - image/*         → "image"
//
// Regression guard: earlier versions hardcoded "image" for every block,
// causing PDF passthrough to fail because the Anthropic API rejects
// image blocks with non-image MIME types.
func TestBuildStreamJSONInput_MimeRouting(t *testing.T) {
	cases := []struct {
		name      string
		images    []ImageContent
		wantTypes []string
	}{
		{
			name: "png image → image block",
			images: []ImageContent{
				{MimeType: "image/png", Data: "abc"},
			},
			wantTypes: []string{"image"},
		},
		{
			name: "pdf → document block",
			images: []ImageContent{
				{MimeType: "application/pdf", Data: "abc"},
			},
			wantTypes: []string{"document"},
		},
		{
			name: "mixed png + pdf → image then document",
			images: []ImageContent{
				{MimeType: "image/jpeg", Data: "xxx"},
				{MimeType: "application/pdf", Data: "yyy"},
			},
			wantTypes: []string{"image", "document"},
		},
		{
			name: "unknown MIME falls back to image",
			images: []ImageContent{
				{MimeType: "application/octet-stream", Data: "zzz"},
			},
			wantTypes: []string{"image"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := buildStreamJSONInput("describe", tc.images)
			raw, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("read stdin: %v", err)
			}

			var msg struct {
				Message struct {
					Content []map[string]any `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				t.Fatalf("parse stream-json: %v\nraw: %s", err, raw)
			}

			// Expect N media blocks + 1 trailing text block.
			wantLen := len(tc.wantTypes) + 1
			if got := len(msg.Message.Content); got != wantLen {
				t.Fatalf("content blocks = %d, want %d\nraw: %s", got, wantLen, raw)
			}

			for i, wantType := range tc.wantTypes {
				gotType, _ := msg.Message.Content[i]["type"].(string)
				if gotType != wantType {
					t.Errorf("block[%d].type = %q, want %q", i, gotType, wantType)
				}
				source, _ := msg.Message.Content[i]["source"].(map[string]any)
				if source == nil {
					t.Errorf("block[%d].source is nil", i)
					continue
				}
				if gotMime, _ := source["media_type"].(string); gotMime != tc.images[i].MimeType {
					t.Errorf("block[%d].source.media_type = %q, want %q", i, gotMime, tc.images[i].MimeType)
				}
			}

			// Trailing text block.
			last := msg.Message.Content[wantLen-1]
			if last["type"] != "text" {
				t.Errorf("trailing block type = %v, want text", last["type"])
			}
			if last["text"] != "describe" {
				t.Errorf("trailing block text = %v, want describe", last["text"])
			}
		})
	}
}

// TestBuildStreamJSONInput_NoText covers the edge case where the caller
// passes images with an empty prompt — no text block should be emitted.
func TestBuildStreamJSONInput_NoText(t *testing.T) {
	r := buildStreamJSONInput("", []ImageContent{
		{MimeType: "image/png", Data: "abc"},
	})
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	var msg struct {
		Message struct {
			Content []map[string]any `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("parse stream-json: %v", err)
	}
	if len(msg.Message.Content) != 1 {
		t.Errorf("content blocks = %d, want 1 (image only)", len(msg.Message.Content))
	}
}

// --- extractFromMessages + cold-seed (k-1/k-2) tests ---

func TestExtractFromMessages(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys-prompt"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
		{Role: "user", Content: "latest question"},
	}
	sys, userMsg, _, prior := extractFromMessages(msgs)
	if sys != "sys-prompt" {
		t.Errorf("system = %q, want sys-prompt", sys)
	}
	if userMsg != "latest question" {
		t.Errorf("userMsg = %q, want 'latest question'", userMsg)
	}
	// priorTurns = non-system turns BEFORE the latest user message.
	if len(prior) != 2 {
		t.Fatalf("priorTurns len = %d, want 2 (%+v)", len(prior), prior)
	}
	if prior[0].Role != "user" || prior[0].Content != "hello" {
		t.Errorf("priorTurns[0] = %+v, want user/hello", prior[0])
	}
	if prior[1].Role != "assistant" || prior[1].Content != "hi there" {
		t.Errorf("priorTurns[1] = %+v, want assistant/'hi there'", prior[1])
	}
}

func TestExtractFromMessages_SingleTurn(t *testing.T) {
	sys, userMsg, _, prior := extractFromMessages([]Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "only message"},
	})
	if sys != "s" || userMsg != "only message" {
		t.Errorf("got sys=%q user=%q", sys, userMsg)
	}
	if len(prior) != 0 {
		t.Errorf("priorTurns = %+v, want empty for single-turn", prior)
	}
}

func TestBuildColdSeedPreamble(t *testing.T) {
	if got := buildColdSeedPreamble(nil); got != "" {
		t.Errorf("empty priorTurns: got %q, want empty", got)
	}
	preamble := buildColdSeedPreamble([]Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "second"},
	})
	if preamble == "" {
		t.Fatal("non-empty priorTurns produced empty preamble")
	}
	for _, want := range []string{"User: first", "Assistant: second", "respond to the new message"} {
		if !strings.Contains(preamble, want) {
			t.Errorf("preamble missing %q; got:\n%s", want, preamble)
		}
	}
	if strings.Index(preamble, "first") > strings.Index(preamble, "second") {
		t.Error("preamble must keep turn order (first before second)")
	}
}
