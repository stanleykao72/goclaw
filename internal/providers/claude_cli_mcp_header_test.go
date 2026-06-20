package providers

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func isLatin1Safe(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// TestBridgeHeaderValueRoundTrip: ASCII passes through verbatim; non-ASCII is
// encoded to a latin-1-safe form and decodes back exactly.
func TestBridgeHeaderValueRoundTrip(t *testing.T) {
	cases := []string{
		"019d47d2-e59c-773f-9ae2-2023c7a6d0a2",            // UUID
		"lineworks:2e56b7af-f9c5-463b-1cfd-04440ec2d00d",  // ascii with colon
		"lineworks-主管群",                                  // CJK (the bug)
		"agent:e-smith-hub:lineworks-主管群:direct:abc",     // CJK + colons
		"/home/ubuntu/.goclaw/workspace/hub/lineworks-主管群/x", // CJK path
		"with\r\nctrl",                                    // control chars
		"",                                                // empty
		"Tiếng Việt",                                      // Vietnamese diacritics
	}
	for _, raw := range cases {
		enc := EncodeBridgeHeaderValue(raw)
		if !isLatin1Safe(enc) {
			t.Fatalf("encoded value not latin-1 safe for %q -> %q", raw, enc)
		}
		if got := DecodeBridgeHeaderValue(enc); got != raw {
			t.Fatalf("round-trip mismatch: raw=%q enc=%q decoded=%q", raw, enc, got)
		}
	}
	// Pure ASCII must be emitted verbatim (no behavior change / no HMAC impact).
	if EncodeBridgeHeaderValue("plain-ascii_1.2~3") != "plain-ascii_1.2~3" {
		t.Fatal("pure ASCII must be verbatim")
	}
	// A non-sentinel value decodes to itself (legacy headers).
	if DecodeBridgeHeaderValue("lineworks-foo") != "lineworks-foo" {
		t.Fatal("non-sentinel decode must be identity")
	}
}

// TestWriteMCPConfig_CJKHeadersLatin1SafeAndVerifiable is the regression test
// for the production incident: a CJK group name ("主管群") in channel / workspace
// / session key must NOT produce non-latin-1 header bytes (which make the
// Node-based claude CLI fail to connect to goclaw-bridge), AND the HMAC must
// still verify after the encode→decode round-trip.
func TestWriteMCPConfig_CJKHeadersLatin1SafeAndVerifiable(t *testing.T) {
	t.Setenv("GOCLAW_DATA_DIR", t.TempDir())
	d := &MCPConfigData{GatewayAddr: "127.0.0.1:9", GatewayToken: "tok-secret"}
	bc := BridgeContext{
		AgentID:     "019d47d2-e59c-773f-9ae2-2023c7a6d0a2",
		UserID:      "lineworks:2e56b7af",
		Channel:     "lineworks-主管群",
		ChatID:      "2e56b7af-f9c5",
		PeerKind:    "direct",
		Workspace:   "/home/ubuntu/.goclaw/workspace/hub/lineworks-主管群/x",
		TenantID:    "0193a5b0-7000-7000-8000-000000000001",
		SenderID:    "lineworks:2e56b7af",
		ChannelType: "lineworks",
	}
	path := d.WriteMCPConfig(context.Background(), "agent:e-smith-hub:lineworks-主管群:direct:2e56b7af", bc)
	if path == "" {
		t.Fatal("WriteMCPConfig returned empty path")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var parsed struct {
		MCPServers map[string]struct {
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	br, ok := parsed.MCPServers["goclaw-bridge"]
	if !ok {
		t.Fatal("no goclaw-bridge entry")
	}
	// EVERY emitted header value must be latin-1 safe (the core fix).
	for k, v := range br.Headers {
		if !isLatin1Safe(v) {
			t.Fatalf("header %s carries non-latin-1 value %q (would break the Node CLI)", k, v)
		}
	}
	// And the HMAC must verify after decoding the transport-encoded headers.
	dec := func(h string) string { return DecodeBridgeHeaderValue(br.Headers[h]) }
	ok2, tenantVerified, senderVerified := VerifyBridgeContext(
		"tok-secret",
		dec("X-Agent-ID"), dec("X-User-ID"), dec("X-Channel"), dec("X-Chat-ID"),
		dec("X-Peer-Kind"), dec("X-Workspace"), dec("X-Tenant-ID"), br.Headers["X-Bridge-Sig"],
		dec("X-Local-Key"), dec("X-Session-Key"), dec("X-Channel-Type"), dec("X-Sender-ID"),
	)
	if !ok2 {
		t.Fatal("HMAC must verify after encode→decode round-trip")
	}
	if !tenantVerified {
		t.Fatal("tenant should verify (full tier)")
	}
	if !senderVerified {
		t.Fatal("sender should verify (full tier)")
	}
	// Sanity: the CJK headers were actually encoded (not passed through raw).
	if br.Headers["X-Channel"] == bc.Channel {
		t.Fatal("X-Channel should have been encoded (was raw CJK)")
	}
	if dec("X-Channel") != bc.Channel {
		t.Fatalf("X-Channel must decode to raw: got %q", dec("X-Channel"))
	}
}
