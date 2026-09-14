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

//! Contract tests over the wire: the agent serving the in-memory fake on a
//! loopback port, NICo's gRPC client on the other end. Proves the mapping
//! (quarantine as empty VRF, previous_vrf, error codes, enforcement) and runs
//! the full conformance suite through the transport.

use std::sync::Arc;

use carbide_fabric::conformance::{self, Fixture};
use carbide_fabric::{
    AgentConfig, Capabilities, FabricError, FabricOperations, FakeFabric, GrpcFabricAgent,
    HostAttachment, PortMembership, Witnesses,
};

async fn start(
    fake: FakeFabric,
) -> (
    GrpcFabricAgent,
    tokio::task::JoinHandle<Result<(), tonic::transport::Error>>,
) {
    let inner: Arc<dyn FabricOperations> = Arc::new(fake);
    let (addr, handle) =
        carbide_fabric_agent::serve_plain(inner, "fake", "127.0.0.1:0".parse().unwrap())
            .await
            .expect("serve");
    let client = GrpcFabricAgent::connect_lazy(&AgentConfig {
        endpoint: format!("http://{addr}"),
        ca_file: None,
        client_cert_file: None,
        client_key_file: None,
        insecure_skip_tls_verify: false,
        timeout_secs: 10,
    })
    .expect("client");
    (client, handle)
}

#[tokio::test]
async fn conformance_suite_passes_through_the_agent() {
    let fake = FakeFabric::new();
    fake.observe("leaf1-ethernet-1-3", &["06:00:00:00:05:01"], None);
    let (client, handle) = start(fake).await;
    let report = conformance::run(Arc::new(client), &Fixture::default()).await;
    handle.abort();
    assert!(
        report.passed(),
        "conformance failures: {:#?}",
        report.failures()
    );
    assert_eq!(
        report.outcomes.len(),
        conformance::CHECK_COUNT,
        "every check ran"
    );
}

#[tokio::test]
async fn capabilities_cross_the_wire_and_carry_the_contract_version() {
    let (client, handle) = start(FakeFabric::new()).await;
    let caps = client.capabilities().await.expect("capabilities");
    handle.abort();
    assert_eq!(
        caps.contract_version,
        carbide_fabric_agent_api::CONTRACT_VERSION
    );
    assert_eq!(caps.adapter, "fake");
    assert!(caps.quarantine_vrf && caps.mac_witness && caps.peering);
}

#[tokio::test]
async fn witness_mismatch_maps_to_failed_precondition_and_back() {
    let fake = FakeFabric::new();
    fake.observe("leaf1-ethernet-1-3", &["06:00:00:00:05:01"], None);
    let (client, handle) = start(fake).await;
    client
        .ensure_vrf(&carbide_fabric::VrfIntent {
            nico_vpc_id: "vpc-x".into(),
            name: "x".into(),
            subnet_cidr: "10.9.0.0/24".into(),
            vlan: 909,
            gateway: "10.9.0.1".into(),
            vni: Some(2024909),
            dhcp_range: None,
        })
        .await
        .expect("ensure");
    let err = client
        .set_port_membership(&PortMembership {
            port: "leaf1-ethernet-1-3".into(),
            vrf: Some("x".into()),
            previous_vrf: None,
            witnesses: Witnesses {
                expected_macs: vec!["02:00:00:00:00:99".into()],
                expected_lldp: None,
            },
            contract: Default::default(),
        })
        .await
        .expect_err("wrong witness must be refused");
    handle.abort();
    match err {
        FabricError::WitnessMismatch { port, detail } => {
            assert_eq!(port, "leaf1-ethernet-1-3");
            assert!(
                detail.contains("06:00:00:00:05:01"),
                "detail names what the switch saw: {detail}"
            );
        }
        other => panic!("expected WitnessMismatch, got {other}"),
    }
}

#[tokio::test]
async fn undeclared_peering_maps_to_unimplemented_and_back() {
    let caps = Capabilities {
        peering: false,
        ..FakeFabric::new().capabilities().await.unwrap()
    };
    let (client, handle) = start(FakeFabric::new().with_capabilities(caps)).await;
    for n in ["p1", "p2"] {
        client
            .ensure_vrf(&carbide_fabric::VrfIntent {
                nico_vpc_id: format!("vpc-{n}"),
                name: n.into(),
                subnet_cidr: "10.8.0.0/24".into(),
                vlan: 908,
                gateway: "10.8.0.1".into(),
                vni: None,
                dhcp_range: None,
            })
            .await
            .expect("ensure");
    }
    let err = client
        .peer_vpcs("p1", "p2")
        .await
        .expect_err("undeclared peering must be refused");
    handle.abort();
    assert!(matches!(err, FabricError::Unsupported(_)), "got {err}");
}

#[tokio::test]
async fn detach_carries_previous_vrf_and_lands_in_quarantine() {
    let fake = FakeFabric::new();
    let probe = fake.clone();
    let (client, handle) = start(fake).await;
    client
        .ensure_vrf(&carbide_fabric::VrfIntent {
            nico_vpc_id: "vpc-q".into(),
            name: "q".into(),
            subnet_cidr: "10.7.0.0/24".into(),
            vlan: 907,
            gateway: "10.7.0.1".into(),
            vni: None,
            dhcp_range: None,
        })
        .await
        .expect("ensure");
    client
        .attach_host(&HostAttachment {
            vpc_name: "q".into(),
            connection: "leaf2-ethernet-1-3".into(),
        })
        .await
        .expect("attach");
    assert_eq!(
        probe
            .memberships()
            .get("leaf2-ethernet-1-3")
            .map(String::as_str),
        Some("q")
    );
    client
        .detach_host(&HostAttachment {
            vpc_name: "q".into(),
            connection: "leaf2-ethernet-1-3".into(),
        })
        .await
        .expect("detach");
    handle.abort();
    assert!(
        !probe.memberships().contains_key("leaf2-ethernet-1-3"),
        "port is in quarantine"
    );
    let last = probe
        .calls()
        .into_iter()
        .rev()
        .find(|c| matches!(c, carbide_fabric::fake::Call::SetPortMembership { .. }));
    assert_eq!(
        last,
        Some(carbide_fabric::fake::Call::SetPortMembership {
            port: "leaf2-ethernet-1-3".into(),
            vrf: None
        })
    );
}

#[tokio::test]
async fn agent_down_is_a_per_call_error_not_a_construction_failure() {
    let client = GrpcFabricAgent::connect_lazy(&AgentConfig {
        endpoint: "http://127.0.0.1:1".into(),
        ca_file: None,
        client_cert_file: None,
        client_key_file: None,
        insecure_skip_tls_verify: false,
        timeout_secs: 2,
    })
    .expect("lazy connect never fails on an unreachable agent");
    let err = client.list_vrfs().await.expect_err("call must fail");
    assert!(matches!(err, FabricError::Agent(_)), "got {err}");
}
