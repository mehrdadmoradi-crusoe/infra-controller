//! Full reconcile against a live Hedgehog vlab, driven from NICo DB state.
//!
//! Seeds a TorVrf VPC + its HostInband segment + a host whose
//! `fabric.nico.io/connection` label names a real vlab connection, then runs the
//! *actual* FabricManager reconcile loop (one iteration) with a real
//! HedgehogFabric backend. The manager derives the VrfIntent from the DB and
//! programs the SONiC leaf -- no hand-built intent.
//!
//!   DATABASE_URL=postgres://postgres@localhost:5432/postgres \
//!   KUBECONFIG=~/kubeconfig-vlab ./vlab_reconcile
//!
//! Then: `sudo hhfab vlab ssh -n leaf-01 -- show vrf | grep -i torvlab`

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
// Real vlab connection on leaf-01 (non-ESLAG, works with l3vni).
const LEAF_CONNECTION: &str = "server-03--unbundled--leaf-01";

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let db_url = std::env::var("DATABASE_URL")
        .unwrap_or_else(|_| "postgres://postgres@localhost:5432/postgres".to_string());
    let pool = sqlx::PgPool::connect(&db_url).await?;
    println!("[1/4] migrating NICo schema ...");
    db::migrations::migrate(&pool).await?;

    println!("[2/4] seeding TorVrf VPC + HostInband segment + host ({LEAF_CONNECTION}) ...");
    let mut txn = pool.begin().await?;
    let vpc: VpcId = uuid::Uuid::new_v4().into();
    db::vpc::persist(
        model::vpc::NewVpc {
            id: vpc,
            tenant_organization_id: "tenant".to_string(),
            network_virtualization_type: VpcVirtualizationType::TorVrf,
            metadata: Metadata { name: "torvlab".to_string(), ..Default::default() },
            network_security_group_id: None,
            routing_profile_type: None,
            vni: Some(104343),
        },
        model::vpc::VpcStatus { vni: None, fabric: None },
        &mut txn,
    )
    .await?;
    let seg = db::network_segment::persist(
        model::network_segment::NewNetworkSegment {
            id: uuid::Uuid::new_v4().into(),
            name: "hostinband".to_string(),
            subdomain_id: None,
            vpc_id: Some(vpc),
            mtu: 1500,
            prefixes: vec![model::network_prefix::NewNetworkPrefix {
                prefix: "10.0.43.0/24".parse().unwrap(),
                gateway: Some("10.0.43.1".parse().unwrap()),
                dhcpv6_link_address: None,
                num_reserved: 1,
            }],
            vlan_id: Some(1043),
            vni: None,
            segment_type: model::network_segment::NetworkSegmentType::HostInband,
            can_stretch: None,
            allocation_strategy: Default::default(),
        },
        &mut txn,
        model::network_segment::NetworkSegmentControllerState::Ready,
    )
    .await?;

    let mut hw = [0u8; 32];
    hw[..16].copy_from_slice(uuid::Uuid::new_v4().as_bytes());
    let machine_id = MachineId::new(MachineIdSource::Tpm, hw, MachineType::Host);
    let machine =
        db::machine::create(&mut txn, None, &machine_id, ManagedHostState::Created, None, 2).await?;
    let labels = HashMap::from([(CONNECTION_LABEL.to_string(), LEAF_CONNECTION.to_string())]);
    db::machine::update_metadata(
        &mut txn,
        &machine_id,
        machine.version,
        Metadata { labels, ..Default::default() },
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
        &mut txn,
    )
    .await?;
    sqlx::query(
        "INSERT INTO instance_addresses (instance_id, address, segment_id, prefix, vpc_id) \
         VALUES ($1, $2::inet, $3, $4::cidr, $5)",
    )
    .bind(instance_id)
    .bind("10.0.43.10")
    .bind(seg.id)
    .bind("10.0.43.0/24")
    .bind(vpc)
    .execute(&mut *txn)
    .await?;
    txn.commit().await?;

    println!("[3/4] running FabricManager reconcile (real HedgehogFabric) ...");
    let fcfg = FabricConfig { enabled: true, namespace: "default".to_string(), ..Default::default() };
    let fabric: Arc<dyn FabricOperations> = Arc::new(HedgehogFabric::try_default(&fcfg).await?);
    let mgr = FabricManager::new(fabric, pool, FabricManagerConfig::default());
    let n = mgr.run_single_iteration().await?;

    println!("[4/4] reconciled {n} ToR-VRF VPC(s) from DB state.");
    println!("\nDONE. Verify on the leaf:");
    println!("  sudo hhfab vlab ssh -n leaf-01 -- show vrf | grep -i torvlab");
    Ok(())
}
