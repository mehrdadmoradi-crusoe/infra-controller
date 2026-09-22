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

// getNVLinkFabric drives the handler, with Core answering each method from
// responses. Two calls are made: the Rack's switch ids, then their records.
func (f rackAccessFixture) getNVLinkFabric(t *testing.T, org string, user *cdbm.User, rackID string, responses map[string]proto.Message) *httptest.ResponseRecorder {
	t.Helper()

	current := new(string)
	run := &tmocks.WorkflowRun{}
	run.On("Get", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		response, ok := responses[*current]
		if !ok || response == nil {
			return
		}
		out := args.Get(1).(*coreproxy.Response)
		respJSON, err := protojson.Marshal(response)
		require.NoError(t, err)
		out.ResponseJSON = respJSON
	}).Return(nil)

	tsc := &tmocks.Client{}
	tsc.On("ExecuteWorkflow", mock.Anything, mock.Anything, coreproxy.WorkflowName,
		mock.MatchedBy(func(req coreproxy.Request) bool {
			*current = req.FullMethod
			return true
		}),
	).Return(run, nil)
	f.scp.IDClientMap[f.siteID.String()] = tsc

	q := url.Values{}
	q.Set("siteId", f.siteID.String())

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v2/org/%s/nico/rack/%s/nvlink-fabric?%s", org, rackID, q.Encode()), nil)
	rec := httptest.NewRecorder()
	ec := e.NewContext(req, rec)
	ec.SetParamNames("orgName", "id")
	ec.SetParamValues(org, rackID)
	ec.Set("user", user)

	handler := NewGetNVLinkFabricHandler(f.dbSession, f.scp, common.GetTestConfig())
	require.NoError(t, handler.Handle(ec))
	return rec
}

func switchIDs(ids ...string) proto.Message {
	out := &cwssaws.SwitchIdList{}
	for _, id := range ids {
		out.Ids = append(out.Ids, &cwssaws.SwitchId{Id: id})
	}
	return out
}

