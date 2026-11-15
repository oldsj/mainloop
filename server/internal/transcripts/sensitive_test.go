package transcripts

import "testing"

func TestSensitiveCall(t *testing.T) {
	cases := []struct {
		args any
		want bool
	}{
		{map[string]any{"file_path": "/workspace/app/.env"}, true},
		{map[string]any{"file_path": "/workspace/app/.env.local"}, true},
		{map[string]any{"file_path": "/home/agent/.ssh/config"}, true},
		{map[string]any{"file_path": "/home/agent/.kube/config"}, true},
		{map[string]any{"file_path": "/home/agent/.aws/credentials"}, true},
		{map[string]any{"path": "certs/server.key"}, true},
		{map[string]any{"path": "certs/tls.pem"}, true},
		{map[string]any{"path": "deploy/db-secret.yaml"}, true},
		{map[string]any{"path": "k8s/secrets"}, true},
		{map[string]any{"command": "cat ~/.netrc"}, true},
		{map[string]any{"command": "head -n 5 id_ed25519"}, true},
		{map[string]any{"command": "kubectl -n app get secret db -o yaml"}, true},
		{map[string]any{"command": "kubectl describe secrets"}, true},
		{map[string]any{"command": "gh auth token"}, true},
		{map[string]any{"command": "op read op://vault/item/field"}, true},
		{map[string]any{"cmd": []any{"bash", "-lc", "source .env && run"}}, true},

		{map[string]any{"file_path": "/workspace/server/internal/credentials/store.go"}, false},
		{map[string]any{"file_path": "/workspace/server/internal/sessions/credentials_test.go"}, false},
		{map[string]any{"command": "grep -rn secret server/"}, false},
		{map[string]any{"command": `git commit -m "redact secret values"`}, false},
		{map[string]any{"command": "kubectl get pods"}, false},
		{map[string]any{"command": "go test ./..."}, false},
		{map[string]any{"pattern": "TODO", "path": "web/src"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := sensitiveCall(c.args); got != c.want {
			t.Errorf("%v: got %v, want %v", c.args, got, c.want)
		}
	}
}
