package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/agent-substrate/substrate/pkg/proto/credproviderpb"
)

const expectedURI = "ate-secret://kubernetes.io/ate-system/round3-dummy/token"

type provider struct {
	credproviderpb.UnimplementedCredentialProviderServer
	secretPath string
}

func (p provider) FetchSecret(_ context.Context, req *credproviderpb.FetchSecretRequest) (*credproviderpb.FetchSecretResponse, error) {
	if req.GetUri() != expectedURI || !strings.HasPrefix(req.GetActorSpiffeId(), "spiffe://") {
		return nil, status.Error(codes.PermissionDenied, "dummy provider request rejected")
	}
	value, err := os.ReadFile(p.secretPath)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "dummy value unavailable")
	}
	log.Printf("credential_fetch=ok actor_identity_present=true")
	return &credproviderpb.FetchSecretResponse{OpaqueBytes: bytes.TrimSpace(value)}, nil
}

func main() {
	bundle := "/run/servicedns/credential-bundle.pem"
	servingCert, err := tls.LoadX509KeyPair(bundle, bundle)
	if err != nil {
		log.Fatal("serving certificate unavailable")
	}
	caBytes, err := os.ReadFile("/run/podidentity-ca/trust-bundle.pem")
	if err != nil {
		log.Fatal("client CA unavailable")
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caBytes) {
		log.Fatal("client CA bundle invalid")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{servingCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	credproviderpb.RegisterCredentialProviderServer(grpcServer, provider{secretPath: "/run/dummy/token"})
	listener, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatal("gRPC listener unavailable")
	}
	go func() {
		log.Printf("credential_provider_ready=true")
		if err := grpcServer.Serve(listener); err != nil {
			log.Fatal("gRPC server failed")
		}
	}()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/plain")
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		want, err := os.ReadFile("/run/dummy/token")
		if err != nil {
			http.Error(w, "dummy value unavailable", http.StatusServiceUnavailable)
			return
		}
		if bytes.Equal(bytes.TrimSpace(want), []byte(r.Header.Get("X-Mainloop-Dummy"))) {
			_, _ = io.WriteString(w, "injected-header-matched")
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "injected-header-mismatch")
	})
	server := &http.Server{
		Addr:      ":8443",
		Handler:   nil,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{servingCert}},
	}
	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatal("TLS echo listener unavailable")
	}
}
