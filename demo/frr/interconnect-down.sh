#!/usr/bin/env bash
# Disable the VPC interconnect: shut the eBGP session in the customer VRF.
# GCP routes are withdrawn everywhere within seconds.
set -e
docker exec tor-leaf-01 vtysh \
  -c 'configure terminal' \
  -c 'router bgp 65100 vrf vrf-blue' \
  -c ' neighbor 172.31.0.2 shutdown' \
  -c 'end' >/dev/null 2>&1
echo "INTERCONNECT DOWN: GCP session shut, routes withdrawn."
