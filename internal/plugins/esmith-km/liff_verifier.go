package esmithkm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/channels/line"
)

// LINELiffVerifier verifies LIFF-issued ID tokens by calling LINE's online
// verify endpoint.
//
// Why not local HMAC: LIFF tokens are signed with the LINE Login Channel's
// secret, which is a different channel from the messaging channel that hosts
// the goclaw bot. Our cfg.ChannelSecret can only verify tokens issued by the
// messaging channel. Rather than surface a second secret via env vars, the
// online verify endpoint accepts just client_id (public — the LIFF ID prefix)
// and handles signature + expiry + audience checks server-side.
//
// The call is rate-limited at LINE's side but tolerated for our volume
// (attendees bubbles are one-verify-per-submit, not a per-message hot path).
type LINELiffVerifier struct {
	ClientID string        // LINE Login channel id (the part of LIFF ID before `-`)
	HTTP     *http.Client  // nil → http.DefaultClient
	Endpoint string        // override for tests; default LINE production
	Timeout  time.Duration // per-call timeout; zero → 5s
}

// NewLINELiffVerifier builds a verifier bound to the given LIFF client id.
// Empty clientID is invalid — the caller (cmd wiring) should short-circuit.
func NewLINELiffVerifier(clientID string) *LINELiffVerifier {
	return &LINELiffVerifier{
		ClientID: clientID,
		Endpoint: "https://api.line.me/oauth2/v2.1/verify",
		Timeout:  5 * time.Second,
	}
}

// VerifyIDToken posts the token + client_id to LINE's verify endpoint and
// translates the response into IDTokenClaims. Satisfies the IDTokenVerifier
// interface already consumed by LiffHandler.
func (v *LINELiffVerifier) VerifyIDToken(idToken string) (*line.IDTokenClaims, error) {
	if v.ClientID == "" {
		return nil, fmt.Errorf("liff verifier: client id not configured")
	}
	client := v.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	timeout := v.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	form := url.Values{
		"id_token":  {idToken},
		"client_id": {v.ClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.Endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("liff verifier: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("liff verifier: post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var body struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		desc := body.Description
		if desc == "" {
			desc = body.Error
		}
		return nil, fmt.Errorf("liff verifier: http %d: %s", resp.StatusCode, desc)
	}

	var payload struct {
		Sub  string `json:"sub"`
		Aud  string `json:"aud"`
		Iss  string `json:"iss"`
		Exp  int64  `json:"exp"`
		Iat  int64  `json:"iat"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("liff verifier: decode: %w", err)
	}
	return &line.IDTokenClaims{
		Sub:  payload.Sub,
		Aud:  payload.Aud,
		Iss:  payload.Iss,
		Exp:  payload.Exp,
		Iat:  payload.Iat,
		Name: payload.Name,
	}, nil
}
