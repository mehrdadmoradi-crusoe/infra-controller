#!/usr/bin/env bash
# Reverse of pb-peer.sh: delete the NICo VpcPeering object, remove the VRF
# leaking from the DPUs, and prove isolation is back (ping fails).
set -euo pipefail
cd "$(dirname -- "${BASH_SOURCE[0]}")"

vpcs=$(./nico-cli.sh -f json vpc show 2>/dev/null | grep -v IGNORING)
BLUE=$(echo "$vpcs"  | jq -r '.vpcs[] | select(.metadata.name=="vpc-blue") | .id')
GREEN=$(echo "$vpcs" | jq -r '.vpcs[] | select(.metadata.name=="vpc-green") | .id')

echo "==== 1. CONTROL PLANE: deleting VpcPeering object ===="
P=$(./nico-cli.sh -f json vpc-peering show 2>/dev/null | grep -v IGNORING \
  | jq -r --arg b "$BLUE" --arg g "$GREEN" \
    '.vpc_peerings[] | select((.vpc_id==$b and .peer_vpc_id==$g) or (.vpc_id==$g and .peer_vpc_id==$b)) | .id')
if [ -n "$P" ]; then
  ./nico-cli.sh vpc-peering delete --id "$P" 2>/dev/null | grep -v IGNORING || true
  echo "deleted peering $P"
else
  echo "no blue<->green peering object found"
fi

echo
echo "==== 2. DATA PLANE: removing VRF leaking from the DPUs ===="
frr/vpc-peering-delete.sh
sleep 3

echo
echo "==== 3. PROOF: isolation restored (ping must fail) ===="
docker exec dpu-hbn-02 ip vrf exec vrf-green ping -c 2 -W 1 -I 10.20.20.1 10.10.10.1 \
  && echo "UNEXPECTED: still reachable" \
  || echo "ISOLATED: vpc-green can no longer reach vpc-blue (expected)"
