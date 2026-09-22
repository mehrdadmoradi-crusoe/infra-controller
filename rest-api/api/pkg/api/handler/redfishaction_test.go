// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	tmocks "go.temporal.io/sdk/mocks"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

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

// The action endpoints make two Core calls: the Machine's BMC address is
// resolved first, then the action itself. The shared Machine fixture answers
// every call with one fixed response, so these tests use a responder keyed by
// method and record each call in order.

type redfishFixture struct {
	org       string
	machineID string
	siteID    string
	user      *cdbm.User
	dbSession *cdb.Session
	scp       *sc.ClientPool
	// calls is every Core method invoked, in order, so a test can assert that
	// the address was resolved before the action was sent.
	calls *[]string
	// lastByMethod is the last request seen for each Core method.
	lastByMethod map[string]*coreproxy.Request
}

// newRedfishFixture builds a Provider-owned Machine on a registered Site, with
// Core answering each method from responses.
func newRedfishFixture(t *testing.T, responses map[string]proto.Message) redfishFixture {
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

	calls := &[]string{}
	lastByMethod := map[string]*coreproxy.Request{}

	// The proxy is synchronous: ExecuteWorkflow for a call is immediately
	// followed by Get for the same call, so the method recorded on the way in
	// is the one whose response Get should serve.
	current := new(string)

	wrun := &tmocks.WorkflowRun{}
	wrun.On("Get", mock.Anything, mock.Anything).Run(func(getArgs mock.Arguments) {
		response, ok := responses[*current]
		if !ok || response == nil {
			return
		}
		out := getArgs.Get(1).(*coreproxy.Response)
		respJSON, err := protojson.Marshal(response)
		require.NoError(t, err)
		out.ResponseJSON = respJSON
	}).Return(nil)

	tsc := &tmocks.Client{}
	tsc.On("ExecuteWorkflow", mock.Anything, mock.Anything, coreproxy.WorkflowName,
		mock.MatchedBy(func(req coreproxy.Request) bool {
			*calls = append(*calls, req.FullMethod)
			captured := req
			lastByMethod[req.FullMethod] = &captured
			*current = req.FullMethod
			return true
		}),
	).Return(wrun, nil)

	scp := sc.NewClientPool(nil)
	scp.IDClientMap[site.ID.String()] = tsc

	return redfishFixture{
		org:          org,
		machineID:    machine.ID,
		siteID:       site.ID.String(),
		user:         user,
		dbSession:    dbSession,
		scp:          scp,
		calls:        calls,
		lastByMethod: lastByMethod,
	}
}

// request drives a handler, optionally with a requestId path parameter.
func (f redfishFixture) request(t *testing.T, handler echo.HandlerFunc, method string, body any, requestID string) *httptest.ResponseRecorder {
	t.Helper()

	var reqBody string
	if body != nil {
		bodyBytes, err := json.Marshal(body)
		require.NoError(t, err)
		reqBody = string(bodyBytes)
	}

	e := echo.New()
	req := httptest.NewRequest(method, "/", strings.NewReader(reqBody))
	if body != nil {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	rec := httptest.NewRecorder()
	ec := e.NewContext(req, rec)

	names := []string{"orgName", "id"}
	values := []string{f.org, f.machineID}
	if requestID != "" {
		names = append(names, "requestId")
		values = append(values, requestID)
	}
	ec.SetParamNames(names...)
	ec.SetParamValues(values...)
	ec.Set("user", f.user)

	require.NoError(t, handler(ec))
	return rec
}

func bmcIPs(ips ...string) proto.Message {
	return &cwssaws.BmcIpList{BmcIps: ips}
}

func TestCreateRedfishActionResolvesTheBmcThenRecordsTheRequest(t *testing.T) {
	f := newRedfishFixture(t, map[string]proto.Message{
		cwssaws.Forge_FindBmcIps_FullMethodName:          bmcIPs("10.0.0.5"),
		cwssaws.Forge_RedfishCreateAction_FullMethodName: &cwssaws.RedfishCreateActionResponse{RequestId: 42},
	})
	handler := NewCreateRedfishActionHandler(f.dbSession, f.scp, common.GetTestConfig())

	rec := f.request(t, handler.Handle, http.MethodPost, model.APIRedfishActionRequest{
		Action:     "Bios.ChangeSettings",
		Target:     "/redfish/v1/Systems/1/Bios/Settings",
		Parameters: `{"Attributes":{"BootMode":"Uefi"}}`,
	}, "")
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	// The address is resolved before the action is sent.
	require.Equal(t, []string{
		cwssaws.Forge_FindBmcIps_FullMethodName,
		cwssaws.Forge_RedfishCreateAction_FullMethodName,
	}, *f.calls)

	var coreReq cwssaws.RedfishCreateActionRequest
	require.NoError(t, protojson.Unmarshal(f.lastByMethod[cwssaws.Forge_RedfishCreateAction_FullMethodName].RequestJSON, &coreReq))
	assert.Equal(t, []string{"10.0.0.5"}, coreReq.GetIps())
	assert.Equal(t, "Bios.ChangeSettings", coreReq.GetAction())
	// The parameters reach Core byte for byte, so what an approver reads is
	// what the BMC receives.
	assert.JSONEq(t, `{"Attributes":{"BootMode":"Uefi"}}`, coreReq.GetParameters())

	var resp model.APIRedfishActionCreated
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, int64(42), resp.RequestID)
	assert.Equal(t, model.RedfishActionPending, resp.Status)
}

