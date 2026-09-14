/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

//! The fabric agent binary.
//!
//! Serves the `fabric_agent.v1` contract and executes it against one fabric
//! controller through an in-process `FabricOperations` adapter. It decides
//! nothing: NICo restates desired state every pass, so the agent can be
//! restarted at any time. It holds the controller credentials; NICo holds
//! only its client identity.

use std::net::SocketAddr;
use std::sync::Arc;

use carbide_fabric::{
    Capabilities, EdaFabric, FabricBackend, FabricConfig, FabricError, FabricOperations,
    HedgehogFabric, PortContract, PortMembership, Witnesses,
};
use carbide_fabric_agent_api::v1 as pb;
use carbide_fabric_agent_api::{FabricAgent, FabricAgentServer, CONTRACT_VERSION};
use clap::Parser;
use tokio_stream::wrappers::ReceiverStream;
use tonic::transport::{Certificate, Identity, Server, ServerTlsConfig};
use tonic::{Code, Request, Response, Status};

#[derive(Parser, Debug)]
#[command(name = "fabric-agent", about = "NICo fabric agent: renders tenant network intent onto a fabric controller")]
struct Args {
    /// Listen address.
    #[arg(long, env = "FABRIC_AGENT_LISTEN", default_value = "0.0.0.0:7443")]
    listen: SocketAddr,
    /// TOML file with the `[fabric]` section (backend, namespace, eda settings).
    /// Controller credentials come from the environment the adapter documents.
    #[arg(long, env = "FABRIC_AGENT_CONFIG")]
    config: String,
    /// PEM server certificate and key. Both or neither.
    #[arg(long, env = "FABRIC_AGENT_TLS_CERT")]
    tls_cert: Option<String>,
    #[arg(long, env = "FABRIC_AGENT_TLS_KEY")]
    tls_key: Option<String>,
    /// PEM CA bundle; when set, clients must present a certificate it signed (mTLS).
    #[arg(long, env = "FABRIC_AGENT_CLIENT_CA")]
    client_ca: Option<String>,
}

#[derive(serde::Deserialize)]
struct AgentFile {
    fabric: FabricConfig,
}

/// The gRPC surface over one in-process adapter.
struct Service {
    inner: Arc<dyn FabricOperations>,
    adapter: String,
}

fn status_from(e: FabricError) -> Status {
    match e {
        FabricError::Unsupported(m) => Status::new(Code::Unimplemented, m),
        FabricError::WitnessMismatch { port, detail } => {
            Status::new(Code::FailedPrecondition, format!("port {port}: {detail}"))
        }
        FabricError::Invalid(m) => Status::new(Code::InvalidArgument, m),
        other => Status::new(Code::Unavailable, other.to_string()),
    }
}

fn to_pb_capabilities(c: Capabilities) -> pb::Capabilities {
    pb::Capabilities {
        contract_version: CONTRACT_VERSION.to_string(),
        adapter: c.adapter,
        peering: c.peering,
        mac_limit: c.mac_limit,
        ip_source_guard: c.ip_source_guard,
        dhcp_snooping: c.dhcp_snooping,
        storm_control: c.storm_control,
        isolated_ports: c.isolated_ports,
        anycast_gateway: c.anycast_gateway,
        vlan_translation: c.vlan_translation,
        events: c.events,
        lldp_witness: c.lldp_witness,
        mac_witness: c.mac_witness,
        quarantine_vrf: c.quarantine_vrf,
    }
}

#[tonic::async_trait]
impl FabricAgent for Service {
    async fn get_capabilities(
        &self,
        _r: Request<pb::GetCapabilitiesRequest>,
    ) -> Result<Response<pb::Capabilities>, Status> {
        let mut caps = self.inner.capabilities().await.map_err(status_from)?;
        if caps.adapter.is_empty() || caps.adapter == "legacy" {
            caps.adapter = self.adapter.clone();
        }
        Ok(Response::new(to_pb_capabilities(caps)))
    }

