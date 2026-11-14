// Package auth verifies who calls the owner API (docs/specs/authentication.md). Every request to an
// operation the contract secures must carry the owner's Pocket ID token (a bearer, or the gateway's
// login cookie) or an allowed Kubernetes ServiceAccount token. Network position, loopback included,
// is never identity: a port-forward arrives on the pod's loopback.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/golang-jwt/jwt/v4"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/httpx"
)

// Mode is how the middleware acts on what it finds.
type Mode string

const (
	// ModeOff skips verification. Development only.
	ModeOff Mode = "off"
	// ModeObserve verifies and logs what enforce would refuse, and refuses nothing.
	ModeObserve Mode = "observe"
	// ModeEnforce refuses a request without an allowed identity.
	ModeEnforce Mode = "enforce"
)

// Scope is what a principal may call.
type Scope string

const (
	// ScopeOwner may call every operation.
	ScopeOwner Scope = "owner"
	// ScopeRead may call GET and HEAD operations, except the interactive live session.
	ScopeRead Scope = "read"
	// ScopeCanary may call the session operations the canary uses (docs/specs/canary.md).
	ScopeCanary Scope = "canary"
)

// canaryOperations are the operationIds ScopeCanary allows, lower-cased (the embedded spec
// capitalizes them): one hidden session it creates, pings, reads, restarts and retires.
var canaryOperations = map[string]bool{
	"listsessions": true, "createsession": true, "sendsessionmessage": true,
	"getsessionconversation": true, "getsessionnative": true, "restartsession": true,
	"stopturn": true, "cancelsession": true, "archivesession": true,
}

// liveOperation is the interactive live session, which read scope may not open.
const liveOperation = "getsessionlive"

// ClientHeader must accompany every owner write. A page on another origin cannot add it without a
// CORS preflight, which only the frontend origin passes.
const ClientHeader = "X-Mainloop-Client"

// Kinds of principal.
const (
	KindOwner          = "owner"
	KindServiceAccount = "service_account"
)

// Principal is the verified caller of a request.
type Principal struct {
	// Kind is KindOwner or KindServiceAccount.
	Kind string
	// Name is the owner's subject or the ServiceAccount's namespace/name.
	Name  string
	Scope Scope
	// Via is "bearer" or "cookie".
	Via string
}

type ctxKey struct{}

// FromContext returns the request's verified principal, if any.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// WithPrincipal returns ctx carrying p (tests and in-process callers).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// Authenticator holds the verifiers and the policy.
type Authenticator struct {
	Mode         Mode
	OIDC         *OIDC // nil: owner tokens are not accepted
	OwnerSubject string
	OwnerGroup   string
	Cookie       string
	// ServiceAccounts is nil when ServiceAccount tokens are not accepted.
	ServiceAccounts *ServiceAccounts
	Scopes          map[string]Scope // namespace/name → scope
	FrontendOrigin  string
	Dev             bool
	Log             *slog.Logger
	Now             func() time.Time

	mu     sync.Mutex
	logged map[string]time.Time
}

// New builds the Authenticator of serve-api from its settings. reviewer may be nil outside the
// cluster, and then ServiceAccount tokens are refused. Outside development the mode must be set,
// and only observe or enforce; enforce also needs the owner's issuer, so the owner can sign in.
func New(cfg *config.Config, reviewer TokenReviewer, client *http.Client, log *slog.Logger) (*Authenticator, error) {
	a := cfg.Auth
	mode := Mode(a.Mode)
	switch {
	case mode == "" && cfg.IsDev():
		mode = ModeOff
	case mode == "":
		return nil, errors.New("auth: MAINLOOP_AUTH_MODE must be set to observe or enforce")
	case mode == ModeOff && !cfg.IsDev():
		return nil, errors.New("auth: MAINLOOP_AUTH_MODE=off is for development only; use observe")
	case mode == ModeEnforce && a.OIDCIssuer == "":
		return nil, errors.New("auth: enforce needs MAINLOOP_AUTH_OIDC_ISSUER, or the owner cannot sign in")
	}
	au := &Authenticator{
		Mode: mode, OwnerSubject: a.OwnerSubject, OwnerGroup: a.OwnerGroup, Cookie: a.OIDCCookie,
		FrontendOrigin: cfg.FrontendOrigin(), Dev: cfg.IsDev(), Log: log, Scopes: map[string]Scope{},
	}
	if a.OIDCIssuer != "" {
		au.OIDC = &OIDC{Issuer: a.OIDCIssuer, Audience: a.OIDCAudience, Client: client}
	}
	for name, scope := range a.ServiceAccounts {
		au.Scopes[name] = Scope(scope)
	}
	if reviewer != nil && len(au.Scopes) > 0 {
		au.ServiceAccounts = &ServiceAccounts{Audience: a.ServiceAccountAudience, Reviewer: reviewer}
	}
	return au, nil
}