// A Machine with no hardware address is looked up by its serial instead, which
// is what a Machine from an expected build carries before it is seen.
func TestCreateRedfishActionFallsBackToTheChassisSerial(t *testing.T) {
	f := newRedfishFixture(t, map[string]proto.Message{
		cwssaws.Forge_FindBmcIps_FullMethodName:          bmcIPs("10.0.0.5"),
		cwssaws.Forge_RedfishCreateAction_FullMethodName: &cwssaws.RedfishCreateActionResponse{RequestId: 7},
	})
	handler := NewCreateRedfishActionHandler(f.dbSession, f.scp, common.GetTestConfig())

	rec := f.request(t, handler.Handle, http.MethodPost, model.APIRedfishActionRequest{
		Action: "Manager.Reset", Target: "/redfish/v1/Managers/1/Actions/Manager.Reset",
	}, "")
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var lookup cwssaws.FindBmcIpsRequest
	require.NoError(t, protojson.Unmarshal(f.lastByMethod[cwssaws.Forge_FindBmcIps_FullMethodName].RequestJSON, &lookup))
	assert.NotEmpty(t, lookup.GetSerial(), "expected the serial branch of the lookup")
	assert.Empty(t, lookup.GetMacAddress())
}

func TestCreateRedfishActionRejectsAnEmptyAction(t *testing.T) {
	f := newRedfishFixture(t, nil)
	handler := NewCreateRedfishActionHandler(f.dbSession, f.scp, common.GetTestConfig())

	rec := f.request(t, handler.Handle, http.MethodPost, model.APIRedfishActionRequest{
		Target: "/redfish/v1/Systems/1",
	}, "")
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Empty(t, *f.calls, "nothing should reach Core for an invalid request")
}

// With no BMC discovered there is nothing to address, and that is a state of
// the Machine rather than a bad request.
func TestCreateRedfishActionConflictsWhenNoBmcIsKnown(t *testing.T) {
	f := newRedfishFixture(t, map[string]proto.Message{
		cwssaws.Forge_FindBmcIps_FullMethodName: bmcIPs(),
	})
	handler := NewCreateRedfishActionHandler(f.dbSession, f.scp, common.GetTestConfig())

	rec := f.request(t, handler.Handle, http.MethodPost, model.APIRedfishActionRequest{
		Action: "Manager.Reset", Target: "/redfish/v1/Managers/1/Actions/Manager.Reset",
	}, "")
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.NotContains(t, *f.calls, cwssaws.Forge_RedfishCreateAction_FullMethodName)
}

func TestListRedfishActionsDerivesStatusAndDropsManagementAddresses(t *testing.T) {
	approved := timestamppb.New(time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC))
	f := newRedfishFixture(t, map[string]proto.Message{
		cwssaws.Forge_FindBmcIps_FullMethodName: bmcIPs("10.0.0.5"),
		cwssaws.Forge_RedfishListActions_FullMethodName: &cwssaws.RedfishListActionsResponse{
			Actions: []*cwssaws.RedfishAction{
				{RequestId: 1, Requester: "alice@customer.example", Action: "Bios.ChangeSettings",
					Target: "/redfish/v1/Systems/1/Bios/Settings", Parameters: "{}",
					MachineIps: []string{"10.0.0.5"}, BoardSerials: []string{"BOARD-1"}},
				{RequestId: 2, Requester: "alice@customer.example", Action: "Manager.Reset",
					Target: "/redfish/v1/Managers/1", Parameters: "{}",
					Approvers: []string{"ops@crusoe.example"}, ApproverDates: []*timestamppb.Timestamp{approved}},
			},
		},
	})
	handler := NewListRedfishActionsHandler(f.dbSession, f.scp, common.GetTestConfig())

	rec := f.request(t, handler.Handle, http.MethodGet, nil, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var actions []model.APIRedfishAction
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &actions))
	require.Len(t, actions, 2)

	assert.Equal(t, model.RedfishActionPending, actions[0].Status)
	assert.Empty(t, actions[0].Approvers)

	// One approval is not enough: Core requires two before an apply is allowed,
	// so a single approval is AwaitingApproval rather than Approved.
	assert.Equal(t, model.RedfishActionAwaitingApproval, actions[1].Status)
	assert.Equal(t, 1, actions[1].ApprovalsHeld)
	assert.Equal(t, model.RedfishActionRequiredApprovals, actions[1].ApprovalsRequired)
	assert.Equal(t, []string{"ops@crusoe.example"}, actions[1].Approvers)
	require.Len(t, actions[1].ApprovedAt, 1)

	// The management network is not exposed: no BMC address or board serial
	// appears anywhere in the response.
	assert.NotContains(t, rec.Body.String(), "10.0.0.5")
	assert.NotContains(t, rec.Body.String(), "BOARD-1")
}

