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

//! An in-memory fabric that implements the contract's invariants exactly.
//!
//! It is the reference every adapter is measured against in the conformance
//! suite, the backend the fabric-agent serves in CI, and a fault-injection
//! harness for the reconcile: every call can be made to fail, stall or lie.
//! It models VRFs, ports and memberships; it does not model packets. What a
//! fake cannot prove (rendering onto a NOS, DHCP relay, traffic) stays with
//! the controller simulators.

use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;

use crate::{
    Capabilities, Enforcement, FabricError, FabricOperations, HostAttachment, PortMembership,
    VrfIntent,
};

/// A VRF as the fake holds it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FakeVrf {
    pub intent: VrfIntentSnapshot,
    pub peers: BTreeSet<String>,
    /// How many times ensure_vrf wrote something for this VRF (a converged
    /// ensure is a read, not a write).
    pub writes: u32,
}

/// The comparable part of a `VrfIntent`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VrfIntentSnapshot {
    pub nico_vpc_id: String,
    pub subnet_cidr: String,
    pub vlan: u16,
    pub gateway: String,
    pub vni: Option<u32>,
}

impl From<&VrfIntent> for VrfIntentSnapshot {
    fn from(i: &VrfIntent) -> Self {
        Self {
            nico_vpc_id: i.nico_vpc_id.clone(),
            subnet_cidr: i.subnet_cidr.clone(),
            vlan: i.vlan,
            gateway: i.gateway.clone(),
            vni: i.vni,
        }
    }
}

/// What the "switch" observes on a port, for witness evaluation.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Observed {
    pub macs: BTreeSet<String>,
    pub lldp: Option<(String, String)>,
}

/// One recorded call, for assertions on idempotency and ordering.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Call {
    EnsureVrf(String),
    DeleteVrf(String),
    ListVrfs,
    ListAttachments(String),
    ListPortMemberships,
    SetPortMembership { port: String, vrf: Option<String> },
    PeerVrfs(String, String),
    GetVrfStatus(String),
    Capabilities,
}

#[derive(Debug, Default)]
struct State {
    vrfs: BTreeMap<String, FakeVrf>,
    /// port -> VRF; a port absent from the map is in quarantine.
    memberships: BTreeMap<String, String>,
    /// Ports the fabric has explicitly placed in quarantine (so a listing can
    /// report them and a converged reconcile stays quiet).
    quarantined: BTreeSet<String>,
    enforced: BTreeMap<String, Enforcement>,
    observed: BTreeMap<String, Observed>,
    calls: Vec<Call>,
    /// Errors to return, consumed one per call.
    fail_queue: VecDeque<FabricError>,
    latency: Duration,
    capabilities: Capabilities,
}

/// Cloneable handle to a shared in-memory fabric.
#[derive(Clone, Default)]
pub struct FakeFabric {
    state: Arc<Mutex<State>>,
}

impl std::fmt::Debug for FakeFabric {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let s = self.state.lock().expect("fake fabric poisoned");
        f.debug_struct("FakeFabric")
            .field("vrfs", &s.vrfs.len())
            .field("memberships", &s.memberships.len())
            .finish()
    }
}

impl FakeFabric {
    /// A fake that declares every capability and enforces every contract.
    pub fn new() -> Self {
        let mut caps = Capabilities {
            contract_version: "1.0.0".to_string(),
            adapter: "fake".to_string(),
            peering: true,
            mac_limit: true,
            ip_source_guard: true,
            dhcp_snooping: true,
            storm_control: true,
            isolated_ports: true,
            anycast_gateway: true,
            vlan_translation: false,
            events: false,
            lldp_witness: true,
            mac_witness: true,
            quarantine_vrf: true,
        };
        caps.events = false;
        let fake = Self::default();
        fake.state
            .lock()
            .expect("fake fabric poisoned")
            .capabilities = caps;
        fake
    }

    /// Override what the fake declares (for example `peering = false`).
    pub fn with_capabilities(self, caps: Capabilities) -> Self {
        self.state
            .lock()
            .expect("fake fabric poisoned")
            .capabilities = caps;
        self
    }

