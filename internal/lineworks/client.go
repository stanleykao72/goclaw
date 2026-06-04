package lineworks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// apiBaseURL is the LINE WORKS Bot/Directory REST base.
	apiBaseURL = "https://www.worksapis.com/v1.0"

	// SignatureHeader is the callback HMAC header LINE WORKS sends.
	SignatureHeader = "X-WORKS-Signature"

	// maxTextLength is the platform cap on a single text message body.
	maxTextLength = 2000
)

// ErrUnauthorized wraps a REST response with HTTP 401 or 403, signalling an
// expired/invalid token or insufficient scope. Callers (e.g. the channel
// adapter) use errors.Is to decide whether to force a token refresh.
var ErrUnauthorized = errors.New("lineworks: unauthorized")

// Client is a LINE WORKS Bot REST client. It wraps a TokenSource for
// authentication and exposes the message-send and directory-lookup
// operations the channel adapter needs. Safe for concurrent use.
type Client struct {
	botID      string
	tokens     *TokenSource
	httpClient *http.Client
}

// ClientConfig configures a Client.
type ClientConfig struct {
	// BotID is the bot's numeric ID, used in /bots/{botId}/... paths.
	BotID string
	// Tokens supplies bearer access tokens for Authorization headers.
	Tokens *TokenSource
	// HTTPClient is optional; a 15s-timeout default is used when nil.
	HTTPClient *http.Client
}

// NewClient builds a Client. BotID and Tokens are required.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.BotID == "" {
		return nil, errors.New("lineworks: ClientConfig.BotID is required")
	}
	if cfg.Tokens == nil {
		return nil, errors.New("lineworks: ClientConfig.Tokens is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{
		botID:      cfg.BotID,
		tokens:     cfg.Tokens,
		httpClient: hc,
	}, nil
}

// textMessage is the request body for a text send.
type textMessage struct {
	Content textContent `json:"content"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// SendTextToUser sends a text message to a user in a 1:1 conversation via
// POST /bots/{botId}/users/{userId}/messages. Text longer than the platform
// cap is truncated to maxTextLength runes.
func (c *Client) SendTextToUser(ctx context.Context, userID, text string) error {
	if userID == "" {
		return errors.New("lineworks: SendTextToUser userID is empty")
	}
	path := fmt.Sprintf("/bots/%s/users/%s/messages", c.botID, userID)
	return c.sendText(ctx, path, text)
}

// SendTextToChannel sends a text message to a room/group via
// POST /bots/{botId}/channels/{channelId}/messages.
func (c *Client) SendTextToChannel(ctx context.Context, channelID, text string) error {
	if channelID == "" {
		return errors.New("lineworks: SendTextToChannel channelID is empty")
	}
	path := fmt.Sprintf("/bots/%s/channels/%s/messages", c.botID, channelID)
	return c.sendText(ctx, path, text)
}

// sendText marshals a text body and POSTs it to the given path. The platform
// returns 201 Created with no body on success.
func (c *Client) sendText(ctx context.Context, path, text string) error {
	if text == "" {
		return nil
	}
	if r := []rune(text); len(r) > maxTextLength {
		text = string(r[:maxTextLength])
	}
	body, err := json.Marshal(textMessage{Content: textContent{Type: "text", Text: text}})
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPost, path, body)
	return err
}

// User is the subset of a LINE WORKS directory user we consume.
//
// Email is the LINE WORKS account id ("<local>@<domain>", internal — rarely
// matches an external system's login). PrivateEmail is the directory's
// "個人電子郵件地址" field, which e-smith populates with the person's real
// login email — it equals the Odoo res.users login for both synced and
// pre-existing accounts, making it the reliable cross-system identity key.
type User struct {
	UserID          string `json:"userId"`
	UserExternalKey string `json:"userExternalKey"`
	Email           string `json:"email"`
	PrivateEmail    string `json:"privateEmail"`
	UserName        struct {
		LastName  string `json:"lastName"`
		FirstName string `json:"firstName"`
	} `json:"userName"`
}

// DisplayName returns the LINE WORKS display name ("lastName"+"firstName",
// e.g. 高玉明). Empty when the directory has no name set.
func (u *User) DisplayName() string {
	return strings.TrimSpace(u.UserName.LastName + u.UserName.FirstName)
}

// GetUser looks up a directory user via GET /users/{userId}. The userId may be
// a resource ID or the "externalKey:{externalKey}" form. Requires the
// directory.read (or user.read) scope on the access token.
func (c *Client) GetUser(ctx context.Context, userID string) (*User, error) {
	if userID == "" {
		return nil, errors.New("lineworks: GetUser userID is empty")
	}
	path := fmt.Sprintf("/users/%s", userID)
	respBody, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var u User
	if err := json.Unmarshal(respBody, &u); err != nil {
		return nil, fmt.Errorf("lineworks: decode user: %w", err)
	}
	return &u, nil
}

// do performs an authenticated request against the API base and returns the
// response body. Non-2xx responses become errors carrying the status and a
// snippet of the body.
func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}

	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBaseURL+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lineworks: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(respBody)
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("%w: %s %s HTTP %d: %s", ErrUnauthorized, method, path, resp.StatusCode, snippet)
		}
		return nil, fmt.Errorf("lineworks: %s %s returned HTTP %d: %s", method, path, resp.StatusCode, snippet)
	}
	return respBody, nil
}

// VerifySignature reports whether the X-WORKS-Signature header value matches
// the HMAC-SHA256 of the raw request body keyed by the bot secret.
//
// The signature is standard (not URL-safe) base64. Comparison is
// constant-time. The bot secret is the bot's own secret, NOT the OAuth
// client_secret. rawBody must be the exact bytes received (read it before
// any JSON decoding, since re-marshaling would change the bytes).
func VerifySignature(botSecret string, rawBody []byte, signatureHeader string) bool {
	if botSecret == "" || signatureHeader == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(botSecret))
	mac.Write(rawBody)
	expected := mac.Sum(nil)

	got, err := base64.StdEncoding.DecodeString(signatureHeader)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, expected) == 1
}
