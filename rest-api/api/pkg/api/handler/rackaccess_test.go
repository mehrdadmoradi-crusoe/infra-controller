// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	tmocks "go.temporal.io/sdk/mocks"

	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/handler/util/common"
	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/model"
	sc "github.com/NVIDIA/infra-controller/rest-api/api/pkg/client/site"
	authz "github.com/NVIDIA/infra-controller/rest-api/auth/pkg/authorization"
	cutil "github.com/NVIDIA/infra-controller/rest-api/common/pkg/util"
	cdb "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db"
	cdbm "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/model"
	cdbp "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/paginator"
	flowv1 "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/flow/protobuf/v1"
)

// rackAccessFixture holds a Site with one Rack that the Tenant reaches through
// an Instance, and a second Rack on the same Site that it does not.
type rackAccessFixture struct {
	dbSession    *cdb.Session
	org          string
	tenantOrg    string
	siteID       uuid.UUID
	providerUser *cdbm.User
	tenantUser   *cdbm.User
	strandedUser *cdbm.User
	heldRackID   string
	otherRackID  string
	scp          *sc.ClientPool
}

// newRackAccessFixture builds a Provider with a Flow-enabled Site, a Tenant in
// its own org holding one Instance on a Machine placed in heldRackID, and a
// second Tenant org with no Instances at all.
func newRackAccessFixture(t *testing.T) rackAccessFixture {
	t.Helper()
	ctx := context.Background()

	dbSession := common.TestInitDB(t)
	t.Cleanup(dbSession.Close)
	common.TestSetupSchema(t, dbSession)
	// Rack placement lives on the expected Machine record, which the shared
	// schema helper does not reset.
	require.NoError(t, dbSession.DB.ResetModel(ctx, (*cdbm.ExpectedMachine)(nil)))

	org := "provider-org"
	providerUser := common.TestBuildUser(t, dbSession, "rack-access-provider", org, []string{authz.ProviderAdminRole})
	ip := common.TestBuildInfrastructureProvider(t, dbSession, "Rack Access Provider", org, providerUser)
	site := common.TestBuildSite(t, dbSession, ip, "Rack Access Site", providerUser)

	sDAO := cdbm.NewSiteDAO(dbSession)
	site, err := sDAO.Update(ctx, nil, cdbm.SiteUpdateInput{
		SiteID: site.ID,
		Status: cutil.GetPtr(cdbm.SiteStatusRegistered),
		Config: &cdbm.SiteConfigUpdateInput{Flow: cutil.GetPtr(true)},
	})
	require.NoError(t, err)
	require.True(t, site.Config.Flow)

	tenantOrg := "tenant-org"
	tenantUser := common.TestBuildUser(t, dbSession, "rack-access-tenant", tenantOrg, []string{authz.TenantAdminRole})
	tenant := common.TestBuildTenant(t, dbSession, "Rack Access Tenant", tenantOrg, tenantUser)
	common.TestBuildTenantAccount(t, dbSession, ip, &tenant.ID, tenantOrg, cdbm.TenantAccountStatusReady, tenantUser)

	strandedOrg := "stranded-org"
	strandedUser := common.TestBuildUser(t, dbSession, "rack-access-stranded", strandedOrg, []string{authz.TenantAdminRole})
	strandedTenant := common.TestBuildTenant(t, dbSession, "Stranded Tenant", strandedOrg, strandedUser)
	common.TestBuildTenantAccount(t, dbSession, ip, &strandedTenant.ID, strandedOrg, cdbm.TenantAccountStatusReady, strandedUser)

	it := common.TestBuildInstanceType(t, dbSession, "rack-access-type", cutil.GetPtr(site.ID), site, nil, providerUser)
	machine := common.TestBuildMachine(t, dbSession, ip, site, &it.ID, cutil.GetPtr("test-controller-machine-type"), cdbm.MachineStatusReady)
	vpc := common.TestBuildVPC(t, dbSession, "rack-access-vpc", ip, tenant, site, nil, nil, nil, cdbm.VpcStatusReady, tenantUser)
	os := common.TestBuildOperatingSystem(t, dbSession, "rack-access-os", tenant, cdbm.OperatingSystemStatusReady, tenantUser)
	common.TestBuildInstance(t, dbSession, "rack-access-instance", tenant.ID, ip.ID, site.ID, it.ID, vpc.ID, &machine.ID, os.ID)

	heldRackID := uuid.New().String()
	expectedMachine := &cdbm.ExpectedMachine{
		ID:        uuid.New(),
		SiteID:    site.ID,
		MachineID: &machine.ID,
		RackID:    &heldRackID,
		CreatedBy: providerUser.ID,
	}
	_, err = dbSession.DB.NewInsert().Model(expectedMachine).Exec(ctx)
	require.NoError(t, err)

	cfg := common.GetTestConfig()
	tcfg, _ := cfg.GetTemporalConfig()

	return rackAccessFixture{
		dbSession:    dbSession,
		org:          org,
		tenantOrg:    tenantOrg,
		siteID:       site.ID,
		providerUser: providerUser,
		tenantUser:   tenantUser,
		strandedUser: strandedUser,
		heldRackID:   heldRackID,
		otherRackID:  uuid.New().String(),
		scp:          sc.NewClientPool(tcfg),
	}
}

