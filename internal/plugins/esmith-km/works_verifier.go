package esmithkm

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/channels/line"
)

// WorksLiffVerifier verifies LINE WORKS (WOFF) OIDC ID tokens locally by
// validating the RS256 JWS signature against the WORKS public JWKS, then the
// standard OIDC claims (iss / aud / exp / iat).
//
// Why local verification (not an online verify endpoint): LINE WORKS has no
// /verify analogue for ID tokens. The documented contract (per the LINE WORKS
// "How to Use an ID Token" reference) is to validate the ID token claims and
// signature client-side using the public certificates published at
// certs/{tenantId}. This mirrors the hand-rolled HS256 verifier at
// internal/channels/line/id_token.go — RS256 here, no JWT/JOSE dependency.
//
// It satisfies the IDTokenVerifier interface (liff_http.go) as a drop-in
// replacement for LINELiffVerifier: the only field km consumes downstream is
// claims.Sub (the per-user identity matched against draft.line_user_id at
// liff_http.go:198,:290). Under the LIFF->WOFF migration the bot side writes
// the WORKS userId into line_user_id, so Sub stays the WORKS userId (the
// token's `sub`) — no change to the ownership check.
type WorksLiffVerifier struct {
	// ClientID is the expected `aud` — the WORKS WOFF/OAuth client id.
	ClientID string
	// TenantID is the WORKS domainId (e.g. 401190607); templates the certs +
	// openid-configuration URLs.
	TenantID string

	// AuthBaseURL is the LINE WORKS auth host root, default
	// https://auth.worksmobile.com. Overridable for tests.
	AuthBaseURL string

	// Issuer pins the expected `iss`. When empty it is derived once from the
	// tenant .well-known/openid-configuration `issuer` field (the exact iss
	// string is a known-uncertain value), with a tenant-templated fallback.
	Issuer string

	// HTTP is the client used to fetch JWKS / discovery; nil -> a client with
	// Timeout. Timeout defaults to 5s.
	HTTP    *http.Client
	Timeout time.Duration

	// JWKSTTL bounds the in-memory key cache; default 1h.
	JWKSTTL time.Duration
	// Leeway is the clock-skew allowance for exp/iat; default 60s.
	Leeway time.Duration

	now func() time.Time

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	keysFetched time.Time
	// inflight single-flights a JWKS refetch so an unknown-kid burst hits the
	// certs endpoint once.
	inflight *sync.Once
	// issuerResolved caches the derived issuer so discovery runs at most once.
	issuerResolved string
	issuerOnce     sync.Once
}

// Errors surfaced by the WORKS verifier. They wrap line.IDToken* sentinels
// where the shape matches so callers can keep using errors.Is on the line
// sentinels for malformed/expired/unsupported cases.
var (
	ErrWorksClientID   = errors.New("works verifier: client id not configured")
	ErrWorksAudience   = errors.New("works verifier: audience mismatch")
	ErrWorksIssuer     = errors.New("works verifier: issuer mismatch")
	ErrWorksKeyUnknown = errors.New("works verifier: signing key (kid) not found in JWKS")
	ErrWorksKeyType    = errors.New("works verifier: JWK is not an RSA key")
)

// NewWorksLiffVerifier builds a verifier bound to the WORKS WOFF client id and
// tenant. Mirrors NewLINELiffVerifier's hardcoded-defaults style: production
// auth host, 1h JWKS TTL, 60s clock-skew leeway, 5s HTTP timeout. Empty
// clientID is invalid — the caller (cmd wiring) should short-circuit; if a
// token is verified anyway it is rejected with ErrWorksClientID.
func NewWorksLiffVerifier(clientID, tenantID string) *WorksLiffVerifier {
	if tenantID == "" {
		tenantID = "401190607"
	}
	return &WorksLiffVerifier{
		ClientID:    clientID,
		TenantID:    tenantID,
		AuthBaseURL: "https://auth.worksmobile.com",
		HTTP:        &http.Client{Timeout: 5 * time.Second},
		Timeout:     5 * time.Second,
		JWKSTTL:     1 * time.Hour,
		Leeway:      60 * time.Second,
		now:         time.Now,
	}
}

func (v *WorksLiffVerifier) httpClient() *http.Client {
	if v.HTTP != nil {
		return v.HTTP
	}
	to := v.Timeout
	if to == 0 {
		to = 5 * time.Second
	}
	return &http.Client{Timeout: to}
}

func (v *WorksLiffVerifier) clock() time.Time {
	if v.now != nil {
		return v.now()
	}
	return time.Now()
}

func (v *WorksLiffVerifier) leeway() time.Duration {
	if v.Leeway > 0 {
		return v.Leeway
	}
	return 60 * time.Second
}

