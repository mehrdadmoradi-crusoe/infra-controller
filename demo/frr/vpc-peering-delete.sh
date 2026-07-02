#!/usr/bin/env bash
# Remove the cross-VRF leaking from every DPU (NICo VpcPeering delete).
set -e
unpeer() {
  docker exec "$1" vtysh \
    -c 'configure terminal' \
    -c "router bgp $2 vrf vrf-blue" \
    -c ' address-family ipv4 unicast' \
    -c '  no import vrf vrf-green' \
    -c 'exit' -c 'exit' \
    -c "router bgp $2 vrf vrf-green" \
    -c ' address-family ipv4 unicast' \
    -c '  no import vrf vrf-blue' \
    -c 'end' >/dev/null 2>&1
  echo "$1: leaking removed"
}
unpeer dpu-hbn-01 65201
unpeer dpu-hbn-02 65202
echo "VPC peering DELETED: VPCs isolated on all DPUs."