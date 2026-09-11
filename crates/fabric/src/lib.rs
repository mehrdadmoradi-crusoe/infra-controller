//! carbide-fabric: delegated ToR-VRF backend for NICo.
//!
//! NICo owns tenant VRF/VPC **intent + lifecycle**; the enforcement backend is
//! pluggable. This crate is the mirror of `carbide-dpf` (the DPU/DPF client): where
//! DPF programs the DPU, this programs the ToR via a Kubernetes-native fabric
//! controller (Hedgehog first) by emitting `vpc.githedgehog.com/v1beta1` CRDs.
//!
//! Ownership seam (preserves "NICo never writes the underlay"): NICo emits
//! `VPC` / `VPCAttachment` / `VPCPeering` / `ExternalPeering`; the fabric team owns
//! `Connection` (wiring) and `External` (edge BGP), which NICo only references by name.
//!
//! Validated against Hedgehog vlab (SONiC): a NICo VPC → `VrfV<name>` on the leaf,
//! host attach → DHCP + gateway reachability, peering → controlled east-west.
//! Three constraints baked in below: VPC name ≤ 11 chars, l3vni needs non-ESLAG
//! connections, subnets must fall in the fabric's IPv4/VLAN namespaces.

use std::collections::BTreeMap;

use async_trait::async_trait;

pub mod eda;
pub use eda::EdaFabric;
use kube::api::{
    Api, ApiResource, DeleteParams, DynamicObject, GroupVersionKind, ListParams, Patch, PatchParams,
};
use kube::Client;
use serde::{Deserialize, Serialize};

