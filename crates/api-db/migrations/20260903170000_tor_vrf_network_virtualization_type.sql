---
--- 20260903170000_tor_vrf_network_virtualization_type.sql
---
--- Adds a fourth network virtualization type, `tor`, for VPCs whose tenant
--- VRF is enforced on the ToR/leaf switch rather than on the DPU. The host
--- attaches via its NIC (no DPU overlay); NICo owns the per-VPC VRF/VPC
--- intent (prefix, VLAN, VNI, gateway from IPAM) and delegates the switch
--- programming to an external Kubernetes-native fabric controller
--- (Hedgehog/EDA) by emitting its CRDs. NICo never writes the underlay.
--- See the `carbide-fabric` crate and `DataPlaneKind::FabricManaged`.
---
--- Additive only: existing rows (etv/fnn/flat) are unchanged.
---

ALTER TYPE network_virtualization_type_t ADD VALUE 'tor';
