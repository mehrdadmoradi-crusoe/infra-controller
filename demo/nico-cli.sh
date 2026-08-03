#!/usr/bin/env bash
# Wrapper for nico-admin-cli inside the nico-api pod (kind cluster demo).
# Usage: ./nico-cli.sh <nico-admin-cli args...>   e.g. ./nico-cli.sh machine show
# Pin the context: deploying the tenant REST stack (§9) switches kubectl to
# kind-nico-rest-local, which would otherwise send these at the wrong cluster.
exec kubectl --context "${NICO_CONTEXT:-kind-nico}" exec -n nico-system deploy/nico-api -- \
  /opt/carbide/nico-admin-cli \
  --root-ca-path=/var/run/secrets/spiffe.io/ca.crt \
  --client-cert-path=/var/run/secrets/spiffe.io/tls.crt \
  --client-key-path=/var/run/secrets/spiffe.io/tls.key \
  -a https://localhost:1079 "$@"