#[derive(Debug, thiserror::Error)]
pub enum FabricError {
    #[error("kube error: {0}")]
    Kube(#[from] kube::Error),
    #[error("serde error: {0}")]
    Serde(#[from] serde_json::Error),
    #[error("invalid fabric intent: {0}")]
    Invalid(String),
    #[error("http error: {0}")]
    Http(#[from] reqwest::Error),
    #[error("eda api error: {0}")]
    Eda(String),
}

/// Off by default, exactly like `DpfConfig`. Enabling this is what makes NICo
/// drive the ToR fabric; with it false the whole backend is dormant.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FabricConfig {
    #[serde(default)]
    pub enabled: bool,
    /// Fabric-controller namespace the CRDs are applied into.
    #[serde(default = "default_namespace")]
    pub namespace: String,
    /// Which fabric controller backs this (Hedgehog via the cluster API, or EDA
    /// via its own API server).
    #[serde(default)]
    pub backend: FabricBackend,
    /// EDA connection details; required when `backend = "eda"`.
    #[serde(default)]
    pub eda: Option<eda::EdaConfig>,
}

fn default_namespace() -> String {
    "default".to_string()
}

impl Default for FabricConfig {
    fn default() -> Self {
        Self { enabled: false, namespace: default_namespace(), backend: FabricBackend::default(), eda: None }
    }
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, Default, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum FabricBackend {
    #[default]
    Hedgehog,
    Eda,
}

/// The tenant VRF/VPC intent NICo hands to the fabric backend. NICo already owns
/// every field (id from the VPC, prefix/gateway from IPAM, vlan from the segment,
/// vni from the VPC's allocated VNI).
#[derive(Debug, Clone)]
pub struct VrfIntent {
    pub nico_vpc_id: String,
    pub name: String,
    pub subnet_cidr: String,
    pub vlan: u16,
    pub gateway: String,
    pub vni: Option<u32>,
    /// DHCP range within the subnet, if NICo asks the fabric to serve leases.
    pub dhcp_range: Option<(String, String)>,
}

/// One host attachment: bind a tenant VPC subnet to the fabric `Connection` for a
/// host's port. `connection` is fabric-team-owned (from NetBox mapping); NICo only
/// references it.
#[derive(Debug, Clone)]
pub struct HostAttachment {
    pub vpc_name: String,
    pub connection: String,
}

/// The application-facing trait NICo's state controller calls — mirror of
/// `DpfOperations`. `MockFabricOperations` (mockall) backs the tests when the
/// `test-support` feature is on.
#[cfg_attr(feature = "test-support", mockall::automock)]
#[async_trait]
pub trait FabricOperations: Send + Sync + std::fmt::Debug {
    /// Create/update the tenant VRF (Hedgehog `VPC`, l3vni).
    async fn ensure_vrf(&self, intent: &VrfIntent) -> Result<(), FabricError>;
    /// Bind a host port into the VRF (Hedgehog `VPCAttachment`).
    async fn attach_host(&self, att: &HostAttachment) -> Result<(), FabricError>;
    /// Permit east-west between two tenant VPCs (Hedgehog `VPCPeering`).
    async fn peer_vpcs(&self, a: &str, b: &str) -> Result<(), FabricError>;
    /// Read back the programmed state of a VRF for reconciliation/status.
    async fn get_vrf_status(&self, vpc_name: &str) -> Result<Option<serde_json::Value>, FabricError>;
    /// List NICo-managed VRFs on the fabric as `(hedgehog_vpc_name, nico_vpc_id)`,
    /// so the reconcile can garbage-collect VRFs whose NICo VPC is gone.
    async fn list_vrfs(&self) -> Result<Vec<(String, String)>, FabricError>;
    /// Tear down a tenant VRF and everything attached to it (VPCAttachments +
    /// VPCPeerings + the VPC). Idempotent: deleting an absent object is a no-op.
    async fn delete_vrf(&self, vpc_name: &str) -> Result<(), FabricError>;
}

/// Hedgehog-backed implementation, applying `vpc.githedgehog.com/v1beta1` CRDs via
/// server-side apply. Dynamic objects (no generated types) keep the crate light;
/// the shape is validated against the live vlab.
#[derive(Clone)]
pub struct HedgehogFabric {
    client: Client,
    namespace: String,
}

impl std::fmt::Debug for HedgehogFabric {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("HedgehogFabric").field("namespace", &self.namespace).finish()
    }
}

const HH_GROUP: &str = "vpc.githedgehog.com";
const HH_VERSION: &str = "v1beta1";
/// Hedgehog rejects VPC names longer than this (validated on the vlab).
const MAX_VPC_NAME: usize = 11;

impl HedgehogFabric {
    pub async fn try_default(cfg: &FabricConfig) -> Result<Self, FabricError> {
        let client = Client::try_default().await?;
        Ok(Self { client, namespace: cfg.namespace.clone() })
    }

    pub fn new(client: Client, namespace: String) -> Self {
        Self { client, namespace }
    }

    /// Map a NICo VPC name/id to a Hedgehog-legal VPC name (≤ 11 chars). Real impl
    /// will keep a stable mapping; this deterministic truncation is enough to start.
    pub fn hedgehog_vpc_name(nico_name: &str) -> String {
        let n: String = nico_name.chars().take(MAX_VPC_NAME).collect();
        n.trim_matches('-').to_string()
    }

    fn api(&self, kind: &str) -> Api<DynamicObject> {
        let gvk = GroupVersionKind::gvk(HH_GROUP, HH_VERSION, kind);
        let ar = ApiResource::from_gvk(&gvk);
        Api::namespaced_with(self.client.clone(), &self.namespace, &ar)
    }

    async fn apply(&self, kind: &str, name: &str, labels: BTreeMap<String, String>,
                   spec: serde_json::Value) -> Result<(), FabricError> {
        let gvk = GroupVersionKind::gvk(HH_GROUP, HH_VERSION, kind);
        let ar = ApiResource::from_gvk(&gvk);
        let mut obj = DynamicObject::new(name, &ar).within(&self.namespace);
        obj.metadata.labels = Some(labels);
        obj.data = serde_json::json!({ "spec": spec });
        let api: Api<DynamicObject> = Api::namespaced_with(self.client.clone(), &self.namespace, &ar);
        api.patch(name, &PatchParams::apply("carbide-fabric").force(), &Patch::Apply(&obj))
            .await?;
        Ok(())
    }

    /// Delete one CRD by kind+name; treat a 404 as success (idempotent).
    async fn delete_obj(&self, kind: &str, name: &str) -> Result<(), FabricError> {
        match self.api(kind).delete(name, &DeleteParams::default()).await {
            Ok(_) => Ok(()),
            Err(kube::Error::Api(ae)) if ae.code == 404 => Ok(()),
            Err(e) => Err(e.into()),
        }
    }

    fn nico_labels(nico_vpc_id: &str, vni: Option<u32>) -> BTreeMap<String, String> {
        let mut l = BTreeMap::new();
        l.insert("nico.io/vpc-id".into(), nico_vpc_id.chars().take(63).collect());
        if let Some(v) = vni {
            l.insert("nico.io/vni".into(), v.to_string());
        }
        l
    }
}

#[async_trait]
impl FabricOperations for HedgehogFabric {
    async fn ensure_vrf(&self, intent: &VrfIntent) -> Result<(), FabricError> {
        let name = Self::hedgehog_vpc_name(&intent.name);
        let mut subnet = serde_json::json!({
            "subnet": intent.subnet_cidr,
            "vlan": intent.vlan,
            "gateway": intent.gateway,
        });
        if let Some((start, end)) = &intent.dhcp_range {
            subnet["dhcp"] = serde_json::json!({
                "enable": true,
                "range": { "start": start, "end": end },
            });
        }
        let spec = serde_json::json!({
            "ipv4Namespace": "default",
            "vlanNamespace": "default",
            "mode": "l3vni",                 // ToR-VRF = routed VRF on the leaf
            "subnets": { "default": subnet },
        });
        tracing::info!(vpc = %name, nico_id = %intent.nico_vpc_id, "fabric: ensure_vrf");
        self.apply("VPC", &name, Self::nico_labels(&intent.nico_vpc_id, intent.vni), spec).await
    }

    async fn attach_host(&self, att: &HostAttachment) -> Result<(), FabricError> {
        let vpc = Self::hedgehog_vpc_name(&att.vpc_name);
        let name = format!("{}--{}", att.connection, vpc);
        let name: String = name.chars().take(253).collect();
        let spec = serde_json::json!({
            "connection": att.connection,       // fabric-team-owned wiring, referenced only
            "subnet": format!("{}/default", vpc),
        });
        tracing::info!(vpc = %vpc, connection = %att.connection, "fabric: attach_host");
        self.apply("VPCAttachment", &name, BTreeMap::new(), spec).await
    }

    async fn peer_vpcs(&self, a: &str, b: &str) -> Result<(), FabricError> {
        let (a, b) = (Self::hedgehog_vpc_name(a), Self::hedgehog_vpc_name(b));
        let name = format!("{}--{}", a, b);
        let spec = serde_json::json!({
            "permit": [ {
                a.clone(): { "subnets": ["default"] },
                b.clone(): { "subnets": ["default"] },
            } ],
        });
        tracing::info!(a = %a, b = %b, "fabric: peer_vpcs");
        self.apply("VPCPeering", &name, BTreeMap::new(), spec).await
    }

    async fn get_vrf_status(&self, vpc_name: &str) -> Result<Option<serde_json::Value>, FabricError> {
        let name = Self::hedgehog_vpc_name(vpc_name);
        match self.api("VPC").get_opt(&name).await? {
            Some(o) => Ok(o.data.get("status").cloned()),
            None => Ok(None),
        }
    }

    async fn list_vrfs(&self) -> Result<Vec<(String, String)>, FabricError> {
        // Only VPCs NICo created carry the nico.io/vpc-id label.
        let lp = ListParams::default().labels("nico.io/vpc-id");
        let mut out = Vec::new();
        for o in self.api("VPC").list(&lp).await? {
            let name = o.metadata.name.clone().unwrap_or_default();
            let nico_id = o
                .metadata
                .labels
                .as_ref()
                .and_then(|l| l.get("nico.io/vpc-id").cloned())
                .unwrap_or_default();
            if !name.is_empty() && !nico_id.is_empty() {
                out.push((name, nico_id));
            }
        }
        Ok(out)
    }

    async fn delete_vrf(&self, vpc_name: &str) -> Result<(), FabricError> {
        let vpc = Self::hedgehog_vpc_name(vpc_name);
        // Attachments reference the VPC via spec.subnet = "<vpc>/<subnet>".
        let subnet_prefix = format!("{vpc}/");
        for a in self.api("VPCAttachment").list(&ListParams::default()).await? {
            let belongs = a
                .data
                .get("spec")
                .and_then(|s| s.get("subnet"))
                .and_then(|v| v.as_str())
                .map(|s| s.starts_with(&subnet_prefix))
                .unwrap_or(false);
            if belongs {
                if let Some(n) = a.metadata.name.as_deref() {
                    self.delete_obj("VPCAttachment", n).await?;
                }
            }
        }
        // Peerings reference the VPC as a key in each spec.permit[] entry.
        for p in self.api("VPCPeering").list(&ListParams::default()).await? {
            let involves = p
                .data
                .get("spec")
                .and_then(|s| s.get("permit"))
                .and_then(|v| v.as_array())
                .map(|arr| arr.iter().filter_map(|e| e.as_object()).any(|m| m.contains_key(&vpc)))
                .unwrap_or(false);
            if involves {
                if let Some(n) = p.metadata.name.as_deref() {
                    self.delete_obj("VPCPeering", n).await?;
                }
            }
        }
        tracing::info!(vpc = %vpc, "fabric: delete_vrf (VPC + attachments + peerings)");
        self.delete_obj("VPC", &vpc).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn vpc_name_is_hedgehog_legal() {
        assert_eq!(HedgehogFabric::hedgehog_vpc_name("openai-training"), "openai-trai");
        assert!(HedgehogFabric::hedgehog_vpc_name("a-very-long-vpc-name").len() <= MAX_VPC_NAME);
        assert_eq!(HedgehogFabric::hedgehog_vpc_name("frontend"), "frontend");
    }

    #[test]
    fn config_defaults_off() {
        let c = FabricConfig::default();
        assert!(!c.enabled);
        assert_eq!(c.backend, FabricBackend::Hedgehog);
    }

    #[test]
    fn nico_labels_carry_traceability() {
        let l = HedgehogFabric::nico_labels("c247304e-bda9", Some(2024542));
        assert_eq!(l.get("nico.io/vni").map(String::as_str), Some("2024542"));
        assert_eq!(l.get("nico.io/vpc-id").map(String::as_str), Some("c247304e-bda9"));
    }
}
