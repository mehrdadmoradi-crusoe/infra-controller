#!/usr/bin/env bash
# Tenant API tour — shows the full tenant-facing NICo API surface, then proves a
# representative slice is live and authenticated against the running REST stack.
#
#   Live stack: kind cluster `nico-rest-local` (Keycloak NodePort 8082, API 8388).
#   Auth:       Keycloak realm nico-dev, client nico-api, user testuser (TENANT_ADMIN
#               on org test-org). Password grant.
#
# Usage:  ./tenant-api-tour.sh            # catalog + live probe
#         ./tenant-api-tour.sh --catalog  # catalog only (no stack needed)
set -uo pipefail

KC=${KC:-http://localhost:8082}
API=${API:-http://localhost:8388}
ORG=${ORG:-test-org}
REALM=${REALM:-nico-dev}
CLIENT=${CLIENT:-nico-api}
SECRET=${SECRET:-nico-local-secret}
USER=${USER_NAME:-testuser}
PASS=${PASS:-demo}

bold(){ printf "\033[1m%s\033[0m\n" "$*"; }
dim(){ printf "\033[2m%s\033[0m\n" "$*"; }

cat <<'BANNER'

  ────────────────────────────────────────────────────────────────
   NICo tenant API — the full surface a customer drives (no BMC,
   no hypervisor; org-scoped REST, Keycloak JWT, role-gated).
  ────────────────────────────────────────────────────────────────
BANNER

bold "Compute & access"
cat <<'EOF'
  instance                  create · batch-create (rack-coherent) · get · list
                            update (triggerReboot / rebootWithCustomIpxe /
                            applyUpdatesOnReboot) · delete · status-history · console
  instance interfaces       net / infiniband / nvlink (read)
  instance-type             list · get
  machine                   get · status-history · dpu-machines  (read)
  sshkey / sshkeygroup      create · get · list · update · delete
  operating-system          create · get · list · update · delete
EOF
echo
bold "Networking"
cat <<'EOF'
  vpc                       create · get · list · update · set-virtualization · delete
  subnet                    create · get · list · update · delete
  vpc-prefix                create · get · list · update · delete
  vpc-peering               create · get · list · delete
                            (own VPCs; cross-tenant peering is provider-approved)
  network-security-group    create · get · list · update · delete
EOF
echo
bold "Fabrics & platform"
cat <<'EOF'
  infiniband-partition      create · get · list · update · delete
  nvlink-logical-partition  create · get · list · update · delete
  dpu-extension-service     create · get · list · update · delete (+ versions)
  allocation                get · list        (view own grants)
  tenant                    current · stats · update-account
  tenant-identity           config + token-delegation (put/get/delete)
  audit / metadata / sku    read
EOF
echo
dim "  All paths are /v2/org/{org}/nico/{resource}. Roughly 60 authenticated"
dim "  tenant operations. This is a cloud API for bare metal, not a wrapper."
echo

[ "${1:-}" = "--catalog" ] && exit 0

echo "  ────────────────────────────────────────────────────────────────"
bold "  Live proof — authenticating as $USER (TENANT_ADMIN) on $ORG"
echo "  ────────────────────────────────────────────────────────────────"

TOK=$(curl -sS --max-time 8 -X POST "$KC/realms/$REALM/protocol/openid-connect/token" \
  -d grant_type=password -d client_id="$CLIENT" -d client_secret="$SECRET" \
  -d username="$USER" -d password="$PASS" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('access_token',''))" 2>/dev/null)

if [ -z "$TOK" ]; then
  echo "  ! could not mint a token from Keycloak at $KC"
  echo "    (is the nico-rest-local stack up and port-forwarded? run with --catalog to skip)"
  exit 1
fi

python3 - "$TOK" <<'PY'
import base64, json, sys
t = sys.argv[1].split(".")
p = t[1] + "=" * (-len(t[1]) % 4)
d = json.loads(base64.urlsafe_b64decode(p))
print("  JWT ok  sub=%s  roles=%s" % (d.get("preferred_username"),
      [r for r in d.get("realm_access", {}).get("roles", []) if ":" in r]))
PY
echo

# GET with retry: the local mock-core is partial and can hard-restart the API
# process mid-request, so an in-flight call may return 000. Retry briefly so the
# live proof is deterministic (this flakiness is the test double, not the API).
get_code(){
  local path=$1 code
  for _ in 1 2 3 4 5; do
    code=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 8 \
      -H "Authorization: Bearer $TOK" "$API/v2/org/$ORG/nico/$path" 2>/dev/null)
    [ "$code" != "000" ] && { echo "$code"; return; }
    sleep 1
  done
  echo "$code"
}

# readiness gate — wait until the API answers before probing
printf "  waiting for API"
for _ in $(seq 1 20); do
  [ "$(get_code tenant/current)" != "000" ] && break
  printf "."; sleep 1
done
echo

echo "  Authenticated GETs against the live tenant API:"
for path in tenant/current vpc subnet network-security-group \
            infiniband-partition nvlink-logical-partition \
            sshkey sshkeygroup operating-system allocation audit; do
  printf "    %-28s HTTP %s\n" "GET $path" "$(get_code "$path")"
done
echo
dim "  200/empty-list = endpoint served, token accepted, RBAC passed."
dim "  Reboot/console/delete are the same authenticated surface (PATCH/POST)."
