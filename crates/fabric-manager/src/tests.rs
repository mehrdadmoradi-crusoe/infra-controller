//! DB-integration tests for the ToR-VRF reconcile.
//!
//! Uses the workspace `#[carbide_macros::sqlx_test]` harness (clones a migrated
//! template DB per test) to seed real rows, then drives the reconcile against a
//! `MockFabricOperations` and asserts the fabric calls. Covers all three verbs:
//! `ensure_vrf` (intent derived from the VPC's HostInband segment), `peer_vpcs`
//! (restricted to fabric-managed peers), `attach_host` (from the operator-declared
//! connection label), plus the level-triggered skip when the segment isn't ready.

use std::collections::HashMap;
use std::sync::Arc;

use carbide_fabric::{FabricError, HostAttachment, MockFabricOperations, VrfIntent};
use carbide_network::virtualization::VpcVirtualizationType;
use carbide_uuid::instance::InstanceId;
use carbide_uuid::machine::{MachineId, MachineIdSource, MachineType};
use carbide_uuid::vpc::VpcId;
use config_version::ConfigVersion;
use model::machine::ManagedHostState;
use model::metadata::Metadata;
use model::network_segment::NetworkSegment;

use super::{CONNECTION_LABEL, FabricManager, FabricManagerConfig};

/// Seed a ToR-VRF VPC and return its id.
async fn seed_tor_vpc(
    conn: &mut sqlx::PgConnection,
    name: &str,
    vni: Option<i32>,
) -> eyre::Result<VpcId> {
    let vpc_id: VpcId = uuid::Uuid::new_v4().into();
    db::vpc::persist(
        model::vpc::NewVpc {
            id: vpc_id,
            tenant_organization_id: "tenant".to_string(),
            network_virtualization_type: VpcVirtualizationType::TorVrf,
            metadata: Metadata { name: name.to_string(), ..Default::default() },
            network_security_group_id: None,
            routing_profile_type: None,
            vni,
        },
        model::vpc::VpcStatus { vni: None, fabric: None },
        conn,
    )
    .await?;
    Ok(vpc_id)
}

/// Seed the VPC's HostInband segment carrying an allocated VLAN + a prefix with a
/// gateway (what `ensure_vrf` derives its intent from). Returns the segment so
/// callers can reuse its id (e.g. as an instance address's `segment_id`).
async fn seed_hostinband_segment(
    conn: &mut sqlx::PgConnection,
    vpc_id: VpcId,
    vlan: i16,
    prefix: &str,
    gateway: &str,
) -> eyre::Result<NetworkSegment> {
    let seg = db::network_segment::persist(
        model::network_segment::NewNetworkSegment {
            id: uuid::Uuid::new_v4().into(),
            name: "hostinband".to_string(),
            subdomain_id: None,
            vpc_id: Some(vpc_id),
            mtu: 1500,
            prefixes: vec![model::network_prefix::NewNetworkPrefix {
                prefix: prefix.parse().unwrap(),
                gateway: Some(gateway.parse().unwrap()),
                dhcpv6_link_address: None,
                num_reserved: 1,
            }],
            vlan_id: Some(vlan),
            vni: None,
            segment_type: model::network_segment::NetworkSegmentType::HostInband,
            can_stretch: None,
            allocation_strategy: Default::default(),
        },
        conn,
        model::network_segment::NetworkSegmentControllerState::Ready,
    )
    .await?;
    Ok(seg)
}

/// Seed a host machine carrying the operator-declared fabric connection label.
async fn seed_host_with_connection(
    conn: &mut sqlx::PgConnection,
    connection: &str,
) -> eyre::Result<MachineId> {
    let mut hw = [0u8; 32];
    hw[..16].copy_from_slice(uuid::Uuid::new_v4().as_bytes());
    let machine_id = MachineId::new(MachineIdSource::Tpm, hw, MachineType::Host);
    // create() stamps a fresh ConfigVersion (V1-T<micros>); update_metadata is an
    // optimistic write, so pass back the exact version it stored, not a new initial().
    let machine =
        db::machine::create(conn, None, &machine_id, ManagedHostState::Created, None, 2).await?;
    let labels = HashMap::from([(CONNECTION_LABEL.to_string(), connection.to_string())]);
    db::machine::update_metadata(
        conn,
        &machine_id,
        machine.version,
        Metadata { labels, ..Default::default() },
    )
    .await?;
    Ok(machine_id)
}

