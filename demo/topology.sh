#!/usr/bin/env bash
# topology.sh — render the LIVE logical topology of the demo fabric as ASCII,
# reflecting current state: which VPC lives on which DPU (+ its NICo VNI),
# whether the two VPCs are PEERED or ISOLATED, and whether the cloud
# interconnect is UP or DOWN. Re-run it after ./pb-peer.sh or interconnect-up.sh
# to show the edges change live. Read-only; ~1s.
set -u
cd "$(dirname -- "${BASH_SOURCE[0]}")"

G=$'\e[1;32m'; R=$'\e[1;31m'; D=$'\e[2m'; B=$'\e[1m'; C=$'\e[36m'; X=$'\e[0m'

# VNIs (fast path: the seed env file; fall back to '?')
BLUE_VNI='?'; GREEN_VNI='?'
[ -f frr/vpc-vnis.env ] && . frr/vpc-vnis.env 2>/dev/null
BLUE_VNI="${VPC_BLUE_VNI:-?}"; GREEN_VNI="${VPC_GREEN_VNI:-?}"

q(){ docker exec "$1" vtysh -c "$2" 2>/dev/null; }

# State probes (data-plane truth, not just config)
PEER=DOWN
q dpu-hbn-02 'show ip route vrf vrf-green' | grep -q '10.10.10.0/24' && PEER=UP
ICON=DOWN
q dpu-hbn-01 'show ip route vrf vrf-blue' | grep -q '10.128.0.0/20' && ICON=UP
# EVPN sessions established with the ToR
NEI=$(q tor-leaf-01 'show bgp l2vpn evpn summary' | grep -cE '65201|65202')

peer_edge()   { [ "$PEER" = UP ] && printf "%s◀══ PEERED (0%% loss) ══▶%s" "$G" "$X" || printf "%s╳╳ ISOLATED (100%% loss) ╳╳%s" "$R" "$X"; }
peer_note()   { [ "$PEER" = UP ] && printf "%sroutes leaked between VRFs on every DPU (one API object)%s" "$D" "$X" || printf "%sdefault: a separate VRF per VPC, no path between them%s" "$D" "$X"; }
icon_edge()   { [ "$ICON" = UP ] && printf "%s══ interconnect UP: 10.128.0.0/20 as EVPN type-5 ══%s" "$G" "$X" || printf "%s·· interconnect DOWN ··%s" "$D" "$X"; }
fab()         { [ "${NEI:-0}" -ge 2 ] && printf "%sEVPN/VXLAN fabric — %s DPUs peered%s" "$G" "$NEI" "$X" || printf "%sfabric: DPUs not fully peered (%s)%s" "$R" "${NEI:-0}" "$X"; }

cat <<EOF

  ${B}NICo logical topology${X}  ·  $(fab)

                         ┌──────────────────────────────┐
                         │  ${B}tor-leaf-01${X}  AS 65100        │
                         │  EVPN transit + border leaf   │
                         └───────┬───────────────┬───────┘
                    type-5/VXLAN │               │ type-5/VXLAN
              ┌──────────────────┘               └──────────────────┐
   ┌──────────┴────────────┐                        ┌───────────────┴────────┐
   │ ${B}dpu-hbn-01${X}  AS 65201    │                        │ ${B}dpu-hbn-02${X}  AS 65202    │
   │ ${C}vrf-blue${X}  VNI ${BLUE_VNI}   │                        │ ${C}vrf-green${X} VNI ${GREEN_VNI}   │
   │ inst ${C}10.10.10.1/24${X}    │  $(peer_edge)  │ inst ${C}10.20.20.1/24${X}   │
   └──────────┬────────────┘                        └────────────────────────┘
              │                    $(peer_note)
     $(icon_edge)
      ┌───────┴─────────┐
      │ ${B}gcp-router${X}      │   "external cloud" · GCP VPC ${C}10.128.0.0/20${X}
      │ AS 16550        │   (AS 16550 = real GCP Partner Interconnect ASN)
      └─────────────────┘

  ${D}Legend:${X} ${G}green = active${X} · ${R}red = isolated/down${X}.  vrf-blue lives on hbn-01,
  vrf-green on hbn-02; every DPU carries a VRF per VPC (table-id = the VNI).
EOF
