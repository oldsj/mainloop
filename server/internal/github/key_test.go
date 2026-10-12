package github_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/github/githubtest"
)

func b64pem(typ string, der []byte) string {
	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

// Mined from backend/tests/runtime/test_github_app.py ConfigurationTests.
func TestParsePrivateKeyAcceptsBothFormatsAndSurroundingSpace(t *testing.T) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(k)
	for name, enc := range map[string]string{
		"pkcs1": b64pem("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(k)),
		"pkcs8": b64pem("PRIVATE KEY", pkcs8),
	} {
		for _, pad := range [][2]string{{"", "\n"}, {"", "\r\n"}, {"  ", "  "}, {" \t\n\r\v\f", " \t\n\r\v\f"}} {
			got, err := github.ParsePrivateKey(pad[0] + enc + pad[1])
			if err != nil || !got.Equal(k) {
				t.Errorf("%s with padding %q: %v", name, pad, err)
			}
		}
	}
}

func TestParsePrivateKeyRefusesAnythingElse(t *testing.T) {
	good := githubtest.PrivateKey()
	ec, _ := x509.MarshalPKCS8PrivateKey(mustEC(t))
	for name, in := range map[string]string{
		"empty":            "",
		"blank":            " \t\n\r\v\f",
		"not base64":       "secret-invalid-base64",
		"newline inside":   good[:64] + "\n" + good[64:],
		"crlf inside":      " " + good[:64] + "\r\n" + good[64:] + " ",
		"space inside":     good[:64] + " " + good[64:],
		"tab inside":       good[:64] + "\t" + good[64:],
		"nbsp around":      " " + good + " ",
		"not pem":          base64.StdEncoding.EncodeToString([]byte("secret-invalid-pem")),
		"ec key":           b64pem("PRIVATE KEY", ec),
		"encrypted pkcs8":  b64pem("ENCRYPTED PRIVATE KEY", []byte("x")),
		"garbage in block": b64pem("RSA PRIVATE KEY", []byte("not der")),
	} {
		_, err := github.ParsePrivateKey(in)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "secret-invalid") || err.Error() != github.ErrInvalidCredentials.Error() {
			t.Errorf("%s: error carries input: %v", name, err)
		}
	}
}

func TestParseAppID(t *testing.T) {
	if id, err := github.ParseAppID(" \t123\n"); err != nil || id != 123 {
		t.Fatalf("got %d, %v", id, err)
	}
	for _, bad := range []string{"", "abc", "0", "-1", "1.5", "1 2"} {
		if _, err := github.ParseAppID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Python required at least 2048 bits (github_auth.py); a smaller key is refused as invalid.
func TestParsePrivateKeyRefusesShortKeys(t *testing.T) {
	for _, bits := range []int{1024, 2047} {
		k, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil {
			t.Fatal(err)
		}
		pkcs8, _ := x509.MarshalPKCS8PrivateKey(k)
		for name, enc := range map[string]string{
			"pkcs1": b64pem("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(k)),
			"pkcs8": b64pem("PRIVATE KEY", pkcs8),
		} {
			if _, err := github.ParsePrivateKey(enc); !errors.Is(err, github.ErrInvalidCredentials) {
				t.Errorf("%d-bit %s: got %v", bits, name, err)
			}
		}
	}
}
