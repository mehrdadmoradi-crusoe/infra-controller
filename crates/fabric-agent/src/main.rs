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

use carbide_fabric::{EdaFabric, FabricBackend, FabricConfig, FabricOperations, HedgehogFabric};
use carbide_fabric_agent::Service;
use clap::Parser;
use tonic::transport::{Certificate, Identity, Server, ServerTlsConfig};
use tonic::{Request, Status};

#[derive(Parser, Debug)]
#[command(
    name = "fabric-agent",
    about = "NICo fabric agent: renders tenant network intent onto a fabric controller"
)]
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
    /// SPIFFE ids (URI SANs) a client certificate must carry, e.g.
    /// `spiffe://nico.local/nico-system/sa/nico-api`. Repeatable. Without it
    /// any certificate the client CA signed is accepted.
    #[arg(
        long = "allowed-client-uri",
        env = "FABRIC_AGENT_ALLOWED_CLIENT_URIS",
        value_delimiter = ','
    )]
    allowed_client_uris: Vec<String>,
}

/// URI SANs of the leaf certificate the client presented, lower-cased.
fn client_uris(req: &Request<()>) -> Vec<String> {
    let Some(certs) = req.peer_certs() else {
        return Vec::new();
    };
    let Some(leaf) = certs.first() else {
        return Vec::new();
    };
    let Ok((_, cert)) = x509_parser::parse_x509_certificate(leaf.as_ref()) else {
        return Vec::new();
    };
    let Ok(Some(san)) = cert.subject_alternative_name() else {
        return Vec::new();
    };
    san.value
        .general_names
        .iter()
        .filter_map(|n| match n {
            x509_parser::extensions::GeneralName::URI(u) => Some(u.to_ascii_lowercase()),
            _ => None,
        })
        .collect()
}

/// Refuse calls from certificates whose SPIFFE id is not on the list. This is
/// the identity check on top of the chain check mTLS already did.
fn authorize(
    allowed: Arc<Vec<String>>,
) -> impl Fn(Request<()>) -> Result<Request<()>, Status> + Clone {
    move |req: Request<()>| {
        if allowed.is_empty() {
            return Ok(req);
        }
        let uris = client_uris(&req);
        if uris
            .iter()
            .any(|u| allowed.iter().any(|a| a.eq_ignore_ascii_case(u)))
        {
            Ok(req)
        } else {
            tracing::warn!(?uris, "fabric-agent: client identity not allowed");
            Err(Status::permission_denied(format!(
                "client identity {uris:?} is not allowed to drive this fabric agent"
            )))
        }
    }
}

#[derive(serde::Deserialize)]
struct AgentFile {
    fabric: FabricConfig,
}

async fn build_adapter(
    cfg: &FabricConfig,
) -> eyre::Result<(Arc<dyn FabricOperations>, &'static str)> {
    Ok(match cfg.backend {
        FabricBackend::Eda => (Arc::new(EdaFabric::try_default(cfg).await?), "eda"),
        FabricBackend::Hedgehog => (
            Arc::new(HedgehogFabric::try_default(cfg).await?),
            "hedgehog",
        ),
        FabricBackend::Fake => (Arc::new(carbide_fabric::FakeFabric::new()), "fake"),
        FabricBackend::Agent => {
            eyre::bail!(
                "fabric-agent cannot front another agent; set backend = \"eda\", \"hedgehog\" or \"fake\""
            )
        }
    })
}

#[tokio::main]
async fn main() -> eyre::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .init();
    let args = Args::parse();
    let file: AgentFile = toml::from_str(&std::fs::read_to_string(&args.config)?)?;
    let (inner, adapter) = build_adapter(&file.fabric).await?;
    let svc = Service::new(inner, adapter);

    if !args.allowed_client_uris.is_empty() && args.client_ca.is_none() {
        eyre::bail!(
            "--allowed-client-uri needs --client-ca (the identity comes from the client certificate)"
        );
    }
    let mut server = Server::builder();
    if let (Some(cert), Some(key)) = (&args.tls_cert, &args.tls_key) {
        let mut tls = ServerTlsConfig::new().identity(Identity::from_pem(
            std::fs::read(cert)?,
            std::fs::read(key)?,
        ));
        if let Some(ca) = &args.client_ca {
            tls = tls.client_ca_root(Certificate::from_pem(std::fs::read(ca)?));
        }
        server = server.tls_config(tls)?;
        tracing::info!(listen = %args.listen, adapter, mtls = args.client_ca.is_some(),
            allowed_clients = ?args.allowed_client_uris, "fabric-agent serving with TLS");
    } else {
        tracing::warn!(listen = %args.listen, adapter, "fabric-agent serving WITHOUT TLS (development only)");
    }
    let allowed = Arc::new(args.allowed_client_uris.clone());
    server
        .add_service(
            carbide_fabric_agent_api::FabricAgentServer::with_interceptor(svc, authorize(allowed)),
        )
        .serve(args.listen)
        .await?;
    Ok(())
}
