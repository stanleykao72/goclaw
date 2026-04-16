package line

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// IDTokenClaims holds the subset of LINE ID token payload fields we need.
// Reference: https://developers.line.biz/en/docs/line-login/verify-id-token/
type IDTokenClaims struct {
	Sub  string `json:"sub"`
	Aud  string `json:"aud"`
	Iss  string `json:"iss"`
	Exp  int64  `json:"exp"`
	Iat  int64  `json:"iat"`
	Name string `json:"name"`
}

var (
	ErrIDTokenMalformed    = errors.New("line: id token malformed")
	ErrIDTokenSignature    = errors.New("line: id token signature invalid")
	ErrIDTokenExpired      = errors.New("line: id token expired")
	ErrIDTokenUnsupported  = errors.New("line: id token alg not supported")
)

// VerifyIDToken validates a LINE LIFF-issued ID token against the channel
// secret using HS256 and returns the decoded claims.
//
// The verification intentionally stays local (HMAC) rather than calling the
// LINE verify endpoint, so the LIFF submit path has no outbound dependency.
func (c *Channel) VerifyIDToken(idToken string) (*IDTokenClaims, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil, ErrIDTokenMalformed
	}
	headerRaw, payloadRaw, sigRaw := parts[0], parts[1], parts[2]

	headerBytes, err := base64.RawURLEncoding.DecodeString(headerRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: header b64: %v", ErrIDTokenMalformed, err)
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("%w: header json: %v", ErrIDTokenMalformed, err)
	}
	if header.Alg != "HS256" {
		return nil, fmt.Errorf("%w: %s", ErrIDTokenUnsupported, header.Alg)
	}

	sig, err := base64.RawURLEncoding.DecodeString(sigRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: sig b64: %v", ErrIDTokenMalformed, err)
	}
	mac := hmac.New(sha256.New, []byte(c.cfg.ChannelSecret))
	mac.Write([]byte(headerRaw + "." + payloadRaw))
	expected := mac.Sum(nil)
	if subtle.ConstantTimeCompare(sig, expected) != 1 {
		return nil, ErrIDTokenSignature
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(payloadRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: payload b64: %v", ErrIDTokenMalformed, err)
	}
	var claims IDTokenClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("%w: payload json: %v", ErrIDTokenMalformed, err)
	}
	if claims.Exp > 0 && time.Now().Unix() >= claims.Exp {
		return nil, ErrIDTokenExpired
	}
	return &claims, nil
}