func (v *WorksLiffVerifier) ttl() time.Duration {
	if v.JWKSTTL > 0 {
		return v.JWKSTTL
	}
	return time.Hour
}

func (v *WorksLiffVerifier) certsURL() string {
	return strings.TrimRight(v.AuthBaseURL, "/") + "/oauth2/v2.0/certs/" + v.TenantID
}

func (v *WorksLiffVerifier) openIDConfigURL() string {
	return strings.TrimRight(v.AuthBaseURL, "/") + "/" + v.TenantID + "/.well-known/openid-configuration"
}

// jwtHeader / jwtPayload are the decoded token parts we read.
type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

type worksClaims struct {
	Sub  string `json:"sub"`
	Name string `json:"name"`
	// aud may be a string or an array per the OIDC spec; decode both.
	Aud audValue `json:"aud"`
	Iss string   `json:"iss"`
	Exp int64    `json:"exp"`
	Iat int64    `json:"iat"`
}

// audValue tolerates aud being either a JSON string or []string.
type audValue []string

func (a *audValue) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*a = []string{s}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	*a = arr
	return nil
}

func (a audValue) contains(want string) bool {
	for _, x := range a {
		if x == want {
			return true
		}
	}
	return false
}

func (a audValue) first() string {
	if len(a) > 0 {
		return a[0]
	}
	return ""
}

// VerifyIDToken validates a LINE WORKS WOFF ID token and maps it into
// line.IDTokenClaims. Satisfies the IDTokenVerifier interface.
func (v *WorksLiffVerifier) VerifyIDToken(idToken string) (*line.IDTokenClaims, error) {
	if v.ClientID == "" {
		return nil, ErrWorksClientID
	}

	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil, line.ErrIDTokenMalformed
	}
	headerRaw, payloadRaw, sigRaw := parts[0], parts[1], parts[2]

	headerBytes, err := base64.RawURLEncoding.DecodeString(headerRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: header b64: %v", line.ErrIDTokenMalformed, err)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("%w: header json: %v", line.ErrIDTokenMalformed, err)
	}
	// Algorithm-confusion guard: hardcode RS256, reject HS256/none/others
	// BEFORE any signature work (mirrors line/id_token.go HS256 gate).
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("%w: %s", line.ErrIDTokenUnsupported, header.Alg)
	}

	sig, err := base64.RawURLEncoding.DecodeString(sigRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: sig b64: %v", line.ErrIDTokenMalformed, err)
	}

	pub, err := v.publicKey(header.Kid)
	if err != nil {
		return nil, err
	}

	digest := sha256.Sum256([]byte(headerRaw + "." + payloadRaw))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return nil, fmt.Errorf("%w: %v", line.ErrIDTokenSignature, err)
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(payloadRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: payload b64: %v", line.ErrIDTokenMalformed, err)
	}
	var claims worksClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("%w: payload json: %v", line.ErrIDTokenMalformed, err)
	}

	// aud == configured WORKS client id.
	if !claims.Aud.contains(v.ClientID) {
		return nil, fmt.Errorf("%w: got %q want %q", ErrWorksAudience, claims.Aud.first(), v.ClientID)
	}

	// iss == expected issuer (derived from discovery or tenant-templated).
	wantIss := v.expectedIssuer()
	if claims.Iss != wantIss {
		return nil, fmt.Errorf("%w: got %q want %q", ErrWorksIssuer, claims.Iss, wantIss)
	}

	now := v.clock()
	leeway := v.leeway()
	if claims.Exp > 0 && now.After(time.Unix(claims.Exp, 0).Add(leeway)) {
		return nil, line.ErrIDTokenExpired
	}
	if claims.Iat > 0 && now.Add(leeway).Before(time.Unix(claims.Iat, 0)) {
		return nil, fmt.Errorf("%w: issued in the future", line.ErrIDTokenMalformed)
	}

	return &line.IDTokenClaims{
		Sub:  claims.Sub,
		Aud:  claims.Aud.first(),
		Iss:  claims.Iss,
		Exp:  claims.Exp,
		Iat:  claims.Iat,
		Name: claims.Name,
	}, nil
}

// expectedIssuer returns the configured issuer, else the discovery-derived
// one (resolved at most once), else the tenant-templated fallback.
func (v *WorksLiffVerifier) expectedIssuer() string {
	if v.Issuer != "" {
		return v.Issuer
	}
	v.issuerOnce.Do(func() {
		v.issuerResolved = v.discoverIssuer()
	})
	if v.issuerResolved != "" {
		return v.issuerResolved
	}
	// Tenant-templated fallback; matches the openid-configuration host shape.
	return strings.TrimRight(v.AuthBaseURL, "/") + "/" + v.TenantID
}

