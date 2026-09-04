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

use carbide_fabric::{HostAttachment, MockFabricOperations, VrfIntent};
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
        model::vpc::VpcStatus { vni: None },
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

    let mgr = FabricManager::new(Arc::new(fabric), pool, FabricManagerConfig::default());
    // Explicit loop so a reconcile error surfaces (run_single_iteration swallows them).
    let vpcs = mgr.list_tor_vrf_vpcs().await?;
    assert_eq!(vpcs.len(), 2, "both ToR-VRF VPCs should be listed");
    for v in &vpcs {
        mgr.reconcile_vpc(v)
            .await
            .map_err(|e| eyre::eyre!("reconcile {} failed: {e}", v.metadata.name))?;
    }
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
    // The host is attached to the fabric Connection named by its label.
    fabric
        .expect_attach_host()
        .withf(|a: &HostAttachment| a.vpc_name == "tor-a" && a.connection == "leaf01/Ethernet1")
        .times(1)
        .returning(|_| Ok(()));

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

    let mgr = FabricManager::new(Arc::new(fabric), pool, FabricManagerConfig::default());
    // The VPC is still counted as reconciled (skipped cleanly).
    assert_eq!(mgr.run_single_iteration().await?, 1);
    Ok(())
}