func TestNVLinkFabricReportsFabricManagerStatePerTray(t *testing.T) {
	f := newRackAccessFixture(t)

	trayTwo := int32(2)
	traySeven := int32(7)
	rec := f.getNVLinkFabric(t, f.org, f.providerUser, f.heldRackID, map[string]proto.Message{
		cwssaws.Forge_FindSwitchIds_FullMethodName: switchIDs("sw-1", "sw-2"),
		cwssaws.Forge_FindSwitchesByIds_FullMethodName: &cwssaws.SwitchList{
			Switches: []*cwssaws.Switch{
				{
					PlacementInRack: &cwssaws.PlacementInRack{TrayIndex: &trayTwo},
					IsPrimary:       true,
					Status: &cwssaws.SwitchStatus{
						SwitchName:   proto.String("nvl-sw-02"),
						PowerState:   proto.String("on"),
						HealthStatus: proto.String("ok"),
						FabricManagerStatusDetails: &cwssaws.FabricManagerStatus{
							FabricManagerState: cwssaws.FabricManagerState_FABRIC_MANAGER_STATE_OK,
						},
					},
				},
				{
					PlacementInRack: &cwssaws.PlacementInRack{TrayIndex: &traySeven},
					Status: &cwssaws.SwitchStatus{
						SwitchName:   proto.String("nvl-sw-07"),
						PowerState:   proto.String("on"),
						HealthStatus: proto.String("critical"),
						FabricManagerStatusDetails: &cwssaws.FabricManagerStatus{
							FabricManagerState: cwssaws.FabricManagerState_FABRIC_MANAGER_STATE_NOT_OK,
							Reason:             proto.String("nmx-c unreachable"),
							ErrorMessage:       proto.String("connection refused"),
						},
					},
				},
			},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var fabric model.APINVLinkFabric
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fabric))
	assert.Equal(t, f.heldRackID, fabric.RackID)
	assert.Equal(t, 2, fabric.SwitchCount)
	assert.Equal(t, 1, fabric.NotOkCount)

	require.Len(t, fabric.Switches, 2)
	assert.Equal(t, model.NVLinkFabricManagerOk, fabric.Switches[0].FabricManagerState)
	require.NotNil(t, fabric.Switches[0].IsPrimary)
	assert.True(t, *fabric.Switches[0].IsPrimary)
	require.NotNil(t, fabric.Switches[0].TrayIndex)
	assert.Equal(t, int32(2), *fabric.Switches[0].TrayIndex)

	assert.Equal(t, model.NVLinkFabricManagerNotOk, fabric.Switches[1].FabricManagerState)
	require.NotNil(t, fabric.Switches[1].FabricManagerReason)
	assert.Equal(t, "nmx-c unreachable", *fabric.Switches[1].FabricManagerReason)
	require.NotNil(t, fabric.Switches[1].TrayIndex)
	assert.Equal(t, int32(7), *fabric.Switches[1].TrayIndex)
}

// A switch whose fabric manager could not be read is Unknown, not Ok. Silence
// is the case a caller most needs to see.
func TestNVLinkFabricReportsUnreadableStateAsUnknown(t *testing.T) {
	f := newRackAccessFixture(t)

	rec := f.getNVLinkFabric(t, f.org, f.providerUser, f.heldRackID, map[string]proto.Message{
		cwssaws.Forge_FindSwitchIds_FullMethodName: switchIDs("sw-1"),
		cwssaws.Forge_FindSwitchesByIds_FullMethodName: &cwssaws.SwitchList{
			Switches: []*cwssaws.Switch{{Status: &cwssaws.SwitchStatus{PowerState: proto.String("on")}}},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var fabric model.APINVLinkFabric
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fabric))
	require.Len(t, fabric.Switches, 1)
	assert.Equal(t, model.NVLinkFabricManagerUnknown, fabric.Switches[0].FabricManagerState)
	assert.Equal(t, 0, fabric.NotOkCount, "unknown is not a failure")
	// But it is not health either, and the summary has to say so: notOkCount
	// of zero on its own would read as a fabric that is fine.
	assert.Equal(t, 1, fabric.UnknownCount)
}

// The management network is not exposed: a switch's BMC details must not reach
// the response even though the Site returns them.
func TestNVLinkFabricDoesNotExposeSwitchManagementDetails(t *testing.T) {
	f := newRackAccessFixture(t)

	rec := f.getNVLinkFabric(t, f.org, f.providerUser, f.heldRackID, map[string]proto.Message{
		cwssaws.Forge_FindSwitchIds_FullMethodName: switchIDs("sw-1"),
		cwssaws.Forge_FindSwitchesByIds_FullMethodName: &cwssaws.SwitchList{
			Switches: []*cwssaws.Switch{{
				BmcInfo: &cwssaws.BmcInfo{
					Ip:  proto.String("10.9.9.9"),
					Mac: proto.String("aa:bb:cc:dd:ee:ff"),
				},
				Status: &cwssaws.SwitchStatus{SwitchName: proto.String("nvl-sw-01")},
			}},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	body := rec.Body.String()
	assert.NotContains(t, body, "10.9.9.9")
	assert.NotContains(t, body, "aa:bb:cc:dd:ee:ff")
}

// A Rack whose switches have not been discovered is an empty fabric, not an
// error.
func TestNVLinkFabricReturnsEmptyWhenNoSwitchesAreKnown(t *testing.T) {
	f := newRackAccessFixture(t)

	rec := f.getNVLinkFabric(t, f.org, f.providerUser, f.heldRackID, map[string]proto.Message{
		cwssaws.Forge_FindSwitchIds_FullMethodName: switchIDs(),
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var fabric model.APINVLinkFabric
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fabric))
	assert.Equal(t, 0, fabric.SwitchCount)
	assert.Empty(t, fabric.Switches)
}

func TestNVLinkFabricIsReachableByTheTenantHoldingTheRack(t *testing.T) {
	f := newRackAccessFixture(t)

	rec := f.getNVLinkFabric(t, f.tenantOrg, f.tenantUser, f.heldRackID, map[string]proto.Message{
		cwssaws.Forge_FindSwitchIds_FullMethodName: switchIDs(),
	})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestNVLinkFabricHidesARackTheTenantDoesNotHold(t *testing.T) {
	f := newRackAccessFixture(t)

	rec := f.getNVLinkFabric(t, f.tenantOrg, f.tenantUser, f.otherRackID, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Could not find Rack")
}

func TestBatchIDsSplitsOnTheLimitAndKeepsOrder(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e"}

	assert.Equal(t, [][]string{{"a", "b"}, {"c", "d"}, {"e"}}, batchIDs(ids, 2))
	assert.Equal(t, [][]string{ids}, batchIDs(ids, 5))
	assert.Equal(t, [][]string{ids}, batchIDs(ids, 99))
	assert.Empty(t, batchIDs([]string{}, 2))

	// A nonsensical size must not divide by zero or loop forever; one batch is
	// the safe reading, and Core will say if it is too many.
	assert.Equal(t, [][]string{ids}, batchIDs(ids, 0))
	assert.Equal(t, [][]string{ids}, batchIDs(ids, -1))
}

// A Rack wider than Core's max_find_by_ids must still be readable. How many
// switches a Rack holds is not the caller's choice and there is nothing for
// them to page, so the handler asks in batches rather than letting Core refuse
// the whole read.
func TestNVLinkFabricAsksForSwitchesInBatches(t *testing.T) {
	f := newRackAccessFixture(t)

	const total = switchLookupBatchSize*2 + 3
	ids := make([]string, 0, total)
	for i := range total {
		ids = append(ids, fmt.Sprintf("switch-%02d", i))
	}

	// Core answers each lookup with the switches that lookup asked for, so the
	// assembled response shows whether every batch was sent and none repeated.
	var asked [][]string
	current := new(string)
	var lastBatch []string

	run := &tmocks.WorkflowRun{}
	run.On("Get", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		out := args.Get(1).(*coreproxy.Response)
		var response proto.Message
		switch *current {
		case cwssaws.Forge_FindSwitchIds_FullMethodName:
			response = switchIDs(ids...)
		case cwssaws.Forge_FindSwitchesByIds_FullMethodName:
			list := &cwssaws.SwitchList{}
			for _, id := range lastBatch {
				list.Switches = append(list.Switches, &cwssaws.Switch{Id: &cwssaws.SwitchId{Id: id}})
			}
			response = list
		default:
			return
		}
		respJSON, err := protojson.Marshal(response)
		require.NoError(t, err)
		out.ResponseJSON = respJSON
	}).Return(nil)

	tsc := &tmocks.Client{}
	tsc.On("ExecuteWorkflow", mock.Anything, mock.Anything, coreproxy.WorkflowName,
		mock.MatchedBy(func(req coreproxy.Request) bool {
			*current = req.FullMethod
			if req.FullMethod == cwssaws.Forge_FindSwitchesByIds_FullMethodName {
				var byIDs cwssaws.SwitchesByIdsRequest
				require.NoError(t, protojson.Unmarshal(req.RequestJSON, &byIDs))
				lastBatch = nil
				for _, sid := range byIDs.GetSwitchIds() {
					lastBatch = append(lastBatch, sid.GetId())
				}
				asked = append(asked, lastBatch)
			}
			return true
		}),
	).Return(run, nil)
	f.scp.IDClientMap[f.siteID.String()] = tsc

	q := url.Values{}
	q.Set("siteId", f.siteID.String())
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v2/org/%s/nico/rack/%s/nvlink-fabric?%s", f.org, f.heldRackID, q.Encode()), nil)
	rec := httptest.NewRecorder()
	ec := e.NewContext(req, rec)
	ec.SetParamNames("orgName", "id")
	ec.SetParamValues(f.org, f.heldRackID)
	ec.Set("user", f.providerUser)

	handler := NewGetNVLinkFabricHandler(f.dbSession, f.scp, common.GetTestConfig())
	require.NoError(t, handler.Handle(ec))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Three lookups: two full batches and the remainder, none over the limit.
	require.Len(t, asked, 3)
	for _, batch := range asked {
		assert.LessOrEqual(t, len(batch), switchLookupBatchSize)
	}

	// And every switch came back exactly once, in order.
	var flat []string
	for _, batch := range asked {
		flat = append(flat, batch...)
	}
	assert.Equal(t, ids, flat)

	var fabric model.APINVLinkFabric
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fabric))
	assert.Equal(t, total, fabric.SwitchCount)
	assert.Len(t, fabric.Switches, total)
}
