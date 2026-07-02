#!/usr/bin/env bash
# Peer the vpc-playbook VPC with vpc-blue (IDs looked up automatically).
# Single foreground command so Runme detects completion cleanly.
set -euo pipefail
cd "$(dirname -- "${BASH_SOURCE[0]}")"

vpcs=$(./nico-cli.sh -f json vpc show 2>/dev/null | grep -v IGNORING)
PB=$(echo "$vpcs"   | jq -r '.vpcs[] | select(.metadata.name=="vpc-playbook") | .id')
BLUE=$(echo "$vpcs" | jq -r '.vpcs[] | select(.metadata.name=="vpc-blue") | .id')

[ -z "$PB" ] && { echo "vpc-playbook not found — run the vpc-create cell first"; exit 0; }
echo "peering vpc-playbook ($PB) <-> vpc-blue ($BLUE)"
./nico-cli.sh vpc-peering create "$PB" "$BLUE" 2>/dev/null | grep -v IGNORING || true
echo "--- current peerings ---"
./nico-cli.sh vpc-peering show 2>/dev/null | grep -v IGNORING
