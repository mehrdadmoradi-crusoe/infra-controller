// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	tmocks "go.temporal.io/sdk/mocks"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/handler/util/common"
	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/model"
	coreproxy "github.com/NVIDIA/infra-controller/rest-api/common/pkg/coreproxy"
	cdbm "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/model"
	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

// getRackTopology drives the handler with the Site returning positions.
func (f rackAccessFixture) getRackTopology(t *testing.T, org string, user *cdbm.User, rackID string, positions proto.Message) *httptest.ResponseRecorder {
	t.Helper()

	run := &tmocks.WorkflowRun{}
	run.On("Get", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		if positions == nil {
			return
		}
		out := args.Get(1).(*coreproxy.Response)
		respJSON, err := protojson.Marshal(positions)
		require.NoError(t, err)
		out.ResponseJSON = respJSON
	}).Return(nil)

	tsc := &tmocks.Client{}
	tsc.On("ExecuteWorkflow", mock.Anything, mock.Anything, coreproxy.WorkflowName, mock.Anything).Return(run, nil)
	f.scp.IDClientMap[f.siteID.String()] = tsc

	q := url.Values{}
	q.Set("siteId", f.siteID.String())

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v2/org/%s/nico/rack/%s/topology?%s", org, rackID, q.Encode()), nil)
	rec := httptest.NewRecorder()
	ec := e.NewContext(req, rec)
	ec.SetParamNames("orgName", "id")
	ec.SetParamValues(org, rackID)
	ec.Set("user", user)

	handler := NewGetRackTopologyHandler(f.dbSession, f.scp, common.GetTestConfig())
	require.NoError(t, handler.Handle(ec))
	return rec
}

func position(machineID string, slot, tray int32, switchID, shelfID string) *cwssaws.MachinePositionInfo {
	out := &cwssaws.MachinePositionInfo{
		MachineId:          &cwssaws.MachineId{Id: machineID},
		PhysicalSlotNumber: proto.Int32(slot),
		ComputeTrayIndex:   proto.Int32(tray),
	}
	if switchID != "" {
		out.SwitchId = &cwssaws.SwitchId{Id: switchID}
	}
	if shelfID != "" {
		out.PowerShelfId = &cwssaws.PowerShelfId{Id: shelfID}
	}
	return out
}

