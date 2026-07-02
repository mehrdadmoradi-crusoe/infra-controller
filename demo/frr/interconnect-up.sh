#!/usr/bin/env bash
# Enable the VPC interconnect: bring up the eBGP session between the
# customer VRF (vrf-blue) on the border leaf and the GCP cloud router.
# In the L3 model this IS the entire interconnect enablement.
set -e
docker exec tor-leaf-01 vtysh \
  -c 'configure terminal' \
  -c 'router bgp 65100 vrf vrf-blue' \
  -c ' no neighbor 172.31.0.2 shutdown' \
  -c 'end' >/dev/null 2>&1
echo "INTERCONNECT UP: vrf-blue <-> GCP (AS 16550) BGP session enabled."
