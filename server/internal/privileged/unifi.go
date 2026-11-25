package privileged

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/redact"
)

// http.unifi sends one approved request to the UniFi console's Network application, with the
// executor's UniFi API key: the owner's network changes that no typed action covers.
const TypeUniFi = "http.unifi"

// Bounds of http.unifi.
const (
	MaxUniFiPathBytes = 2 << 10
	MaxUniFiBodyBytes = 64 << 10
	// unifiTimeout bounds one request to the console, the TLS handshake included.
	unifiTimeout = 30 * time.Second
)

// UniFiArgs are http.unifi's canonical arguments: one request to the console's Network
// application. Path is the console's own (/proxy/network/integration/v1/...,
// /proxy/network/api/s/<site>/..., /proxy/network/v2/api/...), with an optional query. Body is the
// request's JSON, for POST, PUT and PATCH, as text with sorted keys and no spaces: the card shows,
// and the console receives, exactly these bytes. (Kept as text: the args column is jsonb, which
// would reorder a nested value's keys.)
type UniFiArgs struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body,omitempty"`
}

// unifiInput is what the requester sends: the body as a JSON value.
type unifiInput struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// unifiMethods are the methods http.unifi sends.
var unifiMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

var (
	// unifiPathRE is the path: the Network application's APIs only, plain segments.
	unifiPathRE = regexp.MustCompile(`^/proxy/network/(?:integration|api|v2)(?:/[A-Za-z0-9._~-]+)+$`)
	// unifiQueryRE is a query: printable ASCII, no space, fragment or second '?'.
	unifiQueryRE = regexp.MustCompile(`^[A-Za-z0-9._~=&,:;!*'()+%/-]*$`)
)

// unifiHeld are path segments (by substring) whose endpoints stay the owner's own: the console's
// administrators, API keys and backups (a backup holds every secret on the console), and site
// management (which invites and removes administrators).
var unifiHeld = []string{"admin", "apikey", "api-key", "api_key", "backup", "sitemgr"}

func normalizeUniFi(raw json.RawMessage) (any, error) {
	var in unifiInput
	if err := decodeStrict(raw, &in); err != nil {
		return nil, err
	}
	a := UniFiArgs{Method: strings.ToUpper(strings.TrimSpace(in.Method)), Path: in.Path}
	if !slices.Contains(unifiMethods, a.Method) {
		return nil, invalidf("method is one of %s", strings.Join(unifiMethods, ", "))
	}
	if len(a.Path) > MaxUniFiPathBytes {
		return nil, invalidf("path is at most %d KiB", MaxUniFiPathBytes>>10)
	}
	path, query, _ := strings.Cut(a.Path, "?")
	switch {
	case !unifiPathRE.MatchString(path):
		return nil, invalidf("path is the console's Network application path, /proxy/network/integration/..., /proxy/network/api/... " +
			"or /proxy/network/v2/api/..., in plain segments (letters, digits, . _ ~ -; no empty, . or .. segment, no percent-encoding)")
	case !unifiQueryRE.MatchString(query):
		return nil, invalidf("the query is printable ASCII with no space or fragment")
	}
	for seg := range strings.SplitSeq(strings.ToLower(path), "/") {
		if seg == "." || seg == ".." {
			return nil, invalidf("path has a . or .. segment")
		}
		for _, held := range unifiHeld {
			if strings.Contains(seg, held) {
				return nil, invalidf("the console's administrators, API keys, site management and backups are the owner's own (%q)", seg)
			}
		}
	}
	if _, err := url.ParseQuery(query); err != nil {
		return nil, invalidf("the query does not parse: %s", err.Error())
	}
	if redact.Credential(a.Path) {
		return nil, invalidf("path looks like it holds a credential (contains_secret)")
	}
	body, err := normalizeUniFiBody(a.Method, in.Body)
	if err != nil {
		return nil, err
	}
	a.Body = string(body)
	return a, nil
}

// normalizeUniFiBody checks a request's body and returns its canonical form.
func normalizeUniFiBody(method string, raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	if method == "GET" || method == "DELETE" {
		return nil, invalidf("a %s has no body", method)
	}
	if len(raw) > MaxUniFiBodyBytes {
		return nil, invalidf("body is at most %d KiB", MaxUniFiBodyBytes>>10)
	}
	v, err := decodeJSONNumbers(raw)
	if err != nil {
		return nil, invalidf("body is JSON: %s", err.Error())
	}
	switch v.(type) {
	case map[string]any, []any:
	default:
		return nil, invalidf("body is a JSON object or array")
	}
	if key := secretField(v); key != "" {
		return nil, invalidf("body sets %s, a secret field: the owner sets secrets on the console (contains_secret)", key)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, invalidf("body is JSON: %s", err.Error())
	}
	if redact.Credential(string(b)) {
		return nil, invalidf("body holds what looks like a credential (contains_secret)")
	}
	return b, nil
}

