//! Nokia EDA backend for the delegated ToR-VRF model.
//!
//! Second implementation of [`FabricOperations`], behind the same six-call
//! interface as the Hedgehog backend, to prove the model is controller-neutral.
//!
//! EDA exposes its own Kubernetes-style REST API (`/apps/<group>/<version>/
//! namespaces/<ns>/<resource>`), authenticated with a Keycloak bearer token.
//! Objects written straight to the cluster's Kubernetes API are *not* imported
//! by EDA's config engine for these kinds ("k8s import disabled"), so this
//! backend talks to the EDA API server and every write becomes an EDA
//! transaction that the engine renders into SR Linux configuration.
//!
//! Mapping of one NICo isolation domain (VPC) onto EDA's stock services app:
//!
//! | NICo intent            | EDA resource(s)                                      |
//! |------------------------|------------------------------------------------------|
//! | VRF                    | `Router` (EVPN-VXLAN IP-VRF, NICo's VNI as L3 VNI)    |
//! | subnet + gateway       | `BridgeDomain` + `IRBInterface` (anycast gateway)    |
//! | VLAN on host ports     | `VLAN` selecting interfaces labelled for this VPC    |
//! | host attach            | label `nico.io/vpc=<vrf>` on the host's `Interface`  |
//! | delete                 | remove the label, delete VLAN/IRB/BridgeDomain/Router |
//!
//! The one place this touches a fabric-owned object is the label on `Interface`
//! (metadata only, never `spec`): EDA's `VLAN` binds ports through label
//! selectors, so a per-VPC label is the idiomatic hook. That is a decision to
//! confirm with the network team.
//!
//! VPC-to-VPC peering is not implemented here yet: EDA models VRF route leaking
//! through routing policies, not as a permit list between two VRFs. `peer_vpcs`
//! returns an explicit error so the reconcile logs it and carries on.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::{Duration, Instant};

use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use tokio::sync::Mutex;

use crate::{FabricConfig, FabricError, FabricOperations, HostAttachment, VrfIntent};

const SERVICES_GV: &str = "services.eda.nokia.com/v2";
const INTERFACES_GV: &str = "interfaces.eda.nokia.com/v1";

/// Label NICo places on a host's EDA `Interface` to bind it into a VRF's VLAN.
pub const VPC_LABEL: &str = "nico.io/vpc";
const NICO_ID_LABEL: &str = "nico.io/vpc-id";
const NICO_VNI_LABEL: &str = "nico.io/vni";

/// Allocation pools the derived objects draw from (the services app defaults).
const VNI_POOL: &str = "vni-pool";
const EVI_POOL: &str = "evi-pool";
const TUNNEL_INDEX_POOL: &str = "tunnel-index-pool";

/// How to reach the EDA API. Secrets are read from the environment so they never
/// sit in the site TOML: `NICO_EDA_PASSWORD` and `NICO_EDA_CLIENT_SECRET`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EdaConfig {
    /// Base URL of the EDA API server, e.g. `https://eda-api.eda-system.svc`.
    pub api_url: String,
    /// Keycloak user NICo authenticates as.
    pub username: String,
    /// Keycloak realm and client used for the password grant.
    #[serde(default = "EdaConfig::default_realm")]
    pub realm: String,
    #[serde(default = "EdaConfig::default_client_id")]
    pub client_id: String,
    /// Accept the EDA API's certificate without verification (lab use only).
    #[serde(default)]
    pub insecure_skip_tls_verify: bool,
}

impl EdaConfig {
    fn default_realm() -> String {
        "eda".to_string()
    }
    fn default_client_id() -> String {
        "eda".to_string()
    }
}

pub const PASSWORD_ENV: &str = "NICO_EDA_PASSWORD";
pub const CLIENT_SECRET_ENV: &str = "NICO_EDA_CLIENT_SECRET";

#[derive(Clone)]
pub struct EdaFabric {
    http: reqwest::Client,
    cfg: EdaConfig,
    namespace: String,
    password: String,
    client_secret: String,
    token: Arc<Mutex<Option<(String, Instant)>>>,
}

