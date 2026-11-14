package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// reviewTTL bounds how long a positive TokenReview is reused; a token past its own expiry is
	// never reused.
	reviewTTL = time.Minute
	// refusedTTL is how long a refused token is remembered, so a caller repeating a bad token does
	// not reach the cluster API on every request.
	refusedTTL     = 10 * time.Second
	maxCached      = 1024
	reviewTimeout  = 5 * time.Second
	maxConcurrency = 8
)

// Review is the part of a TokenReview's status the verifier reads.
type Review struct {
	Authenticated bool
	Username      string
	Audiences     []string
}

// TokenReviewer asks the cluster who a ServiceAccount token belongs to.
type TokenReviewer interface {
	Review(ctx context.Context, token string, audiences []string) (Review, error)
}

// ServiceAccounts verifies projected ServiceAccount tokens with a TokenReview, which also refuses a
// token whose pod or ServiceAccount is gone. Results are cached by token hash.
type ServiceAccounts struct {
	Audience string
	Reviewer TokenReviewer
	Now      func() time.Time

	once  sync.Once
	sem   chan struct{}
	mu    sync.Mutex
	cache map[[32]byte]reviewed
}

type reviewed struct {
	name  string
	err   error
	until time.Time
}

var errReviewBusy = errors.New("too many token reviews in flight")

// Verify returns the token's ServiceAccount as namespace/name. exp is the token's own expiry, read
// before verification, which only shortens the cache.
func (s *ServiceAccounts) Verify(ctx context.Context, token string, exp time.Time) (string, error) {
	s.once.Do(func() {
		s.sem = make(chan struct{}, maxConcurrency)
		s.cache = map[[32]byte]reviewed{}
	})
	sum := sha256.Sum256([]byte(token))
	now := s.now()
	s.mu.Lock()
	if c, ok := s.cache[sum]; ok && now.Before(c.until) {
		s.mu.Unlock()
		return c.name, c.err
	}
	s.mu.Unlock()

	select {
	case s.sem <- struct{}{}:
	default:
		return "", errReviewBusy
	}
	name, err := s.review(ctx, token)
	<-s.sem

	until := now.Add(refusedTTL)
	if err == nil {
		until = now.Add(reviewTTL)
		if exp.Before(until) {
			until = exp
		}
	}
	s.mu.Lock()
	if len(s.cache) >= maxCached {
		for k, c := range s.cache {
			if !now.Before(c.until) {
				delete(s.cache, k)
			}
		}
		if len(s.cache) >= maxCached {
			s.cache = map[[32]byte]reviewed{}
		}
	}
	s.cache[sum] = reviewed{name: name, err: err, until: until}
	s.mu.Unlock()
	return name, err
}

func (s *ServiceAccounts) review(ctx context.Context, token string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, reviewTimeout)
	defer cancel()
	r, err := s.Reviewer.Review(ctx, token, []string{s.Audience})
	if err != nil {
		return "", fmt.Errorf("token review: %w", err)
	}
	if !r.Authenticated {
		return "", errors.New("token review: not authenticated")
	}
	if !slices.Contains(r.Audiences, s.Audience) {
		return "", errors.New("token review: wrong audience")
	}
	rest, ok := strings.CutPrefix(r.Username, "system:serviceaccount:")
	ns, name, ok2 := strings.Cut(rest, ":")
	if !ok || !ok2 || ns == "" || name == "" {
		return "", errors.New("token review: not a ServiceAccount")
	}
	return ns + "/" + name, nil
}

func (s *ServiceAccounts) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// KubeReviewer is the TokenReviewer of a process running in the cluster. Its ServiceAccount needs
// create on tokenreviews (system:auth-delegator).
type KubeReviewer struct {
	Client kubernetes.Interface
}

// NewInClusterReviewer builds a KubeReviewer from the pod's own credentials.
func NewInClusterReviewer() (*KubeReviewer, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("auth: in-cluster config: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("auth: kubernetes client: %w", err)
	}
	return &KubeReviewer{Client: client}, nil
}

// Review creates a TokenReview for token, scoped to audiences.
func (k *KubeReviewer) Review(ctx context.Context, token string, audiences []string) (Review, error) {
	tr, err := k.Client.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token, Audiences: audiences},
	}, metav1.CreateOptions{})
	if err != nil {
		return Review{}, err
	}
	return Review{Authenticated: tr.Status.Authenticated, Username: tr.Status.User.Username, Audiences: tr.Status.Audiences}, nil
}
