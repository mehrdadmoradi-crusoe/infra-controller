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

//! The fabric conformance suite, as code.
//!
//! An adapter "supports NICo delegated fabric" when `run` passes against it.
//! The suite exercises only the contract: create, idempotent re-create, attach,
//! list, detach, delete with membership cleanup, garbage-collection input,
//! capability refusal and witness refusal. It runs in CI against the in-memory
//! fake (directly and through the gRPC agent) and, on demand, against a real
//! controller. What it cannot see is packets; that stays with simulators.

use std::sync::Arc;

use crate::{
    Capabilities, FabricError, FabricOperations, HostAttachment, PortContract, PortMembership,
    VrfIntent, Witnesses,
};

/// Names and ports the suite may create on the target. On a real controller
/// pick values that do not collide with anything else and that the port
/// objects exist for.
#[derive(Debug, Clone)]
pub struct Fixture {
    pub vrf_a: String,
    pub vrf_b: String,
    pub port_a: String,
    pub port_b: String,
    pub subnet_a: (String, String),
    pub subnet_b: (String, String),
    pub vlan_a: u16,
    pub vlan_b: u16,
    pub vni_a: u32,
    pub vni_b: u32,
    /// MACs the suite claims for the host on `port_a` when it tests witnesses.
    pub host_a_macs: Vec<String>,
}

