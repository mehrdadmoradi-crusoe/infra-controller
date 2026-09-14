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

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;
use std::time::Duration;

use carbide_fabric::{
    Capabilities, FabricError, FabricOperations, PortContract, PortMembership, VrfIntent, Witnesses,
};
use carbide_network::virtualization::VpcVirtualizationType;
use carbide_uuid::machine::MachineId;
use health_report::{HealthProbeAlert, HealthReport, HealthReportApplyMode};
use model::network_segment::NetworkSegmentType;
use model::vpc::Vpc;
use sqlx::PgPool;
use tokio::task::JoinSet;
use tokio_util::sync::CancellationToken;

/// Operator-declared label on a host's machine naming the fabric `Connection`
/// (leaf port wiring) it is cabled to. NetBox-syncable; NICo reads it, never
/// invents it. A host without it is simply not attached this pass.
const CONNECTION_LABEL: &str = "fabric.nico.io/connection";

/// `HealthReport.source` under which the reconcile raises and clears the
/// witness-mismatch alert on a machine.
pub const WITNESS_HEALTH_SOURCE: &str = "fabric-witness";
/// Probe id of that alert.
pub const WITNESS_ALERT_ID: &str = "FabricWitnessMismatch";

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
        Self {
            run_interval: Self::default_run_interval(),
        }
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
    /// Name of the provider-owned VPC whose VRF is the quarantine VRF (see
    /// `FabricConfig::quarantine_vpc`).
    quarantine_vpc: Option<String>,
}

impl std::fmt::Debug for FabricManager {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("FabricManager")
            .field("config", &self.config)
            .finish()
    }
}

impl FabricManager {
    pub fn new(fabric: Arc<dyn FabricOperations>, db: PgPool, config: FabricManagerConfig) -> Self {
        Self {
            fabric,
            db,
            config,
            quarantine_vpc: None,
        }
    }

