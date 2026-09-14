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

//! The EDA adapter against a recorded EDA API: the behaviors the simulators
//! taught us, pinned so they cannot regress without a controller present.
//! Shapes mirror real EDA 26.4 responses (server-side defaults on read,
//! Keycloak password grant, 404 for absent objects, one transaction per write).

use mockito::{Matcher, Server, ServerGuard};
use serde_json::json;

use crate::{
    EdaFabric, FabricConfig, FabricError, FabricOperations, PortContract, PortMembership,
    VrfIntent, Witnesses,
};

const TOKEN_PATH: &str = "/core/httpproxy/v1/keycloak/realms/eda/protocol/openid-connect/token";
const NS: &str = "ns";

async fn eda_against(server: &ServerGuard) -> EdaFabric {
    eda_with(server, json!({}), None).await
}

/// An adapter with extra `[fabric.eda]` fields and an optional quarantine VPC.
async fn eda_with(
    server: &ServerGuard,
    extra: serde_json::Value,
    quarantine_vpc: Option<&str>,
) -> EdaFabric {
    unsafe {
        std::env::set_var(crate::eda::PASSWORD_ENV, "pw");
        std::env::set_var(crate::eda::CLIENT_SECRET_ENV, "secret");
    }
    let mut eda = json!({"api_url": server.url(), "username": "admin"});
    if let (Some(base), Some(more)) = (eda.as_object_mut(), extra.as_object()) {
        for (k, v) in more {
            base.insert(k.clone(), v.clone());
        }
    }
    let cfg = FabricConfig {
        enabled: true,
        namespace: NS.to_string(),
        eda: Some(serde_json::from_value(eda).expect("EdaConfig")),
        quarantine_vpc: quarantine_vpc.map(str::to_string),
        ..FabricConfig::default()
    };
    EdaFabric::try_default(&cfg).await.expect("construct")
}

async fn token_mock(server: &mut ServerGuard, expect: usize) -> mockito::Mock {
    server
        .mock("POST", TOKEN_PATH)
        .with_status(200)
        .with_header("content-type", "application/json")
        .with_body(json!({"access_token": "tok", "expires_in": 300}).to_string())
        .expect(expect)
        .create_async()
        .await
}

/// Guards that fail the test if any write reaches paths matching `path_re`.
async fn no_writes(server: &mut ServerGuard, path_re: &str) -> Vec<mockito::Mock> {
    let mut out = Vec::new();
    for verb in ["PUT", "POST", "PATCH", "DELETE"] {
        out.push(
            server
                .mock(verb, Matcher::Regex(path_re.into()))
                .expect(0)
                .create_async()
                .await,
        );
    }
    out
}

fn intent() -> VrfIntent {
    VrfIntent {
        nico_vpc_id: "vpc-x".into(),
        name: "x".into(),
        subnet_cidr: "10.201.1.0/24".into(),
        vlan: 901,
        gateway: "10.201.1.1".into(),
        vni: Some(2024901),
        dhcp_range: None,
    }
}

fn labels() -> serde_json::Value {
    json!({"nico.io/vpc-id": "vpc-x", "nico.io/vni": "2024901"})
}

/// What EDA returns on GET for each object once NICo has written it, with the
/// server-side defaults EDA adds (extra keys NICo never sets).
fn converged_objects() -> Vec<(&'static str, &'static str, serde_json::Value)> {
    let encap = json!({"vxlan": {"vniPool": "vni-pool", "tunnelIndexPool": "tunnel-index-pool"}});
    let mut router_encap = encap.clone();
    router_encap["vxlan"]["vni"] = json!(2024901);
    vec![
        (
            "routers",
            "nico-x",
            json!({
                "type": "EVPNVXLAN",
                "description": "NICo isolation domain x (vpc-x)",
                "encapOptions": router_encap,
                "eviPool": "evi-pool",
                "routerID": "auto",                 // EDA default
            }),
        ),
        (
            "bridgedomains",
            "nico-x-bd",
            json!({
                "type": "EVPNVXLAN",
                "description": "NICo subnet 10.201.1.0/24 of x",
                "encapOptions": encap,
                "eviPool": "evi-pool",
                "macLearning": {"enabled": true, "agingTimeSeconds": 300},
                "macDuplicationDetection": {"enabled": true}, // EDA default
            }),
        ),
        (
            "irbinterfaces",
            "nico-x",
            json!({
                "bridgeDomain": "nico-x-bd",
                "router": "nico-x",
                "description": "NICo gateway 10.201.1.1 for x",
                "ipAddresses": [{"ipv4Address": {"ipPrefix": "10.201.1.1/24", "primary": true, "anycast": true}}],
                "ipMTU": 1500,                      // EDA default
            }),
        ),
        (
            "vlans",
            "nico-x",
            json!({
                "bridgeDomain": "nico-x-bd",
                "vlanID": "901",
                "interfaceSelectors": ["nico.io/vpc=nico-x"],
                "description": "NICo VLAN 901 for x",
            }),
        ),
    ]
}

