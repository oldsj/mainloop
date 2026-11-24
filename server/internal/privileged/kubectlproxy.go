package privileged

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	kjson "sigs.k8s.io/json"
)

// maxProxyBody bounds a body the proxy reads to rewrite it (a DELETE's options, an eviction). The
// API server's own limit is 3 MiB; those bodies are a few hundred bytes.
const maxProxyBody = 1 << 20

// kubeProxy stands between kubectl and the API server for one run. kubectl's kubeconfig points at
// it over loopback with a token of its own, made for the run and worth nothing elsewhere; the proxy
// checks that token, drops it, and adds the real one. So kubectl never holds the cluster-admin
// credential, and a flag or kubeconfig that sends kubectl elsewhere sends nothing of value.
//
// It also refuses what no command may do, whatever kubectl was asked: impersonation, Secrets, the
// executor's own namespace and roles, token requests, admission objects, the proxy subresources.
// In preflight, before the owner has approved anything, every write is made a server dry run and
// streams (exec, attach, port-forward) are refused.
type kubeProxy struct {
	own       string
	preflight bool
	runToken  string
	token     string
	proxy     *httputil.ReverseProxy
	// tls serves a certificate made for this run (kubectl sends no token over plain HTTP); caPEM
	// is that certificate, for kubectl's kubeconfig.
	tls   *tls.Config
	caPEM []byte
}

// loopbackCert makes a self-signed certificate for 127.0.0.1 that lives as long as one run.
func loopbackCert() (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "mainloop-executor kubectl proxy"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(MaxCommandTimeout + time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func newKubeProxy(cfg *rest.Config, own string, preflight bool) (*kubeProxy, error) {
	target, err := url.Parse(cfg.Host)
	if err != nil || target.Host == "" {
		return nil, fmt.Errorf("kubectl proxy: API server address %q: %w", cfg.Host, err)
	}
	if target.Scheme == "" {
		target.Scheme = "https"
	}
	// The transport carries the TLS settings only; the proxy sets the credential itself.
	bare := rest.AnonymousClientConfig(cfg)
	rt, err := rest.TransportFor(bare)
	if err != nil {
		return nil, fmt.Errorf("kubectl proxy: transport: %w", err)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	cert, certPEM, err := loopbackCert()
	if err != nil {
		return nil, fmt.Errorf("kubectl proxy: certificate: %w", err)
	}
	p := &kubeProxy{own: own, preflight: preflight, runToken: hex.EncodeToString(b), token: cfg.BearerToken,
		tls: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, caPEM: certPEM}
	p.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			for k := range pr.Out.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(k), "Impersonate-") {
					pr.Out.Header.Del(k)
				}
			}
			pr.Out.Header.Set("Authorization", "Bearer "+p.token)
		},
		Transport:     rt,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			status(w, http.StatusBadGateway, "the executor could not reach the API server")
		},
	}
	return p, nil
}

func (p *kubeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(p.runToken)) != 1 {
		status(w, http.StatusUnauthorized, "not this run's token")
		return
	}
	if why := p.refuse(r); why != "" {
		status(w, http.StatusForbidden, "refused by the executor: "+why)
		return
	}
	if p.preflight {
		if err := dryRunRequest(r); err != nil {
			status(w, http.StatusForbidden, "refused by the executor: "+err.Error())
			return
		}
	}
	p.proxy.ServeHTTP(w, r)
}

// apiPath is a request path as the API server routes it.
type apiPath struct {
	resourcePath          bool // under /api or /apis, past discovery
	group                 string
	namespace             string
	resource, name, sub   string
	namespacedCollections bool
}

// parseAPIPath reads /api/v1/... and /apis/<group>/<version>/... Any other path (discovery,
// /version, /openapi) is not a resource path.
func parseAPIPath(path string) apiPath {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	var p apiPath
	switch {
	case len(segs) >= 3 && segs[0] == "api":
		segs = segs[2:]
	case len(segs) >= 4 && segs[0] == "apis":
		p.group, segs = segs[1], segs[3:]
	default:
		return p
	}
	if segs[0] == "watch" { // the old watch paths: /api/v1/watch/namespaces/x/pods
		segs = segs[1:]
	}
	if len(segs) == 0 || segs[0] == "" {
		return p
	}
	p.resourcePath = true
	if segs[0] == "namespaces" && len(segs) >= 3 {
		p.namespace, segs = segs[1], segs[2:]
		p.namespacedCollections = true
	}
	p.resource = segs[0]
	if len(segs) > 1 {
		p.name = segs[1]
	}
	if len(segs) > 2 {
		p.sub = strings.Join(segs[2:], "/")
	}
	return p
}

func reads(method string) bool { return method == http.MethodGet || method == http.MethodHead }

