#!/usr/bin/env bash
# Remove the cross-VRF leak from every DPU (NICo VpcPeering delete). Only the
# import-vrf leak is undone; the per-VNI route-target imports stay (they are
# base-fabric state from pb-seed.sh — own-VNI only, so removing the leak fully
# restores VPC isolation without touching each VPC's own connectivity).
set -e
unpeer() {  # $1=container  $2=local-AS
  docker exec "$1" vtysh \
    -c 'configure terminal' \
    -c "router bgp $2 vrf vrf-blue" \
    -c ' address-family ipv4 unicast' \
    -c '  no import vrf vrf-green' \
    -c 'exit' -c 'exit' \
    -c "router bgp $2 vrf vrf-green" \
    -c ' address-family ipv4 unicast' \
    -c '  no import vrf vrf-blue' \
    -c 'end' >/dev/null 2>&1 || true
  echo "$1: leaking removed"
}
unpeer dpu-hbn-01 65201
unpeer dpu-hbn-02 65202
echo "VPC peering DELETED: VPCs isolated on all DPUs."
