//! Nokia EDA backend for the delegated ToR-VRF model.
//!
//! Second implementation of [`FabricOperations`], behind the same six-call
//! interface as the Hedgehog backend, to prove the model is controller-neutral.
//! EDA is Kubernetes-native: its stock `services.eda.nokia.com/v2` resources are
//! applied through the cluster API and EDA's config engine renders them into SR
//! Linux (or other) device configuration. NICo writes only tenant-level objects
//! and never touches nodes, topology or underlay.
//!
//! Mapping of one NICo isolation domain (VPC) onto EDA:
//!
//! | NICo intent            | EDA resource(s)                                      |
//! |------------------------|------------------------------------------------------|
//! | VRF                    | `Router` (EVPN-VXLAN IP-VRF)                         |
//! | subnet + gateway       | `BridgeDomain` + `IRBInterface` (anycast gateway)    |
//! | VLAN on host ports     | `VLAN` selecting interfaces labelled for this VPC    |
//! | host attach            | label `nico.io/vpc=<vrf>` on the host's `Interface`  |
//! | delete                 | remove the label, delete VLAN/IRB/BridgeDomain/Router |
//!
//! The one place this touches a fabric-owned object is the label on `Interface`
//! (metadata only, never `spec`): EDA's `VLAN` binds ports through label
//! selectors, so a per-VPC label is the idiomatic hook. That is a decision to
//! confirm with the network team; the alternative is per-port `BridgeInterface`
//! objects once the EDA version in use supports a VLAN id on them.
//!
//! VPC-to-VPC peering is not implemented here yet: EDA models VRF route leaking
//! through routing policies against the default router, not as a permit list
//! between two VRFs. `peer_vpcs` returns an explicit error so the reconcile logs
//! it and carries on with everything else.

use std::collections::BTreeMap;

use async_trait::async_trait;
use kube::api::{
    Api, ApiResource, DeleteParams, DynamicObject, GroupVersionKind, ListParams, Patch, PatchParams,
};
use kube::Client;

use crate::{FabricConfig, FabricError, FabricOperations, HostAttachment, VrfIntent};

const EDA_SERVICES_GROUP: &str = "services.eda.nokia.com";
const EDA_SERVICES_VERSION: &str = "v2";
const EDA_INTERFACES_GROUP: &str = "interfaces.eda.nokia.com";
const EDA_INTERFACES_VERSION: &str = "v1";

/// Label NICo places on a host's EDA `Interface` to bind it into a VRF's VLAN.
/// One host belongs to exactly one isolation domain, so a single-valued label is
/// enough.
pub const VPC_LABEL: &str = "nico.io/vpc";
/// Traceability labels, same as the Hedgehog backend.
const NICO_ID_LABEL: &str = "nico.io/vpc-id";
const NICO_VNI_LABEL: &str = "nico.io/vni";

/// EDA allocation pools the derived objects draw from. These are the names the
/// EDA services app ships with; a site may override them.
const VNI_POOL: &str = "vni-pool";
const EVI_POOL: &str = "evi-pool";
const TUNNEL_INDEX_POOL: &str = "tunnel-index-pool";

#[derive(Clone)]
pub struct EdaFabric {
    client: Client,
    namespace: String,
}

impl std::fmt::Debug for EdaFabric {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("EdaFabric").field("namespace", &self.namespace).finish()
    }
}

impl EdaFabric {
    /// Connect with the ambient kube config (`$KUBECONFIG` or in-cluster).
    pub async fn try_default(cfg: &FabricConfig) -> Result<Self, FabricError> {
        let client = Client::try_default().await?;
        Ok(Self { client, namespace: cfg.namespace.clone() })
    }

    pub fn new(client: Client, namespace: String) -> Self {
        Self { client, namespace }
    }

    /// Name shared by the Router, BridgeDomain, IRBInterface and VLAN derived for
    /// one NICo VPC. Prefixed so NICo-owned objects are recognisable next to the
    /// network team's own services; kept within Kubernetes' 63-char label limit
    /// because the same string is used as the interface label value.
    pub fn eda_name(nico_name: &str) -> String {
        let cleaned: String = nico_name
            .chars()
            .map(|c| if c.is_ascii_alphanumeric() || c == '-' { c.to_ascii_lowercase() } else { '-' })
            .collect();
        let cleaned = cleaned.trim_matches('-');
        let mut name = format!("nico-{cleaned}");
        name.truncate(63);
        name.trim_end_matches('-').to_string()
    }