#[carbide_macros::sqlx_test]
async fn reconciles_vrf_and_peering(pool: sqlx::PgPool) -> eyre::Result<()> {
    // A ToR-VRF VPC with its HostInband segment, peered to a second ToR-VRF VPC.
    let mut txn = pool.begin().await?;
    let vpc_a = seed_tor_vpc(&mut txn, "tor-a", Some(10001)).await?;
    seed_hostinband_segment(&mut txn, vpc_a, 100, "10.10.0.0/24", "10.10.0.1").await?;
    let vpc_b = seed_tor_vpc(&mut txn, "tor-b", Some(10002)).await?;
    db::vpc_peering::create(&mut txn, vpc_a, vpc_b, uuid::Uuid::new_v4().into()).await?;
    txn.commit().await?;

    let mut fabric = MockFabricOperations::new();
    // ensure_vrf fires once, for tor-a, with intent derived from its segment.
    fabric
        .expect_ensure_vrf()
        .withf(|i: &VrfIntent| {
            i.name == "tor-a"
                && i.vlan == 100
                && i.subnet_cidr == "10.10.0.0/24"
                && i.gateway == "10.10.0.1"
                && i.vni == Some(10001)
        })
        .times(1)
        .returning(|_| Ok(()));
    // peering fires once, between the two ToR-VRF VPCs (order-agnostic).
    fabric
        .expect_peer_vpcs()
        .withf(|a: &str, b: &str| {
            (a == "tor-a" && b == "tor-b") || (a == "tor-b" && b == "tor-a")
        })
        .times(1)
        .returning(|_, _| Ok(()));
    // no instances placed -> no host attachment.
    fabric.expect_attach_host().times(0).returning(|_| Ok(()));
    // nothing bound on the fabric either -> nothing to detach.
    fabric.expect_list_attachments().returning(|_| Ok(vec![]));
    fabric.expect_detach_host().times(0).returning(|_| Ok(()));
    // The fabric reports a status object -> the reconcile must persist it on the
    // VPC as `programmed` (asserted below).
    fabric
        .expect_get_vrf_status()
        .returning(|_| Ok(Some(serde_json::json!({"ready": true}))));

    let pool2 = pool.clone();
    let mgr = FabricManager::new(Arc::new(fabric), pool, FabricManagerConfig::default());
    // Explicit loop so a reconcile error surfaces (run_single_iteration swallows them).
    let vpcs = mgr.list_tor_vrf_vpcs().await?;
    assert_eq!(vpcs.len(), 2, "both ToR-VRF VPCs should be listed");
    for v in &vpcs {
        mgr.reconcile_vpc(v)
            .await
            .map_err(|e| eyre::eyre!("reconcile {} failed: {e}", v.metadata.name))?;
    }
    // Status persistence: tor-a reached ensure_vrf + get_vrf_status, so its
    // VpcStatus now carries what the fabric reported (tor-b skipped before that).
    let a = db::vpc::find_by(&pool2, db::ObjectColumnFilter::One(db::vpc::IdColumn, &vpc_a))
        .await?
        .pop()
        .expect("tor-a still present");
    let fs = a.status.fabric.expect("fabric status persisted after reconcile");
    assert!(fs.programmed, "fabric reported a status object => programmed");
    assert!(fs.detail.is_some(), "raw controller status retained for diagnosis");
    Ok(())
}