// getRack drives the Rack read handler as the given user.
func (f rackAccessFixture) getRack(t *testing.T, org string, user *cdbm.User, rackID string) *httptest.ResponseRecorder {
	t.Helper()

	run := &tmocks.WorkflowRun{}
	run.On("GetID").Return("rack-access-workflow")
	run.Mock.On("Get", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		resp := args.Get(1).(*flowv1.GetRackInfoResponse)
		resp.Rack = &flowv1.Rack{Info: &flowv1.DeviceInfo{
			Id:           &flowv1.UUID{Id: rackID},
			Name:         "Rack-001",
			Manufacturer: "NVIDIA",
		}}
	}).Return(nil)

	tsc := &tmocks.Client{}
	tsc.Mock.On("ExecuteWorkflow", mock.Anything, mock.Anything, "GetRack", mock.Anything).Return(run, nil)
	f.scp.IDClientMap[f.siteID.String()] = tsc

	q := url.Values{}
	q.Set("siteId", f.siteID.String())

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v2/org/%s/nico/rack/%s?%s", org, rackID, q.Encode()), nil)
	rec := httptest.NewRecorder()
	ec := e.NewContext(req, rec)
	ec.SetParamNames("orgName", "id")
	ec.SetParamValues(org, rackID)
	ec.Set("user", user)

	handler := NewGetRackHandler(f.dbSession, nil, f.scp, common.GetTestConfig())
	require.NoError(t, handler.Handle(ec))
	return rec
}

func TestRackAccessProviderReadsEveryRackOnItsSite(t *testing.T) {
	f := newRackAccessFixture(t)

	rec := f.getRack(t, f.org, f.providerUser, f.otherRackID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var apiRack model.APIRack
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &apiRack))
	assert.Equal(t, f.otherRackID, apiRack.ID)
}

func TestRackAccessTenantReadsTheRackItHoldsAnInstanceIn(t *testing.T) {
	f := newRackAccessFixture(t)

	rec := f.getRack(t, f.tenantOrg, f.tenantUser, f.heldRackID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var apiRack model.APIRack
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &apiRack))
	assert.Equal(t, f.heldRackID, apiRack.ID)
}

func TestRackAccessTenantCannotReadAnotherRackOnTheSameSite(t *testing.T) {
	f := newRackAccessFixture(t)

	rec := f.getRack(t, f.tenantOrg, f.tenantUser, f.otherRackID)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Could not find Rack")
}

func TestRackAccessTenantWithoutInstancesIsRefusedTheSite(t *testing.T) {
	f := newRackAccessFixture(t)

	rec := f.getRack(t, "stranded-org", f.strandedUser, f.heldRackID)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestRackAccessValidationFollowsTheSameReach(t *testing.T) {
	f := newRackAccessFixture(t)

	validate := func(org string, user *cdbm.User, rackID string) *httptest.ResponseRecorder {
		run := &tmocks.WorkflowRun{}
		run.On("GetID").Return("rack-access-validate")
		run.Mock.On("Get", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
			resp := args.Get(1).(*flowv1.ValidateComponentsResponse)
			resp.Diffs = []*flowv1.ComponentDiff{}
			resp.MatchCount = 5
		}).Return(nil)

		tsc := &tmocks.Client{}
		tsc.Mock.On("ExecuteWorkflow", mock.Anything, mock.Anything, "ValidateRackComponents", mock.Anything).Return(run, nil)
		f.scp.IDClientMap[f.siteID.String()] = tsc

		q := url.Values{}
		q.Set("siteId", f.siteID.String())

		e := echo.New()
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v2/org/%s/nico/rack/%s/validation?%s", org, rackID, q.Encode()), nil)
		rec := httptest.NewRecorder()
		ec := e.NewContext(req, rec)
		ec.SetParamNames("orgName", "id")
		ec.SetParamValues(org, rackID)
		ec.Set("user", user)

		handler := NewValidateRackHandler(f.dbSession, nil, f.scp, common.GetTestConfig())
		require.NoError(t, handler.Handle(ec))
		return rec
	}

	held := validate(f.tenantOrg, f.tenantUser, f.heldRackID)
	require.Equal(t, http.StatusOK, held.Code, held.Body.String())

	other := validate(f.tenantOrg, f.tenantUser, f.otherRackID)
	require.Equal(t, http.StatusNotFound, other.Code, other.Body.String())
}

// paging returns an unbounded page request, for test lookups that want every row.
func paging() cdbp.PageInput {
	return cdbp.PageInput{Limit: cutil.GetPtr(cdbp.TotalLimit)}
}