// Middleware returns the owner API's authentication layer. spec is the contract: an operation
// whose security list is empty (GET /health) is public, every other one is secured. A request that
// matches no operation passes on, so the validator answers it 404 or 405.
func (a *Authenticator) Middleware(spec *openapi3.T) (func(http.Handler) http.Handler, error) {
	if a == nil || a.Mode == ModeOff {
		return func(next http.Handler) http.Handler { return next }, nil
	}
	spec.Servers = nil // match on path alone; the Host guard owns hosts
	router, err := gorillamux.NewRouter(spec)
	if err != nil {
		return nil, fmt.Errorf("auth: router: %w", err)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ref := a.decide(r, router)
			if ref != nil {
				a.report(r, ref, p)
				if a.Mode == ModeEnforce {
					ref.write(w)
					return
				}
			} else if p != nil {
				a.seen(p)
			}
			if p != nil {
				r = r.WithContext(WithPrincipal(r.Context(), *p))
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// refusal is why a request is refused. reason is logged, never sent.
type refusal struct {
	status int
	reason string
	route  string
}

func (f *refusal) write(w http.ResponseWriter) {
	switch f.status {
	case http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", `Bearer realm="mainloop"`)
		httpx.WriteError(w, http.StatusUnauthorized, "Authentication required")
	case http.StatusForbidden:
		httpx.WriteError(w, http.StatusForbidden, "Not permitted")
	}
}

func unauthorized(reason string) *refusal {
	return &refusal{status: http.StatusUnauthorized, reason: reason}
}

func forbidden(reason string) *refusal {
	return &refusal{status: http.StatusForbidden, reason: reason}
}

// decide resolves the principal and checks it against the operation.
func (a *Authenticator) decide(r *http.Request, router routers.Router) (*Principal, *refusal) {
	route, _, err := router.FindRoute(r)
	if err != nil || public(route.Operation) {
		return nil, nil
	}
	p, ref := a.identify(r)
	if ref == nil && p == nil {
		ref = unauthorized("no credential")
	}
	if ref == nil {
		ref = a.authorize(r, route.Operation, *p)
	}
	if ref != nil {
		ref.route = route.Path
	}
	return p, ref
}

func public(op *openapi3.Operation) bool {
	return op != nil && op.Security != nil && len(*op.Security) == 0
}

// identify reads the bearer, or else the login cookie. A credential that is present but invalid is
// refused rather than treated as absent.
func (a *Authenticator) identify(r *http.Request) (*Principal, *refusal) {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, token, ok := strings.Cut(h, " ")
		token = strings.TrimSpace(token)
		if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
			return nil, unauthorized("malformed Authorization header")
		}
		return a.bearer(r.Context(), token)
	}
	if a.OIDC != nil {
		if c, err := r.Cookie(a.Cookie); err == nil && c.Value != "" {
			return a.owner(r.Context(), c.Value, "cookie")
		}
	}
	return nil, nil
}

// bearer routes a token by its unverified claims: the owner's issuer goes to the OIDC verifier, a
// projected ServiceAccount token (it carries the kubernetes.io claim) to a TokenReview. Nothing is
// trusted before that verification.
func (a *Authenticator) bearer(ctx context.Context, token string) (*Principal, *refusal) {
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		return nil, unauthorized("malformed token")
	}
	iss, _ := claims["iss"].(string)
	if a.OIDC != nil && iss == a.OIDC.Issuer {
		return a.owner(ctx, token, "bearer")
	}
	if _, ok := claims["kubernetes.io"]; !ok {
		return nil, unauthorized("token from an unknown issuer")
	}
	if a.ServiceAccounts == nil {
		return nil, unauthorized("ServiceAccount tokens are not accepted here")
	}
	if !claims.VerifyAudience(a.ServiceAccounts.Audience, true) {
		return nil, unauthorized("ServiceAccount token for another audience")
	}
	exp, ok := claims["exp"].(float64)
	if !ok || a.now().After(time.Unix(int64(exp), 0)) {
		return nil, unauthorized("ServiceAccount token expired")
	}
	name, err := a.ServiceAccounts.Verify(ctx, token, time.Unix(int64(exp), 0))
	if err != nil {
		return nil, unauthorized(err.Error())
	}
	scope, allowed := a.Scopes[name]
	if !allowed {
		return nil, forbidden("ServiceAccount " + name + " is not allowed")
	}
	return &Principal{Kind: KindServiceAccount, Name: name, Scope: scope, Via: "bearer"}, nil
}

