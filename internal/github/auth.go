package github

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"golang.org/x/oauth2"
)

// TokenClient returns a client that authenticates each request with a personal access token.
func TokenClient(token string) *http.Client {
	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token, TokenType: "token"})
	return oauth2.NewClient(context.Background(), src)
}

// MTLSClient returns a client that authenticates via mutual TLS with the given client certificate and key.
func MTLSClient(certFile, keyFile, caFile string) (*http.Client, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading client certificate: %v", err)
	}
	slog.Info("loaded github client certificate", "expiry", cert.Leaf.NotAfter)

	tlsConfig := &tls.Config{
		// Reload per handshake so externally rotated certs are picked up.
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				slog.Error("reloading github client certificate", "error", err)
				return nil, err
			}
			return &cert, nil
		},
	}
	if caFile == "" {
		// No CA bundle: verify the server against the system trust roots.
		tlsConfig.RootCAs = nil
	} else {
		// A CA bundle was given: verify the server against it.
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("reading CA certificate: %v", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("no CA certificates found in %s", caFile)
		}
		tlsConfig.RootCAs = pool
	}

	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}, nil
}