func TestRackTopologyGroupsHostsBySwitchWithoutExposingSwitchIds(t *testing.T) {
	f := newRackAccessFixture(t)
	machineID := machineInHeldRack(t, f)

	rec := f.getRackTopology(t, f.org, f.providerUser, f.heldRackID, &cwssaws.MachinePositionInfoList{
		MachinePositionInfo: []*cwssaws.MachinePositionInfo{
			position(machineID, 3, 1, "8a3d70d0-4fb7-42c5-89f6-076709f3ee5c", "shelf-uuid-1"),
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var topology model.APIRackTopology
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &topology))
	assert.Equal(t, f.heldRackID, topology.RackID)
	require.Len(t, topology.Hosts, 1)
	assert.Equal(t, machineID, topology.Hosts[0].MachineID)
	require.NotNil(t, topology.Hosts[0].SlotNumber)
	assert.Equal(t, int32(3), *topology.Hosts[0].SlotNumber)
	require.NotNil(t, topology.Hosts[0].SwitchGroup)
	assert.Equal(t, "switch-1", *topology.Hosts[0].SwitchGroup)

	// Our inventory identifiers are ours: the grouping is exposed, the ids are
	// not.
	body := rec.Body.String()
	assert.NotContains(t, body, "8a3d70d0-4fb7-42c5-89f6-076709f3ee5c")
	assert.NotContains(t, body, "shelf-uuid-1")
}

// Hosts on the same switch share a label; hosts on another switch get a
// different one. That is the whole point of the field.
func TestRackTopologyLabelsSharedAndSeparateSwitches(t *testing.T) {
	f := newRackAccessFixture(t)
	machineID := machineInHeldRack(t, f)

	rec := f.getRackTopology(t, f.org, f.providerUser, f.heldRackID, &cwssaws.MachinePositionInfoList{
		MachinePositionInfo: []*cwssaws.MachinePositionInfo{
			position(machineID, 1, 1, "switch-a", "shelf-a"),
			position("host-b", 2, 2, "switch-a", "shelf-a"),
			position("host-c", 3, 3, "switch-b", "shelf-a"),
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var topology model.APIRackTopology
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &topology))
	require.Len(t, topology.Hosts, 3)

	require.NotNil(t, topology.Hosts[0].SwitchGroup)
	require.NotNil(t, topology.Hosts[1].SwitchGroup)
	require.NotNil(t, topology.Hosts[2].SwitchGroup)
	assert.Equal(t, *topology.Hosts[0].SwitchGroup, *topology.Hosts[1].SwitchGroup)
	assert.NotEqual(t, *topology.Hosts[0].SwitchGroup, *topology.Hosts[2].SwitchGroup)

	assert.Equal(t, 2, topology.SwitchGroupCount)
	assert.Equal(t, 1, topology.PowerGroupCount)
	assert.Equal(t, 3, topology.HostCount)
}

// Labels are assigned in slot order, so the same rack reads the same way on
// every request whatever order the Site replies in.
func TestRackTopologyIsStableAcrossSiteOrdering(t *testing.T) {
	f := newRackAccessFixture(t)
	machineID := machineInHeldRack(t, f)

	forward := &cwssaws.MachinePositionInfoList{MachinePositionInfo: []*cwssaws.MachinePositionInfo{
		position(machineID, 1, 1, "switch-a", ""),
		position("host-b", 2, 2, "switch-b", ""),
	}}
	reversed := &cwssaws.MachinePositionInfoList{MachinePositionInfo: []*cwssaws.MachinePositionInfo{
		position("host-b", 2, 2, "switch-b", ""),
		position(machineID, 1, 1, "switch-a", ""),
	}}

	first := f.getRackTopology(t, f.org, f.providerUser, f.heldRackID, forward)
	second := f.getRackTopology(t, f.org, f.providerUser, f.heldRackID, reversed)
	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, http.StatusOK, second.Code)
	assert.JSONEq(t, first.Body.String(), second.Body.String())
}

// A host whose placement is not recorded is still listed. Dropping it would
// make a partly discovered rack look smaller than it is.
func TestRackTopologyListsHostsWithNoRecordedPlacement(t *testing.T) {
	f := newRackAccessFixture(t)
	machineID := machineInHeldRack(t, f)

	rec := f.getRackTopology(t, f.org, f.providerUser, f.heldRackID, &cwssaws.MachinePositionInfoList{
		MachinePositionInfo: []*cwssaws.MachinePositionInfo{
			{MachineId: &cwssaws.MachineId{Id: machineID}},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var topology model.APIRackTopology
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &topology))
	require.Len(t, topology.Hosts, 1)
	assert.Nil(t, topology.Hosts[0].SlotNumber)
	assert.Nil(t, topology.Hosts[0].SwitchGroup)
	assert.Equal(t, 0, topology.SwitchGroupCount)
	// The Site did report this host, just without a placement, so nothing is
	// missing from the read.
	assert.Equal(t, 0, topology.HostsNotReported)
}

// The Site reports placement by management address and omits a host it holds no
// management interface for. Such a host cannot be listed, so the count of them
// is what tells a caller the rack is only partly discovered -- otherwise a
// half-discovered rack reads as a complete smaller one.
func TestRackTopologyCountsHostsTheSiteDidNotReport(t *testing.T) {
	f := newRackAccessFixture(t)
	machineInHeldRack(t, f)

	rec := f.getRackTopology(t, f.org, f.providerUser, f.heldRackID,
		&cwssaws.MachinePositionInfoList{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var topology model.APIRackTopology
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &topology))
	assert.Empty(t, topology.Hosts)
	assert.Equal(t, 0, topology.HostCount)
	assert.Equal(t, 1, topology.HostsNotReported)
}

func TestRackTopologyIsReachableByTheTenantHoldingTheRack(t *testing.T) {
	f := newRackAccessFixture(t)
	machineID := machineInHeldRack(t, f)

	rec := f.getRackTopology(t, f.tenantOrg, f.tenantUser, f.heldRackID, &cwssaws.MachinePositionInfoList{
		MachinePositionInfo: []*cwssaws.MachinePositionInfo{position(machineID, 1, 1, "switch-a", "")},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var topology model.APIRackTopology
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &topology))
	assert.Equal(t, 1, topology.HostCount)
}

func TestRackTopologyHidesARackTheTenantDoesNotHold(t *testing.T) {
	f := newRackAccessFixture(t)

	rec := f.getRackTopology(t, f.tenantOrg, f.tenantUser, f.otherRackID, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Could not find Rack")
}