#[carbide_macros::sqlx_test]
async fn attaches_host_from_connection_label(pool: sqlx::PgPool) -> eyre::Result<()> {
    // A ToR-VRF VPC with a segment and one placed host cabled to a leaf port.
    let mut txn = pool.begin().await?;
    let vpc = seed_tor_vpc(&mut txn, "tor-a", Some(10001)).await?;
    let seg = seed_hostinband_segment(&mut txn, vpc, 100, "10.10.0.0/24", "10.10.0.1").await?;
    let machine_id = seed_host_with_connection(&mut txn, "leaf01/Ethernet1").await?;

    // An instance on that host, addressed in the VPC (ties instance -> vpc via
    // instance_addresses, which is what find_ids joins on).
    // Seed the instance via batch_persist so every config column is a valid,
    // decodable JSON (a raw minimal insert leaves defaults that fail find_by_id's
    // InstanceSnapshot decode). InstanceConfig: only tenant + os need building.
    let instance_id: InstanceId = uuid::Uuid::new_v4().into();
    let os = model::os::OperatingSystem {
        user_data: None,
        variant: model::os::OperatingSystemVariant::OsImage(uuid::Uuid::new_v4()),
        phone_home_enabled: false,
        run_provisioning_instructions_on_every_boot: false,
    };
    let cfg = model::instance::config::InstanceConfig {
        tenant: model::instance::config::tenant_config::TenantConfig {
            tenant_organization_id: "tenant".parse().unwrap(),
            tenant_keyset_ids: vec![],
            hostname: None,
        },
        os,
        network: Default::default(),
        infiniband: Default::default(),
        network_security_group_id: None,
        extension_services: Default::default(),
        nvlink: Default::default(),
        spxconfig: Default::default(),
    };
    db::instance::batch_persist(
        vec![model::instance::NewInstance {
            instance_id,
            machine_id,
            instance_type_id: None,
            config: &cfg,
            metadata: Default::default(),
            config_version: ConfigVersion::initial(),
            network_config_version: ConfigVersion::initial(),
            ib_config_version: ConfigVersion::initial(),
            extension_services_config_version: ConfigVersion::initial(),
            nvlink_config_version: ConfigVersion::initial(),
            spx_config_version: ConfigVersion::initial(),
        }],
        &mut *txn,
    )
    .await?;
    sqlx::query(
        "INSERT INTO instance_addresses (instance_id, address, segment_id, prefix, vpc_id) \
         VALUES ($1, $2::inet, $3, $4::cidr, $5)",
    )
    .bind(instance_id)
    .bind("10.10.0.10")
    .bind(seg.id)
    .bind("10.10.0.0/24")
    .bind(vpc)
    .execute(&mut *txn)
    .await?;
    txn.commit().await?;

    let mut fabric = MockFabricOperations::new();
    fabric.expect_ensure_vrf().returning(|_| Ok(()));
    fabric.expect_get_vrf_status().returning(|_| Ok(None));
    // The host is attached to the fabric Connection named by its label.
    fabric
        .expect_attach_host()
        .withf(|a: &HostAttachment| a.vpc_name == "tor-a" && a.connection == "leaf01/Ethernet1")
        .times(1)
        .returning(|_| Ok(()));
    // The fabric already binds exactly that port -> nothing to detach.
    fabric
        .expect_list_attachments()
        .returning(|_| Ok(vec!["leaf01/Ethernet1".to_string()]));
    fabric.expect_detach_host().times(0).returning(|_| Ok(()));

    let mgr = FabricManager::new(Arc::new(fabric), pool, FabricManagerConfig::default());
    let vpcs = mgr.list_tor_vrf_vpcs().await?;
    for v in &vpcs {
        mgr.reconcile_vpc(v)
            .await
            .map_err(|e| eyre::eyre!("reconcile {} failed: {e}", v.metadata.name))?;
    }
    Ok(())
}

#[carbide_macros::sqlx_test]
async fn skips_vpc_without_ready_segment(pool: sqlx::PgPool) -> eyre::Result<()> {
    // A ToR-VRF VPC with no HostInband segment yet: nothing to program.
    let mut txn = pool.begin().await?;
    seed_tor_vpc(&mut txn, "tor-c", Some(1)).await?;
    txn.commit().await?;

    let mut fabric = MockFabricOperations::new();
    // Must not push an incomplete VRF.
    fabric.expect_ensure_vrf().times(0).returning(|_| Ok(()));
    // run_single_iteration also runs GC; nothing on the fabric to collect.
    fabric.expect_list_vrfs().returning(|| Ok(vec![]));

    let mgr = FabricManager::new(Arc::new(fabric), pool, FabricManagerConfig::default());
    // The VPC is still counted as reconciled (skipped cleanly).
    assert_eq!(mgr.run_single_iteration().await?, 1);
    Ok(())
}

