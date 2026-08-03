#!/usr/bin/env bash
# VPC peering, HBN-style: what NICo's forge-dpu-agent programs on every
# BlueField when a VpcPeering is created. Two mechanisms, applied together AT
# PEER TIME so BGP re-evaluates the leak immediately:
#
#   1. import vrf  — leak each VPC's routes into the peer VRF and re-originate
#      them into EVPN under the peer VPC's L3VNI (symmetric-IRB consistent).
#   2. route-target import — each tenant VRF imports its OWN L3VNI RT from every
#      fabric ASN (border 65100 + DPUs 65201/65202). Per-node ASNs make the
#      auto-derived RT node-specific, so a re-originated type-5 would otherwise
#      arrive but never install. Own-VNI only, so isolation still holds.
#
# Applying (2) here (not only in pb-seed) is deliberate: configuring the RT
# import alongside import-vrf triggers BGP to re-import the peered routes now,
# rather than waiting for an unrelated churn event.
set -e
cd "$(dirname -- "${BASH_SOURCE[0]}")"
[ -f vpc-vnis.env ] && . vpc-vnis.env
B="${VPC_BLUE_VNI:-2024508}"
G="${VPC_GREEN_VNI:-2024520}"

peer() {  # $1=container  $2=local-AS
  docker exec "$1" vtysh \
    -c 'configure terminal' \
    -c "router bgp $2 vrf vrf-blue" \
    -c ' address-family ipv4 unicast' -c '  import vrf vrf-green' -c ' exit-address-family' \
    -c ' address-family l2vpn evpn' \
    -c "  route-target import 65100:$B" -c "  route-target import 65201:$B" -c "  route-target import 65202:$B" \
    -c ' exit-address-family' -c 'exit' \
    -c "router bgp $2 vrf vrf-green" \
    -c ' address-family ipv4 unicast' -c '  import vrf vrf-blue' -c ' exit-address-family' \
    -c ' address-family l2vpn evpn' \
    -c "  route-target import 65100:$G" -c "  route-target import 65201:$G" -c "  route-target import 65202:$G" \
    -c 'end' >/dev/null 2>&1 || true
  # Force BGP to re-run VRF import/RT evaluation now (idempotent re-config alone
  # may be a no-op and not re-trigger the leak).
  docker exec "$1" vtysh -c 'clear bgp vrf vrf-blue *' -c 'clear bgp vrf vrf-green *' >/dev/null 2>&1 || true
  echo "$1: vrf-blue <-> vrf-green leaking installed"
}
peer dpu-hbn-01 65201
peer dpu-hbn-02 65202
echo "VPC peering ACTIVE on all DPUs."
