// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package queue

const (
	// CloudTaskQueue handles all tasks triggered by Cloud API and
	// are meant to be consumed by Cloud system worker
	CloudTaskQueue = "cloud"
	// SiteTaskQueue handles tasks submitted by Site agents running on Site management clusters,
	// meant to be consumed by the Cloud-side Site worker (nico-rest-site-worker). Do not use this
	// for the opposite direction (Cloud dispatching work to a specific Site's agent) — that
	// worker only registers the inventory-sync workflow set on this queue, nothing else.
	SiteTaskQueue = "site"
)

// SiteAgentTaskQueue is the task queue a given Site's own site-agent subscribes to for
// direct, site-agent-executed workflows (e.g. CreateVPCV2, UpdateVPC, UpdateVPCVirtualization,
// DeleteVPCV2 — anything that talks straight to Forge via CoreGrpc). Each site-agent's
// TEMPORAL_SUBSCRIBE_QUEUE is conventionally set to its own Site ID, so the queue name is the
// Site ID itself, in the Site's own Temporal namespace (see api/pkg/client/site/temporal.go for
// how that namespace is resolved). This is distinct from SiteTaskQueue above, which is the
// Cloud-side inventory-sync queue, not this one.
func SiteAgentTaskQueue(siteID string) string {
	return siteID
}
