package preview

import (
	"os"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// Mined from backend/tests/runtime/test_preview_proxy.py: the host parser cases.
func TestParseHost(t *testing.T) {
	long := strings.Repeat("a", 60)
	cases := []struct {
		name, host, base string
		want             Host
		ok               bool
	}{
		{"plain", "3000--ws1--preview.localhost:8001", "http://localhost:8001", Host{3000, "ws1"}, true},
		{"case and trailing dot", "3000--WS1--Preview.LocalHost.:8001", "http://localhost:8001", Host{3000, "ws1"}, true},
		{"https default port", "80--ws1--preview.example.com", "https://example.com", Host{80, "ws1"}, true},
		{"explicit default port is not the same host", "80--ws1--preview.example.com:443", "https://example.com", Host{}, false},
		{"internal double dash in workspace", "3000--a--b--preview.localhost:8001", "http://localhost:8001", Host{3000, "a--b"}, true},
		{"max port", "65535--ws1--preview.localhost:8001", "http://localhost:8001", Host{65535, "ws1"}, true},
		{"port too large", "65536--ws1--preview.localhost:8001", "http://localhost:8001", Host{}, false},
		{"port zero", "0--ws1--preview.localhost:8001", "http://localhost:8001", Host{}, false},
		{"leading zero", "03000--ws1--preview.localhost:8001", "http://localhost:8001", Host{}, false},
		{"port not a number", "web--ws1--preview.localhost:8001", "http://localhost:8001", Host{}, false},
		{"wrong port on host", "3000--ws1--preview.localhost:9000", "http://localhost:8001", Host{}, false},
		{"missing port on host", "3000--ws1--preview.localhost", "http://localhost:8001", Host{}, false},
		{"other domain", "3000--ws1--preview.evil.test:8001", "http://localhost:8001", Host{}, false},
		{"suffix trick", "3000--ws1--preview.localhost.evil.test:8001", "http://localhost:8001", Host{}, false},
		{"extra label", "x.3000--ws1--preview.localhost:8001", "http://localhost:8001", Host{}, false},
		{"api host", "localhost:8001", "http://localhost:8001", Host{}, false},
		{"workspace edge dash", "3000---ws1--preview.localhost:8001", "http://localhost:8001", Host{}, false},
		{"empty workspace", "3000----preview.localhost:8001", "http://localhost:8001", Host{}, false},
		{"label over 63", "3000--" + long + "--preview.localhost:8001", "http://localhost:8001", Host{}, false},
		{"userinfo", "u@3000--ws1--preview.localhost:8001", "http://localhost:8001", Host{}, false},
		{"path", "3000--ws1--preview.localhost:8001/x", "http://localhost:8001", Host{}, false},
		{"space", "3000--ws1 --preview.localhost:8001", "http://localhost:8001", Host{}, false},
		{"empty", "", "http://localhost:8001", Host{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseHost(c.host, c.base)
			if ok != c.ok || got != c.want {
				t.Fatalf("ParseHost(%q, %q) = %+v, %v; want %+v, %v", c.host, c.base, got, ok, c.want, c.ok)
			}
		})
	}
}
