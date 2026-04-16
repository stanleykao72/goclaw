package line

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/config"
)

const testSecret = "s3cr3t-channel-secret-for-tests"

func signTestToken(t *testing.T, claims IDTokenClaims, secret string) string {
	t.Helper()
	header := `{"alg":"HS256","typ":"JWT"}`
	payloadBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	h := base64.RawURLEncoding.EncodeToString([]byte(header))
	p := base64.RawURLEncoding.EncodeToString(payloadBytes)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(h + "." + p))
	s := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return h + "." + p + "." + s
}

func newTestChannel(secret string) *Channel {
	return &Channel{cfg: config.LineConfig{ChannelSecret: secret}}
}

func TestVerifyIDToken(t *testing.T) {
	now := time.Now().Unix()
	validClaims := IDTokenClaims{
		Sub:  "U1234567890abcdef",
		Aud:  "1234567890",
		Iss:  "https://access.line.me",
		Exp:  now + 3600,
		Iat:  now,
		Name: "Test User",
	}

	cases := []struct {
		name    string
		token   func(*testing.T) string
		secret  string
		wantErr error
		check   func(*testing.T, *IDTokenClaims)
	}{
		{
			name:   "Valid returns sub",
			token:  func(t *testing.T) string { return signTestToken(t, validClaims, testSecret) },
			secret: testSecret,
			check: func(t *testing.T, c *IDTokenClaims) {
				if c.Sub != validClaims.Sub {
					t.Errorf("Sub = %q, want %q", c.Sub, validClaims.Sub)
				}
				if c.Name != validClaims.Name {
					t.Errorf("Name = %q, want %q", c.Name, validClaims.Name)
				}
			},
		},
		{
			name: "Tampered signature rejected",
			token: func(t *testing.T) string {
				tok := signTestToken(t, validClaims, testSecret)
				parts := strings.Split(tok, ".")
				// Flip a byte in the signature (still valid base64url).
				if parts[2][0] == 'A' {
					parts[2] = "B" + parts[2][1:]
				} else {
					parts[2] = "A" + parts[2][1:]
				}
				return strings.Join(parts, ".")
			},
			secret:  testSecret,
			wantErr: ErrIDTokenSignature,
		},
		{
			name: "Expired rejected",
			token: func(t *testing.T) string {
				expired := validClaims
				expired.Exp = now - 60
				return signTestToken(t, expired, testSecret)
			},
			secret:  testSecret,
			wantErr: ErrIDTokenExpired,
		},
		{
			name:    "Malformed rejected",
			token:   func(t *testing.T) string { return "not-a-jwt" },
			secret:  testSecret,
			wantErr: ErrIDTokenMalformed,
		},
		{
			name:    "Wrong secret rejected",
			token:   func(t *testing.T) string { return signTestToken(t, validClaims, "different-secret") },
			secret:  testSecret,
			wantErr: ErrIDTokenSignature,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newTestChannel(tc.secret)
			claims, err := ch.VerifyIDToken(tc.token(t))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if claims == nil {
				t.Fatal("claims nil")
			}
			if tc.check != nil {
				tc.check(t, claims)
			}
		})
	}
}

func TestVerifyIDToken_UnsupportedAlg(t *testing.T) {
	header := `{"alg":"none","typ":"JWT"}`
	payload, _ := json.Marshal(IDTokenClaims{Sub: "U1", Exp: time.Now().Unix() + 60})
	h := base64.RawURLEncoding.EncodeToString([]byte(header))
	p := base64.RawURLEncoding.EncodeToString(payload)
	tok := h + "." + p + "."

	ch := newTestChannel(testSecret)
	_, err := ch.VerifyIDToken(tok)
	if !errors.Is(err, ErrIDTokenUnsupported) {
		t.Fatalf("err = %v, want ErrIDTokenUnsupported", err)
	}
}
