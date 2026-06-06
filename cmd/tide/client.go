package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/mem"
)

// adminClient is a hand-rolled gRPC client for the JSON-envelope Admin
// service. The entity proto codegen produces typed buf-generated stubs,
// but bootstrapping that for the Admin service itself would require
// generating proto from proto, so we ship a JSON envelope here.
//
// Wire shape (mirrored from internal/server/admin/grpc.go):
//
//	Method:   /atlantis.admin.v1.Admin/{PlanSchema,ApplyMigration}
//	Codec:    grpc Codec returning raw JSON bytes
//	Request:  json.Marshal(go struct) -> raw bytes
//	Reply:    raw bytes -> json.Unmarshal -> go struct
//
// We use grpc.Invoke directly with a custom CallOption to inject the JSON
// codec, avoiding any generated stub.
type adminClient struct {
	conn *grpc.ClientConn
}

func dial(cfg *tideConfig) (*adminClient, error) {
	var creds credentials.TransportCredentials
	if cfg.TLS.Cert != "" || cfg.TLS.CertPEM != "" {
		var err error
		creds, err = buildTLS(cfg)
		if err != nil {
			return nil, err
		}
	} else {
		fmt.Fprintln(os.Stderr, "tide: TLS not configured — using insecure transport (dev only)")
		creds = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(cfg.Endpoint,
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.ForceCodecV2(jsonCodec{})),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.Endpoint, err)
	}
	return &adminClient{conn: conn}, nil
}

func (c *adminClient) Close() error { return c.conn.Close() }

// invoke runs a unary call with a JSON request and decodes a JSON reply.
// The server side reads / writes the same JSON shape.
func (c *adminClient) invoke(ctx context.Context, method string, req, reply any) error {
	rawReq, err := json.Marshal(req)
	if err != nil {
		return err
	}
	in := jsonMsg{Raw: rawReq}
	var out jsonMsg
	if err := c.conn.Invoke(ctx, method, &in, &out); err != nil {
		return err
	}
	return json.Unmarshal(out.Raw, reply)
}

func buildTLS(cfg *tideConfig) (credentials.TransportCredentials, error) {
	// Source the leaf cert + key. Inline PEM wins when set (config
	// validation in loadPCConfig already rejected the both-set case);
	// otherwise fall back to the file-path variant.
	var (
		cert tls.Certificate
		err  error
	)
	if cfg.TLS.CertPEM != "" {
		cert, err = tls.X509KeyPair([]byte(cfg.TLS.CertPEM), []byte(cfg.TLS.KeyPEM))
		if err != nil {
			return nil, fmt.Errorf("parse TIDE_TLS_CERT_PEM / TIDE_TLS_KEY_PEM: %w", err)
		}
	} else {
		cert, err = tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
		if err != nil {
			return nil, fmt.Errorf("load client cert: %w", err)
		}
	}

	// CA trust anchor — same pattern.
	var caPEM []byte
	if cfg.TLS.CAPEM != "" {
		caPEM = []byte(cfg.TLS.CAPEM)
	} else {
		caPEM, err = os.ReadFile(cfg.TLS.CA)
		if err != nil {
			return nil, fmt.Errorf("read CA: %w", err)
		}
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		src := cfg.TLS.CA
		if cfg.TLS.CAPEM != "" {
			src = "TIDE_TLS_CA_PEM"
		}
		return nil, fmt.Errorf("CA %s contains no usable certs", src)
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}), nil
}

// jsonMsg / jsonCodec are mirrors of internal/server/admin/grpc.go. The
// codec returns the raw bytes; gRPC's transport-layer length-prefixing
// handles framing.
//
// gRPC's modern Codec is CodecV2 which uses BufferSlice for zero-copy
// hand-off. We satisfy that here; the older Codec interface (plain []byte)
// is no longer accepted by ForceCodecV2.
type jsonMsg struct{ Raw []byte }

type jsonCodec struct{}

func (jsonCodec) Marshal(v any) (mem.BufferSlice, error) {
	m, ok := v.(*jsonMsg)
	if !ok {
		return nil, fmt.Errorf("jsonCodec: cannot marshal %T", v)
	}
	return mem.BufferSlice{mem.SliceBuffer(m.Raw)}, nil
}

func (jsonCodec) Unmarshal(data mem.BufferSlice, v any) error {
	m, ok := v.(*jsonMsg)
	if !ok {
		return fmt.Errorf("jsonCodec: cannot unmarshal into %T", v)
	}
	m.Raw = append(m.Raw[:0], data.Materialize()...)
	return nil
}

func (jsonCodec) Name() string { return "json" }