    fn services_api(&self, kind: &str) -> Api<DynamicObject> {
        let gvk = GroupVersionKind::gvk(EDA_SERVICES_GROUP, EDA_SERVICES_VERSION, kind);
        Api::namespaced_with(self.client.clone(), &self.namespace, &ApiResource::from_gvk(&gvk))
    }

    fn interfaces_api(&self) -> Api<DynamicObject> {
        let gvk = GroupVersionKind::gvk(EDA_INTERFACES_GROUP, EDA_INTERFACES_VERSION, "Interface");
        Api::namespaced_with(self.client.clone(), &self.namespace, &ApiResource::from_gvk(&gvk))
    }

    async fn apply_service(
        &self,
        kind: &str,
        name: &str,
        labels: BTreeMap<String, String>,
        spec: serde_json::Value,
    ) -> Result<(), FabricError> {
        let gvk = GroupVersionKind::gvk(EDA_SERVICES_GROUP, EDA_SERVICES_VERSION, kind);
        let ar = ApiResource::from_gvk(&gvk);
        let mut obj = DynamicObject::new(name, &ar).within(&self.namespace);
        obj.metadata.labels = Some(labels);
        obj.data = serde_json::json!({ "spec": spec });
        self.services_api(kind)
            .patch(name, &PatchParams::apply("carbide-fabric").force(), &Patch::Apply(&obj))
            .await?;
        Ok(())
    }

    /// Delete one service object; a 404 is success (idempotent).
    async fn delete_service(&self, kind: &str, name: &str) -> Result<(), FabricError> {
        match self.services_api(kind).delete(name, &DeleteParams::default()).await {
            Ok(_) => Ok(()),
            Err(kube::Error::Api(ae)) if ae.code == 404 => Ok(()),
            Err(e) => Err(e.into()),
        }
    }

    /// Set or clear the per-VPC label on a fabric-owned `Interface`. Metadata-only
    /// merge patch: `spec` is never touched.
    async fn label_interface(&self, interface: &str, vpc: Option<&str>) -> Result<(), FabricError> {
        let patch = serde_json::json!({ "metadata": { "labels": { VPC_LABEL: vpc } } });
        self.interfaces_api()
            .patch(interface, &PatchParams::default(), &Patch::Merge(&patch))
            .await?;
        Ok(())
    }

    fn nico_labels(nico_vpc_id: &str, vni: Option<u32>) -> BTreeMap<String, String> {
        let mut l = BTreeMap::new();
        l.insert(NICO_ID_LABEL.into(), nico_vpc_id.chars().take(63).collect());
        if let Some(v) = vni {
            l.insert(NICO_VNI_LABEL.into(), v.to_string());
        }
        l
    }

    /// `10.0.20.1` + `10.0.20.0/24` -> `10.0.20.1/24` (the IRB wants the gateway
    /// address with the subnet's prefix length).
    fn gateway_prefix(gateway: &str, subnet_cidr: &str) -> Result<String, FabricError> {
        let len = subnet_cidr
            .rsplit_once('/')
            .map(|(_, l)| l)
            .ok_or_else(|| FabricError::Invalid(format!("subnet {subnet_cidr} has no prefix length")))?;
        Ok(format!("{gateway}/{len}"))
    }
}

#[async_trait]
impl FabricOperations for EdaFabric {
    async fn ensure_vrf(&self, intent: &VrfIntent) -> Result<(), FabricError> {
        let name = Self::eda_name(&intent.name);
        let labels = Self::nico_labels(&intent.nico_vpc_id, intent.vni);
        let encap = serde_json::json!({
            "vxlan": { "vniPool": VNI_POOL, "tunnelIndexPool": TUNNEL_INDEX_POOL }
        });
        // The L3 VNI is NICo's own allocation (the VPC's VNI) so it stays stable
        // across backends; the L2 VNI for the bridge domain comes from EDA's pool.
        let mut router_encap = encap.clone();
        if let Some(vni) = intent.vni {
            router_encap["vxlan"]["vni"] = serde_json::json!(vni);
        }
        tracing::info!(vrf = %name, nico_id = %intent.nico_vpc_id, "fabric(eda): ensure_vrf");

        // IP-VRF.
        self.apply_service(
            "Router",
            &name,
            labels.clone(),
            serde_json::json!({
                "type": "EVPNVXLAN",
                "description": format!("NICo isolation domain {} ({})", intent.name, intent.nico_vpc_id),
                "encapOptions": router_encap,
                "eviPool": EVI_POOL,
            }),
        )
        .await?;

        // L2 domain for the (single) HostInband subnet.
        self.apply_service(
            "BridgeDomain",
            &name,
            labels.clone(),
            serde_json::json!({
                "type": "EVPNVXLAN",
                "description": format!("NICo subnet {} of {}", intent.subnet_cidr, intent.name),
                "encapOptions": encap,
                "eviPool": EVI_POOL,
                "macLearning": { "enabled": true, "agingTimeSeconds": 300 },
            }),
        )
        .await?;

        // Anycast gateway for that subnet inside the VRF.
        self.apply_service(
            "IRBInterface",
            &name,
            labels.clone(),
            serde_json::json!({
                "bridgeDomain": name,
                "router": name,
                "description": format!("NICo gateway {} for {}", intent.gateway, intent.name),
                "ipAddresses": [ {
                    "ipv4Address": {
                        "ipPrefix": Self::gateway_prefix(&intent.gateway, &intent.subnet_cidr)?,
                        "primary": true,
                        "anycast": true,
                    }
                } ],
            }),
        )
        .await?;

        // The host-facing VLAN: binds every Interface labelled for this VPC. Hosts
        // are added by attach_host, which sets that label.
        self.apply_service(
            "VLAN",
            &name,
            labels,
            serde_json::json!({
                "bridgeDomain": name,
                "vlanID": intent.vlan.to_string(),
                "interfaceSelectors": [ format!("{VPC_LABEL}={name}") ],
                "description": format!("NICo VLAN {} for {}", intent.vlan, intent.name),
            }),
        )
        .await
    }