impl std::fmt::Debug for EdaFabric {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("EdaFabric")
            .field("api_url", &self.cfg.api_url)
            .field("namespace", &self.namespace)
            .field("username", &self.cfg.username)
            .finish()
    }
}

#[derive(Deserialize)]
struct TokenResponse {
    access_token: String,
    #[serde(default)]
    expires_in: Option<u64>,
}

impl EdaFabric {
    /// Build from the site config; needs `[fabric.eda]` plus the two env secrets.
    pub async fn try_default(cfg: &FabricConfig) -> Result<Self, FabricError> {
        let eda = cfg.eda.clone().ok_or_else(|| {
            FabricError::Invalid("[fabric.eda] is required for backend = \"eda\"".into())
        })?;
        let password = std::env::var(PASSWORD_ENV)
            .map_err(|_| FabricError::Invalid(format!("{PASSWORD_ENV} is not set")))?;
        let client_secret = std::env::var(CLIENT_SECRET_ENV)
            .map_err(|_| FabricError::Invalid(format!("{CLIENT_SECRET_ENV} is not set")))?;
        let http = reqwest::Client::builder()
            .danger_accept_invalid_certs(eda.insecure_skip_tls_verify)
            .timeout(Duration::from_secs(30))
            .build()?;
        let me = Self {
            http,
            cfg: eda,
            namespace: cfg.namespace.clone(),
            password,
            client_secret,
            token: Arc::new(Mutex::new(None)),
        };
        // Probe EDA once so a misconfiguration shows up in the startup log, but
        // never let the fabric controller keep the control plane from starting:
        // the reconcile is level-triggered and converges once EDA is reachable.
        // (Verified the hard way: a stale tunnel to EDA crash-looped nico-api.)
        if let Err(e) = me.token(true).await {
            tracing::warn!(
                api_url = %me.cfg.api_url,
                error = %e,
                "fabric(eda): EDA API not reachable at startup; continuing, reconcile will retry"
            );
        }
        Ok(me)
    }

    /// Name shared by the Router, BridgeDomain, IRBInterface and VLAN derived for
    /// one NICo VPC; also used as the interface label value (≤ 63 chars).
    pub fn eda_name(nico_name: &str) -> String {
        if nico_name.starts_with("nico-") {
            return nico_name.to_string();
        }
        let cleaned: String = nico_name
            .chars()
            .map(|c| {
                if c.is_ascii_alphanumeric() || c == '-' {
                    c.to_ascii_lowercase()
                } else {
                    '-'
                }
            })
            .collect();
        let mut name = format!("nico-{}", cleaned.trim_matches('-'));
        name.truncate(63);
        name.trim_end_matches('-').to_string()
    }

    async fn token(&self, force: bool) -> Result<String, FabricError> {
        let mut guard = self.token.lock().await;
        if !force
            && let Some((t, exp)) = guard.as_ref()
            && Instant::now() < *exp
        {
            return Ok(t.clone());
        }
        let url = format!(
            "{}/core/httpproxy/v1/keycloak/realms/{}/protocol/openid-connect/token",
            self.cfg.api_url.trim_end_matches('/'),
            self.cfg.realm
        );
        let resp = self
            .http
            .post(&url)
            .form(&[
                ("grant_type", "password"),
                ("scope", "openid"),
                ("client_id", self.cfg.client_id.as_str()),
                ("client_secret", self.client_secret.as_str()),
                ("username", self.cfg.username.as_str()),
                ("password", self.password.as_str()),
            ])
            .send()
            .await?;
        if !resp.status().is_success() {
            let s = resp.status();
            let body = resp.text().await.unwrap_or_default();
            return Err(FabricError::Eda(format!(
                "token request failed: {s}: {body}"
            )));
        }
        let tr: TokenResponse = resp.json().await?;
        // Refresh a minute early; EDA tokens are short-lived.
        let ttl = tr.expires_in.unwrap_or(300).saturating_sub(60).max(30);
        *guard = Some((
            tr.access_token.clone(),
            Instant::now() + Duration::from_secs(ttl),
        ));
        Ok(tr.access_token)
    }

