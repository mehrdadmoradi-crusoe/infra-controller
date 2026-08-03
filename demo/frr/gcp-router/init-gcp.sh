#!/bin/sh
# "GCP side" of the interconnect: a plain router with the customer's GCP VPC
# subnet on a dummy interface (stands in for GCP Cloud Router + VPC).
set -e
ip link show dummy0 >/dev/null 2>&1 || ip link add dummy0 type dummy
ip link set dummy0 up
ip addr replace "${GCP_SUBNET_ADDR:?set GCP_SUBNET_ADDR}" dev dummy0

# Disable NIC offload (see init-evpn.sh) — VXLAN offload doesn't survive a VM
# reboot on virtio; sim-only, real hardware keeps offload.
command -v ethtool >/dev/null 2>&1 || apk add --no-cache ethtool >/dev/null 2>&1 || true
for _if in $(ls /sys/class/net | grep -E '^eth'); do
  ethtool -K "$_if" tx off rx off tso off gso off gro off >/dev/null 2>&1 || true
done

# Disable reverse-path filtering (see init-evpn.sh) — the interconnect return
# path is asymmetric across the customer VRF; rp_filter would drop replies.
sysctl -qw net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0 2>/dev/null || true
for _if in $(ls /sys/class/net); do
  sysctl -qw "net.ipv4.conf.$_if.rp_filter=0" 2>/dev/null || true
done

exec /usr/lib/frr/docker-start
