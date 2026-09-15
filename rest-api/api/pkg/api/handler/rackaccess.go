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

// rackNotFoundMessage is returned both when a Rack does not exist and when it
// exists outside the caller's reach, so that Rack reads never disclose Racks
// the caller holds nothing in.
const rackNotFoundMessage = "Could not find Rack with specified ID"

// rackReach records which Racks on a Site a caller may read.
//
// A nil rackReach belongs to the Site's Infrastructure Provider and reaches
// every Rack on the Site. A non-nil one belongs to a Tenant and reaches only
// the Racks that hold Machines the Tenant's Instances occupy.
type rackReach struct {
	rackIDs map[string]bool
}

// allows reports whether the caller may read the named Rack.
func (r *rackReach) allows(rackID string) bool {
	if r == nil {
		return true
	}
	return r.rackIDs[rackID]
}

// rackSiteAccess resolves the Site a Rack read is scoped to and decides who
// may read Racks on it.
//
// Provider Admins and Viewers reach every Rack on a Site of their org's
// Infrastructure Provider. A Tenant Admin reaches the Racks holding Machines
// its Tenant's Instances occupy, and the returned rackReach names them. The
// Site must have Flow enabled, because Rack state is read from the Site.
func rackSiteAccess(ctx context.Context, logger zerolog.Logger, dbSession *cdb.Session, org string, dbUser *cdbm.User, siteID string) (*cdbm.Site, *rackReach, *cutil.APIError) {
	provider, tenant, apiErr := common.IsProviderOrTenant(ctx, logger, dbSession, org, dbUser, true, false)
	if apiErr != nil {
		return nil, nil, apiErr
	}

	site, err := common.GetSiteFromIDString(ctx, nil, siteID, dbSession)
	if err != nil {
		switch {
		case errors.Is(err, common.ErrInvalidID):
			return nil, nil, cutil.NewAPIError(http.StatusBadRequest, "Failed to validate Site specified in request: invalid ID", nil)
		case errors.Is(err, cdb.ErrDoesNotExist):
			return nil, nil, cutil.NewAPIError(http.StatusBadRequest, "Site specified in request does not exist", nil)
		}
		logger.Error().Err(err).Msg("error retrieving Site from DB")
		return nil, nil, cutil.NewAPIError(http.StatusInternalServerError, "Failed to retrieve Site specified in request due to DB error", nil)
	}

	siteConfig := &cdbm.SiteConfig{}
	if site.Config != nil {
		siteConfig = site.Config
	}
	if !siteConfig.Flow {
		logger.Warn().Msg("site does not have NICo Flow enabled")
		return nil, nil, cutil.NewAPIError(http.StatusPreconditionFailed, "Site does not have NICo Flow enabled", nil)
	}

	if provider != nil && site.InfrastructureProviderID == provider.ID {
		return site, nil, nil
	}

	if tenant != nil {
		reach, apiErr := tenantRackReach(ctx, logger, dbSession, tenant, site)
		if apiErr != nil {
			return nil, nil, apiErr
		}
		if len(reach.rackIDs) > 0 {
			return site, reach, nil
		}
		logger.Warn().Msg("Tenant holds no Instance on a Machine placed in a Rack on this Site")
		return nil, nil, cutil.NewAPIError(http.StatusForbidden, "Site specified in request holds no Rack the org's Tenant has an Instance on", nil)
	}

	return nil, nil, cutil.NewAPIError(http.StatusForbidden, "Site specified in request doesn't belong to current org's Provider", nil)
}

// tenantRackReach collects the Racks on the Site that hold Machines the
// Tenant's Instances occupy.
//
// Rack placement is not a column on Machine; the expected Machine record
// carries the Rack a Machine was built into, so the walk is Instances of this
// Tenant on this Site, then their Machines, then those Machines' Racks.
func tenantRackReach(ctx context.Context, logger zerolog.Logger, dbSession *cdb.Session, tenant *cdbm.Tenant, site *cdbm.Site) (*rackReach, *cutil.APIError) {
	reach := &rackReach{rackIDs: map[string]bool{}}

	instances, _, err := cdbm.NewInstanceDAO(dbSession).GetAll(ctx, nil, cdbm.InstanceFilterInput{
		TenantIDs: []uuid.UUID{tenant.ID},
		SiteIDs:   []uuid.UUID{site.ID},
	}, cdbp.PageInput{Limit: cutil.GetPtr(cdbp.TotalLimit)}, nil)
	if err != nil {
		logger.Error().Err(err).Msg("error retrieving Instances for Tenant on Site")
		return nil, cutil.NewAPIError(http.StatusInternalServerError, "Failed to determine Tenant's association with Racks on Site", nil)
	}

	machineIDs := make([]string, 0, len(instances))
	for _, instance := range instances {
		if instance.MachineID != nil && *instance.MachineID != "" {
			machineIDs = append(machineIDs, *instance.MachineID)
		}
	}
	if len(machineIDs) == 0 {
		return reach, nil
	}

	expectedMachines, _, err := cdbm.NewExpectedMachineDAO(dbSession).GetAll(ctx, nil, cdbm.ExpectedMachineFilterInput{
		MachineIDs: machineIDs,
		SiteIDs:    []uuid.UUID{site.ID},
	}, cdbp.PageInput{Limit: cutil.GetPtr(cdbp.TotalLimit)}, nil)
	if err != nil {
		logger.Error().Err(err).Msg("error retrieving expected Machines for Tenant's Machines")
		return nil, cutil.NewAPIError(http.StatusInternalServerError, "Failed to determine Tenant's association with Racks on Site", nil)
	}

	for _, expectedMachine := range expectedMachines {
		if expectedMachine.RackID != nil && *expectedMachine.RackID != "" {
			reach.rackIDs[*expectedMachine.RackID] = true
		}
	}

	return reach, nil
}
