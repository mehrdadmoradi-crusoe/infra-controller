//! Multi-tenant vlab demo, driven from NICo DB state through the real reconcile.
//!
//! Seeds two TorVrf VPCs (each with a HostInband segment + a host on a real vlab
//! connection), optionally a peering between them (env PEER=1), then runs the
//! FabricManager reconcile -> programs both VRFs + attachments (+ peering).
//!
//!   # phase 1 (isolation): two VPCs, no peering
//!   DATABASE_URL=.../nico KUBECONFIG=~/kubeconfig-vlab ./vlab_demo
//!   # phase 2 (controlled peering): NICo programs east-west
//!   PEER=1 DATABASE_URL=.../nico KUBECONFIG=~/kubeconfig-vlab ./vlab_demo

use std::collections::HashMap;
use std::sync::Arc;

use carbide_fabric::{FabricConfig, FabricOperations, HedgehogFabric};
use carbide_fabric_manager::{FabricManager, FabricManagerConfig};
use carbide_network::virtualization::VpcVirtualizationType;
use carbide_uuid::instance::InstanceId;
use carbide_uuid::machine::{MachineId, MachineIdSource, MachineType};
use carbide_uuid::vpc::VpcId;
use config_version::ConfigVersion;
use model::machine::ManagedHostState;
use model::metadata::Metadata;

const CONNECTION_LABEL: &str = "fabric.nico.io/connection";

#[allow(clippy::too_many_arguments)]
async fn seed_vpc_with_host(
    txn: &mut sqlx::PgConnection,
    name: &str,
    vni: i32,
    subnet: &str,
    vlan: i16,
    gateway: &str,
    host_ip: &str,
    connection: &str,
) -> Result<VpcId, Box<dyn std::error::Error>> {
    let vpc: VpcId = uuid::Uuid::new_v4().into();
    db::vpc::persist(
        model::vpc::NewVpc {
            id: vpc,
            tenant_organization_id: "tenant".to_string(),
            network_virtualization_type: VpcVirtualizationType::TorVrf,
            metadata: Metadata {
                name: name.to_string(),
                ..Default::default()
            },
            network_security_group_id: None,
            routing_profile_type: None,
            vni: Some(vni),
        },
        model::vpc::VpcStatus {
            vni: None,
            fabric: None,
        },
        txn,
    )
    .await?;
    let seg = db::network_segment::persist(
        model::network_segment::NewNetworkSegment {
            id: uuid::Uuid::new_v4().into(),
            name: format!("{name}-hostinband"),
            subdomain_id: None,
            vpc_id: Some(vpc),
            mtu: 1500,
            prefixes: vec![model::network_prefix::NewNetworkPrefix {
                prefix: subnet.parse().unwrap(),
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
        txn,
        model::network_segment::NetworkSegmentControllerState::Ready,
    )
    .await?;

    let mut hw = [0u8; 32];
    hw[..16].copy_from_slice(uuid::Uuid::new_v4().as_bytes());
    let machine_id = MachineId::new(MachineIdSource::Tpm, hw, MachineType::Host);
    let machine =
        db::machine::create(txn, None, &machine_id, ManagedHostState::Created, None, 2).await?;
    let labels = HashMap::from([(CONNECTION_LABEL.to_string(), connection.to_string())]);
    db::machine::update_metadata(
        txn,
        &machine_id,
        machine.version,
        Metadata {
            labels,
            ..Default::default()
        },
    )
    .await?;

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
        txn,
    )
    .await?;
    sqlx::query(
        "INSERT INTO instance_addresses (instance_id, address, segment_id, prefix, vpc_id) \
         VALUES ($1, $2::inet, $3, $4::cidr, $5)",
    )
    .bind(instance_id)
    .bind(host_ip)
    .bind(seg.id)
    .bind(subnet)
    .bind(vpc)
    .execute(&mut *txn)
    .await?;
    Ok(vpc)
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let db_url = std::env::var("DATABASE_URL")?;
    let peer = std::env::var("PEER").is_ok();
    let pool = sqlx::PgPool::connect(&db_url).await?;
    println!("[1/3] migrate + seed two TorVrf VPCs (peer={peer}) ...");
    db::migrations::migrate(&pool).await?;
    let mut txn = pool.begin().await?;
    let a = seed_vpc_with_host(
        &mut txn,
        "torvlab",
        104343,
        "10.0.43.0/24",
        1043,
        "10.0.43.1",
        "10.0.43.10",
        "server-03--unbundled--leaf-01",
    )
    .await?;
    let b = seed_vpc_with_host(
        &mut txn,
        "torvlab2",
        104444,
        "10.0.44.0/24",
        1044,
        "10.0.44.1",
        "10.0.44.10",
        "server-04--bundled--leaf-02",
    )
    .await?;
    if peer {
        db::vpc_peering::create(&mut txn, a, b, uuid::Uuid::new_v4().into()).await?;
        println!("      seeded vpc_peering torvlab <-> torvlab2");
    }
    txn.commit().await?;

    println!("[2/3] FabricManager reconcile (real HedgehogFabric) ...");
    let fcfg = FabricConfig {
        enabled: true,
        namespace: "default".to_string(),
        ..Default::default()
    };
    let fabric: Arc<dyn FabricOperations> = Arc::new(HedgehogFabric::try_default(&fcfg).await?);
    let mgr = FabricManager::new(fabric, pool, FabricManagerConfig::default());
    let n = mgr.run_single_iteration().await?;
    println!(
        "[3/3] reconciled {n} ToR-VRF VPC(s){}.",
        if peer { " + peering" } else { "" }
    );
    Ok(())
}
