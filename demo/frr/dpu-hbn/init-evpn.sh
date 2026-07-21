#!/bin/sh
# DPU init — EVPN symmetric IRB, the way NICo/HBN programs a BlueField:
# one kernel VRF per VPC (table id = the VPC's NICo VNI), each with an
# L3VNI: a bridge SVI plus a VXLAN device sourced from this DPU's VTEP
# loopback. Every DPU carries every VPC's VRF; instance subnets live on
# dummy interfaces inside the VRF of the VPC that owns them.
set -e
: "${VTEP_IP:?set VTEP_IP}"

ip addr replace "$VTEP_IP/32" dev lo
ip link set lo up

# All L3VNI SVIs share one system MAC (as HBN/Cumulus does) so the DPU
# advertises a single unambiguous router-MAC in its EVPN type-5 routes.
SYSTEM_MAC="02:de:0${SYSTEM_MAC_ID:?set SYSTEM_MAC_ID}:62:00:01"

setup_l3vni() {
  vrf=$1; vni=$2
  ip link show "$vrf" >/dev/null 2>&1 || ip link add "$vrf" type vrf table "$vni"
  ip link set "$vrf" up
  ip link show "br-$vrf" >/dev/null 2>&1 || ip link add "br-$vrf" type bridge
  ip link set "br-$vrf" address "$SYSTEM_MAC"
  ip link set "br-$vrf" master "$vrf" up
  ip link show "vni$vni" >/dev/null 2>&1 || \
    ip link add "vni$vni" type vxlan id "$vni" local "$VTEP_IP" dstport 4789 nolearning
  ip link set "vni$vni" master "br-$vrf" up
}

# L3VNIs = the VPCs' NICo-allocated VNIs. pb-seed.sh writes them to the mounted
# vpc-vnis.env; we source it here so a plain `docker restart` (no recreate)
# picks up new VNIs — recreate triggers FRR's type-5 origination race, a warm
# restart does not. Falls back to demo defaults if the seed hasn't run.
[ -f /etc/frr/vpc-vnis.env ] && . /etc/frr/vpc-vnis.env
setup_l3vni vrf-blue  "${VPC_BLUE_VNI:-2024508}"
setup_l3vni vrf-green "${VPC_GREEN_VNI:-2024520}"

# Tenant instance hosted on this DPU
if [ -n "${INSTANCE_VRF:-}" ]; then
  ip link show inst0 >/dev/null 2>&1 || ip link add inst0 type dummy
  ip link set inst0 master "$INSTANCE_VRF" up
  ip addr replace "${INSTANCE_ADDR:?set INSTANCE_ADDR}" dev inst0
fi

# Border-leaf role: enslave the interconnect-facing interface (identified by
# its IP) into the customer VRF — the 1:1 customer-to-VRF handoff for a
# cloud interconnect / dedicated port.
if [ -n "${INTERCONNECT_IP:-}" ] && [ -n "${INTERCONNECT_VRF:-}" ]; then
  IC_IF=$(ip -br addr | awk -v ip="$INTERCONNECT_IP" '$0 ~ ip {split($1,a,"@"); print a[1]; exit}')
  if [ -n "$IC_IF" ]; then
    ip link set "$IC_IF" master "$INTERCONNECT_VRF" up
    ip addr replace "$INTERCONNECT_IP" dev "$IC_IF"
    echo "interconnect: $IC_IF ($INTERCONNECT_IP) -> $INTERCONNECT_VRF"
  else
    echo "WARNING: no interface with $INTERCONNECT_IP found" >&2
  fi
fi

exec /usr/lib/frr/docker-start