#[carbide_macros::sqlx_test]
async fn gc_tears_down_orphaned_vrf(pool: sqlx::PgPool) -> eyre::Result<()> {
    // One live ToR-VRF VPC (no segment -> skipped this pass, but still live intent).
    let mut txn = pool.begin().await?;
    let live = seed_tor_vpc(&mut txn, "tor-live", Some(1)).await?;
    txn.commit().await?;
    let live_id = live.to_string();

    let mut fabric = MockFabricOperations::new();
    fabric.expect_ensure_vrf().times(0).returning(|_| Ok(()));
    // The fabric reports two VRFs: one still backed by NICo intent, one whose
    // NICo VPC is gone. Only the orphan must be torn down.
    fabric.expect_list_vrfs().returning(move || {
        Ok(vec![
            ("tor-live".to_string(), live_id.clone()),
            ("torvold".to_string(), "dead-nico-id".to_string()),
        ])
    });
    fabric
        .expect_delete_vrf()
        .withf(|name: &str| name == "torvold")
        .times(1)
        .returning(|_| Ok(()));

    let mgr = FabricManager::new(Arc::new(fabric), pool, FabricManagerConfig::default());
    assert_eq!(mgr.run_single_iteration().await?, 1);
    Ok(())
}

#[carbide_macros::sqlx_test]
async fn fabric_errors_are_isolated_and_not_fatal(pool: sqlx::PgPool) -> eyre::Result<()> {
    // Two live ToR-VRF VPCs, both with ready segments, so both reach the fabric.
    let mut txn = pool.begin().await?;
    let ok = seed_tor_vpc(&mut txn, "tor-ok", Some(1)).await?;
    seed_hostinband_segment(&mut txn, ok, 101, "10.11.0.0/24", "10.11.0.1").await?;
    let bad = seed_tor_vpc(&mut txn, "tor-err", Some(2)).await?;
    seed_hostinband_segment(&mut txn, bad, 102, "10.12.0.0/24", "10.12.0.1").await?;
    txn.commit().await?;

    let unreachable = || FabricError::Invalid("fabric unreachable".to_string());
    let mut fabric = MockFabricOperations::new();
    // The fabric fails for one tenant only; the other must still converge.
    fabric
        .expect_ensure_vrf()
        .withf(|i: &VrfIntent| i.name == "tor-err")
        .returning(move |_| Err(unreachable()));
    fabric
        .expect_ensure_vrf()
        .withf(|i: &VrfIntent| i.name == "tor-ok")
        .returning(|_| Ok(()));
    fabric.expect_get_vrf_status().returning(|_| Ok(None));
    // GC can't list the fabric either: it must degrade to a no-op, not fail the pass.
    fabric.expect_list_vrfs().returning(move || Err(unreachable()));
    fabric.expect_attach_host().times(0).returning(|_| Ok(()));
    // nothing bound on the fabric either -> nothing to detach.
    fabric.expect_list_attachments().returning(|_| Ok(vec![]));
    fabric.expect_detach_host().times(0).returning(|_| Ok(()));
    fabric.expect_delete_vrf().times(0).returning(|_| Ok(()));

    let mgr = FabricManager::new(Arc::new(fabric), pool, FabricManagerConfig::default());
    // No panic, no Err out of the pass: the healthy tenant counts, the failed one
    // is logged and skipped, and GC is skipped when the fabric can't be listed.
    assert_eq!(mgr.run_single_iteration().await?, 1);
    Ok(())
}

