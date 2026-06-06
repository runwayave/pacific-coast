#!/bin/sh
# init-certs.sh — first-boot TLS bootstrap for the atlantis self-host bundle.
#
# Writes to CERT_DIR (default /certs):
#   ca.crt                    — self-signed root CA (public; shared with all services)
#   server.crt / server.key   — atlantis gRPC server cert
#                               SANs: DNS:atlantis, DNS:localhost, IP:127.0.0.1
#                               plus DNS:<ATLANTIS_DOMAIN> if set
#   console.crt / console.key — mTLS client cert for atlantis-console (CN=atlantis-console)
#
# Writes to CA_PRIVATE_DIR (default /ca-private):
#   ca.key   — CA private key (never written to CERT_DIR; only the signer mounts this)
#   ca.crt   — copy so the signer can load the full CA bundle without mounting atl-certs
#
# Idempotent: if all certificate files are already present the script exits
# without touching them. To regenerate, remove the files and restart the
# 'certs' service.
#
# To bring your own CA: pre-populate CERT_DIR and CA_PRIVATE_DIR with the
# files listed above; this script will not overwrite them.

set -e

CERT_DIR="${CERT_DIR:-/certs}"
CA_PRIVATE_DIR="${CA_PRIVATE_DIR:-/ca-private}"
ATLANTIS_DOMAIN="${ATLANTIS_DOMAIN:-}"
# 10-year CA + leaf. There is no online rotation path today; the
# self-host bundle assumes operators run against this CA for the
# cluster's life. To rotate: stop stack, delete files in atl-certs and
# atl-ca-private, restart 'certs' service, re-issue every caller cert
# via `make self-host-caller-cert CALLER=<name>`.
DAYS=3650

mkdir -p "$CERT_DIR" "$CA_PRIVATE_DIR"

if [ -f "$CERT_DIR/ca.crt" ] && \
   [ -f "$CA_PRIVATE_DIR/ca.key" ] && \
   [ -f "$CERT_DIR/server.crt" ] && \
   [ -f "$CERT_DIR/server.key" ] && \
   [ -f "$CERT_DIR/console.crt" ] && \
   [ -f "$CERT_DIR/console.key" ]; then
    echo "[certs] all certificates already present — skipping generation"
    exit 0
fi

# ── CA ────────────────────────────────────────────────────────────────────────
# The CA private key is written ONLY to CA_PRIVATE_DIR (the atl-ca-private
# volume).  It is never written to CERT_DIR (atl-certs), so the atlantis
# server and console containers cannot access it. Only the signer mounts
# atl-ca-private.
echo "[certs] generating local CA..."
openssl ecparam -genkey -name prime256v1 -noout -out "$CA_PRIVATE_DIR/ca.key"
openssl req -new -x509 \
    -key "$CA_PRIVATE_DIR/ca.key" \
    -out "$CERT_DIR/ca.crt" \
    -days "$DAYS" \
    -subj "/CN=atlantis-self-host-ca"

# Copy ca.crt into CA_PRIVATE_DIR so the signer can read the full CA bundle
# without needing to mount atl-certs.
cp "$CERT_DIR/ca.crt" "$CA_PRIVATE_DIR/ca.crt"

# ── Server cert ───────────────────────────────────────────────────────────────
echo "[certs] generating server certificate..."
openssl ecparam -genkey -name prime256v1 -noout -out "$CERT_DIR/server.key"
openssl req -new \
    -key "$CERT_DIR/server.key" \
    -out /tmp/atl-server.csr \
    -subj "/CN=atlantis"

SAN="DNS:atlantis,DNS:localhost,IP:127.0.0.1"
if [ -n "$ATLANTIS_DOMAIN" ]; then
    SAN="${SAN},DNS:${ATLANTIS_DOMAIN}"
fi
printf "subjectAltName=%s\n" "$SAN" > /tmp/atl-server-san.ext

openssl x509 -req \
    -in /tmp/atl-server.csr \
    -CA "$CERT_DIR/ca.crt" \
    -CAkey "$CA_PRIVATE_DIR/ca.key" \
    -CAcreateserial \
    -out "$CERT_DIR/server.crt" \
    -days "$DAYS" \
    -extfile /tmp/atl-server-san.ext

rm /tmp/atl-server.csr /tmp/atl-server-san.ext

# ── Console client cert ───────────────────────────────────────────────────────
# CN=atlantis-console is used by the server's caller-identity check and
# the per-CN rate-limit override (RATE_LIMIT_PER_CALLER).
echo "[certs] generating console client certificate (CN=atlantis-console)..."
openssl ecparam -genkey -name prime256v1 -noout -out "$CERT_DIR/console.key"
openssl req -new \
    -key "$CERT_DIR/console.key" \
    -out /tmp/atl-console.csr \
    -subj "/CN=atlantis-console"

openssl x509 -req \
    -in /tmp/atl-console.csr \
    -CA "$CERT_DIR/ca.crt" \
    -CAkey "$CA_PRIVATE_DIR/ca.key" \
    -CAcreateserial \
    -out "$CERT_DIR/console.crt" \
    -days "$DAYS"

rm /tmp/atl-console.csr

# ── Permissions ───────────────────────────────────────────────────────────────
# 644 so non-root service containers (atlantis UID 10001, console app user,
# signer's "signer" user) can read keys from the volumes. The security
# boundary here is the *volume mount scope*, not the file mode:
#   - atl-certs is mounted by atlantis (ro) + console (ro) + certs (rw)
#   - atl-ca-private is mounted ONLY by signer (ro) + certs (rw)
# Nothing else in the compose stack ever sees the CA key.
chmod 644 "$CERT_DIR/"*.key "$CERT_DIR/"*.crt
chmod 644 "$CA_PRIVATE_DIR/ca.key" "$CA_PRIVATE_DIR/ca.crt"
# (Caller-side keys issued via `make self-host-caller-cert` get 600 because
# they live on the host filesystem, not inside a scoped volume.)

echo "[certs] generated:"
ls -la "$CERT_DIR/"
echo "[certs] CA private dir:"
ls -la "$CA_PRIVATE_DIR/"
echo "[certs] done."
