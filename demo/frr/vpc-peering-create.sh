#!/usr/bin/env bash
# VPC peering, HBN-style: install cross-VRF route leaking between vrf-blue
# and vrf-green on EVERY DPU — what NICo's forge-dpu-agent programs on each
# BlueField when a VpcPeering is created. EVPN-learned routes leak too, so
# peered traffic flows across the VXLAN fabric between DPUs.
set -e
peer() {
  docker exec "$1" vtysh \
    -c 'configure terminal' \
    -c "router bgp $2 vrf vrf-blue" \
    -c ' address-family ipv4 unicast' \
    -c '  import vrf vrf-green' \
    -c 'exit' -c 'exit' \
    -c "router bgp $2 vrf vrf-green" \
    -c ' address-family ipv4 unicast' \
    -c '  import vrf vrf-blue' \
    -c 'end' >/dev/null 2>&1
  echo "$1: vrf-blue <-> vrf-green leaking installed"
}
peer dpu-hbn-01 65201
peer dpu-hbn-02 65202
echo "VPC peering ACTIVE on all DPUs."