#[carbide_macros::sqlx_test]
async fn detaches_port_of_a_released_instance(pool: sqlx::PgPool) -> eyre::Result<()> {
    // A released instance is a soft-deleted row (deleted IS NOT NULL) that
    // lingers while the host is cleaned up. The tenant no longer owns the host,
    // so the reconcile must treat the port as unplaced: no attach, one detach.
    let mut txn = pool.begin().await?;
    let vpc = seed_tor_vpc(&mut txn, "tor-a", Some(10001)).await?;
    let seg = seed_hostinband_segment(&mut txn, vpc, 100, "10.10.0.0/24", "10.10.0.1").await?;
    let machine_id = seed_host_with_connection(&mut txn, "leaf01/Ethernet1").await?;
    let instance_id: InstanceId = uuid::Uuid::new_v4().into();
    let cfg = model::instance::config::InstanceConfig {
        tenant: model::instance::config::tenant_config::TenantConfig {
            tenant_organization_id: "tenant".parse().unwrap(),
            tenant_keyset_ids: vec![],
            hostname: None,
        },
        os: model::os::OperatingSystem {
            user_data: None,
            variant: model::os::OperatingSystemVariant::OsImage(uuid::Uuid::new_v4()),
            phone_home_enabled: false,
            run_provisioning_instructions_on_every_boot: false,
        },
        network: Default::default(),
        infiniband: Default::default(),
        network_security_group_id: None,
        extension_services: Default::default(),
        nvlink: Default::default(),
        spxconfig: Default::default(),
    };
    db::instance::batch_persist(
        vec![model::instance::NewInstance {
            instance_id,
            machine_id,
            instance_type_id: None,
            config: &cfg,
            metadata: Default::default(),
            config_version: ConfigVersion::initial(),
            network_config_version: ConfigVersion::initial(),
            ib_config_version: ConfigVersion::initial(),
            extension_services_config_version: ConfigVersion::initial(),
            nvlink_config_version: ConfigVersion::initial(),
            spx_config_version: ConfigVersion::initial(),
        }],
        &mut *txn,
    )
    .await?;
    sqlx::query(
        "INSERT INTO instance_addresses (instance_id, address, segment_id, prefix, vpc_id) \
         VALUES ($1, $2::inet, $3, $4::cidr, $5)",
    )
    .bind(instance_id)
    .bind("10.10.0.10")
    .bind(seg.id)
    .bind("10.10.0.0/24")
    .bind(vpc)
    .execute(&mut *txn)
    .await?;
    sqlx::query("UPDATE instances SET deleted = now() WHERE id = $1")
        .bind(instance_id)
        .execute(&mut *txn)
        .await?;
    txn.commit().await?;

    let mut fabric = MockFabricOperations::new();
    fabric.expect_ensure_vrf().returning(|_| Ok(()));
    fabric.expect_get_vrf_status().returning(|_| Ok(None));
    fabric.expect_attach_host().times(0).returning(|_| Ok(()));
    fabric
        .expect_list_attachments()
        .returning(|_| Ok(vec!["leaf01/Ethernet1".to_string()]));
    fabric
        .expect_detach_host()
        .withf(|a: &HostAttachment| a.vpc_name == "tor-a" && a.connection == "leaf01/Ethernet1")
        .times(1)
        .returning(|_| Ok(()));

    let mgr = FabricManager::new(Arc::new(fabric), pool, FabricManagerConfig::default());
    for v in &mgr.list_tor_vrf_vpcs().await? {
        mgr.reconcile_vpc(v)
            .await
            .map_err(|e| eyre::eyre!("reconcile {} failed: {e}", v.metadata.name))?;
    }
    Ok(())
}

#[carbide_macros::sqlx_test]
async fn detaches_port_whose_instance_is_gone(pool: sqlx::PgPool) -> eyre::Result<()> {
    // A ToR-VRF VPC with a ready segment but no placed instance, while the fabric
    // still binds a port into its VRF (the instance was released): the reconcile
    // must detach that port and nothing else.
    let mut txn = pool.begin().await?;
    let vpc = seed_tor_vpc(&mut txn, "tor-a", Some(10001)).await?;
    seed_hostinband_segment(&mut txn, vpc, 100, "10.10.0.0/24", "10.10.0.1").await?;
    txn.commit().await?;

    let mut fabric = MockFabricOperations::new();
    fabric.expect_ensure_vrf().returning(|_| Ok(()));
    fabric.expect_get_vrf_status().returning(|_| Ok(None));
    fabric.expect_attach_host().times(0).returning(|_| Ok(()));
    fabric
        .expect_list_attachments()
        .withf(|v: &str| v == "tor-a")
        .returning(|_| Ok(vec!["leaf01/Ethernet1".to_string()]));
    fabric
        .expect_detach_host()
        .withf(|a: &HostAttachment| a.vpc_name == "tor-a" && a.connection == "leaf01/Ethernet1")
        .times(1)
        .returning(|_| Ok(()));

    let mgr = FabricManager::new(Arc::new(fabric), pool, FabricManagerConfig::default());
    for v in &mgr.list_tor_vrf_vpcs().await? {
        mgr.reconcile_vpc(v)
            .await
            .map_err(|e| eyre::eyre!("reconcile {} failed: {e}", v.metadata.name))?;
    }
    Ok(())
}
