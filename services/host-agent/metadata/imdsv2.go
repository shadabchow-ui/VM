package metadata

// imdsv2.go — IMDSv2 token-based session management.
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §6 (IMDSv2 token-based),
//         IMPLEMENTATION_PLAN_V1 §35.
//         04-03-bootstrap-initialization-and-readiness-signaling.md.
//
// Flow:
//   PUT /token { TTL-Seconds: N } → session token (opaque string, max TTL 21600s)
//   GET /metadata/v1/<key> with X-Metadata-Token: <token> → value
//
// Tokens are in-memory only (no DB). They expire automatically.
// Tokens are per-VM: each VM has its own token store.
// The token is a random 32-byte base64url-encoded value.

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

const (
	// maxTokenTTL is the maximum allowed TTL for a session token (6 hours).
	maxTokenTTL = 6 * time.Hour
	// minTokenTTL is the minimum allowed TTL.
	minTokenTTL = 1 * time.Second
	// defaultTokenTTL is used when the client sends TTL-Seconds: 0.
	defaultTokenTTL = 21600 * time.Second
)

// ErrInvalidToken is returned when the token is missing, expired, or malformed.
var ErrInvalidToken = errors.New("invalid or expired metadata token")

// ErrInvalidTTL is returned when the requested TTL is out of range.
var ErrInvalidTTL = errors.New("TTL-Seconds must be between 1 and 21600")

// tokenEntry holds a single active session token.
type tokenEntry struct {
	expiresAt time.Time
}

// TokenStore manages IMDSv2 session tokens for one VM instance.
// All methods are safe for concurrent use.
type TokenStore struct {
	mu     sync.Mutex
	tokens map[string]tokenEntry
}

// NewTokenStore constructs an empty TokenStore.
func NewTokenStore() *TokenStore {
	return &TokenStore{tokens: make(map[string]tokenEntry)}
}

// Issue creates and stores a new session token with the given TTL seconds.
// Returns ErrInvalidTTL if ttlSeconds is out of [1, 21600].
func (ts *TokenStore) Issue(ttlSeconds int) (string, error) {
	if ttlSeconds == 0 {
		ttlSeconds = int(defaultTokenTTL.Seconds())
	}
	if ttlSeconds < 1 || ttlSeconds > int(maxTokenTTL.Seconds()) {
		return "", ErrInvalidTTL
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	ts.mu.Lock()
	ts.tokens[token] = tokenEntry{expiresAt: time.Now().Add(time.Duration(ttlSeconds) * time.Second)}
	ts.mu.Unlock()
	return token, nil
}

// Validate returns nil if the token is present and not expired.
// Returns ErrInvalidToken otherwise.
func (ts *TokenStore) Validate(token string) error {
	if token == "" {
		return ErrInvalidToken
	}
	ts.mu.Lock()
	entry, ok := ts.tokens[token]
	ts.mu.Unlock()
	if !ok || time.Now().After(entry.expiresAt) {
		return ErrInvalidToken
	}
	return nil
}

// Purge removes all expired tokens. Call periodically to avoid unbounded growth.
func (ts *TokenStore) Purge() {
	now := time.Now()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for tok, e := range ts.tokens {
		if now.After(e.expiresAt) {
			delete(ts.tokens, tok)
		}
	}
}