// refuse says why a request may not pass, or "".
func (p *kubeProxy) refuse(r *http.Request) string {
	raw := r.URL.EscapedPath()
	switch {
	case strings.ContainsAny(raw, "%\\") || strings.Contains(raw, "//") || dotSegment(raw):
		return "an escaped or dotted path"
	case !slices.Contains([]string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}, r.Method):
		return r.Method + " requests"
	}
	for k := range r.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "Impersonate-") {
			return "impersonation"
		}
	}
	upgrade := r.Header.Get("Upgrade") != "" || strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
	if upgrade && p.preflight {
		return "a stream before the owner approves the command"
	}
	a := parseAPIPath(r.URL.Path)
	if !a.resourcePath {
		if !reads(r.Method) {
			return "a write outside the resource API"
		}
		return ""
	}
	write := !reads(r.Method)
	switch {
	case a.namespace == p.own || a.resource == "namespaces" && a.name == p.own && !a.namespacedCollections:
		return "the executor's own namespace"
	case a.resource == "namespaces" && a.name == "" && r.Method == http.MethodDelete:
		return "deleting every namespace"
	case a.group == "" && a.resource == "secrets":
		return "Secrets"
	case a.sub == "token" || a.sub == "proxy" || strings.HasPrefix(a.sub, "proxy/"):
		return "the " + a.resource + "/" + a.sub + " subresource"
	case a.group == "admissionregistration.k8s.io" && write:
		return "admission policies and webhooks; change them through GitOps"
	case a.group == "rbac.authorization.k8s.io" && write && (strings.HasPrefix(a.name, p.own+"-") || r.Method == http.MethodDelete && a.name == ""):
		return "the executor's own roles and bindings"
	case upgrade && a.sub != "exec":
		return "a stream other than exec"
	}
	return ""
}

// dotSegment says whether a path has a "." or ".." segment.
func dotSegment(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

var errNotDryRun = errors.New("a write the executor cannot make a dry run")

// dryRunRequest makes a write a server dry run. Create, update and patch take their options from
// the query. A DELETE with a body takes them from the body and ignores the query, so the body is
// decoded, given dryRun All and written back as JSON; one the proxy cannot decode is refused. An
// eviction carries DeleteOptions in its body and is made dry in both places.
func dryRunRequest(r *http.Request) error {
	if reads(r.Method) {
		return nil
	}
	a := parseAPIPath(r.URL.Path)
	if !a.resourcePath {
		return errNotDryRun
	}
	q := r.URL.Query()
	q.Del("dryRun")
	q.Set("dryRun", metav1.DryRunAll)
	r.URL.RawQuery = q.Encode()
	switch {
	case r.Method == http.MethodDelete:
		body, err := readBody(r)
		if err != nil || len(body) == 0 {
			return err
		}
		var opts metav1.DeleteOptions
		if err := decodeBody(r, body, &opts); err != nil {
			return err
		}
		opts.DryRun = []string{metav1.DryRunAll}
		return setBody(r, opts)
	case r.Method == http.MethodPost && a.resource == "pods" && a.sub == "eviction":
		body, err := readBody(r)
		if err != nil {
			return err
		}
		var ev policyv1.Eviction
		if err := decodeBody(r, body, &ev); err != nil {
			return err
		}
		if ev.DeleteOptions == nil {
			ev.DeleteOptions = &metav1.DeleteOptions{}
		}
		ev.DeleteOptions.DryRun = []string{metav1.DryRunAll}
		return setBody(r, ev)
	case a.sub != "" && a.sub != "status" && a.sub != "scale":
		return errNotDryRun
	}
	return nil
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxProxyBody+1))
	_ = r.Body.Close()
	if err != nil || len(body) > maxProxyBody {
		return nil, errNotDryRun
	}
	return body, nil
}

// decodeBody reads JSON only, with the field names matched exactly as the API server matches them.
func decodeBody(r *http.Request, body []byte, into any) error {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return errNotDryRun
	}
	strict, err := kjson.UnmarshalStrict(body, into, kjson.DisallowUnknownFields)
	if err != nil || len(strict) > 0 {
		return errNotDryRun
	}
	return nil
}

func setBody(r *http.Request, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.Header.Set("Content-Length", strconv.Itoa(len(b)))
	r.Header.Set("Content-Type", "application/json")
	return nil
}

// status writes a Kubernetes Status, which kubectl prints as "Error from server (...): message".
func status(w http.ResponseWriter, code int32, msg string) {
	reason := metav1.StatusReasonForbidden
	switch code {
	case http.StatusUnauthorized:
		reason = metav1.StatusReasonUnauthorized
	case http.StatusBadGateway:
		reason = metav1.StatusReasonServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(int(code))
	_ = json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusFailure,
		Message: msg, Reason: reason, Code: code,
	})
}
