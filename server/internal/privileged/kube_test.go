package privileged_test

import (
	"context"
	"testing"

	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/oldsj/mainloop/server/internal/privileged"
)

// Given the executor's own client, when a k8s.* type needs a credential, then it mints a 10-minute
// token for that type's ServiceAccount in the executor's namespace, and the client built on it keeps
// the server and CA but none of the executor's own credentials.
func TestKubeClientsMintAShortToken(t *testing.T) {
	kube := fake.NewClientset()
	var got *authv1.TokenRequest
	var account, namespace string
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		c := a.(k8stesting.CreateAction)
		if c.GetSubresource() != "token" {
			return false, nil, nil
		}
		got = c.GetObject().(*authv1.TokenRequest)
		account, namespace = c.(k8stesting.CreateActionImpl).Name, c.GetNamespace()
		out := got.DeepCopy()
		out.Status.Token = "fictional-minted-token"
		return true, out, nil
	})
	base := &rest.Config{Host: "https://10.0.0.1:443", BearerTokenFile: "/var/run/secrets/token",
		TLSClientConfig: rest.TLSClientConfig{CAFile: "/var/run/secrets/ca.crt"}}
	cfg, err := privileged.KubeClients{Client: kube, Base: base, Namespace: executorNS}.Config(context.Background(), privileged.AccountDelete)
	if err != nil {
		t.Fatal(err)
	}
	if account != privileged.AccountDelete || namespace != executorNS || got.Spec.ExpirationSeconds == nil || *got.Spec.ExpirationSeconds != 600 {
		t.Fatalf("token request for %s/%s: %+v", namespace, account, got.Spec)
	}
	if cfg.BearerToken != "fictional-minted-token" || cfg.BearerTokenFile != "" || cfg.Host != base.Host || cfg.CAFile != base.CAFile {
		t.Fatalf("config = host %s, token file %q, CA %q", cfg.Host, cfg.BearerTokenFile, cfg.CAFile)
	}
}
