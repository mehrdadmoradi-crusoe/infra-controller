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

//! The production transport shape in a unit test: the agent serving mTLS on a
//! loopback port with a throwaway CA, NICo's client presenting a SPIFFE
//! certificate. Pins the crypto-provider selection (the workspace links both
//! ring and aws-lc-rs; the first mTLS deployment in kind crash-looped on
//! exactly this) and the identity check on top of the chain check.

use std::path::PathBuf;
use std::sync::Arc;

use carbide_fabric::{AgentConfig, FabricError, FabricOperations, FakeFabric, GrpcFabricAgent};
use carbide_fabric_agent::TlsFiles;
use rcgen::{BasicConstraints, CertificateParams, IsCa, Issuer, KeyPair, SanType};

struct Pki {
    dir: tempfile::TempDir,
    ca: PathBuf,
    server_cert: PathBuf,
    server_key: PathBuf,
}

impl Pki {
    fn leaf(
        dir: &tempfile::TempDir,
        issuer: &Issuer<'_, KeyPair>,
        name: &str,
        dns: Vec<String>,
        spiffe: Option<&str>,
    ) -> (PathBuf, PathBuf) {
        let key = KeyPair::generate().expect("leaf key");
        let mut params = CertificateParams::new(dns).expect("leaf params");
        if let Some(id) = spiffe {
            params
                .subject_alt_names
                .push(SanType::URI(id.try_into().expect("uri")));
        }
        let cert = params.signed_by(&key, issuer).expect("sign leaf");
        let cert_path = dir.path().join(format!("{name}.crt"));
        let key_path = dir.path().join(format!("{name}.key"));
        std::fs::write(&cert_path, cert.pem()).unwrap();
        std::fs::write(&key_path, key.serialize_pem()).unwrap();
        (cert_path, key_path)
    }
}

/// CA plus server plus two clients: one allowed, one not.
fn pki_with_clients() -> (Pki, (PathBuf, PathBuf), (PathBuf, PathBuf)) {
    let dir = tempfile::tempdir().expect("tempdir");
    let ca_key = KeyPair::generate().expect("ca key");
    let mut ca_params = CertificateParams::new(Vec::<String>::new()).expect("ca params");
    ca_params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    let ca_cert = ca_params.self_signed(&ca_key).expect("ca cert");
    let ca = dir.path().join("ca.crt");
    std::fs::write(&ca, ca_cert.pem()).unwrap();
    let issuer = Issuer::new(ca_params, ca_key);
    let (server_cert, server_key) = Pki::leaf(
        &dir,
        &issuer,
        "server",
        vec!["localhost".into()],
        Some("spiffe://nico.local/nico-system/sa/fabric-agent"),
    );
    let nico = Pki::leaf(
        &dir,
        &issuer,
        "nico-api",
        vec![],
        Some("spiffe://nico.local/nico-system/sa/nico-api"),
    );
    let stranger = Pki::leaf(
        &dir,
        &issuer,
        "machine-a-tron",
        vec![],
        Some("spiffe://nico.local/nico-system/sa/machine-a-tron"),
    );
    (
        Pki {
            dir,
            ca,
            server_cert,
            server_key,
        },
        nico,
        stranger,
    )
}

fn client_for(
    pki: &Pki,
    addr: std::net::SocketAddr,
    identity: &(PathBuf, PathBuf),
) -> GrpcFabricAgent {
    GrpcFabricAgent::connect_lazy(&AgentConfig {
        endpoint: format!("https://localhost:{}", addr.port()),
        ca_file: Some(pki.ca.to_string_lossy().into_owned()),
        client_cert_file: Some(identity.0.to_string_lossy().into_owned()),
        client_key_file: Some(identity.1.to_string_lossy().into_owned()),
        insecure_skip_tls_verify: false,
        timeout_secs: 10,
    })
    .expect("client")
}

#[tokio::test]
async fn mtls_with_the_allowed_spiffe_id_serves_the_contract() {
    let (pki, nico, _stranger) = pki_with_clients();
    let inner: Arc<dyn FabricOperations> = Arc::new(FakeFabric::new());
    let tls = TlsFiles {
        cert: pki.server_cert.clone(),
        key: pki.server_key.clone(),
        client_ca: Some(pki.ca.clone()),
        allowed_client_uris: vec!["spiffe://nico.local/nico-system/sa/nico-api".into()],
    };
    let (addr, handle) =
        carbide_fabric_agent::serve_tls(inner, "fake", "127.0.0.1:0".parse().unwrap(), &tls)
            .await
            .expect("serve tls");
    let client = client_for(&pki, addr, &nico);
    let caps = client.capabilities().await.expect("capabilities over mTLS");
    assert_eq!(caps.adapter, "fake");
    let vrfs = client.list_vrfs().await.expect("list over mTLS");
    assert!(vrfs.is_empty());
    handle.abort();
    drop(pki.dir);
}

#[tokio::test]
async fn a_valid_certificate_with_the_wrong_identity_is_refused() {
    let (pki, _nico, stranger) = pki_with_clients();
    let inner: Arc<dyn FabricOperations> = Arc::new(FakeFabric::new());
    let tls = TlsFiles {
        cert: pki.server_cert.clone(),
        key: pki.server_key.clone(),
        client_ca: Some(pki.ca.clone()),
        allowed_client_uris: vec!["spiffe://nico.local/nico-system/sa/nico-api".into()],
    };
    let (addr, handle) =
        carbide_fabric_agent::serve_tls(inner, "fake", "127.0.0.1:0".parse().unwrap(), &tls)
            .await
            .expect("serve tls");
    // The chain checks out (same CA); the identity does not.
    let client = client_for(&pki, addr, &stranger);
    let err = client.list_vrfs().await.expect_err("stranger refused");
    match err {
        FabricError::Agent(m) => assert!(
            m.contains("PermissionDenied") || m.contains("not allowed"),
            "{m}"
        ),
        other => panic!("expected an agent error, got {other}"),
    }
    handle.abort();
}

#[tokio::test]
async fn a_client_without_a_certificate_cannot_connect_when_mtls_is_required() {
    let (pki, _nico, _stranger) = pki_with_clients();
    let inner: Arc<dyn FabricOperations> = Arc::new(FakeFabric::new());
    let tls = TlsFiles {
        cert: pki.server_cert.clone(),
        key: pki.server_key.clone(),
        client_ca: Some(pki.ca.clone()),
        allowed_client_uris: vec![],
    };
    let (addr, handle) =
        carbide_fabric_agent::serve_tls(inner, "fake", "127.0.0.1:0".parse().unwrap(), &tls)
            .await
            .expect("serve tls");
    let client = GrpcFabricAgent::connect_lazy(&AgentConfig {
        endpoint: format!("https://localhost:{}", addr.port()),
        ca_file: Some(pki.ca.to_string_lossy().into_owned()),
        client_cert_file: None,
        client_key_file: None,
        insecure_skip_tls_verify: false,
        timeout_secs: 5,
    })
    .expect("client");
    let err = client.list_vrfs().await.expect_err("no client certificate");
    assert!(matches!(err, FabricError::Agent(_)), "{err}");
    handle.abort();
}
