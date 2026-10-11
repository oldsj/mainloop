package config

import (
	"errors"
	"net/url"
	"strings"
)

// validate checks the preview settings. The base URL names the wildcard domain preview hosts sit
// under, and the router address is where the CONNECT tunnels go; neither may carry anything that
// would change where a preview request ends up.
func (p Preview) validate(atespace string) []error {
	var errs []error
	if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		errs = append(errs, errors.New("SUBSTRATE_PREVIEW_BASE_URL must be an http(s) origin"))
	}
	if u, err := url.Parse(p.RouterAddress); err != nil || u.Scheme != "http" || u.Hostname() == "" ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		errs = append(errs, errors.New("SUBSTRATE_ROUTER_ADDRESS must be an http origin"))
	}
	if p.ConnectTimeout.Duration() <= 0 {
		errs = append(errs, errors.New("SUBSTRATE_PREVIEW_CONNECT_TIMEOUT_SECONDS must be positive"))
	}
	// The atespace goes into the ate-target-actor header.
	if !dnsLabel.MatchString(atespace) {
		errs = append(errs, errors.New("KAGENT_ACTOR_ATESPACE must be a DNS label"))
	}
	return errs
}

// PreviewOrigin returns the scheme, lower-case host name and port (empty when the base URL has
// none) of the preview base URL.
func (p Preview) PreviewOrigin() (scheme, host, port string) {
	u, err := url.Parse(p.BaseURL)
	if err != nil {
		return "", "", ""
	}
	return strings.ToLower(u.Scheme), strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), u.Port()
}
