package esmithkm

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/channels/line"
)

const (
	testTenantID = "401190607"
	testClientID = "WOFF_CLIENT_abc"
	testKid      = "key-1"
)

// b64u is RawURLEncoding without padding (JWS / JWK convention).
func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwksJSON renders a single-RSA-key JWKS for pub under kid.
func jwksJSON(kid string, pub *rsa.PublicKey) []byte {
	eBytes := big.NewInt(int64(pub.E)).Bytes()
	set := jwks{Keys: []jwk{{
		Kty: "RSA",
		Kid: kid,
		Use: "sig",
		Alg: "RS256",
		N:   b64u(pub.N.Bytes()),
		E:   b64u(eBytes),
	}}}
	data, _ := json.Marshal(set)
	return data
}

// signToken builds and RS256-signs a JWT with the given header alg/kid and
// claims map. When key is nil the signature bytes are garbage (for tamper
// tests the caller signs with a different key instead).
func signToken(t *testing.T, alg, kid string, claims map[string]any, key *rsa.PrivateKey) string {
	t.Helper()
	header := map[string]any{"alg": alg, "typ": "JWT"}
	if kid != "" {
		header["kid"] = kid
	}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signing := b64u(hb) + "." + b64u(cb)

	var sig []byte
	if alg == "RS256" && key != nil {
		digest := sha256.Sum256([]byte(signing))
		s, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		sig = s
	} else {
		sig = []byte("not-a-real-signature")
	}
	return signing + "." + b64u(sig)
}

