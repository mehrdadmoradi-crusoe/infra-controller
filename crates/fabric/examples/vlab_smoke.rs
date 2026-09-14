//! Smoke test: drive a live Hedgehog vlab with the *compiled* carbide-fabric
//! backend (not the Python adapter). Uses the ambient kubeconfig (KUBECONFIG),
//! which should point at the vlab's k3s control node (via an SSH tunnel).
//!
//! Programs a ToR VRF on a real SONiC leaf: ensure_vrf (Hedgehog VPC, l3vni) +
//! attach_host (VPCAttachment onto server-03's unbundled connection on leaf-01).
//! After it runs, `hhfab vlab ssh -n leaf-01 -- show vrf` should list VrfV<name>.
//!
//!   KUBECONFIG=~/kubeconfig-vlab ./vlab_smoke
//!
//! vlab constraints baked in: name <= 11 chars, subnet inside the fabric's
//! IPv4Namespace (10.0.0.0/16) + VLANNamespace (1000-2999), l3vni attaches to a
//! non-ESLAG (unbundled/bundled) connection.

use carbide_fabric::{FabricConfig, FabricOperations, HedgehogFabric, HostAttachment, VrfIntent};

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let cfg = FabricConfig {
        enabled: true,
        namespace: "default".to_string(),
        ..Default::default()
    };
    let fabric = HedgehogFabric::try_default(&cfg).await?;

    let intent = VrfIntent {
        nico_vpc_id: "nico-smoke-1".to_string(),
        name: "nicosmoke".to_string(), // <= 11 chars -> Hedgehog VPC "nicosmoke"
        subnet_cidr: "10.0.42.0/24".to_string(),
        vlan: 1042,
        gateway: "10.0.42.1".to_string(),
        vni: Some(104242),
        dhcp_range: Some(("10.0.42.10".to_string(), "10.0.42.250".to_string())),
    };

    println!(
        "[1/3] ensure_vrf({}) -> Hedgehog VPC l3vni ...",
        intent.name
    );
    fabric.ensure_vrf(&intent).await?;
    println!("      ok");

    println!("[2/3] attach_host(server-03--unbundled--leaf-01) -> VPCAttachment ...");
    fabric
        .attach_host(&HostAttachment {
            vpc_name: intent.name.clone(),
            connection: "server-03--unbundled--leaf-01".to_string(),
        })
        .await?;
    println!("      ok");

    println!("[3/3] get_vrf_status({}) ...", intent.name);
    match fabric.get_vrf_status(&intent.name).await? {
        Some(status) => println!("      status: {status}"),
        None => println!("      (no status yet; fabric still reconciling)"),
    }

    println!("\nDONE. Verify on the leaf:");
    println!("  sudo hhfab vlab ssh -n leaf-01 -- show vrf | grep -i nicosmoke");
    Ok(())
}
