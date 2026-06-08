// Package atltransport builds gRPC transport credentials and connections
// for the atlantis backend. Centralising this here means every caller
// (the typed Go SDK, custom dialers like the job submitter, tide / tidectl)
// reads the same env-var contract and uses the same TLS material.
//
// Env-var contract:
//
//	ATL_TLS_CERT  path to the caller's mTLS client cert (PEM)
//	ATL_TLS_KEY   path to the caller's mTLS private key (PEM)
//	ATL_TLS_CA    path to the atlantis server's CA bundle (PEM)
//
// When ATL_TLS_CERT is unset, [Credentials] returns plain
// insecure.NewCredentials — that's the correct choice for dev / same-cluster
// prod where atlantis is reachable on a private bridge. Cross-network /
// public-endpoint deployments must set all three.
package atltransport

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Credentials returns gRPC transport credentials for the atlantis client.
// See the package doc for the env-var contract. MinVersion=TLS13 matches
// the atlantis server's listener so any downgrade attempt fails cleanly.
func Credentials() (credentials.TransportCredentials, error) {
	certPath := os.Getenv("ATL_TLS_CERT")
	if certPath == "" {
		return insecure.NewCredentials(), nil
	}
	keyPath := os.Getenv("ATL_TLS_KEY")
	caPath := os.Getenv("ATL_TLS_CA")
	if keyPath == "" || caPath == "" {
		return nil, fmt.Errorf("atltransport: ATL_TLS_CERT set but ATL_TLS_KEY or ATL_TLS_CA missing")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("atltransport: load client cert: %w", err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("atltransport: read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("atltransport: no usable certs in CA file %s", caPath)
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}), nil
}

// Dial returns a gRPC client connection to addr with credentials sourced
// from Credentials() plus any extra opts the caller provides. Use this
// for everything except where you need custom call-option machinery
// (e.g. the JSON codec for admin RPCs — see the WithDialOption escape
// hatch in those call sites).
func Dial(addr string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	creds, err := Credentials()
	if err != nil {
		return nil, err
	}
	opts = append([]grpc.DialOption{grpc.WithTransportCredentials(creds)}, opts...)
	return grpc.NewClient(addr, opts...)
}
