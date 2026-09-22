// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/infra-controller/rest-api/common/pkg/coreproxy"
	"github.com/NVIDIA/infra-controller/rest-api/site-workflow/pkg/grpc/client"
)

// A request that names an actor must never be sent under the site's own
// certificate, because Core would then record the change as nobody's. A site
// without an actor CA refuses, and says so in the words the REST tier
// recognises -- before it even looks at whether it is connected to Core, since
// the answer does not depend on that.
func TestInvokeCoreGRPCOnSiteRefusesAnActorWithoutAnActorCA(t *testing.T) {
	m := NewManageCoreProxy(client.NewCoreGrpcAtomicClient(&client.CoreGrpcClientConfig{}), "site-key")

	_, err := m.InvokeCoreGRPCOnSite(context.Background(), coreproxy.Request{
		FullMethod: "/forge.Forge/RedfishCreateAction",
		Actor:      &coreproxy.Actor{User: "alice@example.com", Org: "acme", Group: "PROVIDER_ADMIN"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), coreproxy.ActorUnsupportedMessage)
}

// The same request without an actor takes the ordinary path, which on a site
// with no Core connection fails for the ordinary reason -- not the actor one.
func TestInvokeCoreGRPCOnSiteWithoutAnActorIsTheSitesOwnCall(t *testing.T) {
	m := NewManageCoreProxy(client.NewCoreGrpcAtomicClient(&client.CoreGrpcClientConfig{}), "site-key")

	_, err := m.InvokeCoreGRPCOnSite(context.Background(), coreproxy.Request{
		FullMethod: "/forge.Forge/RedfishListActions",
	})

	require.ErrorIs(t, err, client.ErrCoreGrpcClientNotConnected)
	assert.NotContains(t, err.Error(), coreproxy.ActorUnsupportedMessage)
}

// With a CA configured but unreadable, the failure names the CA, so an operator
// sees a mount problem rather than a Core one.
func TestInvokeCoreGRPCOnSiteReportsAnUnreadableActorCA(t *testing.T) {
	m := NewManageCoreProxy(client.NewCoreGrpcAtomicClient(&client.CoreGrpcClientConfig{
		Address: "core:1079", ServerCAPath: "/nonexistent/ca.crt",
	}), "site-key").WithActorCA("/nonexistent/actor-ca.crt", "/nonexistent/actor-ca.key")

	_, err := m.InvokeCoreGRPCOnSite(context.Background(), coreproxy.Request{
		FullMethod: "/forge.Forge/RedfishCreateAction",
		Actor:      &coreproxy.Actor{User: "alice@example.com"},
	})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), coreproxy.ActorUnsupportedMessage,
		"a configured-but-unreadable CA is a different fault from no CA")
	assert.Contains(t, err.Error(), "no such file")
}