    /// Name the quarantine VPC. Unplaced ports are members of its VRF on
    /// adapters that declare `quarantine_vrf`; without it they are unbound.
    pub fn with_quarantine_vpc(mut self, name: Option<String>) -> Self {
        self.quarantine_vpc = name;
        self
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

    /// One reconcile pass: a fabric VRF for every ToR-VRF VPC, then every
    /// managed port in exactly one VRF (a tenant's or quarantine), then GC.
    pub async fn run_single_iteration(&self) -> eyre::Result<usize> {
        let vpcs = self.list_tor_vrf_vpcs().await?;
        tracing::debug!(
            count = vpcs.len(),
            "fabric-manager: reconciling ToR-VRF VPCs"
        );
        let mut ok = 0usize;
        let mut ready: Vec<&Vpc> = Vec::new();
        for vpc in &vpcs {
            match self.reconcile_vpc(vpc).await {
                Ok(true) => {
                    ok += 1;
                    ready.push(vpc);
                }
                Ok(false) => {}
                Err(e) => {
                    tracing::warn!(vpc = %vpc.id, error = %e, "fabric-manager: VPC reconcile failed")
                }
            }
        }
        if let Some(q) = &self.quarantine_vpc
            && !ready.iter().any(|v| &v.metadata.name == q)
        {
            tracing::warn!(quarantine_vpc = %q,
                "fabric-manager: quarantine VPC is not a ready ToR-VRF VPC; unplaced ports stay where they are");
        }
        // Ports after VRFs, GC after both; a port-side failure is reported after
        // GC has run so one bad read never leaves orphans behind.
        let ports = self.reconcile_ports(&ready).await;
        self.gc_orphaned_vrfs(&vpcs).await;
        ports.map_err(|e| eyre::eyre!("fabric-manager: port reconcile failed: {e}"))?;
        Ok(ok)
    }

    /// Ports NICo wants bound somewhere, keyed by the fabric port name from the
    /// cabling record. Placed hosts (a live instance in a ready ToR-VRF VPC) go
    /// to that VPC's VRF with their NIC MACs as witnesses; every other host that
    /// has a cabling record, discovered or only expected, goes to quarantine.
    async fn desired_ports(&self, ready: &[&Vpc]) -> eyre::Result<BTreeMap<String, DesiredPort>> {
        let mut desired: BTreeMap<String, DesiredPort> = BTreeMap::new();
        for vpc in ready {
            if Some(&vpc.metadata.name) == self.quarantine_vpc.as_ref() {
                continue;
            }
            let instance_ids: Vec<carbide_uuid::instance::InstanceId> = sqlx::query_scalar(
                "SELECT DISTINCT i.id FROM instances i \
                 JOIN instance_addresses a ON a.instance_id = i.id \
                 WHERE a.vpc_id = $1 AND i.deleted IS NULL",
            )
            .bind(vpc.id)
            .fetch_all(&self.db)
            .await
            .map_err(|e| {
                eyre::eyre!(
                    "fabric-manager: list live instances for vpc {}: {e}",
                    vpc.id
                )
            })?;
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
                let Some(port) = machine.metadata.labels.get(CONNECTION_LABEL) else {
                    tracing::debug!(vpc = %vpc.id, machine = %inst.machine_id,
                        "fabric-manager: host has no {CONNECTION_LABEL} label; not attaching");
                    continue;
                };
                let macs: Vec<String> = machine
                    .hardware_info
                    .as_ref()
                    .map(|h| {
                        h.network_interfaces
                            .iter()
                            .map(|n| n.mac_address.to_string().to_ascii_lowercase())
                            .collect()
                    })
                    .unwrap_or_default();
                desired.insert(
                    port.clone(),
                    DesiredPort {
                        vrf: Some(vpc.metadata.name.clone()),
                        machine_id: Some(inst.machine_id),
                        witnesses: Witnesses {
                            expected_macs: macs.clone(),
                            // Zero-DPU hosts report no LLDP today (scout gap);
                            // the adapter skips the check when this is None.
                            expected_lldp: None,
                        },
                        contract: PortContract {
                            allowed_macs: macs,
                            allowed_ips: Vec::new(),
                            dhcp_snooping: true,
                            storm_control: true,
                            isolated_port: false,
                        },
                    },
                );
            }
        }
        // Discovered hosts with a cabling record that are not placed.
        let labelled: Vec<(MachineId, String)> =
            sqlx::query_as("SELECT id, labels->>$1 FROM machines WHERE labels ? $1")
                .bind(CONNECTION_LABEL)
                .fetch_all(&self.db)
                .await
                .map_err(|e| eyre::eyre!("fabric-manager: list labelled machines: {e}"))?;
        for (machine_id, port) in labelled {
            desired.entry(port).or_insert(DesiredPort {
                vrf: None,
                machine_id: Some(machine_id),
                witnesses: Witnesses::default(),
                contract: PortContract::default(),
            });
        }
        // Expected (not yet discovered) hosts: their port must be in quarantine
        // before the first DHCP, or discovery never starts.
        let expected: Vec<(String,)> = sqlx::query_as(
            "SELECT metadata_labels->>$1 FROM expected_machines WHERE metadata_labels ? $1",
        )
        .bind(CONNECTION_LABEL)
        .fetch_all(&self.db)
        .await
        .map_err(|e| eyre::eyre!("fabric-manager: list labelled expected machines: {e}"))?;
        for (port,) in expected {
            desired.entry(port).or_insert(DesiredPort {
                vrf: None,
                machine_id: None,
                witnesses: Witnesses::default(),
                contract: PortContract::default(),
            });
        }
        Ok(desired)
    }