    /// What the switch "sees" on a port; witnesses are checked against this.
    pub fn observe(&self, port: &str, macs: &[&str], lldp: Option<(&str, &str)>) {
        let mut s = self.state.lock().expect("fake fabric poisoned");
        s.observed.insert(
            port.to_string(),
            Observed {
                macs: macs.iter().map(|m| m.to_ascii_lowercase()).collect(),
                lldp: lldp.map(|(c, p)| (c.to_string(), p.to_string())),
            },
        );
    }

    /// Make the next `n` calls fail with `make()`.
    pub fn fail_next(&self, n: usize, make: impl Fn() -> FabricError) {
        let mut s = self.state.lock().expect("fake fabric poisoned");
        for _ in 0..n {
            s.fail_queue.push_back(make());
        }
    }

    pub fn set_latency(&self, d: Duration) {
        self.state.lock().expect("fake fabric poisoned").latency = d;
    }

    pub fn vrfs(&self) -> BTreeMap<String, FakeVrf> {
        self.state
            .lock()
            .expect("fake fabric poisoned")
            .vrfs
            .clone()
    }

    /// port -> VRF for every port bound to a tenant VRF.
    pub fn memberships(&self) -> BTreeMap<String, String> {
        self.state
            .lock()
            .expect("fake fabric poisoned")
            .memberships
            .clone()
    }

    /// Ports explicitly moved into quarantine (by detach, quarantine or delete).
    pub fn quarantined(&self) -> BTreeSet<String> {
        self.state
            .lock()
            .expect("fake fabric poisoned")
            .quarantined
            .clone()
    }

    pub fn calls(&self) -> Vec<Call> {
        self.state
            .lock()
            .expect("fake fabric poisoned")
            .calls
            .clone()
    }

    pub fn clear_calls(&self) {
        self.state
            .lock()
            .expect("fake fabric poisoned")
            .calls
            .clear();
    }

    /// Put the fake back to empty, keeping capabilities and observations.
    pub fn reset(&self) {
        let mut s = self.state.lock().expect("fake fabric poisoned");
        s.vrfs.clear();
        s.memberships.clear();
        s.quarantined.clear();
        s.enforced.clear();
        s.calls.clear();
        s.fail_queue.clear();
    }

    async fn enter(&self, call: Call) -> Result<(), FabricError> {
        let (latency, fail) = {
            let mut s = self.state.lock().expect("fake fabric poisoned");
            s.calls.push(call);
            (s.latency, s.fail_queue.pop_front())
        };
        if !latency.is_zero() {
            tokio::time::sleep(latency).await;
        }
        match fail {
            Some(e) => Err(e),
            None => Ok(()),
        }
    }

    fn check_witnesses(s: &State, m: &PortMembership) -> Result<(), FabricError> {
        let obs = s.observed.get(&m.port).cloned().unwrap_or_default();
        if !m.witnesses.expected_macs.is_empty() {
            let expected: BTreeSet<String> = m
                .witnesses
                .expected_macs
                .iter()
                .map(|x| x.to_ascii_lowercase())
                .collect();
            if expected.intersection(&obs.macs).next().is_none() {
                return Err(FabricError::WitnessMismatch {
                    port: m.port.clone(),
                    detail: format!(
                        "expected one of {:?}, switch learned {:?}",
                        expected, obs.macs
                    ),
                });
            }
        }
        if let Some(exp) = &m.witnesses.expected_lldp
            && obs.lldp.as_ref() != Some(exp)
        {
            return Err(FabricError::WitnessMismatch {
                port: m.port.clone(),
                detail: format!("expected lldp {:?}, switch sees {:?}", exp, obs.lldp),
            });
        }
        Ok(())
    }
}

