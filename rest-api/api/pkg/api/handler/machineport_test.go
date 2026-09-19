// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	sc "github.com/NVIDIA/infra-controller/rest-api/api/pkg/client/site"
	authz "github.com/NVIDIA/infra-controller/rest-api/auth/pkg/authorization"
	coreproxy "github.com/NVIDIA/infra-controller/rest-api/common/pkg/coreproxy"
	cutil "github.com/NVIDIA/infra-controller/rest-api/common/pkg/util"
	cdb "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db"
	cdbm "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/model"
	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

type machinePortFixture struct {
	org       string
	machineID string
	dpuID     string
	user      *cdbm.User
	dbSession *cdb.Session
	scp       *sc.ClientPool
	calls     *[]string
}

// newMachinePortFixture builds a host with one attached DPU, so there is an
// uplink reporter to find, and answers Core per method.
func newMachinePortFixture(t *testing.T, withDpu bool, responses map[string]proto.Message) machinePortFixture {
	t.Helper()

	dbSession := common.TestInitDB(t)
	t.Cleanup(dbSession.Close)
	common.TestSetupSchema(t, dbSession)

	org := "test-org"
	user := common.TestBuildUser(t, dbSession, "test-starfleet-id", org, []string{authz.ProviderAdminRole})
	ip := common.TestBuildInfrastructureProvider(t, dbSession, "Test Infrastructure Provider", org, user)
	site := common.TestBuildSite(t, dbSession, ip, "Test Site", user)
	_, err := cdbm.NewSiteDAO(dbSession).Update(context.Background(), nil, cdbm.SiteUpdateInput{
		SiteID: site.ID,
		Status: cutil.GetPtr(cdbm.SiteStatusRegistered),
	})
	require.NoError(t, err)
	it := common.TestBuildInstanceType(t, dbSession, "test-instance-type", cutil.GetPtr(site.ID), site, nil, user)
	machine := common.TestBuildMachine(t, dbSession, ip, site, &it.ID, cutil.GetPtr("test-controller-machine-type"), cdbm.MachineStatusReady)

	dpuID := "dpu-machine-1"
	if withDpu {
		_, ierr := cdbm.NewMachineInterfaceDAO(dbSession).Create(context.Background(), nil, cdbm.MachineInterfaceCreateInput{
			MachineID:            machine.ID,
			MacAddress:           cutil.GetPtr("02:00:00:00:00:10"),
			AttachedDpuMachineID: &dpuID,
			IpAddresses:          []string{},
		})
		require.NoError(t, ierr)
	}

	calls := &[]string{}
	current := new(string)

	run := &tmocks.WorkflowRun{}
	run.On("Get", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		response, ok := responses[*current]
		if !ok || response == nil {
			return
		}
		out := args.Get(1).(*coreproxy.Response)
		respJSON, merr := protojson.Marshal(response)
		require.NoError(t, merr)
		out.ResponseJSON = respJSON
	}).Return(nil)

	tsc := &tmocks.Client{}
	tsc.On("ExecuteWorkflow", mock.Anything, mock.Anything, coreproxy.WorkflowName,
		mock.MatchedBy(func(req coreproxy.Request) bool {
			*current = req.FullMethod
			*calls = append(*calls, req.FullMethod)
			return true
		}),
	).Return(run, nil)

	scp := sc.NewClientPool(nil)
	scp.IDClientMap[site.ID.String()] = tsc

	return machinePortFixture{
		org: org, machineID: machine.ID, dpuID: dpuID,
		user: user, dbSession: dbSession, scp: scp, calls: calls,
	}
}

func (f machinePortFixture) get(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v2/org/%s/nico/machine/%s/port", f.org, f.machineID), nil)
	rec := httptest.NewRecorder()
	ec := e.NewContext(req, rec)
	ec.SetParamNames("orgName", "id")
	ec.SetParamValues(f.org, f.machineID)
	ec.Set("user", f.user)

	handler := NewGetMachinePortsHandler(f.dbSession, f.scp, common.GetTestConfig())
	require.NoError(t, handler.Handle(ec))
	return rec
}

