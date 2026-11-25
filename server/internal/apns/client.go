// Package apns pushes notifications to the owner's registered phones through Apple's Push
// Notification service (docs/specs/push-notifications.md).
//
// It follows the reconciler rule. A sweep fans each push_notifications row out to one
// apns_deliveries row per device with a valid token, keyed by notification and device, so sweeps,
// replicas and restarts never write a second row. The send loop claims a row, rereads whether the
// owner still needs it and whether the device and its token are still good, and posts it with no
// transaction open. APNs has no idempotency key and nothing to read back, so a retry after an
// uncertain attempt sends again with the same apns-collapse-id: the phone replaces the first copy
// instead of showing two.
package apns

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// Apple's hosts, by the environment an app's aps-environment entitlement names.
const (
	ProductionHost  = "https://api.push.apple.com"
	DevelopmentHost = "https://api.sandbox.push.apple.com"
)

const (
	// tokenMaxAge is when a provider token is replaced. Apple refuses one older than an hour and
	// one replaced more often than every 20 minutes.
	tokenMaxAge = 40 * time.Minute
	tokenMinAge = 20 * time.Minute
	// Credential files in the credentials directory.
	keyFile    = "key.p8"
	keyIDFile  = "key-id"
	teamIDFile = "team-id"
)

// ErrNoCredentials means the credentials directory holds none of the files: the Secret is not
// there yet, and APNs stays off.
var ErrNoCredentials = errors.New("apns: no credentials")

// ErrBadToken means APNs refused the device token itself: it is not, or no longer, valid for
// this app. Retrying cannot help; the device must set a new token.
var ErrBadToken = errors.New("apns: device token not valid")

// ErrRejected means APNs refused the notification for a reason a retry cannot change.
var ErrRejected = errors.New("apns: rejected")

var appleID = regexp.MustCompile(`^[A-Z0-9]{10}$`)

// Credentials sign provider tokens: a token-auth key (.p8), its ID and the team's ID. One key
// serves every app of the team; the topic names the app.
type Credentials struct {
	KeyID  string
	TeamID string
	Key    *ecdsa.PrivateKey
}

// LoadCredentials reads key.p8, key-id and team-id from dir. With none of them there it returns
// ErrNoCredentials (the Secret does not exist yet); with some missing, or one wrong, another
// error. Errors never quote the key.
func LoadCredentials(dir string) (*Credentials, error) {
	// Read through an os.Root, so the fixed names resolve inside dir only (the kubelet's
	// ..data symlinks stay inside it).
	root, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("apns: credentials directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	files := map[string][]byte{}
	var missing []string
	for _, name := range []string{keyFile, keyIDFile, teamIDFile} {
		b, err := root.ReadFile(name)
		switch {
		case errors.Is(err, os.ErrNotExist):
			missing = append(missing, name)
		case err != nil:
			return nil, fmt.Errorf("apns: read %s: %w", name, err)
		default:
			files[name] = b
		}
	}
	switch {
	case len(missing) == 3:
		return nil, ErrNoCredentials
	case len(missing) > 0:
		return nil, fmt.Errorf("apns: credentials incomplete: %s missing", strings.Join(missing, ", "))
	}
	pemBytes, keyID, teamID := files[keyFile], files[keyIDFile], files[teamIDFile]
	c := &Credentials{KeyID: strings.TrimSpace(string(keyID)), TeamID: strings.TrimSpace(string(teamID))}
	if !appleID.MatchString(c.KeyID) {
		return nil, errors.New("apns: key-id must be 10 upper-case letters or digits")
	}
	if !appleID.MatchString(c.TeamID) {
		return nil, errors.New("apns: team-id must be 10 upper-case letters or digits")
	}
	if c.Key, err = ParseKey(pemBytes); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseKey reads a .p8 token-auth key: a P-256 private key in PKCS #8 PEM.
func ParseKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("apns: key.p8 is not a PEM private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("apns: key.p8 is not a PKCS #8 key")
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, errors.New("apns: key.p8 is not a P-256 key")
	}
	return ec, nil
}

// Request is one notification to one device.
type Request struct {
	// Environment is "development" or "production".
	Environment string
	// DeviceToken is the device's token, lower-case hex.
	DeviceToken string
	// ID is sent as apns-id: a UUID naming this delivery.
	ID string
	// CollapseID makes a second copy replace the first on the phone (at most 64 bytes).
	CollapseID string
	// Expiration is when APNs stops trying to deliver it to a phone that is offline.
	Expiration time.Time
	// Priority is 10 (deliver now) or 5 (when convenient).
	Priority int
	Payload  []byte
}

// Result is APNs's answer.
type Result struct {
	Status int
	Reason string
}

// Client sends to APNs over HTTP/2 with token-based authentication.
type Client struct {
	Creds *Credentials
	// Topic is the app's bundle ID.
	Topic string
	// HTTP must speak HTTP/2 (Go's default transport does over TLS).
	HTTP *http.Client
	// Hosts overrides Apple's host per environment (tests).
	Hosts map[string]string
	Now   func() time.Time

	mu     sync.Mutex
	token  string
	issued time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) host(env string) (string, error) {
	if h, ok := c.Hosts[env]; ok {
		return h, nil
	}
	switch dbstate.APNsEnvironment(env) {
	case dbstate.APNsProduction:
		return ProductionHost, nil
	case dbstate.APNsDevelopment:
		return DevelopmentHost, nil
	}
	return "", fmt.Errorf("%w: unknown environment %q", ErrRejected, env)
}

// providerToken returns the cached provider token, or signs a new one when it is due.
func (c *Client) providerToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.token != "" && now.Sub(c.issued) < tokenMaxAge {
		return c.token, nil
	}
	t := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": c.Creds.TeamID, "iat": now.Unix()})
	t.Header["kid"] = c.Creds.KeyID
	signed, err := t.SignedString(c.Creds.Key)
	if err != nil {
		return "", fmt.Errorf("apns: provider token: %w", err)
	}
	c.token, c.issued = signed, now
	return signed, nil
}