// discoverIssuer reads the `issuer` field from the tenant
// .well-known/openid-configuration. Best-effort: returns "" on any failure so
// expectedIssuer falls back to the templated value.
func (v *WorksLiffVerifier) discoverIssuer() string {
	ctx, cancel := context.WithTimeout(context.Background(), v.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.openIDConfigURL(), nil)
	if err != nil {
		return ""
	}
	resp, err := v.httpClient().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return ""
	}
	return doc.Issuer
}

func (v *WorksLiffVerifier) requestTimeout() time.Duration {
	if v.Timeout > 0 {
		return v.Timeout
	}
	return 5 * time.Second
}

// publicKey returns the RSA public key for kid, fetching/refreshing the JWKS
// cache as needed. On an unknown kid (key rotation) a single-flight refetch is
// triggered so a burst of tokens with a new kid hits the certs endpoint once.
func (v *WorksLiffVerifier) publicKey(kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	fresh := v.keys != nil && v.clock().Sub(v.keysFetched) < v.ttl()
	if fresh {
		if k, ok := v.keys[kid]; ok {
			v.mu.Unlock()
			return k, nil
		}
	}
	// Either stale, empty, or unknown kid: arrange a single-flight refetch.
	if v.inflight == nil {
		v.inflight = &sync.Once{}
	}
	once := v.inflight
	v.mu.Unlock()

	var fetchErr error
	once.Do(func() {
		keys, err := v.fetchJWKS()
		v.mu.Lock()
		defer v.mu.Unlock()
		if err != nil {
			fetchErr = err
			// Reset so the next call retries rather than being pinned to a
			// failed fetch.
			v.inflight = nil
			return
		}
		v.keys = keys
		v.keysFetched = v.clock()
		v.inflight = nil
	})
	if fetchErr != nil {
		return nil, fetchErr
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("%w: kid=%q", ErrWorksKeyUnknown, kid)
}

// jwk models one entry of the WORKS certs JWKS. WORKS publishes RSA keys with
// n/e (base64url); x5c PEM fallback is also handled when present.
type jwk struct {
	Kty string   `json:"kty"`
	Kid string   `json:"kid"`
	Use string   `json:"use"`
	Alg string   `json:"alg"`
	N   string   `json:"n"`
	E   string   `json:"e"`
	X5c []string `json:"x5c"`
}

type jwks struct {
	Keys []jwk `json:"keys"`
}

func (v *WorksLiffVerifier) fetchJWKS() (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), v.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.certsURL(), nil)
	if err != nil {
		return nil, fmt.Errorf("works verifier: build jwks request: %w", err)
	}
	resp, err := v.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("works verifier: fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("works verifier: jwks http %d", resp.StatusCode)
	}
	var set jwks
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, fmt.Errorf("works verifier: decode jwks: %w", err)
	}
	out := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		pub, err := jwkToRSA(k)
		if err != nil {
			// Skip non-RSA / malformed keys rather than failing the whole set.
			continue
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, errors.New("works verifier: jwks contained no usable RSA keys")
	}
	return out, nil
}

// jwkToRSA converts a JWK to an *rsa.PublicKey. Prefers n/e; falls back to the
// first x5c cert (PEM/DER) when n/e are absent.
func jwkToRSA(k jwk) (*rsa.PublicKey, error) {
	if k.N != "" && k.E != "" {
		if k.Kty != "" && k.Kty != "RSA" {
			return nil, ErrWorksKeyType
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.N, "="))
		if err != nil {
			return nil, fmt.Errorf("jwk n decode: %w", err)
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.E, "="))
		if err != nil {
			return nil, fmt.Errorf("jwk e decode: %w", err)
		}
		n := new(big.Int).SetBytes(nBytes)
		e := new(big.Int).SetBytes(eBytes)
		if !e.IsInt64() || e.Int64() <= 0 {
			return nil, errors.New("jwk e out of range")
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	}
	if len(k.X5c) > 0 {
		return x5cToRSA(k.X5c[0])
	}
	return nil, ErrWorksKeyType
}

// x5cToRSA parses an x5c entry (base64 DER cert, or a PEM-wrapped cert) into an
// RSA public key.
func x5cToRSA(x5c string) (*rsa.PublicKey, error) {
	der := x5c
	if block, _ := pem.Decode([]byte(x5c)); block != nil {
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("x5c pem parse: %w", err)
		}
		return rsaFromCert(cert)
	}
	raw, err := base64.StdEncoding.DecodeString(der)
	if err != nil {
		return nil, fmt.Errorf("x5c b64 decode: %w", err)
	}
	cert, err := x509.ParseCertificate(raw)
	if err != nil {
		return nil, fmt.Errorf("x5c der parse: %w", err)
	}
	return rsaFromCert(cert)
}

func rsaFromCert(cert *x509.Certificate) (*rsa.PublicKey, error) {
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, ErrWorksKeyType
	}
	return pub, nil
}
