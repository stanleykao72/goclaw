// Package lineworks is a self-contained LINE WORKS API client for goclaw.
//
// It owns the three low-level concerns of talking to the LINE WORKS
// platform and nothing else:
//
//   - Service-Account OAuth: signing an RS256 JWT assertion and exchanging
//     it for a bearer access token, with in-memory caching and automatic
//     refresh (auth.go, TokenSource).
//   - Bot REST: sending text messages to a user (1:1) or a channel/group,
//     plus directory lookup to resolve a LINE WORKS userId to its
//     externalKey (client.go, Client).
//   - Callback signature verification: recomputing the X-WORKS-Signature
//     HMAC-SHA256 over the raw request body and constant-time comparing it
//     (client.go, VerifySignature).
//
// # No goclaw dependencies
//
// This package is a pure SDK layer. It MUST NOT import internal/channels,
// internal/bus, internal/store, internal/config, or any other goclaw
// package. The goclaw channel adapter (internal/channels/lineworks) and the
// workflow plugin (internal/plugins/lineworks-workflow) depend on this
// package, never the other way around. Keeping it dependency-free leaves it
// trivially unit-testable and potentially upstream-able.
//
// # Two distinct secrets
//
// LINE WORKS uses two unrelated secrets that are easy to confuse:
//
//   - The OAuth client_secret (AuthConfig.ClientSecret) authenticates the
//     token-exchange request. Used only by TokenSource.
//   - The Bot Secret (the bot's own secret) is the HMAC key for callback
//     signature verification. Used only by VerifySignature.
//
// They are NOT interchangeable. Callers must store and pass both.
//
// # Authentication reference
//
//   - Token endpoint: https://auth.worksmobile.com/oauth2/v2.0/token
//     (application/x-www-form-urlencoded; grant_type=jwt-bearer assertion).
//   - API base: https://www.worksapis.com/v1.0.
//   - JWT: header {alg:RS256, typ:JWT}; claims iss=client_id,
//     sub=service-account email, iat, exp (<= iat+3600s).
//
// See the developer docs at https://developers.worksmobile.com/ for the
// authoritative wire formats.
package lineworks
