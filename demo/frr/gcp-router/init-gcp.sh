#!/bin/sh
# "GCP side" of the interconnect: a plain router with the customer's GCP VPC
# subnet on a dummy interface (stands in for GCP Cloud Router + VPC).
set -e
ip link show dummy0 >/dev/null 2>&1 || ip link add dummy0 type dummy
ip link set dummy0 up
ip addr replace "${GCP_SUBNET_ADDR:?set GCP_SUBNET_ADDR}" dev dummy0
exec /usr/lib/frr/docker-start
