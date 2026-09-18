// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"errors"
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

	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/handler/util/common"
	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/model"
	cdbm "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/model"
	flowv1 "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/flow/protobuf/v1"
)

// setMachineHealth records a health report on the Machine, the way the
// platform's own sources do.
func setMachineHealth(t *testing.T, f rackAccessFixture, machineID, source string, alerts []map[string]any) {
	t.Helper()
	health := map[string]interface{}{"source": source, "alerts": alerts}
	_, err := cdbm.NewMachineDAO(f.dbSession).Update(context.Background(), nil, cdbm.MachineUpdateInput{
		MachineID: machineID,
		Health:    health,
	})
	require.NoError(t, err)
}

// machineInHeldRack returns the Machine the fixture placed in the held Rack.
func machineInHeldRack(t *testing.T, f rackAccessFixture) string {
	t.Helper()
	expected, _, err := cdbm.NewExpectedMachineDAO(f.dbSession).GetAll(context.Background(), nil,
		cdbm.ExpectedMachineFilterInput{RackIDs: []string{f.heldRackID}}, paging(), nil)
	require.NoError(t, err)
	require.Len(t, expected, 1)
	require.NotNil(t, expected[0].MachineID)
	return *expected[0].MachineID
}

// getIssues drives the Rack issues handler, optionally with components on the
// Rack read, or with that read failing.
func (f rackAccessFixture) getIssues(t *testing.T, org string, user *cdbm.User, rackID string, components []*flowv1.Component, rackReadFails bool) *httptest.ResponseRecorder {
	t.Helper()

	tsc := &tmocks.Client{}
	if rackReadFails {
		tsc.Mock.On("ExecuteWorkflow", mock.Anything, mock.Anything, "GetRack", mock.Anything).
			Return(nil, errors.New("site unreachable"))
	} else {
		run := &tmocks.WorkflowRun{}
		run.On("GetID").Return("rack-issues-workflow")
		run.Mock.On("Get", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
			resp := args.Get(1).(*flowv1.GetRackInfoResponse)
			resp.Rack = &flowv1.Rack{
				Info:       &flowv1.DeviceInfo{Id: &flowv1.UUID{Id: rackID}, Name: "Rack-001"},
				Components: components,
			}
		}).Return(nil)
		tsc.Mock.On("ExecuteWorkflow", mock.Anything, mock.Anything, "GetRack", mock.Anything).Return(run, nil)
	}
	f.scp.IDClientMap[f.siteID.String()] = tsc

	q := url.Values{}
	q.Set("siteId", f.siteID.String())

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v2/org/%s/nico/rack/%s/issues?%s", org, rackID, q.Encode()), nil)
	rec := httptest.NewRecorder()
	ec := e.NewContext(req, rec)
	ec.SetParamNames("orgName", "id")
	ec.SetParamValues(org, rackID)
	ec.Set("user", user)

	handler := NewGetRackIssuesHandler(f.dbSession, nil, f.scp, common.GetTestConfig())
	require.NoError(t, handler.Handle(ec))
	return rec
}

func leakingTray(trayIdx int32) *flowv1.Component {
	return &flowv1.Component{
		Type:       flowv1.ComponentType_COMPONENT_TYPE_NVSWITCH,
		Position:   &flowv1.RackPosition{TrayIdx: trayIdx},
		LeakStatus: flowv1.LeakStatus_LEAK_STATUS_DETECTED,
	}
}

func TestRackIssuesGathersHostAlertsAndLeakingComponents(t *testing.T) {
	f := newRackAccessFixture(t)
	machineID := machineInHeldRack(t, f)
	setMachineHealth(t, f, machineID, "machine-validation", []map[string]any{{
		"id":             "MemoryLatency",
		"target":         "socket1",
		"in_alert_since": "2026-09-18T11:04:00Z",
		"message":        "latency above threshold on socket 1",
	}})

	rec := f.getIssues(t, f.org, f.providerUser, f.heldRackID, []*flowv1.Component{
		leakingTray(10),
		{Type: flowv1.ComponentType_COMPONENT_TYPE_COMPUTE, LeakStatus: flowv1.LeakStatus_LEAK_STATUS_NOT_DETECTED},
	}, false)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var issues model.APIRackIssues
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &issues))
	assert.Equal(t, f.heldRackID, issues.RackID)
	assert.Equal(t, 1, issues.Hosts)
	require.Equal(t, 2, issues.OpenCount, rec.Body.String())

	// Hosts sort before components.
	require.NotNil(t, issues.Issues[0].MachineID)
	assert.Equal(t, machineID, *issues.Issues[0].MachineID)
	assert.Equal(t, "machine-validation", issues.Issues[0].Source)
	assert.Equal(t, "MemoryLatency", issues.Issues[0].ID)
	assert.Contains(t, issues.Issues[0].Message, "socket 1")
	// firstObserved is deliberately not asserted: the cached health report is
	// decoded by field name, so its snake_case `in_alert_since` key does not
	// reach InAlertSince. The field is carried when a source populates it.

	require.NotNil(t, issues.Issues[1].Component)
	assert.Contains(t, *issues.Issues[1].Component, "10")
	assert.Equal(t, model.LeakDetectedAlertID, issues.Issues[1].ID)
}

func TestRackIssuesTreatsUnobservedHealthAsUnknownNotHealthy(t *testing.T) {
	f := newRackAccessFixture(t)

	rec := f.getIssues(t, f.org, f.providerUser, f.heldRackID, nil, false)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var issues model.APIRackIssues
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &issues))
	assert.Equal(t, 1, issues.Hosts)
	assert.Equal(t, 0, issues.OpenCount)
	assert.Empty(t, issues.Issues)
}

func TestRackIssuesStillReportsHostsWhenTheRackReadFails(t *testing.T) {
	f := newRackAccessFixture(t)
	machineID := machineInHeldRack(t, f)
	setMachineHealth(t, f, machineID, "fabric-witness", []map[string]any{{
		"id":      "FabricWitnessMismatch",
		"message": "fabric refused to attach port",
	}})

	rec := f.getIssues(t, f.org, f.providerUser, f.heldRackID, nil, true)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var issues model.APIRackIssues
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &issues))
	require.Equal(t, 1, issues.OpenCount)
	assert.Equal(t, "FabricWitnessMismatch", issues.Issues[0].ID)
}

func TestRackIssuesIsReachableByTheTenantHoldingTheRack(t *testing.T) {
	f := newRackAccessFixture(t)
	machineID := machineInHeldRack(t, f)
	setMachineHealth(t, f, machineID, "machine-validation", []map[string]any{{
		"id": "MemoryLatency", "message": "latency above threshold",
	}})

	rec := f.getIssues(t, f.tenantOrg, f.tenantUser, f.heldRackID, nil, false)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var issues model.APIRackIssues
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &issues))
	assert.Equal(t, 1, issues.OpenCount)
}

func TestRackIssuesHidesARackTheTenantDoesNotHold(t *testing.T) {
	f := newRackAccessFixture(t)

	rec := f.getIssues(t, f.tenantOrg, f.tenantUser, f.otherRackID, nil, false)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Could not find Rack")
}