fn object_path(plural: &str, name: &str) -> String {
    format!("/apps/services.eda.nokia.com/v2/namespaces/{NS}/{plural}/{name}")
}

#[tokio::test]
async fn converged_ensure_vrf_reads_and_never_writes() {
    let mut server = Server::new_async().await;
    let token = token_mock(&mut server, 1).await;
    let mut gets = Vec::new();
    for (plural, name, spec) in converged_objects() {
        gets.push(
            server
                .mock("GET", object_path(plural, name).as_str())
                .with_status(200)
                .with_header("content-type", "application/json")
                .with_body(json!({"metadata": {"name": name, "namespace": NS, "labels": labels()}, "spec": spec}).to_string())
                .expect(1)
                .create_async().await,
        );
    }
    let writes = no_writes(&mut server, "/apps/.*").await;

    let eda = eda_against(&server).await;
    eda.ensure_vrf(&intent())
        .await
        .expect("converged ensure is Ok");

    token.assert();
    for g in gets {
        g.assert();
    }
    for w in writes {
        w.assert();
    }
}

#[tokio::test]
async fn a_changed_field_triggers_one_put_for_that_object_only() {
    let mut server = Server::new_async().await;
    token_mock(&mut server, 1).await;
    for (plural, name, mut spec) in converged_objects() {
        if plural == "vlans" {
            spec["vlanID"] = json!("777"); // drifted on the controller
        }
        server
            .mock("GET", object_path(plural, name).as_str())
            .with_status(200)
            .with_header("content-type", "application/json")
            .with_body(json!({"metadata": {"name": name, "namespace": NS, "labels": labels()}, "spec": spec}).to_string())
            .create_async().await;
    }
    let put_vlan = server
        .mock("PUT", object_path("vlans", "nico-x").as_str())
        .match_body(Matcher::PartialJsonString(
            json!({"spec": {"vlanID": "901"}}).to_string(),
        ))
        .with_status(200)
        .with_body("{}")
        .expect(1)
        .create_async()
        .await;
    let other_writes = no_writes(
        &mut server,
        "/apps/.*/(routers|bridgedomains|irbinterfaces)/.*",
    )
    .await;

    let eda = eda_against(&server).await;
    eda.ensure_vrf(&intent()).await.expect("repair is Ok");
    put_vlan.assert();
    for w in other_writes {
        w.assert();
    }
}

#[tokio::test]
async fn absent_objects_are_created_with_post_in_dependency_order() {
    let mut server = Server::new_async().await;
    token_mock(&mut server, 1).await;
    server
        .mock("GET", Matcher::Regex("/apps/.*".into()))
        .with_status(404)
        .with_body(json!({"message": "not found"}).to_string())
        .create_async()
        .await;
    let mut posts = Vec::new();
    for plural in ["routers", "bridgedomains", "irbinterfaces", "vlans"] {
        posts.push(
            server
                .mock(
                    "POST",
                    format!("/apps/services.eda.nokia.com/v2/namespaces/{NS}/{plural}").as_str(),
                )
                .with_status(201)
                .with_body("{}")
                .expect(1)
                .create_async()
                .await,
        );
    }
    let eda = eda_against(&server).await;
    eda.ensure_vrf(&intent()).await.expect("create is Ok");
    for p in posts {
        p.assert();
    }
}

#[tokio::test]
async fn unauthorized_refreshes_the_token_once_then_reports() {
    let mut server = Server::new_async().await;
    // One probe at construction, one forced refresh after the 401.
    let token = token_mock(&mut server, 2).await;
    server
        .mock("GET", Matcher::Regex("/apps/.*".into()))
        .with_status(401)
        .with_body("expired")
        .expect(2)
        .create_async()
        .await;
    let eda = eda_against(&server).await;
    let err = eda
        .get_vrf_status("x")
        .await
        .expect_err("still 401 after refresh");
    token.assert();
    match err {
        FabricError::Eda(m) => assert!(m.contains("401"), "message carries the status: {m}"),
        other => panic!("expected Eda error, got {other}"),
    }
}

