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

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};

use arc_swap::ArcSwap;
use carbide_secrets::credentials::Credentials;
use carbide_utils::HostPortPair;
pub use nv_redfish::bmc_http::reqwest::BmcError;
use nv_redfish::bmc_http::reqwest::{
    Client as RedfishReqwestClient, ClientParams as RedfishReqwestClientParams,
};
use nv_redfish::bmc_http::{BmcCredentials, CacheSettings, HttpBmc};
use nv_redfish::oem::hpe::ilo_service_ext::ManagerType as HpeManagerType;
use nv_redfish::{Error as NvError, ServiceRoot as NvServiceRoot};
use reqwest::header::HeaderMap;

pub type RedfishBmc = HttpBmc<RedfishReqwestClient>;
pub type ServiceRoot = NvServiceRoot<RedfishBmc>;
pub type Error = NvError<RedfishBmc>;

pub fn new_pool(
    proxy_address: Arc<ArcSwap<Option<HostPortPair>>>,
    proxy_overrides: Arc<ArcSwap<HashMap<std::net::IpAddr, HostPortPair>>>,
) -> Arc<NvRedfishClientPool> {
    NvRedfishClientPool::new(proxy_address, proxy_overrides).into()
}

pub struct NvRedfishClientPool {
    proxy_address: Arc<ArcSwap<Option<HostPortPair>>>,
    /// Per-target overrides of `proxy_address`, keyed by the target BMC's own IP. Checked before
    /// `proxy_address` in `create_bmc` — a hit here wins for that one IP, everything else keeps
    /// using `proxy_address` (or no proxy) exactly as before this field existed.
    proxy_overrides: Arc<ArcSwap<HashMap<std::net::IpAddr, HostPortPair>>>,
    cache: Arc<Mutex<HashMap<PoolKey, Arc<ServiceRoot>>>>,
}

#[derive(Hash, PartialEq, Eq)]
struct PoolKey {
    proxy_address: Arc<Option<HostPortPair>>,
    // HashMap doesn't implement Hash, so this can't just be the map itself. Keying on the
    // ArcSwap snapshot's pointer identity is sufficient: `.store()` always produces a new Arc,
    // so any actual change to the override table correctly invalidates cached clients; the only
    // cost is an unnecessary cache miss in the rare case of storing back an unchanged map.
    proxy_overrides_snapshot: usize,
    bmc_address: SocketAddr,
    credentials: BmcCredentials,
}

impl NvRedfishClientPool {
    pub fn new(
        proxy_address: Arc<ArcSwap<Option<HostPortPair>>>,
        proxy_overrides: Arc<ArcSwap<HashMap<std::net::IpAddr, HostPortPair>>>,
    ) -> Self {
        Self {
            proxy_address,
            proxy_overrides,
            cache: Default::default(),
        }
    }

    pub async fn service_root(
        &self,
        bmc_address: SocketAddr,
        credentials: Credentials,
    ) -> Result<Arc<ServiceRoot>, Error> {
        let Credentials::UsernamePassword { username, password } = credentials;
        let bmc_credentials = BmcCredentials::new(username, password);

        if let Some(sevice_root) = self.cached_root(bmc_address, bmc_credentials.clone()) {
            Ok(sevice_root)
        } else {
            let bmc = self.create_bmc(bmc_address, bmc_credentials.clone(), false)?;
            let service_root = ServiceRoot::new(bmc).await?;
            let service_root = if service_root.vendor()
                == Some(nv_redfish::service_root::Vendor::new("HPE"))
                && let Some(HpeManagerType::Ilo(version)) = service_root
                    .oem_hpe_ilo_service_ext()
                    .ok()
                    .as_ref()
                    .and_then(|v| v.as_ref())
                    .and_then(|v| v.manager_type())
                && version < 7
            {
                // Handle HPE BMC that closing connection right after
                // response. In this case, we add Connection: Close
                // HTTP header to prevent trying to reuse this
                // connection. Otherwise, race condition may happen
                // when reqwest thinks that connection is alive but it
                // is about to close by server. Reusing such
                // connections causes errors.
                let bmc = self.create_bmc(bmc_address, bmc_credentials.clone(), true)?;
                service_root.replace_bmc(bmc.clone())
            } else {
                service_root
            };
            let service_root = Arc::new(service_root);
            self.update_cache(bmc_address, bmc_credentials, service_root.clone());
            Ok(service_root)
        }
    }

