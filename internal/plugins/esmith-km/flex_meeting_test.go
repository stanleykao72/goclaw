package esmithkm

import (
	"strings"
	"testing"
)

// TestParseBaseURL_ExtractsSchemeAndHost covers the helper used by
// buildOdooDeepLink. The previous TrimSuffix-based implementation failed
// when the MCP URL ended with `/mcp/v1/message` (stage35's real endpoint
// path), producing 404-yielding deep links. This test locks in the
// scheme+host extraction regardless of path suffix.
func TestParseBaseURL_ExtractsSchemeAndHost(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "stage35 MCP with /mcp/v1/message suffix",
			in:   "https://odoo-esmith-v18-stage35-29554478.dev.odoo.com/mcp/v1/message",
			want: "https://odoo-esmith-v18-stage35-29554478.dev.odoo.com",
		},
		{
			name: "trailing /mcp/v1",
			in:   "https://example.com/mcp/v1",
			want: "https://example.com",
		},
		{
			name: "trailing /mcp/v1/",
			in:   "https://example.com/mcp/v1/",
			want: "https://example.com",
		},
		{
			name: "bare host",
			in:   "https://example.com",
			want: "https://example.com",
		},
		{
			name: "with port",
			in:   "http://localhost:8069/mcp/v1/message",
			want: "http://localhost:8069",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "invalid URL (no scheme)",
			in:   "not-a-url",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseBaseURL(tc.in); got != tc.want {
				t.Errorf("parseBaseURL(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildOdooDeepLink_ProducesNonMCPURL locks in the fix for the
// 2026-04-08 dual-review finding: deep links must NOT contain the
// `/mcp/v1/message` path segment from the MCP endpoint. They should be
// of the form `{scheme}://{host}/odoo/action-{xml_id}/{id}`.
func TestBuildOdooDeepLink_ProducesNonMCPURL(t *testing.T) {
	h := &Hook{
		cfg: Config{
			MCPURL: "https://odoo-esmith-v18-stage35-29554478.dev.odoo.com/mcp/v1/message",
		},
	}
	got := h.buildOdooDeepLink("job.meeting.minutes", 494)

	if strings.Contains(got, "/mcp/v1") {
		t.Errorf("deep link must not contain /mcp/v1, got %q", got)
	}
	if strings.Contains(got, "/message") {
		t.Errorf("deep link must not contain /message, got %q", got)
	}
	want := "https://odoo-esmith-v18-stage35-29554478.dev.odoo.com/odoo/action-job_working_plan.action_job_meeting_minutes/494"
	if got != want {
		t.Errorf("deep link = %q\n  want = %q", got, want)
	}
}

// TestBuildOdooDeepLink_PrefersExplicitOdooBaseURL ensures that if a
// deployment has ODOO_STAGE35_BASE_URL set explicitly via Config, that
// wins over the MCPURL-derived base.
func TestBuildOdooDeepLink_PrefersExplicitOdooBaseURL(t *testing.T) {
	h := &Hook{
		cfg: Config{
			MCPURL:      "https://wrong.example.com/mcp/v1/message",
			OdooBaseURL: "https://right.example.com",
		},
	}
	got := h.buildOdooDeepLink("job.meeting.minutes", 42)
	if !strings.HasPrefix(got, "https://right.example.com/") {
		t.Errorf("deep link should use OdooBaseURL, got %q", got)
	}
}

// TestBuildOdooDeepLink_EmptyWhenNeitherSet ensures we return "" rather
// than a malformed URL when no base is configured.
func TestBuildOdooDeepLink_EmptyWhenNeitherSet(t *testing.T) {
	h := &Hook{cfg: Config{}}
	if got := h.buildOdooDeepLink("job.meeting.minutes", 1); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}