    async fn attach_host(&self, att: &HostAttachment) -> Result<(), FabricError> {
        let vrf = Self::eda_name(&att.vpc_name);
        tracing::info!(vrf = %vrf, interface = %att.connection, "fabric(eda): attach_host");
        // `connection` is the fabric-owned Interface resource the host is cabled to
        // (e.g. `leaf1-ethernet-1-3`), recorded on the NICo machine from inventory.
        self.label_interface(&att.connection, Some(&vrf)).await
    }

    async fn peer_vpcs(&self, a: &str, b: &str) -> Result<(), FabricError> {
        Err(FabricError::Invalid(format!(
            "VPC peering {a}<->{b} is not implemented on the EDA backend yet \
             (EDA leaks VRF routes through policies, not a permit list)"
        )))
    }

    async fn get_vrf_status(&self, vpc_name: &str) -> Result<Option<serde_json::Value>, FabricError> {
        let name = Self::eda_name(vpc_name);
        match self.services_api("Router").get_opt(&name).await? {
            Some(o) => Ok(o.data.get("status").cloned()),
            None => Ok(None),
        }
    }

    async fn list_vrfs(&self) -> Result<Vec<(String, String)>, FabricError> {
        let lp = ListParams::default().labels(NICO_ID_LABEL);
        let mut out = Vec::new();
        for o in self.services_api("Router").list(&lp).await? {
            let name = o.metadata.name.clone().unwrap_or_default();
            let nico_id = o
                .metadata
                .labels
                .as_ref()
                .and_then(|l| l.get(NICO_ID_LABEL).cloned())
                .unwrap_or_default();
            if !name.is_empty() && !nico_id.is_empty() {
                out.push((name, nico_id));
            }
        }
        Ok(out)
    }

    async fn delete_vrf(&self, vpc_name: &str) -> Result<(), FabricError> {
        // `vpc_name` may already be the fabric-side name (from list_vrfs) or the
        // NICo name; eda_name is idempotent on its own output.
        let name = if vpc_name.starts_with("nico-") {
            vpc_name.to_string()
        } else {
            Self::eda_name(vpc_name)
        };
        tracing::info!(vrf = %name, "fabric(eda): delete_vrf");
        // Detach hosts first so the VLAN has no members when it goes.
        let lp = ListParams::default().labels(&format!("{VPC_LABEL}={name}"));
        for i in self.interfaces_api().list(&lp).await? {
            if let Some(n) = i.metadata.name.as_deref() {
                self.label_interface(n, None).await?;
            }
        }
        for kind in ["VLAN", "IRBInterface", "BridgeDomain", "Router"] {
            self.delete_service(kind, &name).await?;
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn eda_name_is_prefixed_and_label_safe() {
        assert_eq!(EdaFabric::eda_name("frontend"), "nico-frontend");
        assert_eq!(EdaFabric::eda_name("OpenAI Training"), "nico-openai-training");
        assert!(EdaFabric::eda_name(&"x".repeat(100)).len() <= 63);
    }

    #[test]
    fn gateway_takes_subnet_prefix_length() {
        assert_eq!(EdaFabric::gateway_prefix("10.0.20.1", "10.0.20.0/24").unwrap(), "10.0.20.1/24");
        assert!(EdaFabric::gateway_prefix("10.0.20.1", "10.0.20.0").is_err());
    }
}
