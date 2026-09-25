package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log"
	"net"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/agent-substrate/substrate/pkg/proto/credproviderpb"
)

const (
	expectedURI       = "ate-secret://kubernetes.io/mainloop-control/claude-oauth/oauth-token"
	credentialPath    = "/run/claude/oauth-token"
	servingBundlePath = "/run/servicedns/credential-bundle.pem"
	clientCAPath      = "/run/podidentity-ca/trust-bundle.pem"
)

type provider struct {
	credproviderpb.UnimplementedCredentialProviderServer
	expectedActorSPIFFEID string
}

func (p provider) FetchSecret(_ context.Context, req *credproviderpb.FetchSecretRequest) (*credproviderpb.FetchSecretResponse, error) {
	if req.GetUri() != expectedURI || req.GetActorSpiffeId() != p.expectedActorSPIFFEID {
		return nil, status.Error(codes.PermissionDenied, "credential request rejected")
	}
	value, err := os.ReadFile(credentialPath)
	if err != nil {
		log.Printf("credential_fetch=unavailable actor_identity_match=true")
		return nil, status.Error(codes.Unavailable, "credential unavailable")
	}
	if len(value) == 0 {
		return nil, status.Error(codes.NotFound, "credential unavailable")
	}
	log.Printf("credential_fetch=ok actor_identity_match=true credential_kind=claude-oauth")
	return &credproviderpb.FetchSecretResponse{OpaqueBytes: value}, nil
}

func main() {
	expectedActorSPIFFEID := strings.TrimSpace(os.Getenv("EXPECTED_ACTOR_SPIFFE_ID"))
	if expectedActorSPIFFEID == "" {
		log.Fatal("EXPECTED_ACTOR_SPIFFE_ID is required")
	}

	servingCert, err := tls.LoadX509KeyPair(servingBundlePath, servingBundlePath)
	if err != nil {
		log.Fatal("serving certificate unavailable")
	}
	caBytes, err := os.ReadFile(clientCAPath)
	if err != nil {
		log.Fatal("client CA unavailable")
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caBytes) {
		log.Fatal("client CA bundle invalid")
	}
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{servingCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	credproviderpb.RegisterCredentialProviderServer(grpcServer, provider{expectedActorSPIFFEID: expectedActorSPIFFEID})
	listener, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatal("gRPC listener unavailable")
	}
	log.Printf("credential_provider_ready=true")
	if err := grpcServer.Serve(listener); err != nil {
		log.Fatal("gRPC server failed")
	}
}
