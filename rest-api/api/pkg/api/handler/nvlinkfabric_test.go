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