    fn url(&self, gv: &str, plural: &str, name: Option<&str>) -> String {
        let base = format!(
            "{}/apps/{gv}/namespaces/{}/{plural}",
            self.cfg.api_url.trim_end_matches('/'),
            self.namespace
        );
        match name {
            Some(n) => format!("{base}/{n}"),
            None => base,
        }
    }

    /// Send with a bearer token; on 401 refresh the token once and retry.
    async fn send(
        &self,
        build: impl Fn() -> reqwest::RequestBuilder,
    ) -> Result<reqwest::Response, FabricError> {
        let tok = self.token(false).await?;
        let resp = build().bearer_auth(&tok).send().await?;
        if resp.status() == reqwest::StatusCode::UNAUTHORIZED {
            let tok = self.token(true).await?;
            return Ok(build().bearer_auth(&tok).send().await?);
        }
        Ok(resp)
    }

    async fn get(
        &self,
        gv: &str,
        plural: &str,
        name: &str,
    ) -> Result<Option<serde_json::Value>, FabricError> {
        let url = self.url(gv, plural, Some(name));
        let resp = self.send(|| self.http.get(&url)).await?;
        match resp.status() {
            reqwest::StatusCode::NOT_FOUND => Ok(None),
            s if s.is_success() => Ok(Some(resp.json().await?)),
            s => Err(FabricError::Eda(format!(
                "GET {url}: {s}: {}",
                resp.text().await.unwrap_or_default()
            ))),
        }
    }

    /// Create or replace one object. EDA has no server-side apply: POST if the
    /// object is absent, PUT (full replace) if present. Both are one transaction.
    async fn upsert(
        &self,
        gv: &str,
        kind: &str,
        plural: &str,
        name: &str,
        labels: &BTreeMap<String, String>,
        spec: serde_json::Value,
    ) -> Result<(), FabricError> {
        let body = serde_json::json!({
            "apiVersion": gv,
            "kind": kind,
            "metadata": { "name": name, "namespace": self.namespace, "labels": labels },
            "spec": spec,
        });
        let current = self.get(gv, plural, name).await?;
        // Every PUT is an EDA transaction, so skip it when nothing would change:
        // the level-triggered reconcile calls this every interval.
        if let Some(cur) = &current {
            // EDA fills in server-side defaults on read (e.g. `ipMTU` on an
            // IRBInterface), so compare only the fields NICo actually sets:
            // a strict equality would PUT on every pass and never converge.
            let same_spec = match (
                cur.get("spec").and_then(|s| s.as_object()),
                spec.as_object(),
            ) {
                (Some(cur_spec), Some(want)) => {
                    want.iter().all(|(k, v)| cur_spec.get(k) == Some(v))
                }
                _ => cur.get("spec") == Some(&spec),
            };
            let same_labels = cur
                .pointer("/metadata/labels")
                .map(|l| {
                    labels
                        .iter()
                        .all(|(k, v)| l.get(k).and_then(|x| x.as_str()) == Some(v))
                })
                .unwrap_or(labels.is_empty());
            if same_spec && same_labels {
                return Ok(());
            }
        }
        let exists = current.is_some();
        let url = if exists {
            self.url(gv, plural, Some(name))
        } else {
            self.url(gv, plural, None)
        };
        let resp = self
            .send(|| {
                let rb = if exists {
                    self.http.put(&url)
                } else {
                    self.http.post(&url)
                };
                rb.json(&body)
            })
            .await?;
        if !resp.status().is_success() {
            let s = resp.status();
            return Err(FabricError::Eda(format!(
                "{} {kind}/{name}: {s}: {}",
                if exists { "PUT" } else { "POST" },
                resp.text().await.unwrap_or_default()
            )));
        }
        Ok(())
    }

    async fn delete(&self, gv: &str, plural: &str, name: &str) -> Result<(), FabricError> {
        let url = self.url(gv, plural, Some(name));
        let resp = self.send(|| self.http.delete(&url)).await?;
        match resp.status() {
            reqwest::StatusCode::NOT_FOUND => Ok(()),
            s if s.is_success() => Ok(()),
            s => Err(FabricError::Eda(format!(
                "DELETE {url}: {s}: {}",
                resp.text().await.unwrap_or_default()
            ))),
        }
    }

