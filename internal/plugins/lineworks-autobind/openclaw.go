package lineworksautobind

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// channelName is the value the openclaw binding stores as the channel
// discriminator (sync.link ext_ref is "<channel>:<channel_uid>"). The agent
// path keys per-user credentials under "lineworks:<uid>", so the binding
// channel must match for a consistent identity namespace.
const channelName = "lineworks"

// bindResponse is the /api/openclaw/user/bind result. On success `code` carries
// the 6-digit verification code (the endpoint returns it to the gateway rather
// than emailing it). On failure `error`/`message` describe why (e.g.
// "mcp_not_enabled", "already_bound", "user_not_found").
type bindResponse struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Error   string `json:"error"`
	Message string `json:"message"`
}

// verifyResponse is the /api/openclaw/user/verify result. On success `api_key`
// is the freshly minted per-user Odoo API key (scope odoo.plugin.mcp) and
// `user` carries the bound res.users identity (id/login/name).
type verifyResponse struct {
	Success bool       `json:"success"`
	APIKey  string     `json:"api_key"`
	User    verifyUser `json:"user"`
	Error   string     `json:"error"`
	Message string     `json:"message"`
}

// verifyUser is the bound Odoo res.users identity returned by verify.
type verifyUser struct {
	ID    int    `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
}

// openclawBind calls /api/openclaw/user/bind and returns the verification code.
// A non-success body becomes an error carrying the server's error code so the
// caller can log actionable causes (mcp_not_enabled / already_bound / ...).
func (h *Hook) openclawBind(ctx context.Context, lwUserID, login string) (string, error) {
	var res bindResponse
	if err := h.postJSON(ctx, "/api/openclaw/user/bind", map[string]any{
		"channel":        channelName,
		"channel_uid":    lwUserID,
		"login_or_email": login,
	}, &res); err != nil {
		return "", err
	}
	if !res.Success || res.Code == "" {
		return "", fmt.Errorf("bind rejected: %s (%s)", emptyOr(res.Error, "no_code"), res.Message)
	}
	return res.Code, nil
}

// openclawVerify calls /api/openclaw/user/verify with the code and returns the
// minted api_key plus the bound Odoo res.users identity.
func (h *Hook) openclawVerify(ctx context.Context, lwUserID, code string) (string, verifyUser, error) {
	var res verifyResponse
	if err := h.postJSON(ctx, "/api/openclaw/user/verify", map[string]any{
		"channel":     channelName,
		"channel_uid": lwUserID,
		"code":        code,
	}, &res); err != nil {
		return "", verifyUser{}, err
	}
	if !res.Success || res.APIKey == "" {
		return "", verifyUser{}, fmt.Errorf("verify rejected: %s (%s)", emptyOr(res.Error, "no_api_key"), res.Message)
	}
	return res.APIKey, res.User, nil
}

// postJSON posts body as JSON to {OdooBaseURL}{path} with the gateway bearer
// token and decodes the response into dst.
func (h *Hook) postJSON(ctx context.Context, path string, body map[string]any, dst any) error {
	if h.cfg.OdooBaseURL == "" || h.cfg.MCPToken == "" {
		return errors.New("lineworks-autobind: OdooBaseURL/MCPToken not configured")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	url := strings.TrimRight(h.cfg.OdooBaseURL, "/") + path
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.cfg.MCPToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

func emptyOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// ----------------------------------------------------------------------------
// MCP transport (JSON-RPC tools/call) — mirrors the lineworks-workflow plugin.
// ----------------------------------------------------------------------------

type mcpRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int            `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *mcpError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

// mcpToolCall posts a JSON-RPC tools/call and decodes structuredContent (or the
// first text content) into dst. Package var so tests can stub it.
var mcpToolCall = func(ctx context.Context, endpoint, token, tool string, args map[string]any, dst any) error {
	if endpoint == "" || token == "" {
		return errors.New("lineworks-autobind: MCP endpoint/token not configured")
	}
	raw, err := json.Marshal(mcpRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("mcp http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mcp http %d", resp.StatusCode)
	}
	var envelope mcpResponse
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("mcp decode: %w", err)
	}
	if envelope.Error != nil {
		return envelope.Error
	}
	var wrapper struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(envelope.Result, &wrapper); err != nil {
		return fmt.Errorf("mcp result envelope: %w", err)
	}
	if len(wrapper.StructuredContent) > 0 && string(wrapper.StructuredContent) != "null" {
		return json.Unmarshal(wrapper.StructuredContent, dst)
	}
	if len(wrapper.Content) > 0 && wrapper.Content[0].Type == "text" {
		return json.Unmarshal([]byte(wrapper.Content[0].Text), dst)
	}
	return errors.New("mcp result has no usable payload")
}