    fn cached_root(
        &self,
        bmc_address: SocketAddr,
        credentials: BmcCredentials,
    ) -> Option<Arc<ServiceRoot>> {
        let proxy_address = self.proxy_address.load();
        let key = PoolKey {
            proxy_address: proxy_address.clone(),
            proxy_overrides_snapshot: Arc::as_ptr(&self.proxy_overrides.load()) as usize,
            bmc_address,
            credentials,
        };
        self.cache
            .lock()
            .expect("nv-redish client cache mutex poisoned")
            .get(&key)
            .cloned()
    }

    fn update_cache(
        &self,
        bmc_address: SocketAddr,
        credentials: BmcCredentials,
        root: Arc<ServiceRoot>,
    ) {
        let proxy_address = self.proxy_address.load();
        let key = PoolKey {
            proxy_address: proxy_address.clone(),
            proxy_overrides_snapshot: Arc::as_ptr(&self.proxy_overrides.load()) as usize,
            bmc_address,
            credentials,
        };
        let mut cache = self
            .cache
            .lock()
            .expect("nv-redish client cache mutex poisoned");
        cache.insert(key, root);
    }

    pub fn create_bmc(
        &self,
        bmc_address: SocketAddr,
        credentials: BmcCredentials,
        connection_close: bool,
    ) -> Result<Arc<RedfishBmc>, Error> {
        let per_target_override = self.proxy_overrides.load().get(&bmc_address.ip()).cloned();
        let proxy_address = self.proxy_address.load();
        let (bmc_url, headers) = resolve_bmc_url_and_headers(
            bmc_address,
            per_target_override,
            proxy_address.as_ref().clone(),
            connection_close,
        );

        let client = RedfishReqwestClient::with_params(
            RedfishReqwestClientParams::new().accept_invalid_certs(true),
        )
        .map_err(|err| Error::Bmc(err.into()))?;
        Ok(Arc::new(RedfishBmc::with_custom_headers(
            client,
            bmc_url,
            credentials,
            CacheSettings::with_capacity(10),
            headers,
        )))
    }
}

/// Pure URL/header resolution, split out of `create_bmc` so it's testable without going through
/// `HttpBmc` (which exposes no public accessor for its resolved endpoint). `per_target_override`
/// wins over `global_proxy` when both are set for this target; `None`/`None` means "connect to
/// `bmc_address` directly" — the pre-existing, unproxied behavior.
fn resolve_bmc_url_and_headers(
    bmc_address: SocketAddr,
    per_target_override: Option<HostPortPair>,
    global_proxy: Option<HostPortPair>,
    connection_close: bool,
) -> (url::Url, HeaderMap) {
    let effective_override = per_target_override.or(global_proxy);

    let bmc_url = match effective_override.as_ref() {
        // No override
        None => format!("https://{bmc_address}"),
        Some(HostPortPair::HostAndPort(h, p)) => format!("https://{h}:{p}"),
        Some(HostPortPair::HostOnly(h)) => format!("https://{h}:{}", bmc_address.port()),
        Some(HostPortPair::PortOnly(p)) => format!("https://{}:{p}", bmc_address.ip()),
    }
    .parse::<url::Url>()
    .expect("Generated URI is expected to be valid");

    let mut headers = HeaderMap::new();
    if effective_override.is_some() {
        headers.insert(
            reqwest::header::FORWARDED,
            format!("host={}", bmc_address.ip())
                .parse()
                .expect("Generated header is expected to be valid"),
        );
    }
    if connection_close {
        headers.insert(
            reqwest::header::CONNECTION,
            reqwest::header::HeaderValue::from_static("Close"),
        );
    }

    (bmc_url, headers)
}

#[cfg(test)]
mod tests {
    use super::*;

    // create_bmc's actual output (Arc<RedfishBmc>) has no public accessor for its resolved
    // endpoint, so these test the extracted pure resolver directly — same logic create_bmc
    // calls, with the same three inputs it passes in.