    async fn list(
        &self,
        gv: &str,
        plural: &str,
        label_selector: &str,
    ) -> Result<Vec<serde_json::Value>, FabricError> {
        let url = self.url(gv, plural, None);
        let resp = self
            .send(|| {
                self.http
                    .get(&url)
                    .query(&[("labelSelector", label_selector)])
            })
            .await?;
        if !resp.status().is_success() {
            let s = resp.status();
            return Err(FabricError::Eda(format!(
                "LIST {url}: {s}: {}",
                resp.text().await.unwrap_or_default()
            )));
        }
        let v: serde_json::Value = resp.json().await?;
        Ok(v.get("items")
            .and_then(|i| i.as_array())
            .cloned()
            .unwrap_or_default())
    }

    /// Set or clear the per-VPC label on a fabric-owned `Interface`. JSON Patch on
    /// metadata only; `spec` is never touched. The label key's `/` is escaped
    /// as `~1` per RFC 6901.
    async fn label_interface(&self, interface: &str, vpc: Option<&str>) -> Result<(), FabricError> {
        let path = format!("/metadata/labels/{}", VPC_LABEL.replace('/', "~1"));
        let patch = match vpc {
            Some(v) => serde_json::json!([{ "op": "add", "path": path, "value": v }]),
            None => serde_json::json!([{ "op": "remove", "path": path }]),
        };
        let url = self.url(INTERFACES_GV, "interfaces", Some(interface));
        let resp = self
            .send(|| {
                self.http
                    .patch(&url)
                    .header("Content-Type", "application/json-patch+json")
                    .body(patch.to_string())
            })
            .await?;
        if !resp.status().is_success() {
            let s = resp.status();
            return Err(FabricError::Eda(format!(
                "PATCH interface {interface}: {s}: {}",
                resp.text().await.unwrap_or_default()
            )));
        }
        Ok(())
    }

    fn nico_labels(nico_vpc_id: &str, vni: Option<u32>) -> BTreeMap<String, String> {
        let mut l = BTreeMap::new();
        l.insert(NICO_ID_LABEL.into(), nico_vpc_id.chars().take(63).collect());
        if let Some(v) = vni {
            l.insert(NICO_VNI_LABEL.into(), v.to_string());
        }
        l
    }

    /// BridgeDomain name derived from the Router name; kept distinct because both
    /// become SR Linux network-instances (see `ensure_vrf`). Stays within 63 chars.
    fn bd_name(router_name: &str) -> String {
        let base: String = router_name.chars().take(60).collect();
        format!("{base}-bd")
    }

    /// `10.0.20.1` + `10.0.20.0/24` -> `10.0.20.1/24`.
    fn gateway_prefix(gateway: &str, subnet_cidr: &str) -> Result<String, FabricError> {
        let len = subnet_cidr
            .rsplit_once('/')
            .map(|(_, l)| l)
            .ok_or_else(|| {
                FabricError::Invalid(format!("subnet {subnet_cidr} has no prefix length"))
            })?;
        Ok(format!("{gateway}/{len}"))
    }
}

