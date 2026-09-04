//! carbide-fabric-manager: periodic reconcile of NICo's ToR-VRF VPC intent to an
//! external fabric controller (Hedgehog/EDA) via `carbide_fabric::FabricOperations`.
//!
//! Design: a level-triggered periodic manager (like nvlink-manager / ib-fabric),
//! NOT a per-object state controller -- NICo VPCs carry a `VpcStatus`, not a
//! state-controller `ControllerState`. Every `run_interval` it lists the `TorVrf`
//! VPCs and, for each, reconciles the VRF (`ensure_vrf` from the VPC's own
//! HostInband segment), every placed host's attachment (`attach_host`, keyed off
//! the operator-declared `fabric.nico.io/connection` machine label), and peering
//! with other fabric-managed VPCs (`peer_vpcs` from `vpc_peering`).
//!
//! Validated end to end against a Hedgehog vlab via the `nico2hedgehog.py` adapter,
//! which is this reconcile's executable spec.

use std::sync::Arc;
use std::time::Duration;

use model::network_segment::NetworkSegmentType;
use model::vpc::Vpc;
use carbide_fabric::{FabricOperations, HostAttachment, VrfIntent};
use carbide_network::virtualization::VpcVirtualizationType;
use sqlx::PgPool;
use tokio::task::JoinSet;
use tokio_util::sync::CancellationToken;

/// Operator-declared label on a host's machine naming the fabric `Connection`
/// (leaf port wiring) it is cabled to. NetBox-syncable; NICo reads it, never
/// invents it. A host without it is simply not attached this pass.
const CONNECTION_LABEL: &str = "fabric.nico.io/connection";

#[cfg(test)]
mod tests;

/// Reconcile-loop tuning. Whether the loop runs at all is gated in `setup.rs` on
/// `fabric.enabled` (the backend switch), so there is deliberately no second
/// `enabled` here to get out of sync with it -- this only carries the cadence.
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct FabricManagerConfig {
    #[serde(default = "FabricManagerConfig::default_run_interval")]
    #[serde(with = "humantime_serde_secs")]
    pub run_interval: Duration,
}

impl FabricManagerConfig {
    pub const fn default_run_interval() -> Duration {
        Duration::from_secs(30)
    }
}

impl Default for FabricManagerConfig {
    fn default() -> Self {
        Self { run_interval: Self::default_run_interval() }
    }
}

/// Minimal seconds-as-number (de)serializer so the config stays plain, matching
/// how other managers accept an integer number of seconds.
mod humantime_serde_secs {
    use std::time::Duration;

    use serde::{Deserialize, Deserializer, Serializer};

    pub fn serialize<S: Serializer>(d: &Duration, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_u64(d.as_secs())
    }
    pub fn deserialize<'de, D: Deserializer<'de>>(d: D) -> Result<Duration, D::Error> {
        Ok(Duration::from_secs(u64::deserialize(d)?))
    }
}

/// Reconciles ToR-VRF VPC intent onto the fabric controller on a timer.
pub struct FabricManager {
    fabric: Arc<dyn FabricOperations>,
    db: PgPool,
    config: FabricManagerConfig,
}

impl std::fmt::Debug for FabricManager {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("FabricManager").field("config", &self.config).finish()
    }
}

impl FabricManager {
    pub fn new(fabric: Arc<dyn FabricOperations>, db: PgPool, config: FabricManagerConfig) -> Self {
        Self { fabric, db, config }
    }

    /// Spawn the reconcile loop into `join_set` (mirror of the other periodic
    /// managers). The caller gates this on `fabric.enabled`, so it spawns
    /// unconditionally here.
    pub fn start(self, join_set: &mut JoinSet<()>, cancel_token: CancellationToken) {
        join_set.spawn(async move { self.run(cancel_token).await });
    }

    /// Timer loop; stops on cancel.
    pub async fn run(&self, cancel_token: CancellationToken) {
        let mut ticker = tokio::time::interval(self.config.run_interval);
        loop {
            tokio::select! {
                _ = cancel_token.cancelled() => {
                    tracing::info!("fabric-manager: shutting down");
                    return;
                }
                _ = ticker.tick() => {
                    if let Err(e) = self.run_single_iteration().await {
                        tracing::warn!(error = %e, "fabric-manager: reconcile iteration failed");
                    }
                }
            }
        }
    }

    /// One reconcile pass: ensure a fabric VRF for every ToR-VRF VPC.
    pub async fn run_single_iteration(&self) -> eyre::Result<usize> {
        let vpcs = self.list_tor_vrf_vpcs().await?;
        tracing::debug!(count = vpcs.len(), "fabric-manager: reconciling ToR-VRF VPCs");
        let mut ok = 0usize;
        for vpc in &vpcs {
            match self.reconcile_vpc(vpc).await {
                Ok(()) => ok += 1,
                Err(e) => tracing::warn!(vpc = %vpc.id, error = %e, "fabric-manager: VPC reconcile failed"),
            }
        }
        Ok(ok)
    }

