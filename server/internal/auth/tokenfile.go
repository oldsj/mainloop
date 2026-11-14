package auth

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// TokenSource returns the bearer for the next request.
type TokenSource func() (string, error)

// TokenFile reads the bearer from path on every call, so a token the kubelet rotates is picked up.
// An empty path returns nil: requests go without a bearer.
func TokenFile(path string) TokenSource {
	if path == "" {
		return nil
	}
	return func() (string, error) {
		raw, err := os.ReadFile(path) // #nosec G304 -- the operator or chart names this file
		if err != nil {
			return "", fmt.Errorf("token file: %w", err)
		}
		tok := strings.TrimSpace(string(raw))
		if tok == "" {
			return "", errors.New("token file is empty")
		}
		return tok, nil
	}
}