    #[test]
    fn no_override_and_no_global_proxy_targets_the_real_bmc_directly() {
        let target: SocketAddr = "10.0.0.7:443".parse().unwrap();
        let (url, headers) = resolve_bmc_url_and_headers(target, None, None, false);
        assert_eq!(url.host_str(), Some("10.0.0.7"));
        // 443 is https's default port, so url::Url::port() normalizes it away to None;
        // port_or_known_default() is the right check for "what port does this actually resolve
        // to" regardless of whether it happens to be the scheme default.
        assert_eq!(url.port_or_known_default(), Some(443));
        assert!(!headers.contains_key(reqwest::header::FORWARDED));
    }

    #[test]
    fn global_proxy_still_applies_when_no_per_target_override_exists() {
        // Regression guard: adding proxy_overrides must not change behavior for a target
        // that has no entry in it — this is exactly today's simulated-fleet behavior.
        let target: SocketAddr = "10.0.0.7:443".parse().unwrap();
        let global_proxy = Some(HostPortPair::HostAndPort(
            "machine-a-tron-bmc-mock.nico-system.svc.cluster.local".to_string(),
            1266,
        ));

        let (url, headers) = resolve_bmc_url_and_headers(target, None, global_proxy, false);
        assert_eq!(
            url.host_str(),
            Some("machine-a-tron-bmc-mock.nico-system.svc.cluster.local")
        );
        assert_eq!(url.port(), Some(1266));
        assert!(headers.contains_key(reqwest::header::FORWARDED));
    }

    #[test]
    fn per_target_override_wins_over_the_global_proxy_for_its_own_ip() {
        // The actual case this whole change exists for: a real BMC IP gets redirected
        // through a tunnel bridge even though a global proxy (the simulated fleet's mock) is
        // also configured.
        let real_bmc: SocketAddr = "172.24.0.107:443".parse().unwrap();
        let per_target_override = Some(HostPortPair::HostAndPort(
            "real-bridge.internal".to_string(),
            8444,
        ));
        let global_proxy = Some(HostPortPair::HostAndPort(
            "machine-a-tron-bmc-mock.nico-system.svc.cluster.local".to_string(),
            1266,
        ));

        let (url, headers) =
            resolve_bmc_url_and_headers(real_bmc, per_target_override, global_proxy, false);
        assert_eq!(url.host_str(), Some("real-bridge.internal"));
        assert_eq!(url.port(), Some(8444));
        assert!(headers.contains_key(reqwest::header::FORWARDED));
    }

    #[test]
    fn pool_lookup_is_scoped_to_the_matching_ip_only() {
        // Exercises the actual pool-level lookup NvRedfishClientPool::create_bmc performs
        // (proxy_overrides keyed by IP, falling back to proxy_address), not just the resolver.
        let real_bmc: SocketAddr = "172.24.0.107:443".parse().unwrap();
        let unrelated_target: SocketAddr = "10.0.0.9:443".parse().unwrap();

        let mut overrides_map = HashMap::new();
        overrides_map.insert(
            real_bmc.ip(),
            HostPortPair::HostAndPort("real-bridge.internal".to_string(), 8444),
        );
        let proxy_overrides = Arc::new(ArcSwap::new(Arc::new(overrides_map)));
        let proxy_address = Arc::new(ArcSwap::new(Arc::new(Some(HostPortPair::HostAndPort(
            "machine-a-tron-bmc-mock.nico-system.svc.cluster.local".to_string(),
            1266,
        )))));

        let lookup = |target: SocketAddr| -> Option<HostPortPair> {
            proxy_overrides
                .load()
                .get(&target.ip())
                .cloned()
                .or_else(|| proxy_address.load().as_ref().clone())
        };

        assert_eq!(
            lookup(real_bmc),
            Some(HostPortPair::HostAndPort(
                "real-bridge.internal".to_string(),
                8444
            ))
        );
        assert_eq!(
            lookup(unrelated_target),
            Some(HostPortPair::HostAndPort(
                "machine-a-tron-bmc-mock.nico-system.svc.cluster.local".to_string(),
                1266
            ))
        );

        // Same removal an admin RPC handler would do: store back an empty map.
        proxy_overrides.store(Arc::new(HashMap::new()));
        assert_eq!(
            lookup(real_bmc),
            Some(HostPortPair::HostAndPort(
                "machine-a-tron-bmc-mock.nico-system.svc.cluster.local".to_string(),
                1266
            ))
        );
    }
}
