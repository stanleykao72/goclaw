package lineworks

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// authTokenURL is the LINE WORKS OAuth 2.0 token endpoint.
	authTokenURL = "https://auth.worksmobile.com/oauth2/v2.0/token"

	// jwtBearerGrant is the RFC 7523 grant type for JWT-bearer assertions.
	jwtBearerGrant = "urn:ietf:params:oauth:grant-type:jwt-bearer"

	// jwtTTL is the lifetime baked into the signed assertion. The platform
	// caps exp at iat+3600s; we stay comfortably under that.
	jwtTTL = 50 * time.Minute

	// tokenSkew is how early (before the reported expiry) we proactively
	// refresh the access token, so an in-flight request never races expiry.
	tokenSkew = 5 * time.Minute
)

// AuthConfig holds the service-account credentials needed to obtain a
// bearer access token. All fields are required except Scopes, which
// defaults to {"bot", "bot.message"} when empty.
type AuthConfig struct {
	// ClientID is the OAuth client (app) ID. Becomes the JWT `iss`.
	ClientID string
	// ClientSecret authenticates the token-exchange POST. NOT the bot secret.
	ClientSecret string
	// ServiceAccount is the service-account email ([id]@domain). Becomes `sub`.
	ServiceAccount string
	// PrivateKeyPEM is the RSA private key (PKCS#1 or PKCS#8 PEM) used to
	// RS256-sign the JWT assertion.
	PrivateKeyPEM string
	// Scopes are the OAuth scopes requested, comma-joined on the wire.
	Scopes []string
}

// tokenResponse mirrors the token endpoint's JSON body. Note expires_in is
// delivered as a JSON string ("3600"), not a number.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    string `json:"expires_in"`
	Scope        string `json:"scope"`
}

// TokenSource issues and caches LINE WORKS bearer tokens. It is safe for
// concurrent use; Token serializes refreshes under a mutex.
type TokenSource struct {
	cfg        AuthConfig
	privateKey *rsa.PrivateKey
	httpClient *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewTokenSource validates the config, parses the RSA private key from PEM,
// and returns a ready TokenSource. No network call happens here; the first
// Token call performs the initial exchange.
func NewTokenSource(cfg AuthConfig) (*TokenSource, error) {
	if cfg.ClientID == "" {
		return nil, errors.New("lineworks: AuthConfig.ClientID is required")
	}
	if cfg.ClientSecret == "" {
		return nil, errors.New("lineworks: AuthConfig.ClientSecret is required")
	}
	if cfg.ServiceAccount == "" {
		return nil, errors.New("lineworks: AuthConfig.ServiceAccount is required")
	}
	if cfg.PrivateKeyPEM == "" {
		return nil, errors.New("lineworks: AuthConfig.PrivateKeyPEM is required")
	}
	key, err := parseRSAPrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"bot", "bot.message"}
	}
	return &TokenSource{
		cfg:        cfg,
		privateKey: key,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// parseRSAPrivateKey decodes a PEM block and parses it as an RSA private key.
// Accepts both PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE KEY") encodings.
func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("lineworks: private key PEM is not decodable")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("lineworks: parse private key: %w", err)
	}
	rsaKey, ok := keyAny.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("lineworks: private key is %T, want *rsa.PrivateKey", keyAny)
	}
	return rsaKey, nil
}

// Token returns a valid bearer access token, refreshing it if the cached one
// is missing or within tokenSkew of expiry. Safe for concurrent callers.
func (t *TokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.token != "" && time.Now().Before(t.expiry.Add(-tokenSkew)) {
		return t.token, nil
	}

	assertion, err := t.signJWT(time.Now())
	if err != nil {
		return "", err
	}

	form := url.Values{}
	form.Set("assertion", assertion)
	form.Set("grant_type", jwtBearerGrant)
	form.Set("client_id", t.cfg.ClientID)
	form.Set("client_secret", t.cfg.ClientSecret)
	form.Set("scope", strings.Join(t.cfg.Scopes, ","))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("lineworks: token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("lineworks: token endpoint returned HTTP %d", resp.StatusCode)
	}

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("lineworks: decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("lineworks: token endpoint returned empty access_token")
	}

	// expires_in is a string of seconds; default to jwtTTL on parse failure.
	ttl := jwtTTL
	if secs, err := strconv.Atoi(tr.ExpiresIn); err == nil && secs > 0 {
		ttl = time.Duration(secs) * time.Second
	}
	t.token = tr.AccessToken
	t.expiry = time.Now().Add(ttl)
	return t.token, nil
}

// signJWT builds and RS256-signs the service-account assertion as of now.
func (t *TokenSource) signJWT(now time.Time) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iss": t.cfg.ClientID,
		"sub": t.cfg.ServiceAccount,
		"iat": now.Unix(),
		"exp": now.Add(jwtTTL).Unix(),
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, t.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("lineworks: sign jwt: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
