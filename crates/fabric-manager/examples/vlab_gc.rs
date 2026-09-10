//! Reconcile-only runner against a live vlab: NO seeding. Reads whatever intent
//! is already in NICo's DB and runs ONE FabricManager pass (ensure + GC).
//!
//! Proves the delete half of the level-triggered loop: soft-delete a TorVrf VPC
//! in the DB (`UPDATE vpcs SET deleted = now() ...`), run this, and the fabric
//! tears its VRF (+ attachments + peerings) down. Prints the fabric's VRF list
//! before and after so the GC effect is visible.
//!
//!   DATABASE_URL=postgres://postgres@localhost:5432/nico \
//!   KUBECONFIG=~/kubeconfig-vlab ./vlab_gc

use std::sync::Arc;

use carbide_fabric::{FabricConfig, FabricOperations, HedgehogFabric};
use carbide_fabric_manager::{FabricManager, FabricManagerConfig};

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let db_url = std::env::var("DATABASE_URL")?;
    let pool = sqlx::PgPool::connect(&db_url).await?;
    db::migrations::migrate(&pool).await?;

    let fcfg = FabricConfig { enabled: true, namespace: "default".to_string(), ..Default::default() };
    let fabric: Arc<dyn FabricOperations> = Arc::new(HedgehogFabric::try_default(&fcfg).await?);
    let mgr = FabricManager::new(fabric.clone(), pool, FabricManagerConfig::default());

    println!("fabric VRFs BEFORE: {:?}", fabric.list_vrfs().await?);
    let n = mgr.run_single_iteration().await?;
    println!("reconciled {n} live ToR-VRF VPC(s); GC ran.");
    println!("fabric VRFs AFTER:  {:?}", fabric.list_vrfs().await?);
    Ok(())
}