func TestApproveRedfishActionIsRefusedForATenant(t *testing.T) {
	f := newRedfishFixture(t, nil)
	grant := testGrantTenantOnMachine(t, f.dbSession, f.machineID, true, false)
	f.org, f.user = grant.org, grant.user

	handler := NewApproveRedfishActionHandler(f.dbSession, f.scp, common.GetTestConfig())
	rec := f.request(t, handler.Handle, http.MethodPost, nil, "42")

	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Empty(t, *f.calls, "a refused approval must not reach Core")
}

func TestApproveAndApplyReachCoreForTheProvider(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler func(f redfishFixture) RedfishActionHandler
		method  string
	}{
		{"approve", func(f redfishFixture) RedfishActionHandler {
			return NewApproveRedfishActionHandler(f.dbSession, f.scp, common.GetTestConfig())
		}, cwssaws.Forge_RedfishApproveAction_FullMethodName},
		{"apply", func(f redfishFixture) RedfishActionHandler {
			return NewApplyRedfishActionHandler(f.dbSession, f.scp, common.GetTestConfig())
		}, cwssaws.Forge_RedfishApplyAction_FullMethodName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRedfishFixture(t, nil)
			rec := f.request(t, tc.handler(f).Handle, http.MethodPost, nil, "42")
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

			var coreReq cwssaws.RedfishActionID
			require.NoError(t, protojson.Unmarshal(f.lastByMethod[tc.method].RequestJSON, &coreReq))
			assert.Equal(t, int64(42), coreReq.GetRequestId())

			// Approving does not need the Machine's address, so it is not looked up.
			assert.NotContains(t, *f.calls, cwssaws.Forge_FindBmcIps_FullMethodName)
		})
	}
}

// Withdrawing a pending request takes a change away rather than making one, so
// the Tenant that asked may do it.
func TestCancelRedfishActionIsAllowedForATenant(t *testing.T) {
	f := newRedfishFixture(t, nil)
	grant := testGrantTenantOnMachine(t, f.dbSession, f.machineID, true, false)
	f.org, f.user = grant.org, grant.user

	handler := NewCancelRedfishActionHandler(f.dbSession, f.scp, common.GetTestConfig())
	rec := f.request(t, handler.Handle, http.MethodPost, nil, "42")

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Contains(t, *f.calls, cwssaws.Forge_RedfishCancelAction_FullMethodName)
}

func TestRedfishActionRejectsAnUnparsableRequestID(t *testing.T) {
	f := newRedfishFixture(t, nil)
	handler := NewApproveRedfishActionHandler(f.dbSession, f.scp, common.GetTestConfig())

	for _, bad := range []string{"not-a-number", "0", "-1"} {
		rec := f.request(t, handler.Handle, http.MethodPost, nil, bad)
		assert.Equal(t, http.StatusBadRequest, rec.Code, bad)
	}
	assert.Empty(t, *f.calls)
}

// Core refuses to record a change it cannot attribute to a user, and it says
// so with a message this API has to recognise, because the gRPC code it
// arrives under is not distinguishable from any other internal failure. A bare
// 500 would read as a fault that might clear on retry. This one never does, so
// it is reported as not implemented, and the message says what is missing.
//
// The string matched here is Core's own: the Rust
// CarbideError::ClientCertificateMissingInformation formats as
// "Client certificate presented has missing information: {what}." If that
// wording changes upstream this test keeps the stale copy and so fails, which
// is the intent.
func TestUnattributableChangeIsReportedAsNotImplemented(t *testing.T) {
	for _, what := range []string{"external user info", "external user name"} {
		coreErr := cutil.NewAPIError(http.StatusInternalServerError,
			"Client certificate presented has missing information: "+what+". (type: Error, retryable: true)", nil)

		got := asUnattributable(coreErr)

		require.NotNil(t, got)
		assert.Equal(t, http.StatusNotImplemented, got.Code, what)
		// Core's own wording is not passed through: it names a retryable error
		// and a certificate the caller never presented and could not supply.
		assert.Equal(t, redfishActionUnattributable, got.Message)
		assert.NotContains(t, got.Message, "retryable")
	}
}

