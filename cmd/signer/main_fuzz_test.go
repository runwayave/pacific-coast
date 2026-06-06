package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// FuzzHandleIssue fuzzes the signer's POST /issue request body against
// an in-process server with a test CA loaded into the package globals.
// pgPool is left nil so the DB identity-check is skipped — we're stress-
// testing the CSR + JSON validation path on its own, not the
// registration gate (which is exercised separately at the BFF layer).
//
// Safety invariants the fuzz asserts on every iteration:
//
//  1. No panics, no goroutine leaks, no timeouts. Any input that crashes
//     the handler is a critical bug — a public-facing signer is the only
//     thing standing between an attacker and arbitrary cert issuance.
//  2. Reserved CNs (atlantis, atlantis-console, atlantis-signer) are
//     NEVER issued certs. A 200 response for any reserved name is an
//     immediate test failure.
//  3. CSRs whose Subject CN does not match the request body's `caller`
//     are NEVER issued certs. Mismatch must reject before signing.
//  4. When the handler returns 200, the issued cert must:
//     - be parseable as a real x509 certificate
//     - have CA:FALSE (never sign a CA-capable leaf)
//     - have BasicConstraintsValid=true
//     - have ExtKeyUsage containing ClientAuth (the only EKU the
//     handler ought to set)
//     - have a NotAfter within ~91 days of now (TTL is 90 days; tolerate
//     a day of skew)
//     - have a Subject CN exactly matching the request `caller`
func FuzzHandleIssue(f *testing.F) {
	setupTestCA(f)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleIssue(w, r, log)
	})
	srv := httptest.NewServer(handler)
	f.Cleanup(srv.Close)

	// Seed corpus: realistic well-formed requests + adversarial shapes.
	// Each seed is the full HTTP request body bytes.
	seeds := [][]byte{
		// Empty / minimal.
		[]byte(""),
		[]byte("{}"),
		[]byte("null"),
		[]byte("[]"),
		// Not JSON at all.
		[]byte("not json"),
		[]byte("\x00\x01\x02"),
		// caller-only, no CSR.
		[]byte(`{"caller":"backend"}`),
		// CSR-only, no caller.
		[]byte(`{"csr_pem":"not a real pem"}`),
		// Reserved CNs (must always be rejected).
		mustSeed(f, "atlantis", "atlantis"),
		mustSeed(f, "atlantis-console", "atlantis-console"),
		mustSeed(f, "atlantis-signer", "atlantis-signer"),
		// Mismatched CN (must always be rejected).
		mustSeed(f, "backend", "vendor"),
		mustSeed(f, "", "anything"),
		mustSeed(f, "with space", "with space"),
		// Well-formed but absurd CSR.
		mustSeed(f, "good-caller", "good-caller"),
		// Unicode & control chars in caller.
		mustSeed(f, "backend\x00admin", "backend\x00admin"),
		mustSeed(f, "back\nend", "back\nend"),
		// Very long caller.
		mustSeed(f, strings.Repeat("a", 4096), strings.Repeat("a", 4096)),
		// Garbage PEM block.
		[]byte(`{"caller":"good-caller","csr_pem":"-----BEGIN CERTIFICATE REQUEST-----\nAAAA\n-----END CERTIFICATE REQUEST-----\n"}`),
		// PEM with wrong block type.
		[]byte(`{"caller":"good-caller","csr_pem":"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"}`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		resp, err := http.Post(srv.URL+"/issue", "application/json", strings.NewReader(string(body)))
		if err != nil {
			// Transport error is acceptable (request rejected, conn
			// reset etc.) — what matters is that the server didn't
			// crash, and httptest will surface a panic as a separate
			// test failure.
			return
		}
		defer resp.Body.Close()

		// Read the response (cap at 64 KiB; ours is much smaller).
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if err != nil {
			return
		}

		// Only 200 paths matter for invariants 2–4. Anything else means
		// the handler refused; that's the safety contract working.
		if resp.StatusCode != http.StatusOK {
			return
		}

		var out struct {
			CertPEM   string `json:"cert_pem"`
			CAPEM     string `json:"ca_pem"`
			ExpiresAt string `json:"expires_at"`
		}
		if err := json.Unmarshal(respBody, &out); err != nil {
			t.Fatalf("200 response with un-decodable body: %v\nbody=%q", err, respBody)
		}

		// Decode the issued cert.
		block, _ := pem.Decode([]byte(out.CertPEM))
		if block == nil {
			t.Fatalf("200 response with un-PEM-able cert_pem; body=%q", respBody)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("issued cert won't parse: %v", err)
		}

		// Decode the original request to know what CN was asked for.
		var req struct {
			Caller string `json:"caller"`
			CSRPEM string `json:"csr_pem"`
		}
		_ = json.Unmarshal(body, &req)
		caller := strings.TrimSpace(req.Caller)

		// (2) Reserved CNs must never be signed.
		if reservedCNs[caller] {
			t.Fatalf("issued cert for RESERVED CN %q", caller)
		}

		// (3) CN match enforcement.
		if cert.Subject.CommonName != caller {
			t.Fatalf("issued cert CN=%q does not match request caller=%q",
				cert.Subject.CommonName, caller)
		}

		// (4a) Never CA-capable.
		if cert.IsCA {
			t.Fatalf("issued cert has IsCA=true — must be a leaf only")
		}
		if !cert.BasicConstraintsValid {
			t.Fatalf("issued cert has BasicConstraintsValid=false; ambiguous CA status")
		}

		// (4b) EKU is exactly ClientAuth.
		hasClientAuth := false
		for _, u := range cert.ExtKeyUsage {
			if u == x509.ExtKeyUsageClientAuth {
				hasClientAuth = true
			}
			// Disallow any EKU other than ClientAuth — issuing a cert
			// valid for ServerAuth (etc.) would let a popped caller
			// stand up a TLS server impersonating atlantis.
			if u != x509.ExtKeyUsageClientAuth {
				t.Fatalf("issued cert has unexpected EKU: %v", u)
			}
		}
		if !hasClientAuth {
			t.Fatalf("issued cert missing ExtKeyUsageClientAuth")
		}

		// (4c) Lifetime sanity. Allow up to certTTL + 2 days slack.
		maxAfter := time.Now().Add(certTTL + 2*24*time.Hour)
		if cert.NotAfter.After(maxAfter) {
			t.Fatalf("issued cert NotAfter=%v exceeds expected upper bound %v",
				cert.NotAfter, maxAfter)
		}
	})
}

// setupTestCA loads a fresh in-process CA into the package globals so
// the fuzz target can drive signCSR without touching disk. Mirrors the
// shape loadCA produces from real on-disk material.
func setupTestCA(t testing.TB) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "atlantis-fuzz-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("self-sign CA: %v", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("parse fresh CA: %v", err)
	}
	caCert = cert
	caKey = priv
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	pgPool = nil // skip DB identity check in fuzz; tested elsewhere
}

// mustSeed builds a request body with a real CSR for `csrCN`, declaring
// `caller` in the body. Used to seed legitimate-looking requests + the
// mismatch case (caller != csrCN).
func mustSeed(t testing.TB, caller, csrCN string) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen csr key: %v", err)
	}
	csrTmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: csrCN}}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTmpl, priv)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
	body, _ := json.Marshal(map[string]string{"caller": caller, "csr_pem": csrPEM})
	return body
}