#[async_trait]
impl FabricOperations for EdaFabric {
    async fn ensure_vrf(&self, intent: &VrfIntent) -> Result<(), FabricError> {
        let name = Self::eda_name(&intent.name);
        // SR Linux renders both a Router (ip-vrf) and a BridgeDomain (mac-vrf) as
        // a `network-instance` keyed by the EDA object name, so the two must not
        // share one: the leaf rejects the config with a type conflict otherwise.
        let bd_name = Self::bd_name(&name);
        let labels = Self::nico_labels(&intent.nico_vpc_id, intent.vni);
        let encap = serde_json::json!({
            "vxlan": { "vniPool": VNI_POOL, "tunnelIndexPool": TUNNEL_INDEX_POOL }
        });
        // The L3 VNI is NICo's own allocation so it stays stable across backends;
        // the L2 VNI for the bridge domain comes from EDA's pool.
        let mut router_encap = encap.clone();
        if let Some(vni) = intent.vni {
            router_encap["vxlan"]["vni"] = serde_json::json!(vni);
        }
        tracing::info!(vrf = %name, nico_id = %intent.nico_vpc_id, "fabric(eda): ensure_vrf");

        self.upsert(SERVICES_GV, "Router", "routers", &name, &labels, serde_json::json!({
            "type": "EVPNVXLAN",
            "description": format!("NICo isolation domain {} ({})", intent.name, intent.nico_vpc_id),
            "encapOptions": router_encap,
            "eviPool": EVI_POOL,
        })).await?;

        self.upsert(
            SERVICES_GV,
            "BridgeDomain",
            "bridgedomains",
            &bd_name,
            &labels,
            serde_json::json!({
                "type": "EVPNVXLAN",
                "description": format!("NICo subnet {} of {}", intent.subnet_cidr, intent.name),
                "encapOptions": encap,
                "eviPool": EVI_POOL,
                "macLearning": { "enabled": true, "agingTimeSeconds": 300 },
            }),
        )
        .await?;

        self.upsert(
            SERVICES_GV,
            "IRBInterface",
            "irbinterfaces",
            &name,
            &labels,
            serde_json::json!({
                "bridgeDomain": bd_name,
                "router": name,
                "description": format!("NICo gateway {} for {}", intent.gateway, intent.name),
                "ipAddresses": [ {
                    "ipv4Address": {
                        "ipPrefix": Self::gateway_prefix(&intent.gateway, &intent.subnet_cidr)?,
                        "primary": true,
                        "anycast": true,
                    }
                } ],
            }),
        )
        .await?;

        // Host-facing VLAN: binds every Interface labelled for this VPC. Hosts are
        // added by attach_host, which sets that label.
        self.upsert(
            SERVICES_GV,
            "VLAN",
            "vlans",
            &name,
            &labels,
            serde_json::json!({
                "bridgeDomain": bd_name,
                "vlanID": intent.vlan.to_string(),
                "interfaceSelectors": [ format!("{VPC_LABEL}={name}") ],
                "description": format!("NICo VLAN {} for {}", intent.vlan, intent.name),
            }),
        )
        .await
    }

    async fn attach_host(&self, att: &HostAttachment) -> Result<(), FabricError> {
        let vrf = Self::eda_name(&att.vpc_name);
        tracing::info!(vrf = %vrf, interface = %att.connection, "fabric(eda): attach_host");
        // `connection` is the fabric-owned Interface the host is cabled to (e.g.
        // `<leaf>-ethernet-1-40`), recorded on the NICo machine from inventory.
        // Idempotent: skip the transaction if the label is already right.
        if let Some(cur) = self
            .get(INTERFACES_GV, "interfaces", &att.connection)
            .await?
        {
            let have = cur
                .pointer("/metadata/labels")
                .and_then(|l| l.get(VPC_LABEL))
                .and_then(|v| v.as_str());
            if have == Some(vrf.as_str()) {
                return Ok(());
            }
        } else {
            return Err(FabricError::Invalid(format!(
                "interface {} does not exist on the fabric",
                att.connection
            )));
        }
        self.label_interface(&att.connection, Some(&vrf)).await
    }

    async fn list_attachments(&self, vpc_name: &str) -> Result<Vec<String>, FabricError> {
        let vrf = Self::eda_name(vpc_name);
        Ok(self
            .list(INTERFACES_GV, "interfaces", &format!("{VPC_LABEL}={vrf}"))
            .await?
            .into_iter()
            .filter_map(|i| {
                i.pointer("/metadata/name")
                    .and_then(|v| v.as_str())
                    .map(str::to_string)
            })
            .collect())
    }

