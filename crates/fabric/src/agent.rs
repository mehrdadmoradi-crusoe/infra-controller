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

//! `FabricOperations` over the `fabric_agent.v1` gRPC contract.
//!
//! This is the production transport: NICo talks to an out-of-process fabric
//! agent (run by the network team next to the fabric) and never to a vendor
//! controller directly. The agent holds the controller credentials; NICo holds
//! only its mTLS identity. Everything vendor-specific lives behind the agent.

use std::time::Duration;

use async_trait::async_trait;
use carbide_fabric_agent_api::{FabricAgentClient, v1 as pb};
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Identity};
use tonic::{Code, Request, Status};

use crate::{
    AgentConfig, Capabilities, Enforcement, FabricError, FabricOperations, HostAttachment,
    PortMembership, VrfIntent,
};

#[derive(Clone)]
pub struct GrpcFabricAgent {
    client: FabricAgentClient<Channel>,
    endpoint: String,
}

impl std::fmt::Debug for GrpcFabricAgent {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("GrpcFabricAgent")
            .field("endpoint", &self.endpoint)
            .finish()
    }
}

impl GrpcFabricAgent {
    /// Build the channel lazily: the agent may be unreachable at NICo startup
    /// and the reconcile must not depend on it being up. Every call surfaces
    /// connection errors per pass instead.
    pub fn connect_lazy(cfg: &AgentConfig) -> Result<Self, FabricError> {
        let mut endpoint = Channel::from_shared(cfg.endpoint.clone())
            .map_err(|e| FabricError::Invalid(format!("agent endpoint: {e}")))?
            .timeout(Duration::from_secs(cfg.timeout_secs))
            .connect_timeout(Duration::from_secs(10));
        if cfg.endpoint.starts_with("https://") {
            // The workspace builds tonic with `tls-ring` only (no system or
            // webpki roots), so the trust anchor must be given explicitly.
            let mut tls = ClientTlsConfig::new();
            match &cfg.ca_file {
                Some(ca) => {
                    let pem = std::fs::read(ca)
                        .map_err(|e| FabricError::Invalid(format!("agent ca_file {ca}: {e}")))?;
                    tls = tls.ca_certificate(Certificate::from_pem(pem));
                }
                None if cfg.insecure_skip_tls_verify => {}
                None => {
                    return Err(FabricError::Invalid(
                        "fabric.agent: https endpoint needs ca_file (or insecure_skip_tls_verify for development)"
                            .to_string(),
                    ));
                }
            }
            if let (Some(cert), Some(key)) = (&cfg.client_cert_file, &cfg.client_key_file) {
                let cert_pem = std::fs::read(cert)
                    .map_err(|e| FabricError::Invalid(format!("agent client cert {cert}: {e}")))?;
                let key_pem = std::fs::read(key)
                    .map_err(|e| FabricError::Invalid(format!("agent client key {key}: {e}")))?;
                tls = tls.identity(Identity::from_pem(cert_pem, key_pem));
            }
            if cfg.insecure_skip_tls_verify {
                tracing::warn!("fabric agent: TLS verification disabled (development only)");
            }
            endpoint = endpoint
                .tls_config(tls)
                .map_err(|e| FabricError::Agent(e.to_string()))?;
        }
        Ok(Self {
            client: FabricAgentClient::new(endpoint.connect_lazy()),
            endpoint: cfg.endpoint.clone(),
        })
    }

    fn map_status(port: Option<&str>, s: Status) -> FabricError {
        match s.code() {
            Code::Unimplemented => FabricError::Unsupported(s.message().to_string()),
            Code::FailedPrecondition if port.is_some() => FabricError::WitnessMismatch {
                port: port.unwrap_or_default().to_string(),
                detail: s.message().to_string(),
            },
            _ => FabricError::Agent(format!("{}: {}", s.code(), s.message())),
        }
    }

    fn request_id() -> String {
        // The reconcile is level-triggered, so the id identifies the pass and
        // call, not a user request; it still lets the agent tie every downstream
        // transaction back to NICo.
        format!("nico-{}", uuid::Uuid::new_v4())
    }
}

fn to_pb_membership(m: &PortMembership, request_id: String) -> pb::SetPortMembershipRequest {
    pb::SetPortMembershipRequest {
        port: m.port.clone(),
        vrf: m.vrf.clone().unwrap_or_default(),
        previous_vrf: m.previous_vrf.clone().unwrap_or_default(),
        witnesses: Some(pb::Witnesses {
            expected_macs: m.witnesses.expected_macs.clone(),
            expected_lldp: m
                .witnesses
                .expected_lldp
                .as_ref()
                .map(|(c, p)| pb::LldpNeighbor {
                    chassis_id: c.clone(),
                    port_id: p.clone(),
                }),
        }),
        contract: Some(pb::PortContract {
            allowed_macs: m.contract.allowed_macs.clone(),
            allowed_ips: m.contract.allowed_ips.clone(),
            dhcp_snooping: m.contract.dhcp_snooping,
            storm_control: m.contract.storm_control,
            isolated_port: m.contract.isolated_port,
        }),
        request_id,
    }
}

