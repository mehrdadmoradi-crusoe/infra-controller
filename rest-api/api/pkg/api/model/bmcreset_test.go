// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cutil "github.com/NVIDIA/infra-controller/rest-api/common/pkg/util"
)

func TestAPIBmcResetRequestToProto(t *testing.T) {
	req := APIBmcResetRequest{UseIpmiTool: cutil.GetPtr(true)}
	require.NoError(t, req.Validate())

	protoReq := req.ToProto("machine-1")
	assert.Equal(t, "machine-1", protoReq.GetMachineId())
	assert.True(t, protoReq.GetUseIpmitool())
}

// Asking for the Redfish path is spelled `false`, and that is a choice rather
// than a missing field.
func TestAPIBmcResetRequestAcceptsUseIpmiToolFalse(t *testing.T) {
	req := APIBmcResetRequest{UseIpmiTool: cutil.GetPtr(false)}
	require.NoError(t, req.Validate())

	protoReq := req.ToProto("machine-1")
	assert.Equal(t, "machine-1", protoReq.GetMachineId())
	assert.False(t, protoReq.GetUseIpmitool())
}

// Leaving it out is still an error: the caller has to pick a mechanism.
func TestAPIBmcResetRequestRejectsMissingUseIpmiTool(t *testing.T) {
	req := APIBmcResetRequest{}
	err := req.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "useIpmiTool")
}
