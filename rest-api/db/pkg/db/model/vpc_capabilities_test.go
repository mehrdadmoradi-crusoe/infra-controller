// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"testing"

	"github.com/stretchr/testify/assert"

	cutil "github.com/NVIDIA/infra-controller/rest-api/common/pkg/util"
)

func TestVpcTypeSubnetCapabilities(t *testing.T) {
	tests := []struct {
		desc            string
		virtType        *string
		supportsSubnets bool
		hostInband      bool
	}{
		{desc: "untyped legacy rows behave as Ethernet Virtualizer", virtType: nil, supportsSubnets: true, hostInband: false},
		{desc: "Ethernet Virtualizer carves tenant segments", virtType: cutil.GetPtr(VpcEthernetVirtualizer), supportsSubnets: true, hostInband: false},
		{desc: "ToR carves HostInband segments", virtType: cutil.GetPtr(VpcTor), supportsSubnets: true, hostInband: true},
		{desc: "FNN has no Subnets", virtType: cutil.GetPtr(VpcFNN), supportsSubnets: false, hostInband: false},
		{desc: "Flat has no Subnets", virtType: cutil.GetPtr(VpcFlat), supportsSubnets: false, hostInband: false},
		{desc: "unknown types support nothing", virtType: cutil.GetPtr("SOMETHING_ELSE"), supportsSubnets: false, hostInband: false},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			assert.Equal(t, tc.supportsSubnets, VpcTypeSupportsSubnets(tc.virtType))
			assert.Equal(t, tc.hostInband, VpcTypeBindsSubnetsToHostInband(tc.virtType))
		})
	}
}

func TestVpcNetworkVirtualizationTypeMapAcceptsTor(t *testing.T) {
	assert.True(t, VpcNetworkVirtualzationTypeMap[VpcTor])
	assert.True(t, VpcNetworkVirtualzationTypeMap[VpcFlat])
	assert.False(t, VpcNetworkVirtualzationTypeMap[VpcEthernetVirtualizerWithNVUE])
}
