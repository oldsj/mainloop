package auth_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oldsj/mainloop/server/internal/auth"
)

func TestTokenFileRereadsTheFile(t *testing.T) {
	if auth.TokenFile("") != nil {
		t.Fatal("no path must mean no token source")
	}
	path := filepath.Join(t.TempDir(), "token")
	src := auth.TokenFile(path)
	if _, err := src(); err == nil {
		t.Fatal("a missing file must be an error")
	}
	for _, tok := range []string{"first", "rotated"} {
		if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := src(); err != nil || got != tok {
			t.Fatalf("got %q, %v; want %q", got, err, tok)
		}
	}
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := src(); err == nil {
		t.Fatal("an empty file must be an error, not an empty bearer")
	}
}