// owner verifies a Pocket ID token and checks it names the owner.
func (a *Authenticator) owner(ctx context.Context, token, via string) (*Principal, *refusal) {
	claims, err := a.OIDC.Verify(ctx, token)
	if err != nil {
		return nil, unauthorized(err.Error())
	}
	if claims.Subject != a.OwnerSubject {
		return nil, forbidden("subject is not the owner")
	}
	if a.OwnerGroup != "" && !slices.Contains(claims.Groups, a.OwnerGroup) {
		return nil, forbidden("owner token lacks the owner group")
	}
	return &Principal{Kind: KindOwner, Name: claims.Subject, Scope: ScopeOwner, Via: via}, nil
}

// authorize checks the principal's scope, and that an owner write came from the frontend: the
// gateway can turn the login cookie into a bearer, so this applies to every owner write, and a
// same-site page (a workspace preview) is held off by the header and the Origin.
func (a *Authenticator) authorize(r *http.Request, op *openapi3.Operation, p Principal) *refusal {
	opID := strings.ToLower(op.OperationID)
	switch p.Scope {
	case ScopeOwner:
	case ScopeRead:
		if !safe(r.Method) || opID == liveOperation {
			return forbidden("read scope")
		}
	case ScopeCanary:
		if !canaryOperations[opID] {
			return forbidden("canary scope")
		}
	default:
		return forbidden("unknown scope")
	}
	if p.Kind == KindOwner && !safe(r.Method) {
		if r.Header.Get(ClientHeader) == "" {
			return forbidden("owner write without " + ClientHeader)
		}
		if origin, present := r.Header["Origin"]; present && !httpx.OriginAllowed(origin[0], r.Host, a.FrontendOrigin, a.Dev) {
			return forbidden("owner write from another origin")
		}
	}
	return nil
}

func safe(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// logEvery limits repeated log lines: a polling client would otherwise log on every request.
const logEvery = 10 * time.Minute

// once reports whether key has not been logged within logEvery, and records it.
func (a *Authenticator) once(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if a.logged == nil || len(a.logged) > 1000 {
		a.logged = map[string]time.Time{}
	}
	if last, ok := a.logged[key]; ok && now.Sub(last) < logEvery {
		return false
	}
	a.logged[key] = now
	return true
}

// report logs a refusal, or in observe mode what enforce would refuse.
func (a *Authenticator) report(r *http.Request, f *refusal, p *Principal) {
	who := "anonymous"
	if p != nil {
		who = p.Kind + ":" + p.Name
	}
	if !a.once(strings.Join([]string{f.reason, r.Method, f.route, who}, "|")) {
		return
	}
	decision, level := "refused", slog.LevelWarn
	if a.Mode == ModeObserve {
		decision, level = "would_refuse", slog.LevelInfo
	}
	a.Log.Log(r.Context(), level, "auth: "+decision,
		"request_id", httpx.RequestID(r.Context()), "status", f.status, "reason", f.reason,
		"method", r.Method, "route", f.route, "principal", who,
		"remote", r.RemoteAddr, "forwarded_for", r.Header.Get("X-Forwarded-For"))
}

// seen logs a verified principal the first time in a while, so an observe window shows who calls.
func (a *Authenticator) seen(p *Principal) {
	if a.once("seen|" + p.Kind + "|" + p.Name + "|" + p.Via) {
		a.Log.Info("auth: principal", "kind", p.Kind, "name", p.Name, "scope", string(p.Scope), "via", p.Via, "mode", string(a.Mode))
	}
}
