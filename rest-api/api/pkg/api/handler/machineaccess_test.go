// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/handler/util/common"
	authz "github.com/NVIDIA/infra-controller/rest-api/auth/pkg/authorization"
	cutil "github.com/NVIDIA/infra-controller/rest-api/common/pkg/util"
	cdb "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db"
	cdbm "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/model"
)

// tenantOnMachine is a Tenant Admin caller in its own org, built by
// testGrantTenantOnMachine, for exercising tenant reach on out-of-band
// Machine endpoints.
type tenantOnMachine struct {
	org    string
	user   *cdbm.User
	tenant *cdbm.Tenant
}

// testGrantTenantOnMachine builds a Tenant in the org "tenant-org" with a
// Tenant Admin user and an account with the Machine's Infrastructure
// Provider. With withInstance the Tenant holds a Ready Instance on the
// Machine and the Machine is marked assigned; with privileged the Tenant
// has targeted Instance creation enabled.
func testGrantTenantOnMachine(t *testing.T, dbSession *cdb.Session, machineID string, withInstance bool, privileged bool) tenantOnMachine {
	t.Helper()
	ctx := context.Background()

	mDAO := cdbm.NewMachineDAO(dbSession)
	machine, err := mDAO.GetByID(ctx, nil, machineID, []string{cdbm.SiteRelationName}, false)
	require.NoError(t, err)
	require.NotNil(t, machine.Site)

	org := "tenant-org"
	user := common.TestBuildUser(t, dbSession, "tenant-starfleet-id", org, []string{authz.TenantAdminRole})
	tenant := common.TestBuildTenant(t, dbSession, "Test Tenant", org, user)
	ip := &cdbm.InfrastructureProvider{ID: machine.InfrastructureProviderID}
	common.TestBuildTenantAccount(t, dbSession, ip, &tenant.ID, org, cdbm.TenantAccountStatusReady, user)

	if privileged {
		tenant = testMachineUpdateTenantCapability(t, dbSession, tenant)
	}

	if withInstance {
		require.NotNil(t, machine.InstanceTypeID)
		vpc := common.TestBuildVPC(t, dbSession, "tenant-vpc", ip, tenant, machine.Site, nil, nil, nil, cdbm.VpcStatusReady, user)
		os := common.TestBuildOperatingSystem(t, dbSession, "tenant-os", tenant, cdbm.OperatingSystemStatusReady, user)
		common.TestBuildInstance(t, dbSession, "tenant-instance", tenant.ID, ip.ID, machine.SiteID, *machine.InstanceTypeID, vpc.ID, &machine.ID, os.ID)
		_, err = mDAO.Update(ctx, nil, cdbm.MachineUpdateInput{
			MachineID:  machineID,
			IsAssigned: cutil.GetPtr(true),
		})
		require.NoError(t, err)
	}

	return tenantOnMachine{org: org, user: user, tenant: tenant}
}