func decodeJSONNumbers(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if d.More() {
		return nil, errors.New("more than one JSON value")
	}
	return v, nil
}

// unifiSecretWords mark a field that holds a secret, by name (lowercased, without _ and -): the
// console's x_ fields (x_passphrase, x_password, x_authkey...) by their prefix, and these.
var unifiSecretWords = []string{"password", "passphrase", "passwd", "secret", "psk", "preshared", "privatekey", "token",
	"apikey", "authkey", "credential", "radiussecret"}

func isSecretField(key string) bool {
	k := strings.ToLower(key)
	if strings.HasPrefix(k, "x_") {
		return true
	}
	k = strings.NewReplacer("_", "", "-", "").Replace(k)
	return slices.ContainsFunc(unifiSecretWords, func(w string) bool { return strings.Contains(k, w) })
}

// secretField is the name of the first field in v that holds a secret and is set, or "".
func secretField(v any) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if isSecretField(k) && t[k] != nil && t[k] != "" {
				return k
			}
			if s := secretField(t[k]); s != "" {
				return s
			}
		}
	case []any:
		for _, e := range t {
			if s := secretField(e); s != "" {
				return s
			}
		}
	}
	return ""
}

// redactSecretFields replaces every set secret field's value with "[redacted]".
func redactSecretFields(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if isSecretField(k) && e != nil && e != "" {
				t[k] = "[redacted]"
				continue
			}
			t[k] = redactSecretFields(e)
		}
	case []any:
		for i, e := range t {
			t[i] = redactSecretFields(e)
		}
	}
	return v
}

// unifiText is what the card and the agent see of a console's answer: JSON with its secret fields
// redacted, keys sorted and indented; other text through the session redactor; and in both, no
// trace of the API key.
func unifiText(body []byte, apiKey string) string {
	text := string(body)
	if v, err := decodeJSONNumbers(body); err == nil {
		if b, err := json.MarshalIndent(redactSecretFields(v), "", "  "); err == nil {
			text = string(b)
		}
	}
	return strings.TrimSpace(string(cleanOutput([]byte(text), []string{apiKey})))
}