    /// Bring every managed port to its desired VRF. Diffs against the fabric's
    /// own listing so a converged pass makes no writes; refusals (witnesses)
    /// become a health alert on the machine and are retried next pass.
    async fn reconcile_ports(&self, ready: &[&Vpc]) -> eyre::Result<()> {
        let caps: Capabilities = self.fabric.capabilities().await?;
        let desired = self.desired_ports(ready).await?;
        let current: BTreeMap<String, Option<String>> = self
            .fabric
            .list_port_memberships()
            .await?
            .into_iter()
            .map(|m| (m.port, m.vrf))
            .collect();
        let quarantine = self.quarantine_vpc.as_deref();
        let is_quarantine = |v: Option<&str>| v.is_none() || v == quarantine;
        let ready_names: BTreeSet<&str> = ready.iter().map(|v| v.metadata.name.as_str()).collect();

        // 1. Ports bound to a tenant VRF that NICo no longer places there.
        for (port, cur) in &current {
            let cur = cur.as_deref();
            if is_quarantine(cur) {
                continue;
            }
            let want = desired.get(port).and_then(|d| d.vrf.as_deref());
            if want == cur || (want.is_some() && !is_quarantine(want)) {
                // Same VRF, or a move between tenant VRFs (done in step 2).
                continue;
            }
            tracing::info!(%port, from = ?cur, "fabric-manager: port leaves tenant VRF");
            let m = PortMembership {
                port: port.clone(),
                vrf: None,
                previous_vrf: cur.map(str::to_string),
                ..PortMembership::default()
            };
            if let Err(e) = self.fabric.set_port_membership(&m).await {
                tracing::warn!(%port, error = %e, "fabric-manager: quarantine failed");
            }
        }

        // 2. Placed hosts whose port is not yet in their tenant VRF.
        for (port, d) in &desired {
            let Some(vrf) = d.vrf.as_deref() else {
                continue;
            };
            if !ready_names.contains(vrf) {
                continue;
            }
            let cur = current.get(port).cloned().flatten();
            if cur.as_deref() == Some(vrf) {
                continue;
            }
            let m = PortMembership {
                port: port.clone(),
                vrf: Some(vrf.to_string()),
                previous_vrf: cur.filter(|c| Some(c.as_str()) != quarantine),
                witnesses: d.witnesses.clone(),
                contract: d.contract.clone(),
            };
            match self.fabric.set_port_membership(&m).await {
                Ok(enforced) => {
                    tracing::info!(%port, %vrf, ?enforced, "fabric-manager: port bound to tenant VRF");
                    if let Some(id) = &d.machine_id {
                        self.clear_witness_alert(id).await;
                    }
                }
                Err(FabricError::WitnessMismatch { detail, .. }) => {
                    tracing::warn!(%port, %vrf, %detail, "fabric-manager: attach refused by witnesses");
                    if let Some(id) = &d.machine_id {
                        self.raise_witness_alert(id, port, &detail).await;
                    }
                }
                Err(e) => tracing::warn!(%port, %vrf, error = %e, "fabric-manager: attach failed"),
            }
        }

        // 3. Unplaced hosts whose port the fabric does not list yet: into
        // quarantine, where the adapter has one to offer.
        if caps.quarantine_vrf {
            for (port, d) in &desired {
                if d.vrf.is_some() || current.contains_key(port) {
                    continue;
                }
                let m = PortMembership {
                    port: port.clone(),
                    ..PortMembership::default()
                };
                match self.fabric.set_port_membership(&m).await {
                    Ok(_) => tracing::info!(%port, "fabric-manager: port placed in quarantine"),
                    Err(e) => {
                        tracing::warn!(%port, error = %e, "fabric-manager: quarantine failed")
                    }
                }
            }
        }
        Ok(())
    }

    async fn raise_witness_alert(&self, machine_id: &MachineId, port: &str, detail: &str) {
        let now = chrono::Utc::now();
        let Ok(id) = WITNESS_ALERT_ID.parse() else {
            return;
        };
        let report = HealthReport {
            source: WITNESS_HEALTH_SOURCE.to_string(),
            triggered_by: None,
            observed_at: Some(now),
            successes: vec![],
            alerts: vec![HealthProbeAlert {
                id,
                target: Some(port.to_string()),
                in_alert_since: Some(now),
                message: format!("fabric refused to attach port {port}: {detail}"),
                tenant_message: None,
                classifications: vec![],
            }],
        };
        if let Err(e) = self.write_health_report(machine_id, &report).await {
            tracing::warn!(machine = %machine_id, error = %e, "fabric-manager: raising witness alert failed");
        }
    }

    async fn clear_witness_alert(&self, machine_id: &MachineId) {
        if let Err(e) = self.remove_health_report(machine_id).await {
            tracing::debug!(machine = %machine_id, error = %e, "fabric-manager: clearing witness alert");
        }
    }

    async fn write_health_report(
        &self,
        machine_id: &MachineId,
        report: &HealthReport,
    ) -> eyre::Result<()> {
        let mut conn = self.db.acquire().await?;
        db::machine::insert_health_report(
            &mut conn,
            machine_id,
            HealthReportApplyMode::Merge,
            report,
            false,
        )
        .await?;
        Ok(())
    }

    async fn remove_health_report(&self, machine_id: &MachineId) -> eyre::Result<()> {
        let mut conn = self.db.acquire().await?;
        db::machine::remove_health_report(
            &mut conn,
            machine_id,
            HealthReportApplyMode::Merge,
            WITNESS_HEALTH_SOURCE,
        )
        .await?;
        Ok(())
    }

