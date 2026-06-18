package providers

import "testing"

// wantUsage is the expected token mapping for a parse test case.
type wantUsage struct {
	prompt, completion, total, cacheCreate, cacheRead int
}

func assertUsage(t *testing.T, got *Usage, want wantUsage) {
	t.Helper()
	if got == nil {
		t.Fatal("usage is nil")
	}
	if got.PromptTokens != want.prompt {
		t.Errorf("PromptTokens = %d, want %d", got.PromptTokens, want.prompt)
	}
	if got.CompletionTokens != want.completion {
		t.Errorf("CompletionTokens = %d, want %d", got.CompletionTokens, want.completion)
	}
	if got.TotalTokens != want.total {
		t.Errorf("TotalTokens = %d, want %d", got.TotalTokens, want.total)
	}
	if got.CacheCreationTokens != want.cacheCreate {
		t.Errorf("CacheCreationTokens = %d, want %d", got.CacheCreationTokens, want.cacheCreate)
	}
	if got.CacheReadTokens != want.cacheRead {
		t.Errorf("CacheReadTokens = %d, want %d", got.CacheReadTokens, want.cacheRead)
	}
}

func TestParseJSONArray_Usage(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		content string
		finish  string
		usage   *wantUsage
	}{
		{
			name: "cache tokens mapped",
			data: `[
				{"type":"assistant","message":{"content":[{"type":"text","text":"partial"}]}},
				{"type":"result","subtype":"success","result":"hello","usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":50,"cache_read_input_tokens":200}}
			]`,
			content: "hello",
			finish:  "stop",
			usage:   &wantUsage{prompt: 100, completion: 20, total: 120, cacheCreate: 50, cacheRead: 200},
		},
		{
			name: "no cache fields (backward compat) defaults to zero",
			data: `[
				{"type":"result","subtype":"success","result":"ok","usage":{"input_tokens":10,"output_tokens":5}}
			]`,
			content: "ok",
			finish:  "stop",
			usage:   &wantUsage{prompt: 10, completion: 5, total: 15, cacheCreate: 0, cacheRead: 0},
		},
		{
			name: "error subtype yields error finish reason",
			data: `[
				{"type":"result","subtype":"error","result":"boom","usage":{"input_tokens":3,"output_tokens":0,"cache_read_input_tokens":7}}
			]`,
			content: "boom",
			finish:  "error",
			usage:   &wantUsage{prompt: 3, completion: 0, total: 3, cacheCreate: 0, cacheRead: 7},
		},
		{
			name: "assistant text fallback when result empty",
			data: `[
				{"type":"assistant","message":{"content":[{"type":"text","text":"from-blocks"}]}},
				{"type":"result","subtype":"success","result":""}
			]`,
			content: "from-blocks",
			finish:  "stop",
			usage:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := parseJSONArray([]byte(tc.data))
			if resp == nil {
				t.Fatal("parseJSONArray returned nil")
			}
			if resp.Content != tc.content {
				t.Errorf("Content = %q, want %q", resp.Content, tc.content)
			}
			if resp.FinishReason != tc.finish {
				t.Errorf("FinishReason = %q, want %q", resp.FinishReason, tc.finish)
			}
			if tc.usage == nil {
				if resp.Usage != nil {
					t.Errorf("Usage = %+v, want nil", resp.Usage)
				}
				return
			}
			assertUsage(t, resp.Usage, *tc.usage)
		})
	}
}

func TestParseSingleJSONResult_Usage(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		content string
		finish  string
		usage   wantUsage
	}{
		{
			name:    "cache tokens mapped",
			line:    `{"type":"result","subtype":"success","result":"answer","usage":{"input_tokens":80,"output_tokens":12,"cache_creation_input_tokens":40,"cache_read_input_tokens":160}}`,
			content: "answer",
			finish:  "stop",
			usage:   wantUsage{prompt: 80, completion: 12, total: 92, cacheCreate: 40, cacheRead: 160},
		},
		{
			name:    "no cache fields defaults to zero",
			line:    `{"type":"result","subtype":"success","result":"answer","usage":{"input_tokens":80,"output_tokens":12}}`,
			content: "answer",
			finish:  "stop",
			usage:   wantUsage{prompt: 80, completion: 12, total: 92, cacheCreate: 0, cacheRead: 0},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := parseSingleJSONResult([]byte(tc.line))
			if resp == nil {
				t.Fatal("parseSingleJSONResult returned nil")
			}
			if resp.Content != tc.content {
				t.Errorf("Content = %q, want %q", resp.Content, tc.content)
			}
			if resp.FinishReason != tc.finish {
				t.Errorf("FinishReason = %q, want %q", resp.FinishReason, tc.finish)
			}
			assertUsage(t, resp.Usage, tc.usage)
		})
	}
}