fn from_pb_enforcement(e: Option<pb::Enforcement>) -> Enforcement {
    let e = e.unwrap_or_default();
    Enforcement {
        mac_limit: e.mac_limit,
        ip_source_guard: e.ip_source_guard,
        dhcp_snooping: e.dhcp_snooping,
        storm_control: e.storm_control,
        isolated_port: e.isolated_port,
    }
}

#[async_trait]
impl FabricOperations for GrpcFabricAgent {
    async fn ensure_vrf(&self, intent: &VrfIntent) -> Result<(), FabricError> {
        let req = pb::EnsureVrfRequest {
            vrf: Some(pb::VrfSpec {
                name: intent.name.clone(),
                nico_vpc_id: intent.nico_vpc_id.clone(),
                subnet_cidr: intent.subnet_cidr.clone(),
                vlan: u32::from(intent.vlan),
                gateway: intent.gateway.clone(),
                vni: intent.vni,
            }),
            request_id: Self::request_id(),
        };
        self.client
            .clone()
            .ensure_vrf(Request::new(req))
            .await
            .map(|_| ())
            .map_err(|s| Self::map_status(None, s))
    }

    async fn attach_host(&self, att: &HostAttachment) -> Result<(), FabricError> {
        self.set_port_membership(&PortMembership {
            port: att.connection.clone(),
            vrf: Some(att.vpc_name.clone()),
            ..PortMembership::default()
        })
        .await
        .map(|_| ())
    }

    async fn list_attachments(&self, vpc_name: &str) -> Result<Vec<String>, FabricError> {
        let resp = self
            .client
            .clone()
            .list_port_memberships(Request::new(pb::ListPortMembershipsRequest {
                vrf: vpc_name.to_string(),
            }))
            .await
            .map_err(|s| Self::map_status(None, s))?
            .into_inner();
        Ok(resp.memberships.into_iter().map(|m| m.port).collect())
    }

    async fn detach_host(&self, att: &HostAttachment) -> Result<(), FabricError> {
        self.set_port_membership(&PortMembership {
            port: att.connection.clone(),
            vrf: None,
            previous_vrf: Some(att.vpc_name.clone()),
            ..PortMembership::default()
        })
        .await
        .map(|_| ())
    }

    async fn peer_vpcs(&self, a: &str, b: &str) -> Result<(), FabricError> {
        self.client
            .clone()
            .peer_vrfs(Request::new(pb::PeerVrfsRequest {
                a: a.to_string(),
                b: b.to_string(),
                request_id: Self::request_id(),
            }))
            .await
            .map(|_| ())
            .map_err(|s| Self::map_status(None, s))
    }

    async fn get_vrf_status(
        &self,
        vpc_name: &str,
    ) -> Result<Option<serde_json::Value>, FabricError> {
        let st = self
            .client
            .clone()
            .get_vrf_status(Request::new(pb::GetVrfStatusRequest {
                name: vpc_name.to_string(),
            }))
            .await
            .map_err(|s| Self::map_status(None, s))?
            .into_inner();
        if !st.present {
            return Ok(None);
        }
        let raw = if st.raw_json.is_empty() {
            serde_json::Value::Null
        } else {
            serde_json::from_str(&st.raw_json).unwrap_or(serde_json::Value::String(st.raw_json))
        };
        Ok(Some(serde_json::json!({
            "programmed": st.programmed,
            "nodes": st.nodes,
            "operationalState": st.operational_state,
            "raw": raw,
        })))
    }

    async fn list_vrfs(&self) -> Result<Vec<(String, String)>, FabricError> {
        let resp = self
            .client
            .clone()
            .list_vrfs(Request::new(pb::ListVrfsRequest {}))
            .await
            .map_err(|s| Self::map_status(None, s))?
            .into_inner();
        Ok(resp
            .vrfs
            .into_iter()
            .map(|v| (v.name, v.nico_vpc_id))
            .collect())
    }

    async fn delete_vrf(&self, vpc_name: &str) -> Result<(), FabricError> {
        self.client
            .clone()
            .delete_vrf(Request::new(pb::DeleteVrfRequest {
                name: vpc_name.to_string(),
                request_id: Self::request_id(),
            }))
            .await
            .map(|_| ())
            .map_err(|s| Self::map_status(None, s))
    }

    async fn capabilities(&self) -> Result<Capabilities, FabricError> {
        let c = self
            .client
            .clone()
            .get_capabilities(Request::new(pb::GetCapabilitiesRequest {}))
            .await
            .map_err(|s| Self::map_status(None, s))?
            .into_inner();
        Ok(Capabilities {
            contract_version: c.contract_version,
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
        })
    }

    async fn set_port_membership(&self, m: &PortMembership) -> Result<Enforcement, FabricError> {
        let resp = self
            .client
            .clone()
            .set_port_membership(Request::new(to_pb_membership(m, Self::request_id())))
            .await
            .map_err(|s| Self::map_status(Some(&m.port), s))?
            .into_inner();
        Ok(from_pb_enforcement(resp.enforced))
    }
}
