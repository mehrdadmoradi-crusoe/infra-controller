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

//! The fabric agent contract (`fabric_agent.v1`): generated protobuf types and
//! tonic service stubs. See `proto/fabric_agent.proto` for the semantics and the
//! invariants every agent implements.

/// Contract version implemented by this crate. Agents report the highest version
/// they speak in `Capabilities.contract_version`.
pub const CONTRACT_VERSION: &str = "1.0.0";

#[allow(non_snake_case, unknown_lints, clippy::all)]
#[rustfmt::skip]
pub mod v1 {
    include!(concat!(env!("OUT_DIR"), "/fabric_agent.v1.rs"));
}

pub use v1::fabric_agent_client::FabricAgentClient;
pub use v1::fabric_agent_server::{FabricAgent, FabricAgentServer};