    /// All non-deleted VPCs whose virtualization type is ToR-VRF (wire `tor`).
    async fn list_tor_vrf_vpcs(&self) -> eyre::Result<Vec<Vpc>> {
        let query = "SELECT * FROM vpcs \
                     WHERE network_virtualization_type = 'tor' AND deleted IS NULL";
        let vpcs: Vec<Vpc> = sqlx::query_as(query)
            .fetch_all(&self.db)
            .await
            .map_err(|e| eyre::eyre!("listing ToR-VRF VPCs: {e}"))?;
        Ok(vpcs)
    }

    /// Ensure the fabric state for one ToR-VRF VPC matches NICo's intent:
    /// the VRF itself, every placed host's attachment, and peering with other
    /// fabric-managed VPCs. Every field has a real source (no invented data);
    /// anything not provisioned yet is skipped so the next pass picks it up
    /// (level-triggered).
    async fn reconcile_vpc(&self, vpc: &Vpc) -> eyre::Result<()> {
        debug_assert_eq!(
            vpc.config.network_virtualization_type,
            VpcVirtualizationType::TorVrf
        );

        // (a) The VPC owns a HostInband segment; it supplies subnet/VLAN/gateway.
        let segments = db::network_segment::for_vpc(&self.db, vpc.id).await?;
        let Some(seg) = segments
            .iter()
            .find(|s| s.config.segment_type == NetworkSegmentType::HostInband)
        else {
            tracing::info!(vpc = %vpc.id, "fabric-manager: no HostInband segment yet; skipping");
            return Ok(());
        };
        let Some(vlan_id) = seg.status.vlan_id else {
            tracing::info!(vpc = %vpc.id, "fabric-manager: segment VLAN not allocated yet; skipping");
            return Ok(());
        };
        // A HostInband segment normally has one prefix; take the first that carries
        // a gateway (subnet + gateway must go to the fabric together).
        let Some(prefix) = seg.prefixes.iter().find(|p| p.gateway.is_some()) else {
            tracing::info!(vpc = %vpc.id, "fabric-manager: segment prefix/gateway not ready; skipping");
            return Ok(());
        };
        let intent = VrfIntent {
            nico_vpc_id: vpc.id.to_string(),
            name: vpc.metadata.name.clone(),
            subnet_cidr: prefix.prefix.to_string(),
            vlan: vlan_id as u16,
            gateway: prefix
                .gateway
                .expect("gateway checked Some above")
                .to_string(),
            vni: vpc.config.vni.map(|v| v as u32),
            dhcp_range: None,
        };
        self.fabric.ensure_vrf(&intent).await?;

        // (b) Attach every placed host whose operator-declared Connection label is
        // set. NICo references the fabric-owned wiring by name; it never invents it.
        let instance_ids = db::instance::find_ids(
            &self.db,
            model::instance::InstanceSearchFilter {
                label: None,
                tenant_org_id: None,
                vpc_id: Some(vpc.id.to_string()),
                instance_type_id: None,
            },
        )
        .await?;
        for id in instance_ids {
            let Some(inst) = db::instance::find_by_id(&self.db, id).await? else {
                continue;
            };
            let Some(machine) = db::machine::find_one(
                &self.db,
                &inst.machine_id,
                model::machine::machine_search_config::MachineSearchConfig::default(),
            )
            .await?
            else {
                continue;
            };
            let Some(connection) = machine.metadata.labels.get(CONNECTION_LABEL) else {
                tracing::debug!(vpc = %vpc.id, machine = %inst.machine_id,
                    "fabric-manager: host has no {CONNECTION_LABEL} label; not attaching");
                continue;
            };
            self.fabric
                .attach_host(&HostAttachment {
                    vpc_name: vpc.metadata.name.clone(),
                    connection: connection.clone(),
                })
                .await?;
        }

        // (c) Peer with other fabric-managed (TorVrf) VPCs. NICo programs peering
        // only within the fabric-managed set; a Flat peer's side is the operator's.
        let mut conn = self
            .db
            .acquire()
            .await
            .map_err(|e| eyre::eyre!("fabric-manager: acquire connection: {e}"))?;
        let peer_ids = db::vpc_peering::get_vpc_peer_ids(&mut conn, vpc.id).await?;
        drop(conn);
        for peer_id in peer_ids {
            let Some(peer) = db::vpc::find_by(
                &self.db,
                db::ObjectColumnFilter::One(db::vpc::IdColumn, &peer_id),
            )
            .await?
            .pop() else {
                continue;
            };
            if peer.config.network_virtualization_type == VpcVirtualizationType::TorVrf {
                self.fabric
                    .peer_vpcs(&vpc.metadata.name, &peer.metadata.name)
                    .await?;
            }
        }
        Ok(())
    }
}
