// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/handler/util/common"
	cutil "github.com/NVIDIA/infra-controller/rest-api/common/pkg/util"
	cdb "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db"
	cdbm "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/model"
	cdbp "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/paginator"
)

// machineNotFoundMessage is returned both when a Machine does not exist and
// when it exists outside the caller's reach, so that out-of-band endpoints
// never disclose Machines the caller is not associated with.
const machineNotFoundMessage = "Could not find Machine with specified ID"

// outOfBandMachineAccess resolves the Machine a caller may operate out of
// band (power, BMC reset, health reports) and enforces who may reach it.
//
// Provider Admins and Viewers reach every Machine of their org's
// Infrastructure Provider. Tenant Admins reach a Machine when their Tenant
// holds an Instance on it; a Tenant with targeted Instance creation reaches
// every Machine of an Infrastructure Provider it has an account with. A
// Machine outside the caller's reach is reported as not found.
//
// The Machine is loaded with its Site relation so callers can check the
// Site's registration state without a second query.
func outOfBandMachineAccess(ctx context.Context, logger zerolog.Logger, dbSession *cdb.Session, org string, dbUser *cdbm.User, machineID string) (*cdbm.Machine, *cutil.APIError) {
	provider, tenant, apiErr := common.IsProviderOrTenant(ctx, logger, dbSession, org, dbUser, true, false)
	if apiErr != nil {
		return nil, apiErr
	}

	machine, err := cdbm.NewMachineDAO(dbSession).GetByID(ctx, nil, machineID, []string{cdbm.SiteRelationName}, false)
	if err != nil {
		if errors.Is(err, cdb.ErrDoesNotExist) {
			return nil, cutil.NewAPIError(http.StatusNotFound, machineNotFoundMessage, nil)
		}
		logger.Error().Err(err).Msg("failed to retrieve Machine details from DB")
		return nil, cutil.NewAPIError(http.StatusInternalServerError, "Failed to retrieve Machine details, DB error", nil)
	}

	switch {
	case provider != nil:
		if machine.InfrastructureProviderID != provider.ID {
			logger.Warn().Msg("Machine doesn't belong to org's Infrastructure Provider")
			return nil, cutil.NewAPIError(http.StatusNotFound, machineNotFoundMessage, nil)
		}
	case tenant != nil:
		reaches, apiErr := tenantReachesMachine(ctx, logger, dbSession, tenant, machine)
		if apiErr != nil {
			return nil, apiErr
		}
		if !reaches {
			logger.Warn().Msg("Tenant holds no Instance on the Machine and no privileged account with its Infrastructure Provider")
			return nil, cutil.NewAPIError(http.StatusNotFound, machineNotFoundMessage, nil)
		}
	default:
		return nil, cutil.NewAPIError(http.StatusForbidden, "User does not have Provider or Tenant Admin role with org", nil)
	}

	return machine, nil
}

// tenantReachesMachine reports whether the Tenant is associated with the
// Machine: through an Instance it holds on the Machine, or, for a Tenant with
// targeted Instance creation, through an account with the Machine's
// Infrastructure Provider.
func tenantReachesMachine(ctx context.Context, logger zerolog.Logger, dbSession *cdb.Session, tenant *cdbm.Tenant, machine *cdbm.Machine) (bool, *cutil.APIError) {
	if tenant.Config.TargetedInstanceCreation {
		_, taCount, err := cdbm.NewTenantAccountDAO(dbSession).GetAll(ctx, nil, cdbm.TenantAccountFilterInput{
			InfrastructureProviderID: &machine.InfrastructureProviderID,
			TenantIDs:                []uuid.UUID{tenant.ID},
		}, cdbp.PageInput{}, []string{})
		if err != nil {
			logger.Error().Err(err).Msg("error retrieving Tenant Account for Machine's Infrastructure Provider")
			return false, cutil.NewAPIError(http.StatusInternalServerError, "Failed to determine Tenant's association with Machine", nil)
		}
		if taCount > 0 {
			return true, nil
		}
	}

	_, iCount, err := cdbm.NewInstanceDAO(dbSession).GetAll(ctx, nil, cdbm.InstanceFilterInput{
		TenantIDs:  []uuid.UUID{tenant.ID},
		MachineIDs: []string{machine.ID},
	}, cdbp.PageInput{}, nil)
	if err != nil {
		logger.Error().Err(err).Msg("error retrieving Instances for Tenant on Machine")
		return false, cutil.NewAPIError(http.StatusInternalServerError, "Failed to determine Tenant's association with Machine", nil)
	}
	return iCount > 0, nil
}