// Every other failure keeps the status Core gave it, so a genuine 404 or a
// timeout is not disguised as an unimplemented feature.
func TestUnattributableLeavesOtherFailuresAlone(t *testing.T) {
	assert.Nil(t, asUnattributable(nil))

	for _, tc := range []struct {
		code int
		msg  string
	}{
		{http.StatusNotFound, "Could not find Redfish action with specified ID"},
		{http.StatusGatewayTimeout, "Core proxy request timed out"},
		{http.StatusInternalServerError, "some other internal failure"},
		{http.StatusBadRequest, "insufficient approvals"},
	} {
		got := asUnattributable(cutil.NewAPIError(tc.code, tc.msg, nil))
		require.NotNil(t, got)
		assert.Equal(t, tc.code, got.Code, tc.msg)
		assert.Equal(t, tc.msg, got.Message)
	}
}

// The regression test for the defect this whole surface shipped with.
//
// Core refuses to record a change it cannot attribute, and every handler test
// above passes anyway, because the shared fixture's proxy can only succeed.
// This one injects the refusal and asserts the handler turns it into a 501
// the caller can act on, rather than the bare 500 the shared gRPC mapping
// produces for an Unauthenticated it has no case for.
func TestCreateRedfishActionReportsCoreRefusal(t *testing.T) {
	f := newRedfishFixture(t, nil)

	// Core's own wording, from CarbideError::ClientCertificateMissingInformation.
	coreRefusal := errors.New(
		"Client certificate presented has missing information: external user info. " +
			"(type: Error, retryable: true)")

	tsc, calls := newCoreProxyMock(t,
		map[string]proto.Message{
			cwssaws.Forge_FindBmcIps_FullMethodName: bmcIPs("10.0.0.5"),
		},
		map[string]error{
			cwssaws.Forge_RedfishCreateAction_FullMethodName: coreRefusal,
		})
	f.scp.IDClientMap[f.siteID] = tsc

	handler := NewCreateRedfishActionHandler(f.dbSession, f.scp, common.GetTestConfig())
	rec := f.request(t, handler.Handle, http.MethodPost, map[string]any{
		"action":     "Bios.ChangeSettings",
		"target":     "/redfish/v1/Systems/1/Bios/Settings",
		"parameters": `{"Attributes":{"BootMode":"Uefi"}}`,
	}, "")

	require.Equal(t, http.StatusNotImplemented, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), redfishActionUnattributable)
	// Core's raw wording is not passed on: it names a retryable error and a
	// certificate the caller never presented and could not supply.
	assert.NotContains(t, rec.Body.String(), "retryable")
	assert.NotContains(t, rec.Body.String(), "Client certificate presented")

	// The address was still resolved first, so the failure is the Core
	// refusing the change, not this API failing to reach it.
	assert.Equal(t, []string{
		cwssaws.Forge_FindBmcIps_FullMethodName,
		cwssaws.Forge_RedfishCreateAction_FullMethodName,
	}, coreMethodsCalled(calls))
}

// A Core failure that is not the attribution refusal keeps its own status, so
// the 501 above cannot swallow a genuine fault.
func TestCreateRedfishActionPassesOtherCoreFailuresThrough(t *testing.T) {
	f := newRedfishFixture(t, nil)

	tsc, _ := newCoreProxyMock(t,
		map[string]proto.Message{
			cwssaws.Forge_FindBmcIps_FullMethodName: bmcIPs("10.0.0.5"),
		},
		map[string]error{
			cwssaws.Forge_RedfishCreateAction_FullMethodName: errors.New("database is unavailable"),
		})
	f.scp.IDClientMap[f.siteID] = tsc

	handler := NewCreateRedfishActionHandler(f.dbSession, f.scp, common.GetTestConfig())
	rec := f.request(t, handler.Handle, http.MethodPost, map[string]any{
		"action":     "Bios.ChangeSettings",
		"target":     "/redfish/v1/Systems/1/Bios/Settings",
		"parameters": "{}",
	}, "")

	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), redfishActionUnattributable)
}