    async fn detach_host(&self, att: &HostAttachment) -> Result<(), FabricError> {
        let vrf = Self::eda_name(&att.vpc_name);
        // Only clear the label if it still points at this VRF: a port that has
        // since been attached to another VPC must not lose that binding.
        let Some(cur) = self
            .get(INTERFACES_GV, "interfaces", &att.connection)
            .await?
        else {
            return Ok(());
        };
        let have = cur
            .pointer("/metadata/labels")
            .and_then(|l| l.get(VPC_LABEL))
            .and_then(|v| v.as_str());
        if have != Some(vrf.as_str()) {
            return Ok(());
        }
        tracing::info!(vrf = %vrf, interface = %att.connection, "fabric(eda): detach_host");
        self.label_interface(&att.connection, None).await
    }

    async fn peer_vpcs(&self, a: &str, b: &str) -> Result<(), FabricError> {
        Err(FabricError::Invalid(format!(
            "VPC peering {a}<->{b} is not implemented on the EDA backend yet \
             (EDA leaks VRF routes through policies, not a permit list)"
        )))
    }

    async fn get_vrf_status(
        &self,
        vpc_name: &str,
    ) -> Result<Option<serde_json::Value>, FabricError> {
        let name = Self::eda_name(vpc_name);
        Ok(self
            .get(SERVICES_GV, "routers", &name)
            .await?
            .and_then(|o| o.get("status").cloned()))
    }

    async fn list_vrfs(&self) -> Result<Vec<(String, String)>, FabricError> {
        let mut out = Vec::new();
        for o in self.list(SERVICES_GV, "routers", NICO_ID_LABEL).await? {
            let name = o
                .pointer("/metadata/name")
                .and_then(|v| v.as_str())
                .unwrap_or_default();
            let nico_id = o
                .pointer(&format!(
                    "/metadata/labels/{}",
                    NICO_ID_LABEL.replace('/', "~1")
                ))
                .and_then(|v| v.as_str())
                .unwrap_or_default();
            if !name.is_empty() && !nico_id.is_empty() {
                out.push((name.to_string(), nico_id.to_string()));
            }
        }
        Ok(out)
    }

    async fn delete_vrf(&self, vpc_name: &str) -> Result<(), FabricError> {
        let name = Self::eda_name(vpc_name);
        tracing::info!(vrf = %name, "fabric(eda): delete_vrf");
        // Detach hosts first so the VLAN has no members when it goes.
        for i in self
            .list(INTERFACES_GV, "interfaces", &format!("{VPC_LABEL}={name}"))
            .await?
        {
            if let Some(n) = i.pointer("/metadata/name").and_then(|v| v.as_str()) {
                self.label_interface(n, None).await?;
            }
        }
        let bd_name = Self::bd_name(&name);
        for (kind, plural, obj) in [
            ("VLAN", "vlans", &name),
            ("IRBInterface", "irbinterfaces", &name),
            ("BridgeDomain", "bridgedomains", &bd_name),
            ("Router", "routers", &name),
        ] {
            tracing::debug!(kind, name = %obj, "fabric(eda): delete");
            self.delete(SERVICES_GV, plural, obj).await?;
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn eda_name_is_prefixed_and_label_safe() {
        assert_eq!(EdaFabric::eda_name("frontend"), "nico-frontend");
        assert_eq!(
            EdaFabric::eda_name("OpenAI Training"),
            "nico-openai-training"
        );
        assert_eq!(EdaFabric::eda_name("nico-frontend"), "nico-frontend");
        assert!(EdaFabric::eda_name(&"x".repeat(100)).len() <= 63);
    }

    #[test]
    fn gateway_takes_subnet_prefix_length() {
        assert_eq!(
            EdaFabric::gateway_prefix("10.0.20.1", "10.0.20.0/24").unwrap(),
            "10.0.20.1/24"
        );
        assert!(EdaFabric::gateway_prefix("10.0.20.1", "10.0.20.0").is_err());
    }

    #[test]
    fn eda_config_defaults() {
        let c: EdaConfig =
            serde_json::from_str(r#"{"api_url":"https://x","username":"nico"}"#).unwrap();
        assert_eq!(c.realm, "eda");
        assert_eq!(c.client_id, "eda");
        assert!(!c.insecure_skip_tls_verify);
    }
}