#[tokio::test]
async fn controller_errors_carry_status_and_body() {
    let mut server = Server::new_async().await;
    token_mock(&mut server, 1).await;
    server
        .mock("GET", Matcher::Regex("/apps/.*".into()))
        .with_status(503)
        .with_body("engine restarting")
        .create_async()
        .await;
    let eda = eda_against(&server).await;
    let err = eda.list_vrfs().await.expect_err("5xx is an error");
    match err {
        FabricError::Eda(m) => assert!(m.contains("503") && m.contains("engine restarting"), "{m}"),
        other => panic!("expected Eda error, got {other}"),
    }
}

#[tokio::test]
async fn unreachable_controller_at_construction_does_not_fail_startup() {
    // Nothing listening: the token probe fails and is logged; construction succeeds.
    unsafe {
        std::env::set_var(crate::eda::PASSWORD_ENV, "pw");
        std::env::set_var(crate::eda::CLIENT_SECRET_ENV, "secret");
    }
    let cfg = FabricConfig {
        enabled: true,
        namespace: NS.to_string(),
        eda: Some(
            serde_json::from_value(json!({"api_url": "http://127.0.0.1:9", "username": "admin"}))
                .expect("EdaConfig"),
        ),
        ..FabricConfig::default()
    };
    let eda = EdaFabric::try_default(&cfg)
        .await
        .expect("startup must not depend on EDA");
    assert!(
        eda.list_vrfs().await.is_err(),
        "calls fail per pass instead"
    );
}

fn iface_path(name: &str) -> String {
    format!("/apps/interfaces.eda.nokia.com/v1/namespaces/{NS}/interfaces/{name}")
}

/// A fabric-owned `Interface` as EDA returns it, with the host-facing member.
fn interface(name: &str, label: Option<&str>, node: &str, node_if: &str) -> serde_json::Value {
    let mut labels = json!({"eda.nokia.com/role": "Edge"});
    if let Some(l) = label {
        labels["nico.io/vpc"] = json!(l);
    }
    json!({
        "metadata": {"name": name, "namespace": NS, "labels": labels},
        "spec": {"enabled": true, "encapType": "Dot1q", "ethernet": {"stormControl": {}}, "lldp": true,
                 "members": [{"node": node, "interface": node_if.replace('/', "-")}]},
        "status": {"members": [{"node": node, "nodeInterface": node_if, "operationalState": "Up", "neighbors": []}],
                   "operationalState": "Up"}
    })
}

fn mac_row(node: &str, destination: &str, mac: &str) -> serde_json::Value {
    json!({".namespace.name": NS, ".namespace.node.name": node,
           ".namespace.node.srl.network-instance.name": "nico-provisioning-bd",
           "address": mac, "destination": destination, "destination-type": "sub-interface", "type": "learnt"})
}

async fn eql_mock(server: &mut ServerGuard, rows: Vec<serde_json::Value>) -> mockito::Mock {
    server
        .mock("GET", "/core/query/v1/eql")
        .match_query(Matcher::Any)
        .with_status(200)
        .with_header("content-type", "application/json")
        .with_body(json!({"data": rows}).to_string())
        .create_async()
        .await
}

const PORT: &str = "leaf1-ethernet-1-3";
const HOST_MAC: &str = "06:00:00:00:05:01";

#[tokio::test]
async fn capabilities_follow_the_configuration() {
    let mut server = Server::new_async().await;
    token_mock(&mut server, 2).await;
    let plain = eda_against(&server).await;
    let c = plain.capabilities().await.expect("caps");
    assert_eq!(c.adapter, "eda");
    assert!(!c.quarantine_vrf && c.mac_witness && c.lldp_witness && c.storm_control);
    assert!(
        !c.peering && !c.mac_limit && !c.ip_source_guard && !c.dhcp_snooping && !c.isolated_ports
    );
    let configured = eda_with(&server, json!({"witness": "off"}), Some("provisioning")).await;
    let c = configured.capabilities().await.expect("caps");
    assert!(c.quarantine_vrf && !c.mac_witness && !c.lldp_witness);
}

