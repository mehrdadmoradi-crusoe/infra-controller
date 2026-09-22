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

// The event log walk makes one BMC lookup and then several Redfish reads, so
// the fixture answers the browse calls by the URI asked for.

type eventLogFixture struct {
	org       string
	machineID string
	user      *cdbm.User
	dbSession *cdb.Session
	scp       *sc.ClientPool
	// siteID lets a test swap the Site's proxy client, so a Core failure can
	// be injected -- see newCoreProxyMock.
	siteID string
	// browsed is every Redfish URI requested, in order.
	browsed *[]string
}

// newEventLogFixture builds a Provider-owned Machine whose controller answers
// the URIs in bodies. A URI with no entry returns an empty body, which is how
// a controller that has no such resource behaves here.
func newEventLogFixture(t *testing.T, bodies map[string]string) eventLogFixture {
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

	browsed := &[]string{}
	pending := new(coreproxy.Request)

	run := &tmocks.WorkflowRun{}
	run.On("Get", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		out := args.Get(1).(*coreproxy.Response)

		var response proto.Message
		switch pending.FullMethod {
		case cwssaws.Forge_FindBmcIps_FullMethodName:
			response = &cwssaws.BmcIpList{BmcIps: []string{"10.0.0.5"}}
		case cwssaws.Forge_RedfishBrowse_FullMethodName:
			var req cwssaws.RedfishBrowseRequest
			require.NoError(t, protojson.Unmarshal(pending.RequestJSON, &req))
			*browsed = append(*browsed, req.GetUri())
			response = &cwssaws.RedfishBrowseResponse{Text: bodies[req.GetUri()]}
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
			*pending = req
			return true
		}),
	).Return(run, nil)

	scp := sc.NewClientPool(nil)
	scp.IDClientMap[site.ID.String()] = tsc

	return eventLogFixture{
		org:       org,
		machineID: machine.ID,
		user:      user,
		dbSession: dbSession,
		scp:       scp,
		siteID:    site.ID.String(),
		browsed:   browsed,
	}
}

func (f eventLogFixture) get(t *testing.T, query map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	q := url.Values{}
	for k, v := range query {
		q.Set(k, v)
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v2/org/%s/nico/machine/%s/event-log?%s", f.org, f.machineID, q.Encode()), nil)
	rec := httptest.NewRecorder()
	ec := e.NewContext(req, rec)
	ec.SetParamNames("orgName", "id")
	ec.SetParamValues(f.org, f.machineID)
	ec.Set("user", f.user)

	handler := NewGetMachineEventLogHandler(f.dbSession, f.scp, common.GetTestConfig())
	require.NoError(t, handler.Handle(ec))
	return rec
}

// A controller whose system is named `Self` rather than `1`: the walk follows
// the controller's own links, so the vendor's naming does not matter.
func systemLogBodies() map[string]string {
	return map[string]string{
		"https://10.0.0.5/redfish/v1/Systems":                  `{"Members":[{"@odata.id":"/redfish/v1/Systems/Self"}]}`,
		"https://10.0.0.5/redfish/v1/Systems/Self/LogServices": `{"Members":[{"@odata.id":"/redfish/v1/Systems/Self/LogServices/SEL"}]}`,
		"https://10.0.0.5/redfish/v1/Systems/Self/LogServices/SEL/Entries": `{
			"Members@odata.count": 2,
			"Members": [
				{"Id":"1","Created":"2026-09-18T09:00:00Z","Severity":"Warning","Message":"Correctable memory error on DIMM A1","MessageId":"Alert.1.0.MemoryError","EntryType":"SEL"},
				{"Id":"2","Created":"2026-09-18T11:30:00Z","Severity":"Critical","Message":"Power supply 2 failed","MessageId":"Alert.1.0.PowerSupplyFailure","EntryType":"SEL"}
			]}`,
	}
}

func TestMachineEventLogNormalisesEntriesNewestFirst(t *testing.T) {
	f := newEventLogFixture(t, systemLogBodies())

	rec := f.get(t, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var log model.APIEventLog
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &log))
	assert.Equal(t, f.machineID, log.MachineID)
	assert.Equal(t, model.EventLogSourceSystem, log.Source)
	assert.Equal(t, []string{"SEL"}, log.Services)
	assert.False(t, log.Truncated)

	require.Len(t, log.Entries, 2)
	// Newest first: the power supply failure is later than the memory error.
	assert.Equal(t, "2", log.Entries[0].ID)
	require.NotNil(t, log.Entries[0].MessageID)
	assert.Equal(t, "Alert.1.0.PowerSupplyFailure", *log.Entries[0].MessageID)
	assert.Equal(t, "SEL", log.Entries[0].Service)
	assert.Equal(t, "1", log.Entries[1].ID)
}