#[async_trait]
impl FabricOperations for FakeFabric {
    async fn ensure_vrf(&self, intent: &VrfIntent) -> Result<(), FabricError> {
        self.enter(Call::EnsureVrf(intent.name.clone())).await?;
        let mut s = self.state.lock().expect("fake fabric poisoned");
        let snap = VrfIntentSnapshot::from(intent);
        match s.vrfs.get_mut(&intent.name) {
            Some(v) if v.intent == snap => {}
            Some(v) => {
                v.intent = snap;
                v.writes += 1;
            }
            None => {
                s.vrfs.insert(
                    intent.name.clone(),
                    FakeVrf {
                        intent: snap,
                        peers: BTreeSet::new(),
                        writes: 1,
                    },
                );
            }
        }
        Ok(())
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
        self.enter(Call::ListAttachments(vpc_name.to_string()))
            .await?;
        let s = self.state.lock().expect("fake fabric poisoned");
        Ok(s.memberships
            .iter()
            .filter(|(_, v)| v.as_str() == vpc_name)
            .map(|(p, _)| p.clone())
            .collect())
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
        self.enter(Call::PeerVrfs(a.to_string(), b.to_string()))
            .await?;
        let mut s = self.state.lock().expect("fake fabric poisoned");
        if !s.capabilities.peering {
            return Err(FabricError::Unsupported("peering".to_string()));
        }
        if !s.vrfs.contains_key(a) || !s.vrfs.contains_key(b) {
            return Err(FabricError::Invalid(format!(
                "peer_vpcs: unknown vrf {a} or {b}"
            )));
        }
        s.vrfs
            .get_mut(a)
            .expect("checked")
            .peers
            .insert(b.to_string());
        s.vrfs
            .get_mut(b)
            .expect("checked")
            .peers
            .insert(a.to_string());
        Ok(())
    }

    async fn get_vrf_status(
        &self,
        vpc_name: &str,
    ) -> Result<Option<serde_json::Value>, FabricError> {
        self.enter(Call::GetVrfStatus(vpc_name.to_string())).await?;
        let s = self.state.lock().expect("fake fabric poisoned");
        Ok(s.vrfs.get(vpc_name).map(|v| {
            let nodes: BTreeSet<String> = s
                .memberships
                .iter()
                .filter(|(_, vrf)| vrf.as_str() == vpc_name)
                .map(|(p, _)| p.split('-').next().unwrap_or(p).to_string())
                .collect();
            serde_json::json!({
                "programmed": true,
                "nodes": nodes,
                "operationalState": "Up",
                "vni": v.intent.vni,
            })
        }))
    }

    async fn list_vrfs(&self) -> Result<Vec<(String, String)>, FabricError> {
        self.enter(Call::ListVrfs).await?;
        let s = self.state.lock().expect("fake fabric poisoned");
        Ok(s.vrfs
            .iter()
            .map(|(n, v)| (n.clone(), v.intent.nico_vpc_id.clone()))
            .collect())
    }

    async fn delete_vrf(&self, vpc_name: &str) -> Result<(), FabricError> {
        self.enter(Call::DeleteVrf(vpc_name.to_string())).await?;
        let mut s = self.state.lock().expect("fake fabric poisoned");
        // Deleting a VRF returns its ports to quarantine and drops its peerings.
        let freed: Vec<String> = s
            .memberships
            .iter()
            .filter(|(_, v)| v.as_str() == vpc_name)
            .map(|(p, _)| p.clone())
            .collect();
        s.memberships.retain(|_, v| v.as_str() != vpc_name);
        s.quarantined.extend(freed);
        for v in s.vrfs.values_mut() {
            v.peers.remove(vpc_name);
        }
        s.vrfs.remove(vpc_name);
        Ok(())
    }

    async fn capabilities(&self) -> Result<Capabilities, FabricError> {
        self.enter(Call::Capabilities).await?;
        Ok(self
            .state
            .lock()
            .expect("fake fabric poisoned")
            .capabilities
            .clone())
    }