func TestMachinePortsReportsFlapCountsAndTheFarEnd(t *testing.T) {
	f := newMachinePortFixture(t, true, map[string]proto.Message{
		cwssaws.Forge_GetAllManagedHostNetworkStatus_FullMethodName: &cwssaws.ManagedHostNetworkStatusResponse{
			All: []*cwssaws.DpuNetworkStatus{{
				DpuMachineId: &cwssaws.MachineId{Id: "dpu-machine-1"},
				FabricInterfaces: []*cwssaws.FabricInterfaceData{{
					InterfaceName: "p0",
					LinkData: &cwssaws.LinkData{
						State:            proto.String("up"),
						CarrierUp:        proto.Bool(true),
						Mtu:              proto.Uint32(9000),
						CarrierUpCount:   proto.Uint32(14),
						CarrierDownCount: proto.Uint32(13),
					},
				}},
			}},
		},
		cwssaws.Forge_FindConnectedDevicesByDpuMachineIds_FullMethodName: &cwssaws.ConnectedDeviceList{
			ConnectedDevices: []*cwssaws.ConnectedDevice{{
				Id:              &cwssaws.MachineId{Id: "dpu-machine-1"},
				LocalPort:       "p0",
				RemotePort:      "swp15",
				NetworkDeviceId: proto.String("switch-uuid-abc"),
			}},
		},
	})

	rec := f.get(t)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var ports model.APIMachinePorts
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ports))
	require.Len(t, ports.Ports, 1)

	port := ports.Ports[0]
	assert.Equal(t, "p0", port.InterfaceName)
	require.NotNil(t, port.CarrierUp)
	assert.True(t, *port.CarrierUp)
	// Up now, but it has gone down thirteen times: the case this endpoint is for.
	require.NotNil(t, port.CarrierDownCount)
	assert.Equal(t, uint32(13), *port.CarrierDownCount)
	require.NotNil(t, port.RemotePort)
	assert.Equal(t, "swp15", *port.RemotePort)

	// The switch is labelled, not identified.
	require.NotNil(t, port.SwitchGroup)
	assert.Equal(t, "switch-1", *port.SwitchGroup)
	assert.NotContains(t, rec.Body.String(), "switch-uuid-abc")

	// And the response says plainly that nothing was read from the switch.
	assert.False(t, ports.SwitchSideAvailable)
}

// Another host's DPU must not appear: Core answers for the whole Site, so the
// narrowing happens here.
func TestMachinePortsExcludesOtherHostsDpus(t *testing.T) {
	f := newMachinePortFixture(t, true, map[string]proto.Message{
		cwssaws.Forge_GetAllManagedHostNetworkStatus_FullMethodName: &cwssaws.ManagedHostNetworkStatusResponse{
			All: []*cwssaws.DpuNetworkStatus{
				{
					DpuMachineId: &cwssaws.MachineId{Id: "dpu-machine-1"},
					FabricInterfaces: []*cwssaws.FabricInterfaceData{
						{InterfaceName: "p0", LinkData: &cwssaws.LinkData{CarrierUp: proto.Bool(true)}},
					},
				},
				{
					DpuMachineId: &cwssaws.MachineId{Id: "someone-elses-dpu"},
					FabricInterfaces: []*cwssaws.FabricInterfaceData{
						{InterfaceName: "p9", LinkData: &cwssaws.LinkData{CarrierUp: proto.Bool(false)}},
					},
				},
			},
		},
	})

	rec := f.get(t)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var ports model.APIMachinePorts
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ports))
	require.Len(t, ports.Ports, 1)
	assert.Equal(t, "p0", ports.Ports[0].InterfaceName)
	assert.NotContains(t, rec.Body.String(), "p9")
}

// A host with no DPU has no uplink reporter, which is answered from the
// database without troubling the Site.
func TestMachinePortsAnswersWithoutTheSiteWhenNoDpuIsAttached(t *testing.T) {
	f := newMachinePortFixture(t, false, nil)

	rec := f.get(t)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var ports model.APIMachinePorts
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ports))
	assert.Empty(t, ports.Ports)
	assert.Empty(t, *f.calls, "no DPU means nothing to ask the Site")
}

// The far-end lookup is best effort: losing it must not hide a flapping link.
func TestMachinePortsStillReportsHostSideWhenTheFarEndIsUnknown(t *testing.T) {
	f := newMachinePortFixture(t, true, map[string]proto.Message{
		cwssaws.Forge_GetAllManagedHostNetworkStatus_FullMethodName: &cwssaws.ManagedHostNetworkStatusResponse{
			All: []*cwssaws.DpuNetworkStatus{{
				DpuMachineId: &cwssaws.MachineId{Id: "dpu-machine-1"},
				FabricInterfaces: []*cwssaws.FabricInterfaceData{
					{InterfaceName: "p0", LinkData: &cwssaws.LinkData{
						CarrierUp:        proto.Bool(false),
						CarrierDownCount: proto.Uint32(41),
					}},
				},
			}},
		},
		// No answer for the connected-devices call.
	})

	rec := f.get(t)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var ports model.APIMachinePorts
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ports))
	require.Len(t, ports.Ports, 1)
	require.NotNil(t, ports.Ports[0].CarrierDownCount)
	assert.Equal(t, uint32(41), *ports.Ports[0].CarrierDownCount)
	assert.Nil(t, ports.Ports[0].RemotePort)
}

func TestMachinePortsIsReachableByTheTenantHoldingTheMachine(t *testing.T) {
	f := newMachinePortFixture(t, false, nil)
	grant := testGrantTenantOnMachine(t, f.dbSession, f.machineID, true, false)
	f.org, f.user = grant.org, grant.user

	rec := f.get(t)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestMachinePortsHidesAMachineTheTenantDoesNotHold(t *testing.T) {
	f := newMachinePortFixture(t, false, nil)
	grant := testGrantTenantOnMachine(t, f.dbSession, f.machineID, false, false)
	f.org, f.user = grant.org, grant.user

	rec := f.get(t)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Could not find Machine")
}