// unifiWords are the request's words, lowercased, for the card's best-effort tags. Commands such
// as forget-sta and power-cycle split into their words.
func unifiWords(a UniFiArgs) []string {
	return strings.FieldsFunc(strings.ToLower(a.Path+" "+string(a.Body)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// unifiDeleteWords and unifiRestartWords mark a request that may delete, forget or reset, or
// restart or upgrade devices: a word scan for the tags and a delete's expiry, not a guard.
var (
	unifiDeleteWords  = []string{"delete", "remove", "forget", "reset", "factory", "destroy", "purge"}
	unifiRestartWords = []string{"restart", "reboot", "cycle", "upgrade", "provision", "adopt"}
)

func unifiMatches(a UniFiArgs, list []string) []string {
	var out []string
	for _, w := range unifiWords(a) {
		if slices.Contains(list, w) && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

func unifiExpiry(args any) time.Duration {
	if a, ok := args.(UniFiArgs); ok && (a.Method == "DELETE" || len(unifiMatches(a, unifiDeleteWords)) > 0) {
		return DeleteExpiry
	}
	return UndecidedExpiry
}

// unifiPreviews says whether the request has a preview: a GET of the same path shows the object a
// PUT, PATCH or DELETE changes. A GET is its own read, and a POST creates or commands.
func unifiPreviews(method string) bool {
	return method == "PUT" || method == "PATCH" || method == "DELETE"
}

// UniFiFacts are http.unifi's frozen facts: the console as the executor reached it and, for a PUT,
// PATCH or DELETE, the object as a GET of the same path showed it.
type UniFiFacts struct {
	Address string `json:"address"`
	// Certificate is the console's pinned TLS certificate (SHA256 fingerprint); the executor checks
	// it again just before the request.
	Certificate string `json:"certificate"`
	// Preview is PreviewRead (Current is the GET's answer, redacted) or PreviewNone.
	Preview   string    `json:"preview"`
	Current   string    `json:"current,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// unifiAgentFacts leave out the console's address, and the current object, which the executor
// read with the console's key before anyone approved.
func unifiAgentFacts(facts json.RawMessage) json.RawMessage {
	var f UniFiFacts
	if err := json.Unmarshal(facts, &f); err != nil {
		return nil
	}
	note := ""
	if f.Current != "" {
		note = "the object as the console showed it is on the owner's card only"
	}
	b, err := json.Marshal(struct {
		Certificate string    `json:"certificate"`
		Preview     string    `json:"preview"`
		Note        string    `json:"note,omitempty"`
		CheckedAt   time.Time `json:"checked_at"`
	}{f.Certificate, f.Preview, note, f.CheckedAt})
	if err != nil {
		return nil
	}
	return b
}

// UniFiHello is the console as the executor reached it, before sending anything.
type UniFiHello struct {
	Address     string
	Certificate string
}

// UniFiRequest is one request. Expect, if set, is the console as the owner saw it: nothing is
// sent unless the console reached still matches.
type UniFiRequest struct {
	Method string
	Path   string
	Body   []byte
	Expect *UniFiHello
}

// UniFiResponse is the console's answer. APIKey is the key the request carried, for redaction.
type UniFiResponse struct {
	Status int
	Body   []byte
	APIKey string
}

// UniFi reaches the console with the executor's API key.
type UniFi interface {
	// Hello connects, checks the console's certificate against the pinned one, and closes:
	// nothing is sent. A *refusal says why the console cannot be reached as asked.
	Hello(ctx context.Context) (UniFiHello, error)
	// Do sends one request and reads the answer. An error before any byte of the request was
	// sent is a *NothingRan.
	Do(ctx context.Context, req UniFiRequest) (UniFiResponse, error)
}

// UniFiResult is what an http.unifi run stores and the agent reads: the console's status and its
// answer, redacted and bounded.
type UniFiResult struct {
	Status int    `json:"status"`
	Body   string `json:"body,omitempty"`
}

// unifiGet reads the path as the preview shows it: a 2xx answer, redacted, within the card's bound.
func unifiGet(ctx context.Context, u UniFi, path string, expect *UniFiHello) (string, error) {
	res, err := u.Do(ctx, UniFiRequest{Method: "GET", Path: path, Expect: expect})
	if err != nil {
		return "", err
	}
	text := unifiText(res.Body, res.APIKey)
	switch {
	case res.Status == 404:
		return "", refuse("the console has nothing at that path (GET answered 404)")
	case res.Status < 200 || res.Status > 299:
		return "", refuse("a GET of the path answered %d, so the card cannot show what the request changes", res.Status)
	case len(text) > MaxPreflightOutput:
		return "", refuse("a GET of the path answers more than the %d KiB a card shows whole; name one object", MaxPreflightOutput>>10)
	}
	return text, nil
}

func preflightUniFi(ctx context.Context, s *Service, raw json.RawMessage, now time.Time) (any, error) {
	var a UniFiArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if s.UniFi == nil {
		return nil, refuse("this executor has no UniFi access")
	}
	h, err := s.UniFi.Hello(ctx)
	if err != nil {
		return nil, err
	}
	f := UniFiFacts{Address: h.Address, Certificate: h.Certificate, Preview: PreviewNone, CheckedAt: now}
	if unifiPreviews(a.Method) {
		if f.Current, err = unifiGet(ctx, s.UniFi, a.Path, &h); err != nil {
			return nil, err
		}
		f.Preview = PreviewRead
	}
	return f, nil
}

func renderUniFi(r dbq.PrivilegedAction, ctx map[string]any) (string, string, map[string]any, error) {
	var f UniFiFacts
	var a UniFiArgs
	if json.Unmarshal(r.FrozenFacts, &f) != nil || json.Unmarshal(r.Args, &a) != nil {
		return "", "", nil, fmt.Errorf("privileged: %s facts cannot be read", r.Type)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Sends this once to the UniFi console (%s), with the executor's UniFi API key:\n", f.Address)
	b.WriteString(fenced("http", a.Method+" "+a.Path))
	if len(a.Body) > 0 {
		var pretty bytes.Buffer
		if json.Indent(&pretty, []byte(a.Body), "", "  ") != nil {
			pretty.Reset()
			pretty.WriteString(a.Body)
		}
		b.WriteString("With this body:\n" + fenced("json", pretty.String()))
	}
	switch {
	case f.Preview == PreviewRead:
		b.WriteString("\nWhat is there now (a GET of the same path; secret fields redacted):\n" + fenced("json", f.Current))
		b.WriteString("Just before the request, the executor reads it again and sends nothing if it changed. ")
	case a.Method == "GET":
		b.WriteString("\nIt only reads. ")
	default:
		b.WriteString("\nNothing can preview it. ")
	}
	fmt.Fprintf(&b, "The console presented its pinned certificate (%s); the executor checks that again just before the request. ", f.Certificate)
	b.WriteString("There is no retry: if the answer is lost, the outcome is uncertain and it is not sent again. ")
	b.WriteString("The console's answer (status and body, secret fields redacted, bounded) goes back to the agent and is stored with the action.")
	tags := []string{"UniFi API, " + a.Method}
	if a.Method != "GET" {
		tags = append(tags, "changes the network, which can cut access to it, Mainloop's included")
	}
	if w := unifiMatches(a, unifiDeleteWords); a.Method == "DELETE" || len(w) > 0 {
		what := "DELETE"
		if len(w) > 0 {
			what = strings.Join(w, ", ")
		}
		tags = append([]string{"destructive: may delete, forget or reset (" + what + ")"}, tags...)
	}
	if w := unifiMatches(a, unifiRestartWords); len(w) > 0 {
		tags = append(tags, "may restart or reprovision devices ("+strings.Join(w, ", ")+")")
	}
	if f.Preview == PreviewNone && a.Method != "GET" {
		tags = append(tags, "no preview")
	}
	fmt.Fprintf(&b, "\nRisk: %s. (The delete and restart tags come from a word scan, not a guarantee.)", strings.Join(tags, ", "))
	return fmt.Sprintf("Send %s to the UniFi console", a.Method), b.String(), ctx, nil
}

// unifi runs http.unifi: the console's certificate and, for a PUT, PATCH or DELETE, the object
// again; then the request once.
func (r *Runner) unifi(ctx context.Context, row dbq.PrivilegedAction) outcome {
	var f UniFiFacts
	var a UniFiArgs
	if json.Unmarshal(row.FrozenFacts, &f) != nil || json.Unmarshal(row.Args, &a) != nil {
		return outcome{state: StateFailed, err: "the frozen facts cannot be read"}
	}
	if r.UniFi == nil {
		return outcome{state: StateFailed, err: "this executor no longer reaches the UniFi console"}
	}
	if done, out := r.startedBefore(ctx, row); done {
		return out
	}
	h, err := r.UniFi.Hello(ctx)
	var refused *refusal
	switch {
	case errors.As(err, &refused):
		return outcome{state: StateStale, err: "the UniFi console is not as the owner saw it: " + refused.msg + "; nothing was sent"}
	case err != nil:
		return retry(err) // nothing sent yet
	case h != UniFiHello{Address: f.Address, Certificate: f.Certificate}:
		return outcome{state: StateStale, err: "the UniFi console's pinned certificate or address changed since the owner saw it; nothing was sent"}
	}
	if f.Preview == PreviewRead {
		now, err := unifiGet(ctx, r.UniFi, a.Path, &h)
		switch {
		case errors.As(err, &refused):
			return outcome{state: StateStale, err: "the object is not as the owner saw it: " + refused.msg + "; nothing was sent"}
		case err != nil:
			return retry(err) // a read only
		case now != f.Current:
			return outcome{state: StateStale, err: "the object changed since the owner saw it; nothing was sent"}
		}
	}
	if err := r.extendLease(ctx, row, unifiTimeout); err != nil {
		return retry(err)
	}
	if err := r.event(ctx, row.ID, stepStarted, boundText([]byte("unifi "+a.Method+" "+a.Path), 1<<10)); err != nil {
		return retry(err)
	}
	res, err := r.UniFi.Do(ctx, UniFiRequest{Method: a.Method, Path: a.Path, Body: []byte(a.Body), Expect: &h})
	var before *NothingRan
	switch {
	case errors.As(err, &before):
		// Final, not retried: the started event is written, and a retry must never guess.
		return outcome{state: StateFailed, err: "nothing was sent to the UniFi console: " + safeError(before.Err)}
	case err != nil && a.Method == "GET":
		return outcome{state: StateFailed, err: "the read did not finish: " + safeError(err) + "; a GET changes nothing"}
	case err != nil:
		return outcome{state: StateUncertain, err: "the request was sent but its answer was lost: " + safeError(err) +
			"; the console may have applied it"}
	}
	result := UniFiResult{Status: res.Status, Body: boundText([]byte(unifiText(res.Body, res.APIKey)), MaxCommandOutput)}
	if res.Status < 200 || res.Status > 299 {
		return outcome{state: StateFailed, result: result, err: fmt.Sprintf("the UniFi console answered %d", res.Status)}
	}
	return outcome{state: StateSucceeded, result: result}
}
