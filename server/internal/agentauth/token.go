// Package agentauth identifies the agents that call Mainloop's MCP server: bearer tokens, the
// credential rows behind them, the per-actor tool table and the lifecycle of the Kubernetes
// Secrets that carry the bearers into the agent's egress.
package agentauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// TokenPrefix starts every Mainloop agent bearer, so a leaked one is recognisable.
const TokenPrefix = "ml_"

// Token derives the bearer for a credential: "ml_" plus the hex HMAC-SHA256 of the credential id
// under key. Only [HashToken] of it is stored. The id is the credential's, not the binding's, so
// a binding that is revoked and enrolled again gets a different bearer and the stored hashes
// stay unique.
func Token(key []byte, credentialID string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(credentialID))
	return TokenPrefix + hex.EncodeToString(m.Sum(nil))
}

// HashToken is the stored form of a bearer: the hex SHA-256 of the whole token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ErrNoBearer means the Authorization header carries no usable bearer token.
var ErrNoBearer = errors.New("agentauth: no bearer token")

// BearerToken extracts the token from an Authorization header value. The scheme is matched
// case-insensitively; a blank token or a token without the Mainloop prefix is refused so a
// foreign credential never reaches the database.
func BearerToken(header string) (string, error) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", ErrNoBearer
	}
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, TokenPrefix) || len(token) <= len(TokenPrefix) {
		return "", ErrNoBearer
	}
	return token, nil
}
