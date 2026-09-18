// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/handler/util/common"
	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/model"
	cutil "github.com/NVIDIA/infra-controller/rest-api/common/pkg/util"
	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

func TestMachineRepairRequestRecordsAReservedHealthSource(t *testing.T) {
	fixture := common.NewTestSetupProviderMachineHandlerFixture(t, nil)
	handler := NewMachineRepairRequestHandler(fixture.DBSession, fixture.SiteClientPool, fixture.Config)

	rec := fixture.Request(t, handler.Handle, http.MethodPost, "/", model.APIMachineRepairRequest{
		Summary:   "GPU 3 reports XID 79 on every job",
		Target:    cutil.GetPtr(model.RepairTargetGPU),
		Urgency:   cutil.GetPtr(model.RepairUrgencyHigh),
		Component: cutil.GetPtr("3"),
	}, "")
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, cwssaws.Forge_InsertMachineHealthReport_FullMethodName, fixture.ProxiedReq.FullMethod)

	var coreReq cwssaws.InsertMachineHealthReportRequest
	require.NoError(t, protojson.Unmarshal(fixture.ProxiedReq.RequestJSON, &coreReq))
	assert.Equal(t, fixture.MachineID, coreReq.GetMachineId().GetId())

	entry := coreReq.GetHealthReportEntry()
	assert.Equal(t, cwssaws.HealthReportApplyMode_Merge, entry.GetMode())
	assert.Equal(t, model.RepairRequestSource, entry.GetReport().GetSource())
	require.Len(t, entry.GetReport().GetAlerts(), 1)

	alert := entry.GetReport().GetAlerts()[0]
	assert.Equal(t, model.RepairRequestAlertID, alert.GetId())
	assert.Equal(t, "GPU/3", alert.GetTarget())
	assert.Contains(t, alert.GetMessage(), "HIGH")
	assert.Contains(t, alert.GetMessage(), "XID 79")

	var resp model.APIMachineRepairRequestResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, model.RepairRequestStatusSubmitted, resp.Status)
	assert.Equal(t, model.RepairTargetGPU, resp.Target)
	assert.Nil(t, resp.TicketRef)
}

func TestMachineRepairRequestDefaultsToTheWholeHost(t *testing.T) {
	fixture := common.NewTestSetupProviderMachineHandlerFixture(t, nil)
	handler := NewMachineRepairRequestHandler(fixture.DBSession, fixture.SiteClientPool, fixture.Config)

	rec := fixture.Request(t, handler.Handle, http.MethodPost, "/", model.APIMachineRepairRequest{
		Summary: "host stops responding under sustained load",
	}, "")
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var coreReq cwssaws.InsertMachineHealthReportRequest
	require.NoError(t, protojson.Unmarshal(fixture.ProxiedReq.RequestJSON, &coreReq))
	alert := coreReq.GetHealthReportEntry().GetReport().GetAlerts()[0]
	assert.Equal(t, model.RepairTargetHost, alert.GetTarget())
	assert.Contains(t, alert.GetMessage(), "NORMAL")

	var resp model.APIMachineRepairRequestResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, model.RepairTargetHost, resp.Target)
	assert.Equal(t, model.RepairUrgencyNormal, resp.Urgency)
}

func TestMachineRepairRequestAllowsTenantHoldingInstance(t *testing.T) {
	fixture := common.NewTestSetupProviderMachineHandlerFixture(t, nil)
	grant := testGrantTenantOnMachine(t, fixture.DBSession, fixture.MachineID, true, false)
	fixture.Org, fixture.User = grant.org, grant.user
	handler := NewMachineRepairRequestHandler(fixture.DBSession, fixture.SiteClientPool, fixture.Config)

	rec := fixture.Request(t, handler.Handle, http.MethodPost, "/", model.APIMachineRepairRequest{
		Summary: "one of the front-end links keeps flapping",
		Target:  cutil.GetPtr(model.RepairTargetPort),
	}, "")
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, cwssaws.Forge_InsertMachineHealthReport_FullMethodName, fixture.ProxiedReq.FullMethod)
}

func TestMachineRepairRequestHidesMachineFromTenantWithoutInstance(t *testing.T) {
	fixture := common.NewTestSetupProviderMachineHandlerFixture(t, nil)
	grant := testGrantTenantOnMachine(t, fixture.DBSession, fixture.MachineID, false, false)
	fixture.Org, fixture.User = grant.org, grant.user
	handler := NewMachineRepairRequestHandler(fixture.DBSession, fixture.SiteClientPool, fixture.Config)

	rec := fixture.Request(t, handler.Handle, http.MethodPost, "/", model.APIMachineRepairRequest{
		Summary: "this host is not mine to report on",
	}, "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, fixture.ProxiedReq.FullMethod)
}

func TestMachineRepairRequestRejectsAnEmptyOrUnknownRequest(t *testing.T) {
	for _, tc := range []struct {
		desc string
		body model.APIMachineRepairRequest
	}{
		{desc: "no summary", body: model.APIMachineRepairRequest{}},
		{desc: "summary too short", body: model.APIMachineRepairRequest{Summary: "broken"}},
		{desc: "unknown target", body: model.APIMachineRepairRequest{Summary: "a real description", Target: cutil.GetPtr("CHASSIS")}},
		{desc: "unknown urgency", body: model.APIMachineRepairRequest{Summary: "a real description", Urgency: cutil.GetPtr("YESTERDAY")}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			fixture := common.NewTestSetupProviderMachineHandlerFixture(t, nil)
			handler := NewMachineRepairRequestHandler(fixture.DBSession, fixture.SiteClientPool, fixture.Config)

			rec := fixture.Request(t, handler.Handle, http.MethodPost, "/", tc.body, "")
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Empty(t, fixture.ProxiedReq.FullMethod)
		})
	}
}

func TestWithdrawMachineRepairRequestRemovesOnlyTheReservedSource(t *testing.T) {
	fixture := common.NewTestSetupProviderMachineHandlerFixture(t, nil)
	grant := testGrantTenantOnMachine(t, fixture.DBSession, fixture.MachineID, true, false)
	fixture.Org, fixture.User = grant.org, grant.user
	handler := NewWithdrawMachineRepairRequestHandler(fixture.DBSession, fixture.SiteClientPool, fixture.Config)

	rec := fixture.Request(t, handler.Handle, http.MethodDelete, "/", nil, "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, cwssaws.Forge_RemoveMachineHealthReport_FullMethodName, fixture.ProxiedReq.FullMethod)
	assert.Contains(t, string(fixture.ProxiedReq.RequestJSON), model.RepairRequestSource)
}