// expire drops the cached token after APNs called it expired, unless it is too young to replace.
func (c *Client) expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.now().Sub(c.issued) >= tokenMinAge {
		c.token = ""
	}
}

// Send posts r. A nil error means APNs accepted it. Otherwise the error wraps ErrBadToken or
// ErrRejected when a retry cannot help; any other error may succeed later.
func (c *Client) Send(ctx context.Context, r Request) (Result, error) {
	host, err := c.host(r.Environment)
	if err != nil {
		return Result{}, err
	}
	tok, err := c.providerToken()
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/3/device/"+r.DeviceToken, bytes.NewReader(r.Payload))
	if err != nil {
		return Result{}, fmt.Errorf("%w: request: %w", ErrRejected, withoutURL(err))
	}
	req.Header.Set("authorization", "bearer "+tok)
	req.Header.Set("apns-topic", c.Topic)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", strconv.Itoa(r.Priority))
	req.Header.Set("apns-expiration", strconv.FormatInt(r.Expiration.Unix(), 10))
	req.Header.Set("content-type", "application/json")
	if r.ID != "" {
		req.Header.Set("apns-id", r.ID)
	}
	if r.CollapseID != "" {
		req.Header.Set("apns-collapse-id", r.CollapseID)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("apns: POST /3/device/…: %w", withoutURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	res := Result{Status: resp.StatusCode}
	if resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return res, nil
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&body)
	res.Reason = body.Reason
	return res, classify(c, res)
}

// withoutURL drops the request URL from a transport error: its path is the device token, which
// must not reach a stored detail or a log.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// classify turns a refusal into an error (docs: "Handling notification responses from APNs").
func classify(c *Client, r Result) error {
	why := fmt.Sprintf("status %d %s", r.Status, r.Reason)
	switch {
	case r.Status == http.StatusGone,
		r.Reason == "BadDeviceToken", r.Reason == "Unregistered", r.Reason == "DeviceTokenNotForTopic":
		return fmt.Errorf("%w: %s", ErrBadToken, why)
	case r.Reason == "ExpiredProviderToken":
		c.expire()
		return fmt.Errorf("apns: %s", why)
	case r.Status == http.StatusTooManyRequests, r.Status >= 500,
		r.Reason == "InvalidProviderToken", r.Reason == "TooManyProviderTokenUpdates":
		// A wrong key or team is a configuration error, fixed by redeploying: keep retrying until
		// attempts run out rather than dropping the notification at once.
		return fmt.Errorf("apns: %s", why)
	default:
		return fmt.Errorf("%w: %s", ErrRejected, why)
	}
}
