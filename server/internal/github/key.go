package github

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"strconv"
	"strings"
)

// asciiSpace is the surrounding whitespace a pasted secret may carry.
const asciiSpace = " \t\r\n\v\f"

// ErrInvalidCredentials means the App id or private key is unusable. The message never carries
// any part of the key.
var ErrInvalidCredentials = errors.New("github: invalid App credentials")

// ParseAppID parses the numeric GitHub App id, ignoring surrounding ASCII whitespace.
func ParseAppID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.Trim(s, asciiSpace), 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalidCredentials
	}
	return id, nil
}

// ParsePrivateKey decodes a single-line base64 PEM RSA key (PKCS#1 or PKCS#8). Surrounding ASCII
// whitespace is ignored; whitespace inside the base64 is not. Encrypted, non-RSA and
// under-2048-bit keys are refused.
func ParsePrivateKey(b64 string) (*rsa.PrivateKey, error) {
	trimmed := strings.Trim(b64, asciiSpace)
	// The base64 decoder skips CR and LF; a key broken across lines is not the documented format.
	if strings.ContainsAny(trimmed, asciiSpace) {
		return nil, ErrInvalidCredentials
	}
	raw, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	block, rest := pem.Decode(raw)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, ErrInvalidCredentials
	}
	key, err := parseRSA(block)
	if err != nil || key.N.BitLen() < minKeyBits {
		return nil, ErrInvalidCredentials
	}
	return key, nil
}

// minKeyBits is the smallest RSA modulus GitHub Apps accept.
const minKeyBits = 2048

func parseRSA(block *pem.Block) (*rsa.PrivateKey, error) {
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("not an RSA key")
		}
		return key, nil
	default:
		return nil, errors.New("unsupported PEM block")
	}
}