impl Default for Fixture {
    fn default() -> Self {
        Self {
            vrf_a: "conf-a".to_string(),
            vrf_b: "conf-b".to_string(),
            port_a: "leaf1-ethernet-1-3".to_string(),
            port_b: "leaf2-ethernet-1-3".to_string(),
            subnet_a: ("10.201.1.0/24".to_string(), "10.201.1.1".to_string()),
            subnet_b: ("10.201.2.0/24".to_string(), "10.201.2.1".to_string()),
            vlan_a: 901,
            vlan_b: 902,
            vni_a: 2024901,
            vni_b: 2024902,
            host_a_macs: vec!["06:00:00:00:05:01".to_string()],
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Outcome {
    pub check: &'static str,
    pub passed: bool,
    pub detail: String,
}

/// Number of outcomes a full run records. Skipped witness checks still record
/// an outcome, so this is constant across adapters.
pub const CHECK_COUNT: usize = 13;

#[derive(Debug, Default)]
pub struct Report {
    pub outcomes: Vec<Outcome>,
}

impl Report {
    pub fn passed(&self) -> bool {
        self.outcomes.iter().all(|o| o.passed)
    }
    pub fn failures(&self) -> Vec<&Outcome> {
        self.outcomes.iter().filter(|o| !o.passed).collect()
    }
    fn record(&mut self, check: &'static str, r: Result<String, String>) {
        self.outcomes.push(match r {
            Ok(detail) => Outcome {
                check,
                passed: true,
                detail,
            },
            Err(detail) => Outcome {
                check,
                passed: false,
                detail,
            },
        });
    }
}

fn intent(name: &str, vpc_id: &str, subnet: &(String, String), vlan: u16, vni: u32) -> VrfIntent {
    VrfIntent {
        nico_vpc_id: vpc_id.to_string(),
        name: name.to_string(),
        subnet_cidr: subnet.0.clone(),
        vlan,
        gateway: subnet.1.clone(),
        vni: Some(vni),
        dhcp_range: None,
    }
}

async fn has_vrf(f: &dyn FabricOperations, vpc_id: &str) -> Result<bool, FabricError> {
    Ok(f.list_vrfs().await?.iter().any(|(_, id)| id == vpc_id))
}

/// Run every check against `fabric`. Never panics on a failed check; the
/// report says which failed and why. Leaves the fixture VRFs deleted.
pub async fn run(fabric: Arc<dyn FabricOperations>, fx: &Fixture) -> Report {
    let mut rep = Report::default();
    let f: &dyn FabricOperations = fabric.as_ref();
    let id_a = format!("vpc-{}", fx.vrf_a);
    let id_b = format!("vpc-{}", fx.vrf_b);
    let ia = intent(&fx.vrf_a, &id_a, &fx.subnet_a, fx.vlan_a, fx.vni_a);
    let ib = intent(&fx.vrf_b, &id_b, &fx.subnet_b, fx.vlan_b, fx.vni_b);

    // Start clean so a previous failed run cannot mask results.
    let _ = f.delete_vrf(&fx.vrf_a).await;
    let _ = f.delete_vrf(&fx.vrf_b).await;

    let caps: Capabilities = match f.capabilities().await {
        Ok(c) => {
            rep.record(
                "capabilities",
                Ok(format!("{} v{}", c.adapter, c.contract_version)),
            );
            c
        }
        Err(e) => {
            rep.record("capabilities", Err(e.to_string()));
            Capabilities::default()
        }
    };

    // 1. create
    rep.record(
        "create",
        match f.ensure_vrf(&ia).await {
            Ok(()) => match has_vrf(f, &id_a).await {
                Ok(true) => Ok("vrf listed with its NICo id".to_string()),
                Ok(false) => {
                    Err("ensure_vrf ok but list_vrfs does not carry the NICo id".to_string())
                }
                Err(e) => Err(e.to_string()),
            },
            Err(e) => Err(e.to_string()),
        },
    );

    // 2. idempotent re-create and status
    rep.record(
        "recreate_idempotent",
        match f.ensure_vrf(&ia).await {
            Ok(()) => match f.get_vrf_status(&fx.vrf_a).await {
                Ok(Some(_)) => Ok("second ensure ok, status present".to_string()),
                Ok(None) => Err("status absent after ensure".to_string()),
                Err(e) => Err(e.to_string()),
            },
            Err(e) => Err(e.to_string()),
        },
    );

    // 3. attach (legacy path) and list
    rep.record(
        "attach",
        match f
            .attach_host(&HostAttachment {
                vpc_name: fx.vrf_a.clone(),
                connection: fx.port_a.clone(),
            })
            .await
        {
            Ok(()) => match f.list_attachments(&fx.vrf_a).await {
                Ok(ports) if ports.contains(&fx.port_a) => Ok(format!("{} bound", fx.port_a)),
                Ok(ports) => Err(format!("port not listed after attach: {ports:?}")),
                Err(e) => Err(e.to_string()),
            },
            Err(e) => Err(e.to_string()),
        },
    );

    // 4. attach is idempotent
    rep.record(
        "attach_idempotent",
        match f
            .attach_host(&HostAttachment {
                vpc_name: fx.vrf_a.clone(),
                connection: fx.port_a.clone(),
            })
            .await
        {
            Ok(()) => match f.list_attachments(&fx.vrf_a).await {
                Ok(ports) if ports.iter().filter(|p| **p == fx.port_a).count() == 1 => {
                    Ok("bound exactly once".to_string())
                }
                Ok(ports) => Err(format!("duplicate or missing binding: {ports:?}")),
                Err(e) => Err(e.to_string()),
            },
            Err(e) => Err(e.to_string()),
        },
    );

    // 5. second VRF, isolation at the contract level: port_b in b only
    let _ = f.ensure_vrf(&ib).await;
    rep.record(
        "isolation_listing",
        match f
            .attach_host(&HostAttachment {
                vpc_name: fx.vrf_b.clone(),
                connection: fx.port_b.clone(),
            })
            .await
        {
            Ok(()) => match (
                f.list_attachments(&fx.vrf_a).await,
                f.list_attachments(&fx.vrf_b).await,
            ) {
                (Ok(a), Ok(b))
                    if !a.contains(&fx.port_b)
                        && b.contains(&fx.port_b)
                        && !b.contains(&fx.port_a) =>
                {
                    Ok("each port listed under its own VRF only".to_string())
                }
                (Ok(a), Ok(b)) => Err(format!("cross-listing: a={a:?} b={b:?}")),
                (Err(e), _) | (_, Err(e)) => Err(e.to_string()),
            },
            Err(e) => Err(e.to_string()),
        },
    );

    // 6. peering: honored when declared, refused with Unsupported when not
    rep.record(
        "peering_matches_capability",
        match f.peer_vpcs(&fx.vrf_a, &fx.vrf_b).await {
            Ok(()) if caps.peering => Ok("peered".to_string()),
            Ok(()) => Err("peer_vpcs succeeded but capabilities.peering is false".to_string()),
            Err(FabricError::Unsupported(_)) if !caps.peering => {
                Ok("declared unsupported, refused".to_string())
            }
            Err(e) => Err(format!("peering: {e}")),
        },
    );

    // 7. detach via membership to quarantine
    rep.record(
        "detach_to_quarantine",
        match f
            .set_port_membership(&PortMembership {
                port: fx.port_a.clone(),
                vrf: None,
                previous_vrf: Some(fx.vrf_a.clone()),
                ..PortMembership::default()
            })
            .await
        {
            Ok(_) => match f.list_attachments(&fx.vrf_a).await {
                Ok(ports) if !ports.contains(&fx.port_a) => Ok("port left the VRF".to_string()),
                Ok(ports) => Err(format!("port still listed: {ports:?}")),
                Err(e) => Err(e.to_string()),
            },
            Err(e) => Err(e.to_string()),
        },
    );

    // 8. detach is idempotent (a second move to quarantine is a no-op)
    rep.record(
        "detach_idempotent",
        match f
            .set_port_membership(&PortMembership {
                port: fx.port_a.clone(),
                vrf: None,
                previous_vrf: Some(fx.vrf_a.clone()),
                ..PortMembership::default()
            })
            .await
        {
            Ok(_) => Ok("second detach ok".to_string()),
            Err(e) => Err(e.to_string()),
        },
    );

    // 9. witnesses: only meaningful when the adapter declares them
    if caps.mac_witness {
        let bad = PortMembership {
            port: fx.port_a.clone(),
            vrf: Some(fx.vrf_a.clone()),
            previous_vrf: None,
            witnesses: Witnesses {
                expected_macs: vec!["02:ff:ff:ff:ff:ff".to_string()],
                expected_lldp: None,
            },
            contract: PortContract::default(),
        };
        rep.record(
            "witness_mismatch_refused",
            match f.set_port_membership(&bad).await {
                Err(FabricError::WitnessMismatch { .. }) => {
                    Ok("refused with WitnessMismatch".to_string())
                }
                Ok(_) => Err("attach with a wrong MAC witness was accepted".to_string()),
                Err(e) => Err(format!("wrong error kind: {e}")),
            },
        );
        let good = PortMembership {
            port: fx.port_a.clone(),
            vrf: Some(fx.vrf_a.clone()),
            previous_vrf: None,
            witnesses: Witnesses {
                expected_macs: fx.host_a_macs.clone(),
                expected_lldp: None,
            },
            contract: PortContract {
                allowed_macs: fx.host_a_macs.clone(),
                ..PortContract::default()
            },
        };
        rep.record(
            "witness_match_attaches_and_enforces",
            match f.set_port_membership(&good).await {
                Ok(enf) if enf.mac_limit || !caps.mac_limit => {
                    Ok(format!("attached, enforcement {enf:?}"))
                }
                Ok(enf) => Err(format!("mac_limit declared but not enforced: {enf:?}")),
                Err(e) => Err(e.to_string()),
            },
        );
    } else {
        rep.record(
            "witness_mismatch_refused",
            Ok("skipped: adapter declares no mac_witness".to_string()),
        );
        rep.record(
            "witness_match_attaches_and_enforces",
            Ok("skipped: adapter declares no mac_witness".to_string()),
        );
        let _ = f
            .attach_host(&HostAttachment {
                vpc_name: fx.vrf_a.clone(),
                connection: fx.port_a.clone(),
            })
            .await;
    }

    // 10. delete removes the VRF and its memberships; list no longer carries it
    rep.record(
        "delete_cleans_memberships",
        match f.delete_vrf(&fx.vrf_a).await {
            Ok(()) => match (has_vrf(f, &id_a).await, f.list_attachments(&fx.vrf_a).await) {
                (Ok(false), Ok(ports)) if ports.is_empty() => {
                    Ok("vrf gone, no memberships".to_string())
                }
                (Ok(true), _) => Err("vrf still listed after delete".to_string()),
                (_, Ok(ports)) => Err(format!("memberships survive delete: {ports:?}")),
                (Err(e), _) | (_, Err(e)) => Err(e.to_string()),
            },
            Err(e) => Err(e.to_string()),
        },
    );

    // 11. delete is idempotent
    rep.record(
        "delete_idempotent",
        match f.delete_vrf(&fx.vrf_a).await {
            Ok(()) => Ok("second delete ok".to_string()),
            Err(e) => Err(e.to_string()),
        },
    );

    let _ = f.delete_vrf(&fx.vrf_b).await;
    rep
}