    async fn ensure_vrf(
        &self,
        r: Request<pb::EnsureVrfRequest>,
    ) -> Result<Response<pb::EnsureVrfResponse>, Status> {
        let r = r.into_inner();
        let v = r
            .vrf
            .ok_or_else(|| Status::invalid_argument("vrf is required"))?;
        let vlan = u16::try_from(v.vlan)
            .map_err(|_| Status::invalid_argument(format!("vlan {} out of range", v.vlan)))?;
        tracing::info!(request_id = %r.request_id, vrf = %v.name, "ensure_vrf");
        self.inner
            .ensure_vrf(&carbide_fabric::VrfIntent {
                nico_vpc_id: v.nico_vpc_id,
                name: v.name,
                subnet_cidr: v.subnet_cidr,
                vlan,
                gateway: v.gateway,
                vni: v.vni,
                dhcp_range: None,
            })
            .await
            .map_err(status_from)?;
        Ok(Response::new(pb::EnsureVrfResponse {}))
    }

    async fn delete_vrf(
        &self,
        r: Request<pb::DeleteVrfRequest>,
    ) -> Result<Response<pb::DeleteVrfResponse>, Status> {
        let r = r.into_inner();
        tracing::info!(request_id = %r.request_id, vrf = %r.name, "delete_vrf");
        self.inner.delete_vrf(&r.name).await.map_err(status_from)?;
        Ok(Response::new(pb::DeleteVrfResponse {}))
    }

    async fn list_vrfs(
        &self,
        _r: Request<pb::ListVrfsRequest>,
    ) -> Result<Response<pb::ListVrfsResponse>, Status> {
        let vrfs = self.inner.list_vrfs().await.map_err(status_from)?;
        Ok(Response::new(pb::ListVrfsResponse {
            vrfs: vrfs
                .into_iter()
                .map(|(name, nico_vpc_id)| pb::VrfRef { name, nico_vpc_id })
                .collect(),
        }))
    }

    async fn set_port_membership(
        &self,
        r: Request<pb::SetPortMembershipRequest>,
    ) -> Result<Response<pb::SetPortMembershipResponse>, Status> {
        let r = r.into_inner();
        let witnesses = r.witnesses.unwrap_or_default();
        let contract = r.contract.unwrap_or_default();
        let m = PortMembership {
            port: r.port.clone(),
            vrf: (!r.vrf.is_empty()).then(|| r.vrf.clone()),
            previous_vrf: (!r.previous_vrf.is_empty()).then(|| r.previous_vrf.clone()),
            witnesses: Witnesses {
                expected_macs: witnesses.expected_macs,
                expected_lldp: witnesses.expected_lldp.map(|l| (l.chassis_id, l.port_id)),
            },
            contract: PortContract {
                allowed_macs: contract.allowed_macs,
                allowed_ips: contract.allowed_ips,
                dhcp_snooping: contract.dhcp_snooping,
                storm_control: contract.storm_control,
                isolated_port: contract.isolated_port,
            },
        };
        tracing::info!(request_id = %r.request_id, port = %m.port, vrf = ?m.vrf, "set_port_membership");
        let e = self.inner.set_port_membership(&m).await.map_err(status_from)?;
        Ok(Response::new(pb::SetPortMembershipResponse {
            enforced: Some(pb::Enforcement {
                mac_limit: e.mac_limit,
                ip_source_guard: e.ip_source_guard,
                dhcp_snooping: e.dhcp_snooping,
                storm_control: e.storm_control,
                isolated_port: e.isolated_port,
            }),
        }))
    }

    async fn list_port_memberships(
        &self,
        r: Request<pb::ListPortMembershipsRequest>,
    ) -> Result<Response<pb::ListPortMembershipsResponse>, Status> {
        let r = r.into_inner();
        if r.vrf.is_empty() {
            // Legacy adapters index ports by VRF only; a full listing means
            // walking every VRF.
            let vrfs = self.inner.list_vrfs().await.map_err(status_from)?;
            let mut out = Vec::new();
            for (name, _) in vrfs {
                for port in self.inner.list_attachments(&name).await.map_err(status_from)? {
                    out.push(pb::PortMembership { port, vrf: name.clone(), enforced: None });
                }
            }
            return Ok(Response::new(pb::ListPortMembershipsResponse { memberships: out }));
        }
        let ports = self.inner.list_attachments(&r.vrf).await.map_err(status_from)?;
        Ok(Response::new(pb::ListPortMembershipsResponse {
            memberships: ports
                .into_iter()
                .map(|port| pb::PortMembership { port, vrf: r.vrf.clone(), enforced: None })
                .collect(),
        }))
    }