// newTestVerifier wires a verifier at an httptest server serving JWKS +
// discovery, with a fixed clock at `now`.
func newTestVerifier(t *testing.T, key *rsa.PrivateKey, now time.Time) (*WorksLiffVerifier, *int32, *int32) {
	t.Helper()
	var certHits, discHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/v2.0/certs/"+testTenantID, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&certHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksJSON(testKid, &key.PublicKey))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	issuer := srv.URL + "/" + testTenantID
	mux.HandleFunc("/"+testTenantID+"/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&discHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issuer":"` + issuer + `"}`))
	})

	v := NewWorksLiffVerifier(testClientID, testTenantID)
	v.AuthBaseURL = srv.URL
	v.HTTP = srv.Client()
	v.now = func() time.Time { return now }
	return v, &certHits, &discHits
}

func validClaims(now time.Time) map[string]any {
	return map[string]any{
		"sub":  "works-user-123",
		"name": "Test User",
		"aud":  testClientID,
		"iss":  "", // filled by caller after issuer is known
		"exp":  now.Add(5 * time.Minute).Unix(),
		"iat":  now.Unix(),
	}
}

func TestWorksVerifier_Valid(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	v, _, _ := newTestVerifier(t, key, now)

	claims := validClaims(now)
	claims["iss"] = v.expectedIssuer() // resolves via discovery

	tok := signToken(t, "RS256", testKid, claims, key)
	got, err := v.VerifyIDToken(tok)
	if err != nil {
		t.Fatalf("VerifyIDToken: unexpected error: %v", err)
	}
	if got.Sub != "works-user-123" {
		t.Errorf("Sub = %q want works-user-123", got.Sub)
	}
	if got.Aud != testClientID {
		t.Errorf("Aud = %q want %q", got.Aud, testClientID)
	}
	if got.Name != "Test User" {
		t.Errorf("Name = %q want Test User", got.Name)
	}
}

func TestWorksVerifier_WrongAud(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	v, _, _ := newTestVerifier(t, key, now)

	claims := validClaims(now)
	claims["iss"] = v.expectedIssuer()
	claims["aud"] = "some-other-client"

	tok := signToken(t, "RS256", testKid, claims, key)
	_, err := v.VerifyIDToken(tok)
	if !errors.Is(err, ErrWorksAudience) {
		t.Fatalf("want ErrWorksAudience, got %v", err)
	}
}

func TestWorksVerifier_WrongIssuer(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	v, _, _ := newTestVerifier(t, key, now)

	claims := validClaims(now)
	claims["iss"] = "https://evil.example.com/" + testTenantID

	tok := signToken(t, "RS256", testKid, claims, key)
	_, err := v.VerifyIDToken(tok)
	if !errors.Is(err, ErrWorksIssuer) {
		t.Fatalf("want ErrWorksIssuer, got %v", err)
	}
}

func TestWorksVerifier_Expired(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	v, _, _ := newTestVerifier(t, key, now)

	claims := validClaims(now)
	claims["iss"] = v.expectedIssuer()
	claims["exp"] = now.Add(-100 * time.Second).Unix() // beyond 60s leeway

	tok := signToken(t, "RS256", testKid, claims, key)
	_, err := v.VerifyIDToken(tok)
	if !errors.Is(err, line.ErrIDTokenExpired) {
		t.Fatalf("want ErrIDTokenExpired, got %v", err)
	}
}

func TestWorksVerifier_AlgConfusion(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	v, _, _ := newTestVerifier(t, key, now)

	claims := validClaims(now)
	claims["iss"] = v.expectedIssuer()

	for _, alg := range []string{"HS256", "none", "ES256"} {
		tok := signToken(t, alg, testKid, claims, key)
		_, err := v.VerifyIDToken(tok)
		if !errors.Is(err, line.ErrIDTokenUnsupported) {
			t.Errorf("alg=%s: want ErrIDTokenUnsupported, got %v", alg, err)
		}
	}
}

func TestWorksVerifier_TamperedAndWrongKey(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	v, _, _ := newTestVerifier(t, key, now)

	claims := validClaims(now)
	claims["iss"] = v.expectedIssuer()

	// (a) signed by a different key than the JWKS publishes.
	tok := signToken(t, "RS256", testKid, claims, otherKey)
	if _, err := v.VerifyIDToken(tok); !errors.Is(err, line.ErrIDTokenSignature) {
		t.Errorf("wrong key: want ErrIDTokenSignature, got %v", err)
	}

	// (b) valid signature then payload tampered.
	good := signToken(t, "RS256", testKid, claims, key)
	parts := strings.Split(good, ".")
	tampered := map[string]any{"sub": "attacker", "aud": testClientID, "iss": claims["iss"], "exp": claims["exp"], "iat": claims["iat"]}
	tb, _ := json.Marshal(tampered)
	parts[1] = b64u(tb)
	if _, err := v.VerifyIDToken(strings.Join(parts, ".")); !errors.Is(err, line.ErrIDTokenSignature) {
		t.Errorf("tampered payload: want ErrIDTokenSignature, got %v", err)
	}
}

func TestWorksVerifier_Malformed(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	v, _, _ := newTestVerifier(t, key, now)

	for _, tok := range []string{"", "a.b", "only-one-part", "a.b.c.d"} {
		if _, err := v.VerifyIDToken(tok); !errors.Is(err, line.ErrIDTokenMalformed) {
			t.Errorf("token %q: want ErrIDTokenMalformed, got %v", tok, err)
		}
	}
}

func TestWorksVerifier_UnknownKidRefetchThenError(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	v, certHits, _ := newTestVerifier(t, key, now)

	claims := validClaims(now)
	claims["iss"] = v.expectedIssuer()

	// Prime cache with a valid verify (kid present).
	tok := signToken(t, "RS256", testKid, claims, key)
	if _, err := v.VerifyIDToken(tok); err != nil {
		t.Fatalf("prime: %v", err)
	}
	primeHits := atomic.LoadInt32(certHits)

	// Token with an unknown kid -> exactly one refetch then ErrWorksKeyUnknown.
	tokUnknown := signToken(t, "RS256", "rotated-kid-2", claims, key)
	_, err := v.VerifyIDToken(tokUnknown)
	if !errors.Is(err, ErrWorksKeyUnknown) {
		t.Fatalf("want ErrWorksKeyUnknown, got %v", err)
	}
	if got := atomic.LoadInt32(certHits) - primeHits; got != 1 {
		t.Errorf("unknown kid triggered %d refetches, want 1", got)
	}
}

func TestWorksVerifier_JWKSCacheWithinTTL(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	v, certHits, _ := newTestVerifier(t, key, now)

	claims := validClaims(now)
	claims["iss"] = v.expectedIssuer()
	tok := signToken(t, "RS256", testKid, claims, key)

	for i := 0; i < 5; i++ {
		if _, err := v.VerifyIDToken(tok); err != nil {
			t.Fatalf("verify %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(certHits); got != 1 {
		t.Errorf("certs endpoint hit %d times across 5 verifies, want 1 (cache)", got)
	}
}

func TestWorksVerifier_ClockSkewLeeway(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()

	// exp == now-30s passes with 60s leeway.
	v1, _, _ := newTestVerifier(t, key, now)
	c1 := validClaims(now)
	c1["iss"] = v1.expectedIssuer()
	c1["exp"] = now.Add(-30 * time.Second).Unix()
	tok1 := signToken(t, "RS256", testKid, c1, key)
	if _, err := v1.VerifyIDToken(tok1); err != nil {
		t.Errorf("exp=now-30s should pass within 60s leeway, got %v", err)
	}

	// exp == now-90s fails (beyond 60s leeway).
	v2, _, _ := newTestVerifier(t, key, now)
	c2 := validClaims(now)
	c2["iss"] = v2.expectedIssuer()
	c2["exp"] = now.Add(-90 * time.Second).Unix()
	tok2 := signToken(t, "RS256", testKid, c2, key)
	if _, err := v2.VerifyIDToken(tok2); !errors.Is(err, line.ErrIDTokenExpired) {
		t.Errorf("exp=now-90s should fail beyond 60s leeway, got %v", err)
	}
}

func TestWorksVerifier_EmptyClientIDRejects(t *testing.T) {
	v := NewWorksLiffVerifier("", testTenantID)
	if _, err := v.VerifyIDToken("a.b.c"); !errors.Is(err, ErrWorksClientID) {
		t.Fatalf("empty client id: want ErrWorksClientID, got %v", err)
	}
}

// Interface conformance: *WorksLiffVerifier must satisfy IDTokenVerifier so it
// is a drop-in for the LiffHandler.
var _ IDTokenVerifier = (*WorksLiffVerifier)(nil)