    async fn set_port_membership(&self, m: &PortMembership) -> Result<Enforcement, FabricError> {
        self.enter(Call::SetPortMembership {
            port: m.port.clone(),
            vrf: m.vrf.clone(),
        })
        .await?;
        let mut s = self.state.lock().expect("fake fabric poisoned");
        match &m.vrf {
            Some(vrf) => {
                if !s.vrfs.contains_key(vrf) {
                    return Err(FabricError::Invalid(format!(
                        "set_port_membership: vrf {vrf} does not exist"
                    )));
                }
                Self::check_witnesses(&s, m)?;
                s.memberships.insert(m.port.clone(), vrf.clone());
                s.quarantined.remove(&m.port);
                let enforced = Enforcement {
                    mac_limit: !m.contract.allowed_macs.is_empty(),
                    ip_source_guard: !m.contract.allowed_ips.is_empty(),
                    dhcp_snooping: m.contract.dhcp_snooping,
                    storm_control: m.contract.storm_control,
                    isolated_port: m.contract.isolated_port,
                };
                s.enforced.insert(m.port.clone(), enforced.clone());
                Ok(enforced)
            }
            None => {
                // Into quarantine: never refused, idempotent.
                s.memberships.remove(&m.port);
                s.enforced.remove(&m.port);
                s.quarantined.insert(m.port.clone());
                Ok(Enforcement::default())
            }
        }
    }

    async fn list_port_memberships(&self) -> Result<Vec<PortMembership>, FabricError> {
        self.enter(Call::ListPortMemberships).await?;
        let s = self.state.lock().expect("fake fabric poisoned");
        let mut out: Vec<PortMembership> = s
            .memberships
            .iter()
            .map(|(p, v)| PortMembership {
                port: p.clone(),
                vrf: Some(v.clone()),
                ..PortMembership::default()
            })
            .collect();
        out.extend(s.quarantined.iter().map(|p| PortMembership {
            port: p.clone(),
            ..PortMembership::default()
        }));
        Ok(out)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::conformance::{self, Fixture};

    #[tokio::test]
    async fn fake_passes_the_conformance_suite() {
        let fake = FakeFabric::new();
        fake.observe("leaf1-ethernet-1-3", &["06:00:00:00:05:01"], None);
        let report = conformance::run(Arc::new(fake), &Fixture::default()).await;
        assert!(report.passed(), "{:#?}", report.failures());
        assert_eq!(
            report.outcomes.len(),
            conformance::CHECK_COUNT,
            "every check ran"
        );
    }

    #[tokio::test]
    async fn injected_failures_surface_once_and_state_is_untouched() {
        let fake = FakeFabric::new();
        fake.fail_next(1, || FabricError::Agent("injected".into()));
        let intent = VrfIntent {
            nico_vpc_id: "vpc-1".into(),
            name: "one".into(),
            subnet_cidr: "10.1.0.0/24".into(),
            vlan: 101,
            gateway: "10.1.0.1".into(),
            vni: Some(1),
            dhcp_range: None,
        };
        assert!(fake.ensure_vrf(&intent).await.is_err());
        assert!(
            fake.vrfs().is_empty(),
            "a failed call must not partially apply"
        );
        fake.ensure_vrf(&intent)
            .await
            .expect("second call succeeds");
        fake.ensure_vrf(&intent)
            .await
            .expect("converged ensure is a no-op");
        assert_eq!(
            fake.vrfs()["one"].writes,
            1,
            "converged ensure does not write"
        );
    }

    #[tokio::test]
    async fn delete_returns_ports_to_quarantine_and_drops_peerings() {
        let fake = FakeFabric::new();
        for (n, id) in [("a", "vpc-a"), ("b", "vpc-b")] {
            fake.ensure_vrf(&VrfIntent {
                nico_vpc_id: id.into(),
                name: n.into(),
                subnet_cidr: "10.2.0.0/24".into(),
                vlan: 102,
                gateway: "10.2.0.1".into(),
                vni: None,
                dhcp_range: None,
            })
            .await
            .unwrap();
        }
        fake.attach_host(&HostAttachment {
            vpc_name: "a".into(),
            connection: "p1".into(),
        })
        .await
        .unwrap();
        fake.peer_vpcs("a", "b").await.unwrap();
        fake.delete_vrf("a").await.unwrap();
        assert!(!fake.memberships().contains_key("p1"));
        assert!(
            fake.vrfs()["b"].peers.is_empty(),
            "peering to a deleted VRF is gone"
        );
    }
}
