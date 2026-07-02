#!/usr/bin/env bash
# Remove the vpc-playbook VPC and any peerings involving it.
set -euo pipefail
cd "$(dirname -- "${BASH_SOURCE[0]}")"

PB=$(./nico-cli.sh -f json vpc show 2>/dev/null | grep -v IGNORING \
  | jq -r '.vpcs[] | select(.metadata.name=="vpc-playbook") | .id')
[ -z "$PB" ] && { echo "vpc-playbook not found — nothing to clean"; exit 0; }

for P in $(./nico-cli.sh -f json vpc-peering show 2>/dev/null | grep -v IGNORING \
    | jq -r --arg id "$PB" '.vpc_peerings[] | select(.vpc_id==$id or .peer_vpc_id==$id) | .id'); do
  echo "deleting peering $P"
  ./nico-cli.sh vpc-peering delete --id "$P" 2>/dev/null | grep -v IGNORING || true
done

grpcurl -insecure --cert /tmp/nico-certs/tls.crt --key /tmp/nico-certs/tls.key \
  -d "{\"id\":{\"value\":\"$PB\"}}" localhost:1079 forge.Forge.DeleteVpc >/dev/null
echo "vpc-playbook deleted"
