package config

import (
	"strings"
	"testing"
)

func TestPreviewSettingsAreValidated(t *testing.T) {
	good, err := Parse(fixtureEnv(t, "prod.env"))
	if err != nil {
		t.Fatal(err)
	}
	if scheme, host, _ := good.Preview.PreviewOrigin(); scheme == "" || host == "" {
		t.Fatalf("origin = %q %q", scheme, host)
	}
	cases := []struct {
		key, value, want string
	}{
		{"SUBSTRATE_PREVIEW_BASE_URL", "ftp://previews.test", "SUBSTRATE_PREVIEW_BASE_URL"},
		{"SUBSTRATE_PREVIEW_BASE_URL", "https://previews.test/app", "SUBSTRATE_PREVIEW_BASE_URL"},
		{"SUBSTRATE_PREVIEW_BASE_URL", "https://user@previews.test", "SUBSTRATE_PREVIEW_BASE_URL"},
		{"SUBSTRATE_PREVIEW_BASE_URL", "previews.test", "SUBSTRATE_PREVIEW_BASE_URL"},
		{"SUBSTRATE_ROUTER_ADDRESS", "https://router.test", "SUBSTRATE_ROUTER_ADDRESS"},
		{"SUBSTRATE_ROUTER_ADDRESS", "http://router.test/x", "SUBSTRATE_ROUTER_ADDRESS"},
		{"SUBSTRATE_PREVIEW_CONNECT_TIMEOUT_SECONDS", "0", "CONNECT_TIMEOUT"},
		{"KAGENT_ACTOR_ATESPACE", "kagent\r\nx: y", "KAGENT_ACTOR_ATESPACE"},
	}
	for _, c := range cases {
		vars := fixtureEnv(t, "prod.env")
		vars[c.key] = c.value
		if _, err := Parse(vars); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s=%q: err = %v, want mention of %s", c.key, c.value, err, c.want)
		}
	}
	vars := fixtureEnv(t, "prod.env")
	vars["SUBSTRATE_PREVIEW_BASE_URL"] = "https://previews.example.test"
	vars["SUBSTRATE_ROUTER_ADDRESS"] = "http://router.test:8081"
	if _, err := Parse(vars); err != nil {
		t.Errorf("valid preview settings rejected: %v", err)
	}
}
