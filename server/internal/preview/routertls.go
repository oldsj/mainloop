package preview

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// routerTLS is the client side of the router's mutual TLS. Both files are projected volumes the
// kubelet rewrites (the pod certificate is short-lived), so they are read on every connect rather
// than once.
type routerTLS struct {
	serverName string
	// credential holds the private key and the certificate chain in one PEM file.
	credential string
	// ca is the trust bundle that signs the router's serving certificate.
	ca string
}

func (r routerTLS) config() (*tls.Config, error) {
	caPEM, err := os.ReadFile(r.ca)
	if err != nil {
		return nil, fmt.Errorf("router trust bundle: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("router trust bundle: no certificates in " + r.ca)
	}
	bundle, err := os.ReadFile(r.credential)
	if err != nil {
		return nil, fmt.Errorf("router client certificate: %w", err)
	}
	// X509KeyPair takes the CERTIFICATE blocks from the first argument and the key from the
	// second, skipping the other blocks, so one credential bundle serves as both.
	cert, err := tls.X509KeyPair(bundle, bundle)
	if err != nil {
		return nil, fmt.Errorf("router client certificate %s: %w", r.credential, err)
	}
	return &tls.Config{
		ServerName:   r.serverName,
		RootCAs:      roots,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}
