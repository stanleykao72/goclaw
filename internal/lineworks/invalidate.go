package lineworks

import "time"

// Invalidate clears the cached access token so the next Token call performs a
// fresh JWT-bearer exchange. Used by the channel adapter's one-shot retry path:
// when a send returns ErrUnauthorized (token revoked or rejected server-side
// before its reported expiry), the adapter invalidates and retries once rather
// than waiting for the proactive-skew refresh window.
func (t *TokenSource) Invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token = ""
	t.expiry = time.Time{}
}

// InvalidateToken drops the client's cached bearer token via its TokenSource.
// Exposed so the channel adapter can force a refresh between the two attempts
// of its auth-error retry without reaching into TokenSource directly.
func (c *Client) InvalidateToken() {
	if c.tokens != nil {
		c.tokens.Invalidate()
	}
}
