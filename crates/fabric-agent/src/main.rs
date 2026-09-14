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
        tracing::info!(listen = %args.listen, adapter, mtls = args.client_ca.is_some(), "fabric-agent serving with TLS");
    } else {
        tracing::warn!(listen = %args.listen, adapter, "fabric-agent serving WITHOUT TLS (development only)");
    }
    server
        .add_service(svc.into_server())
        .serve(args.listen)
        .await?;
    Ok(())
}