#[tokio::test]
async fn quarantine_moves_the_port_to_the_quarantine_label_and_is_listed_as_none() {
    let mut server = Server::new_async().await;
    token_mock(&mut server, 1).await;
    server
        .mock("GET", iface_path(PORT).as_str())
        .with_status(200)
        .with_body(interface(PORT, Some("nico-x"), "leaf1", "ethernet-1/3").to_string())
        .create_async()
        .await;
    let relabel = server
        .mock("PATCH", iface_path(PORT).as_str())
        .match_header("content-type", "application/json-patch+json")
        .match_body(Matcher::Regex(r#""value":"nico-provisioning""#.into()))
        .with_status(200)
        .with_body("{}")
        .expect(1)
        .create_async()
        .await;
    // Listing: one port in quarantine, one in a tenant VRF this process named.
    server
        .mock("GET", "/apps/interfaces.eda.nokia.com/v1/namespaces/ns/interfaces")
        .match_query(Matcher::UrlEncoded("labelSelector".into(), "nico.io/vpc".into()))
        .with_status(200)
        .with_body(json!({"items": [
            interface(PORT, Some("nico-provisioning"), "leaf1", "ethernet-1/3"),
            interface("leaf2-ethernet-1-3", Some("nico-openai-training"), "leaf2", "ethernet-1/3"),
        ]}).to_string())
        .create_async()
        .await;

    let eda = eda_with(&server, json!({}), Some("provisioning")).await;
    eda.set_port_membership(&PortMembership {
        port: PORT.into(),
        vrf: None,
        previous_vrf: Some("x".into()),
        ..PortMembership::default()
    })
    .await
    .expect("quarantine ok");
    relabel.assert();

    // Names this process has not seen are handed back as EDA spells them
    // (the reconcile learns NICo's spelling from ensure_vrf in the same pass).
    let listed = eda.list_port_memberships().await.expect("list");
    let q = listed
        .iter()
        .find(|m| m.port == PORT)
        .expect("quarantined port listed");
    assert!(q.vrf.is_none(), "quarantine label lists as None: {q:?}");
    let t = listed
        .iter()
        .find(|m| m.port == "leaf2-ethernet-1-3")
        .expect("tenant port listed");
    assert_eq!(t.vrf.as_deref(), Some("nico-openai-training"));
}

#[tokio::test]
async fn witness_mismatch_is_refused_and_writes_nothing() {
    let mut server = Server::new_async().await;
    token_mock(&mut server, 1).await;
    server
        .mock("GET", iface_path(PORT).as_str())
        .with_status(200)
        .with_body(interface(PORT, Some("nico-provisioning"), "leaf1", "ethernet-1/3").to_string())
        .create_async()
        .await;
    // The leaf learned a different host on ethernet-1/3 (and an unrelated one elsewhere).
    eql_mock(
        &mut server,
        vec![
            mac_row("leaf1", "ethernet-1/3.900", "AA:BB:CC:DD:EE:FF"),
            mac_row("leaf1", "ethernet-1/5.900", HOST_MAC),
        ],
    )
    .await;
    let writes = no_writes(&mut server, "/apps/.*").await;

    let eda = eda_with(&server, json!({}), Some("provisioning")).await;
    let err = eda
        .set_port_membership(&PortMembership {
            port: PORT.into(),
            vrf: Some("x".into()),
            previous_vrf: None,
            witnesses: Witnesses {
                expected_macs: vec![HOST_MAC.into()],
                expected_lldp: None,
            },
            contract: PortContract::default(),
        })
        .await
        .expect_err("refused");
    match err {
        FabricError::WitnessMismatch { port, detail } => {
            assert_eq!(port, PORT);
            assert!(
                detail.contains("aa:bb:cc:dd:ee:ff") && detail.contains("ethernet-1/3"),
                "{detail}"
            );
        }
        other => panic!("expected WitnessMismatch, got {other}"),
    }
    for w in writes {
        w.assert();
    }
}

#[tokio::test]
async fn nothing_learned_yet_is_refused_until_the_host_speaks() {
    let mut server = Server::new_async().await;
    token_mock(&mut server, 1).await;
    server
        .mock("GET", iface_path(PORT).as_str())
        .with_status(200)
        .with_body(interface(PORT, None, "leaf1", "ethernet-1/3").to_string())
        .create_async()
        .await;
    eql_mock(&mut server, vec![]).await;
    let eda = eda_against(&server).await;
    let err = eda
        .set_port_membership(&PortMembership {
            port: PORT.into(),
            vrf: Some("x".into()),
            witnesses: Witnesses {
                expected_macs: vec![HOST_MAC.into()],
                expected_lldp: None,
            },
            ..PortMembership::default()
        })
        .await
        .expect_err("refused");
    assert!(matches!(err, FabricError::WitnessMismatch { .. }), "{err}");
    assert!(err.to_string().contains("learned no MAC"), "{err}");
}

#[tokio::test]
async fn witness_match_binds_the_port_and_applies_storm_control() {
    let mut server = Server::new_async().await;
    token_mock(&mut server, 1).await;
    server
        .mock("GET", iface_path(PORT).as_str())
        .with_status(200)
        .with_body(interface(PORT, Some("nico-provisioning"), "leaf1", "ethernet-1/3").to_string())
        .create_async()
        .await;
    eql_mock(
        &mut server,
        vec![mac_row("leaf1", "ethernet-1/3.900", HOST_MAC)],
    )
    .await;
    let relabel = server
        .mock("PATCH", iface_path(PORT).as_str())
        .match_body(Matcher::Regex(
            r#"/metadata/labels/nico.io~1vpc.*"value":"nico-x""#.into(),
        ))
        .with_status(200)
        .with_body("{}")
        .expect(1)
        .create_async()
        .await;
    let storm = server
        .mock("PATCH", iface_path(PORT).as_str())
        .match_body(Matcher::Regex(
            r#"/spec/ethernet/stormControl.*"unit":"BandwidthPercentage""#.into(),
        ))
        .with_status(200)
        .with_body("{}")
        .expect(1)
        .create_async()
        .await;

    let eda = eda_with(&server, json!({}), Some("provisioning")).await;
    let enforced = eda
        .set_port_membership(&PortMembership {
            port: PORT.into(),
            vrf: Some("x".into()),
            previous_vrf: None,
            witnesses: Witnesses {
                expected_macs: vec![HOST_MAC.to_ascii_uppercase()],
                expected_lldp: None,
            },
            contract: PortContract {
                allowed_macs: vec![HOST_MAC.into()],
                storm_control: true,
                ..PortContract::default()
            },
        })
        .await
        .expect("bound");
    assert!(enforced.storm_control, "storm control reported as enforced");
    assert!(
        !enforced.mac_limit && !enforced.ip_source_guard,
        "nothing else claimed"
    );
    relabel.assert();
    storm.assert();
}

#[tokio::test]
async fn witness_policy_log_binds_despite_mismatch() {
    let mut server = Server::new_async().await;
    token_mock(&mut server, 1).await;
    server
        .mock("GET", iface_path(PORT).as_str())
        .with_status(200)
        .with_body(interface(PORT, None, "leaf1", "ethernet-1/3").to_string())
        .create_async()
        .await;
    eql_mock(
        &mut server,
        vec![mac_row("leaf1", "ethernet-1/3.900", "AA:BB:CC:DD:EE:FF")],
    )
    .await;
    let relabel = server
        .mock("PATCH", iface_path(PORT).as_str())
        .with_status(200)
        .with_body("{}")
        .expect_at_least(1)
        .create_async()
        .await;
    let eda = eda_with(&server, json!({"witness": "log"}), None).await;
    eda.set_port_membership(&PortMembership {
        port: PORT.into(),
        vrf: Some("x".into()),
        witnesses: Witnesses {
            expected_macs: vec![HOST_MAC.into()],
            expected_lldp: None,
        },
        ..PortMembership::default()
    })
    .await
    .expect("bound under log policy");
    relabel.assert();
}

#[tokio::test]
async fn lldp_witness_compares_switch_identity_and_port() {
    let mut server = Server::new_async().await;
    token_mock(&mut server, 1).await;
    server
        .mock("GET", iface_path(PORT).as_str())
        .with_status(200)
        .with_body(interface(PORT, None, "leaf1", "ethernet-1/3").to_string())
        .create_async()
        .await;
    // Only the LLDP query runs (no MAC witnesses given): the leaf's chassis id.
    eql_mock(
        &mut server,
        vec![json!({".namespace.node.name": "leaf1", "chassis-id": "02:54:B5:FF:00:11"})],
    )
    .await;
    let eda = eda_against(&server).await;
    let wrong = eda
        .set_port_membership(&PortMembership {
            port: PORT.into(),
            vrf: Some("x".into()),
            witnesses: Witnesses {
                expected_macs: vec![],
                expected_lldp: Some(("leaf2".into(), "ethernet-1/3".into())),
            },
            ..PortMembership::default()
        })
        .await
        .expect_err("host saw a different switch");
    assert!(
        matches!(wrong, FabricError::WitnessMismatch { .. }),
        "{wrong}"
    );
    let wrong_port = eda
        .set_port_membership(&PortMembership {
            port: PORT.into(),
            vrf: Some("x".into()),
            witnesses: Witnesses {
                expected_macs: vec![],
                expected_lldp: Some(("02:54:b5:ff:00:11".into(), "ethernet-1/7".into())),
            },
            ..PortMembership::default()
        })
        .await
        .expect_err("host saw a different port");
    assert!(
        matches!(wrong_port, FabricError::WitnessMismatch { .. }),
        "{wrong_port}"
    );
}
