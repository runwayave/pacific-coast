// Package main implements the atlantis cert signer: a narrow HTTP service that
// holds the intermediate CA private key and signs caller leaf certificates on
// behalf of the console.
//
// The signer never exports the CA key. It accepts a PEM-encoded CSR
// (POST /issue) and returns a signed leaf cert.  The CN in the CSR must
// match the caller name in the request body, and it must not be on the
// reserved-CN denylist — those names belong to atlantis infrastructure.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// reservedCNs cannot be signed regardless of whether they are registered
// callers.  These are atlantis infrastructure identities; a compromised
// console must not be able to impersonate them.
var reservedCNs = map[string]bool{
	"atlantis":         true,
	"atlantis-console": true,
	"atlantis-signer":  true,
}

// certTTL is the lifetime of issued leaf certs. Short TTL means expiry
// acts as a natural revocation mechanism — no CRL/OCSP needed.
const certTTL = 90 * 24 * time.Hour

var (
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPEM  []byte
	// pgPool is non-nil iff PG_URL was set at boot. When nil the signer
	// falls back to the reserved-CN denylist only — sufficient for dev
	// where the bundled signer can't see atlantis's DB independently.
	// Production must set PG_URL so issuance is gated on a registered
	// caller_identities row.
	pgPool *pgxpool.Pool
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	caDir := envOr("CA_DIR", "/ca-private")
	listen := envOr("SIGNER_LISTEN", ":7070")
	pgURL := os.Getenv("PG_URL")

	var err error
	caCert, caKey, caPEM, err = loadCA(caDir)
	if err != nil {
		log.Error("load CA", "err", err)
		os.Exit(1)
	}

	// Optional: connect to atlantis's Postgres so the issuance path can
	// verify the requested CN is a registered caller. Read-only access
	// to atlantis.caller_identities is all we need. Failing to connect
	// is fatal in production posture — without DB the signer would fall
	// back to denylist-only and an operator could mint arbitrary CNs —
	// so we exit rather than silently degrade.
	if pgURL != "" {
		cfg, perr := pgxpool.ParseConfig(pgURL)
		if perr != nil {
			log.Error("parse PG_URL", "err", perr)
			os.Exit(1)
		}
		cfg.MaxConns = 4
		cfg.MinConns = 1
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		pgPool, perr = pgxpool.NewWithConfig(ctx, cfg)
		cancel()
		if perr != nil {
			log.Error("connect to PG", "err", perr)
			os.Exit(1)
		}
		defer pgPool.Close()
	}

	log.Info("atlantis-signer ready",
		"ca_cn", caCert.Subject.CommonName,
		"ca_expires", caCert.NotAfter.Format(time.RFC3339),
		"listen", listen,
		"identity_check", pgPool != nil,
	)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /issue", func(w http.ResponseWriter, r *http.Request) {
		handleIssue(w, r, log)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	// Graceful shutdown on SIGTERM / SIGINT.
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Error("signer exited", "err", err)
		os.Exit(1)
	}
}

func handleIssue(w http.ResponseWriter, r *http.Request, log *slog.Logger) {
	var req struct {
		Caller string `json:"caller"`
		CSRPEM string `json:"csr_pem"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	caller := strings.TrimSpace(req.Caller)
	if caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}
	if req.CSRPEM == "" {
		jsonError(w, "csr_pem is required", http.StatusBadRequest)
		return
	}
	if reservedCNs[caller] {
		jsonError(w, fmt.Sprintf("caller %q is a reserved infrastructure name and cannot be issued a cert", caller), http.StatusForbidden)
		return
	}

	// Defense-in-depth registration check. The console BFF also verifies
	// the caller is registered before reaching us, but a compromised BFF
	// shouldn't be able to mint arbitrary CNs. When pgPool is nil (dev
	// bundle without PG_URL) we skip this layer and rely on the BFF check
	// and the reserved-CN denylist alone — operators should set PG_URL in
	// prod.
	if pgPool != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		var registered bool
		err := pgPool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM atlantis.caller_identities WHERE caller = $1)`,
			caller).Scan(&registered)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			log.Error("identity lookup", "caller", caller, "err", err)
			jsonError(w, "identity lookup failed", http.StatusInternalServerError)
			return
		}
		if !registered {
			jsonError(w, fmt.Sprintf("caller %q is not registered — operator must Add Caller in the console first", caller), http.StatusForbidden)
			return
		}
	}

	csr, err := parseCSR(req.CSRPEM)
	if err != nil {
		jsonError(w, "invalid CSR: "+err.Error(), http.StatusBadRequest)
		return
	}
	if csr.Subject.CommonName != caller {
		jsonError(w, fmt.Sprintf("CSR CN %q must match caller %q", csr.Subject.CommonName, caller), http.StatusBadRequest)
		return
	}

	certPEM, expiresAt, err := signCSR(csr)
	if err != nil {
		log.Error("sign CSR", "caller", caller, "err", err)
		jsonError(w, "signing failed", http.StatusInternalServerError)
		return
	}

	log.Info("issued cert",
		"caller", caller,
		"expires", expiresAt.Format(time.RFC3339),
		"remote", r.RemoteAddr,
	)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"cert_pem":   certPEM,
		"ca_pem":     string(caPEM),
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	})
}

func signCSR(csr *x509.CertificateRequest) (string, time.Time, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	expiresAt := now.Add(certTTL)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      csr.Subject,
		// Slight backdate to tolerate clock skew between containers.
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              expiresAt,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		IsCA:                  false,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, csr.PublicKey, caKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create certificate: %w", err)
	}

	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))
	return certPEM, expiresAt, nil
}

func parseCSR(pemStr string) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("not a valid PEM CERTIFICATE REQUEST block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	return csr, csr.CheckSignature()
}

// loadCA reads the CA certificate and private key from dir.
// The key is expected to be a SEC1 EC private key (openssl ecparam output).
func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	crtPath := dir + "/ca.crt"
	crtBytes, err := os.ReadFile(crtPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read %s: %w", crtPath, err)
	}
	block, _ := pem.Decode(crtBytes)
	if block == nil {
		return nil, nil, nil, fmt.Errorf("%s: no PEM block", crtPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse %s: %w", crtPath, err)
	}

	keyPath := dir + "/ca.key"
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read %s: %w", keyPath, err)
	}
	keyBlock, _ := pem.Decode(keyBytes)
	if keyBlock == nil {
		return nil, nil, nil, fmt.Errorf("%s: no PEM block", keyPath)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse %s: %w", keyPath, err)
	}

	return cert, key, crtBytes, nil
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
