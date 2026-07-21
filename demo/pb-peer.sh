#!/usr/bin/env bash
# VPC peering, end to end:
#   1. create the VpcPeering object in NICo (control plane)
#   2. apply per-VPC VRF route leaking on every DPU — the job forge-dpu-agent
#      does on real BlueFields when it sees the peering object
#   3. prove it: leaked route + cross-VPC ping over the VXLAN fabric
set -euo pipefail
cd "$(dirname -- "${BASH_SOURCE[0]}")"

vpcs=$(./nico-cli.sh -f json vpc show 2>/dev/null | grep -v IGNORING)
BLUE=$(echo "$vpcs"  | jq -r '.vpcs[] | select(.metadata.name=="vpc-blue") | .id')
GREEN=$(echo "$vpcs" | jq -r '.vpcs[] | select(.metadata.name=="vpc-green") | .id')
if [ -z "$BLUE" ] || [ -z "$GREEN" ]; then
  echo "vpc-blue/vpc-green not found in NICo — run ./pb-seed.sh first"; exit 1
fi

echo "==== 1. CONTROL PLANE: VpcPeering object in NICo ===="
existing=$(./nico-cli.sh -f json vpc-peering show 2>/dev/null | grep -v IGNORING \
  | jq -r --arg b "$BLUE" --arg g "$GREEN" \
    '.vpc_peerings[] | select((.vpc_id==$b and .peer_vpc_id==$g) or (.vpc_id==$g and .peer_vpc_id==$b)) | .id')
if [ -n "$existing" ]; then
  echo "peering already exists: $existing"
else
  ./nico-cli.sh vpc-peering create "$BLUE" "$GREEN" 2>/dev/null | grep -v IGNORING
fi

echo
echo "==== 2. DATA PLANE: applying VRF leaking on the DPUs (as forge-dpu-agent) ===="
frr/vpc-peering-create.sh
# wait for the cross-VPC route to actually leak in (EVPN type-5 + import vrf),
# rather than a fixed sleep — the fabric converges in a few seconds
echo -n "   waiting for vpc-blue's route to leak into vrf-green"
for t in $(seq 1 12); do
  sleep 2; echo -n "."
  docker exec dpu-hbn-02 vtysh -c 'show ip route vrf vrf-green' 2>/dev/null \
    | grep -q '10.10.10.0/24' && { echo " leaked (~$((t*2))s)"; break; }
done

echo
echo "==== 3. PROOF: leaked cross-VRF route on dpu-hbn-02 ===="
docker exec dpu-hbn-02 vtysh -c 'show ip route vrf vrf-green' | grep -E '^B|^C' || true

echo
echo "==== 4. PROOF: vpc-green instance pings vpc-blue instance over VXLAN ===="
docker exec dpu-hbn-02 ip vrf exec vrf-green ping -c 3 -W 1 -I 10.20.20.1 10.10.10.1
echo
echo "PEERED: 10.20.20.1 (vpc-green, dpu-hbn-02) can reach 10.10.10.1 (vpc-blue, dpu-hbn-01)"