    /// Tear down fabric VRFs whose NICo VPC no longer exists (or is no longer
    /// ToR-VRF). This is the delete half of the level-triggered loop: whatever
    /// dropped out of NICo's intent -- however it dropped out -- is removed from
    /// the fabric on the next pass. Failures are logged, never fatal, so one bad
    /// object can't stall the rest.
    async fn gc_orphaned_vrfs(&self, live: &[Vpc]) {
        let live_ids: std::collections::HashSet<String> =
            live.iter().map(|v| v.id.to_string()).collect();
        let existing = match self.fabric.list_vrfs().await {
            Ok(e) => e,
            Err(e) => {
                tracing::warn!(error = %e, "fabric-manager: list_vrfs for GC failed");
                return;
            }
        };
        for (fabric_name, nico_id) in existing {
            if live_ids.contains(&nico_id) {
                continue;
            }
            tracing::info!(vpc = %fabric_name, nico_id = %nico_id,
                "fabric-manager: GC tearing down orphaned VRF");
            if let Err(e) = self.fabric.delete_vrf(&fabric_name).await {
                tracing::warn!(vpc = %fabric_name, error = %e, "fabric-manager: GC delete_vrf failed");
            }
        }
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
    /// One short-lived pooled connection for one status row. Kept as its own
    /// function so the connection is never alive across a fabric call.
    async fn persist_fabric_status(
        &self,
        vpc_id: carbide_uuid::vpc::VpcId,
        status: &model::vpc::FabricVrfStatus,
    ) -> eyre::Result<()> {
        let mut conn = self.db.acquire().await?;
        db::vpc::set_fabric_status(vpc_id, status, &mut conn).await?;
        Ok(())
    }

    /// Returns `Ok(true)` when the VRF was ensured (segment ready), `Ok(false)`
    /// when the VPC is not provisioned far enough yet.
    async fn reconcile_vpc(&self, vpc: &Vpc) -> eyre::Result<bool> {
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
            return Ok(false);
        };
        let Some(vlan_id) = seg.status.vlan_id else {
            tracing::info!(vpc = %vpc.id, "fabric-manager: segment VLAN not allocated yet; skipping");
            return Ok(false);
        };
        // A HostInband segment normally has one prefix; take the first that carries
        // a gateway (subnet + gateway must go to the fabric together).
        let Some(prefix) = seg.prefixes.iter().find(|p| p.gateway.is_some()) else {
            tracing::info!(vpc = %vpc.id, "fabric-manager: segment prefix/gateway not ready; skipping");
            return Ok(false);
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
            // The allocated VNI lives in status; config.vni is only the tenant's
            // explicit request, if any.
            vni: vpc.status.vni.or(vpc.config.vni).map(|v| v as u32),
            dhcp_range: None,
        };
        self.fabric.ensure_vrf(&intent).await?;
        // Read back what the fabric actually programmed (drift visibility). This is
        // informational -- a status read failing must not fail the reconcile.
        match self.fabric.get_vrf_status(&vpc.metadata.name).await {
            Ok(observed) => {
                let fabric_status = model::vpc::FabricVrfStatus {
                    programmed: observed.is_some(),
                    observed_at: chrono::Utc::now(),
                    detail: observed,
                };
                // Persist what the fabric reports so NICo's VpcStatus reflects the
                // programmed state (drift visibility for operators/API). Non-fatal:
                // a status write failing must not fail the reconcile.
                if let Err(e) = self.persist_fabric_status(vpc.id, &fabric_status).await {
                    tracing::warn!(vpc = %vpc.id, error = %e,
                        "fabric-manager: persisting fabric status failed");
                }
            }
            Err(e) => {
                tracing::warn!(vpc = %vpc.id, error = %e, "fabric-manager: get_vrf_status failed")
            }
        }

        // (b) Port memberships are reconciled across all VPCs in `reconcile_ports`.

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
        Ok(true)
    }
}

/// Where NICo wants one port and what the fabric may check and enforce there.
#[derive(Debug, Clone)]
struct DesiredPort {
    /// Tenant VPC name, or `None` for quarantine.
    vrf: Option<String>,
    /// The machine on the port, when NICo knows it (alerts land here).
    machine_id: Option<MachineId>,
    witnesses: Witnesses,
    contract: PortContract,
}