    async fn peer_vrfs(
        &self,
        r: Request<pb::PeerVrfsRequest>,
    ) -> Result<Response<pb::PeerVrfsResponse>, Status> {
        let r = r.into_inner();
        tracing::info!(request_id = %r.request_id, a = %r.a, b = %r.b, "peer_vrfs");
        self.inner.peer_vpcs(&r.a, &r.b).await.map_err(status_from)?;
        Ok(Response::new(pb::PeerVrfsResponse {}))
    }

    async fn get_vrf_status(
        &self,
        r: Request<pb::GetVrfStatusRequest>,
    ) -> Result<Response<pb::VrfStatus>, Status> {
        let name = r.into_inner().name;
        let st = self.inner.get_vrf_status(&name).await.map_err(status_from)?;
        Ok(Response::new(match st {
            None => pb::VrfStatus { present: false, ..Default::default() },
            Some(v) => {
                let nodes = v
                    .get("nodes")
                    .and_then(|n| n.as_array())
                    .map(|a| a.iter().filter_map(|x| x.as_str().map(str::to_string)).collect())
                    .unwrap_or_default();
                let operational_state = v
                    .get("operationalState")
                    .and_then(|s| s.as_str())
                    .unwrap_or_default()
                    .to_string();
                pb::VrfStatus {
                    present: true,
                    programmed: operational_state.eq_ignore_ascii_case("up"),
                    nodes,
                    operational_state,
                    raw_json: v.to_string(),
                }
            }
        }))
    }

    type WatchEventsStream = ReceiverStream<Result<pb::FabricEvent, Status>>;

    async fn watch_events(
        &self,
        _r: Request<pb::WatchEventsRequest>,
    ) -> Result<Response<Self::WatchEventsStream>, Status> {
        // Events are declared per adapter in Capabilities; the in-process
        // adapters do not produce them yet, so the stream stays open and empty
        // rather than lying with synthesized events.
        let (_tx, rx) = tokio::sync::mpsc::channel(1);
        Ok(Response::new(ReceiverStream::new(rx)))
    }
}

async fn build_adapter(cfg: &FabricConfig) -> eyre::Result<(Arc<dyn FabricOperations>, &'static str)> {
    Ok(match cfg.backend {
        FabricBackend::Eda => (Arc::new(EdaFabric::try_default(cfg).await?), "eda"),
        FabricBackend::Hedgehog => (Arc::new(HedgehogFabric::try_default(cfg).await?), "hedgehog"),
        FabricBackend::Agent => {
            eyre::bail!("fabric-agent cannot front another agent; set backend = \"eda\" or \"hedgehog\"")
        }
    })
}

#[tokio::main]
async fn main() -> eyre::Result<()> {
    tracing_subscriber::fmt().with_env_filter(
        tracing_subscriber::EnvFilter::try_from_default_env()
            .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
    )
    .init();
    let args = Args::parse();
    let file: AgentFile = toml::from_str(&std::fs::read_to_string(&args.config)?)?;
    let (inner, adapter) = build_adapter(&file.fabric).await?;
    let svc = Service { inner, adapter: adapter.to_string() };

    let mut server = Server::builder();
    if let (Some(cert), Some(key)) = (&args.tls_cert, &args.tls_key) {
        let mut tls = ServerTlsConfig::new()
            .identity(Identity::from_pem(std::fs::read(cert)?, std::fs::read(key)?));
        if let Some(ca) = &args.client_ca {
            tls = tls.client_ca_root(Certificate::from_pem(std::fs::read(ca)?));
        }
        server = server.tls_config(tls)?;
        tracing::info!(listen = %args.listen, adapter, mtls = args.client_ca.is_some(), "fabric-agent serving with TLS");
    } else {
        tracing::warn!(listen = %args.listen, adapter, "fabric-agent serving WITHOUT TLS (development only)");
    }
    server
        .add_service(FabricAgentServer::new(svc))
        .serve(args.listen)
        .await?;
    Ok(())
}