// The caller names a source, never a path, and the handler builds every URI.
func TestMachineEventLogBuildsItsOwnRedfishPaths(t *testing.T) {
	f := newEventLogFixture(t, systemLogBodies())

	rec := f.get(t, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.Equal(t, []string{
		"https://10.0.0.5/redfish/v1/Systems",
		"https://10.0.0.5/redfish/v1/Systems/Self/LogServices",
		"https://10.0.0.5/redfish/v1/Systems/Self/LogServices/SEL/Entries",
	}, *f.browsed)
}

func TestMachineEventLogReadsTheManagerLogWhenAsked(t *testing.T) {
	f := newEventLogFixture(t, map[string]string{
		"https://10.0.0.5/redfish/v1/Managers":                                 `{"Members":[{"@odata.id":"/redfish/v1/Managers/BMC"}]}`,
		"https://10.0.0.5/redfish/v1/Managers/BMC/LogServices":                 `{"Members":[{"@odata.id":"/redfish/v1/Managers/BMC/LogServices/Journal"}]}`,
		"https://10.0.0.5/redfish/v1/Managers/BMC/LogServices/Journal/Entries": `{"Members":[{"Id":"9","Message":"BMC reset requested"}]}`,
	})

	rec := f.get(t, map[string]string{"source": model.EventLogSourceManager})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var log model.APIEventLog
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &log))
	assert.Equal(t, model.EventLogSourceManager, log.Source)
	assert.Equal(t, []string{"Journal"}, log.Services)
	require.Len(t, log.Entries, 1)
	assert.Equal(t, "BMC reset requested", log.Entries[0].Message)
}

func TestMachineEventLogRejectsAnUnknownSource(t *testing.T) {
	f := newEventLogFixture(t, nil)

	rec := f.get(t, map[string]string{"source": "switch"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Empty(t, *f.browsed, "nothing should reach the controller for an unknown source")
}

func TestMachineEventLogRejectsAnOutOfRangeLimit(t *testing.T) {
	f := newEventLogFixture(t, nil)

	for _, bad := range []string{"0", "-5", "5000", "many"} {
		rec := f.get(t, map[string]string{"limit": bad})
		assert.Equal(t, http.StatusBadRequest, rec.Code, bad)
	}
	assert.Empty(t, *f.browsed)
}

// A short list must not read as a quiet host: when the controller holds more
// than was returned, the response says so.
func TestMachineEventLogMarksATruncatedRead(t *testing.T) {
	f := newEventLogFixture(t, systemLogBodies())

	rec := f.get(t, map[string]string{"limit": "1"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var log model.APIEventLog
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &log))
	require.Len(t, log.Entries, 1)
	assert.True(t, log.Truncated)
}

// A link that leaves the controller's Redfish tree is not followed.
func TestMachineEventLogIgnoresLinksOutsideTheRedfishTree(t *testing.T) {
	f := newEventLogFixture(t, map[string]string{
		"https://10.0.0.5/redfish/v1/Systems": `{"Members":[
			{"@odata.id":"http://attacker.example/steal"},
			{"@odata.id":"/redfish/v1/Systems/1"}]}`,
		"https://10.0.0.5/redfish/v1/Systems/1/LogServices": `{"Members":[]}`,
	})

	rec := f.get(t, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	for _, uri := range *f.browsed {
		assert.NotContains(t, uri, "attacker.example")
	}
}

// A controller with no log services is a host with an empty log, not an error.
func TestMachineEventLogReturnsEmptyWhenNoServicesExist(t *testing.T) {
	f := newEventLogFixture(t, map[string]string{
		"https://10.0.0.5/redfish/v1/Systems":               `{"Members":[{"@odata.id":"/redfish/v1/Systems/1"}]}`,
		"https://10.0.0.5/redfish/v1/Systems/1/LogServices": `{"Members":[]}`,
	})

	rec := f.get(t, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var log model.APIEventLog
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &log))
	assert.Empty(t, log.Entries)
	assert.Empty(t, log.Services)
	assert.False(t, log.Truncated)
}

// Reading a controller's log takes several Core calls: the BMC address, then
// a walk down the Redfish tree. A failure at the first step is not the same as
// a failure partway through the walk, and neither should be reported as an
// empty log -- a caller reading "no entries" must not be looking at a read
// that never happened.
func TestMachineEventLogReportsACoreFailureRatherThanAnEmptyLog(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
	}{
		{"the BMC address cannot be resolved", cwssaws.Forge_FindBmcIps_FullMethodName},
		{"the controller cannot be read", cwssaws.Forge_RedfishBrowse_FullMethodName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEventLogFixture(t, nil)
			tsc, _ := newCoreProxyMock(t,
				map[string]proto.Message{
					cwssaws.Forge_FindBmcIps_FullMethodName: bmcIPs("10.0.0.5"),
				},
				map[string]error{tc.method: errors.New("core is unavailable")})
			f.scp.IDClientMap[f.siteID] = tsc

			rec := f.get(t, nil)

			assert.GreaterOrEqual(t, rec.Code, 400,
				"a read that did not happen must not be reported as a successful empty log: %s", rec.Body.String())
		})
	}
}
