// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"
	"go.opentelemetry.io/otel/attribute"

	"github.com/NVIDIA/infra-controller/rest-api/api/internal/config"
	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/handler/util/common"
	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/model"
	sc "github.com/NVIDIA/infra-controller/rest-api/api/pkg/client/site"
	auth "github.com/NVIDIA/infra-controller/rest-api/auth/pkg/authorization"
	cutil "github.com/NVIDIA/infra-controller/rest-api/common/pkg/util"
	cdb "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db"
	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

// ~~~~~ Rack Topology Handler ~~~~~ //

// GetRackTopologyHandler reports where each host physically sits in a Rack.
//
// A scheduler placing a job needs to know which hosts share locality and which
// share a failure domain, and accepting a delivered tranche needs to know the
// rack was populated as designed. Both are questions about placement.
//
// This deliberately reports placement rather than cabling. The Site can also
// describe the network devices a host is attached to, but that record carries
// switch management addresses, and the underlay is not addressable through this
// API. Placement answers the question that was actually being asked without
// going near it.
type GetRackTopologyHandler struct {
	dbSession  *cdb.Session
	scp        *sc.ClientPool
	tracerSpan *cutil.TracerSpan
}

func NewGetRackTopologyHandler(dbSession *cdb.Session, scp *sc.ClientPool, _ *config.Config) GetRackTopologyHandler {
	return GetRackTopologyHandler{
		dbSession:  dbSession,
		scp:        scp,
		tracerSpan: cutil.NewTracerSpan(),
	}
}

// Handle godoc
// @Summary Read the physical layout of a Rack
// @Description Where each host sits in the Rack, and which hosts share a switch or a power shelf. Provider Admins and Viewers for their own Racks; Tenant Admins for a Rack they hold an Instance in.
// @Tags rack
// @Produce json
// @Security ApiKeyAuth
// @Param org path string true "Name of NGC organization"
// @Param id path string true "ID of Rack"
// @Param siteId query string true "ID of the Site"
// @Success 200 {object} model.APIRackTopology
// @Router /v2/org/{org}/nico/rack/{id}/topology [get]
func (h GetRackTopologyHandler) Handle(c echo.Context) error {
	org, dbUser, ctx, logger, handlerSpan := common.SetupHandler("RackTopology", "Get", c, h.tracerSpan)
	if handlerSpan != nil {
		defer handlerSpan.End()
	}

	if dbUser == nil {
		logger.Error().Msg("invalid User object found in request context")
		return cutil.NewAPIErrorResponse(c, http.StatusInternalServerError, "Failed to retrieve current user", nil)
	}

	ok, err := auth.ValidateOrgMembership(dbUser, org)
	if !ok {
		if err != nil {
			logger.Error().Err(err).Msg("error validating org membership for User in request")
		} else {
			logger.Warn().Msg("could not validate org membership for user, access denied")
		}
		return cutil.NewAPIErrorResponse(c, http.StatusForbidden, fmt.Sprintf("Failed to validate membership for org: %s", org), nil)
	}

	rackStrID := c.Param("id")
	if rackStrID == "" {
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest, "Rack ID was not specified in URL", nil)
	}
	h.tracerSpan.SetAttribute(handlerSpan, attribute.String("rack_id", rackStrID), logger)

	siteStrID := c.QueryParam("siteId")
	if siteStrID == "" {
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest, "siteId query parameter is required", nil)
	}

	site, reach, apiErr := rackSiteAccess(ctx, logger, h.dbSession, org, dbUser, siteStrID)
	if apiErr != nil {
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, apiErr.Data)
	}
	if !reach.allows(rackStrID) {
		logger.Warn().Msg("Tenant holds no Instance on a Machine in the requested Rack")
		return cutil.NewAPIErrorResponse(c, http.StatusNotFound, rackNotFoundMessage, nil)
	}

	// Which hosts the Rack's expected build places in it, from the control
	// plane's own records, then where the Site says each one sits.
	machines, apiErr := machinesInRack(ctx, logger, h.dbSession, site, rackStrID)
	if apiErr != nil {
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, apiErr.Data)
	}
	if len(machines) == 0 {
		return c.JSON(http.StatusOK, model.NewAPIRackTopology(rackStrID, nil))
	}

	machineIDs := make([]*cwssaws.MachineId, 0, len(machines))
	for i := range machines {
		machineIDs = append(machineIDs, &cwssaws.MachineId{Id: machines[i].ID})
	}

	stc, err := h.scp.GetClientByID(site.ID)
	if err != nil {
		logger.Error().Err(err).Msg("failed to retrieve Temporal client for Site")
		return cutil.NewAPIErrorResponse(c, http.StatusInternalServerError, "Failed to retrieve workflow client for Site", nil)
	}

	coreResp := &cwssaws.MachinePositionInfoList{}
	if apiErr := common.ExecuteCoreGRPC(ctx, stc, cwssaws.Forge_GetMachinePositionInfo_FullMethodName,
		&cwssaws.MachinePositionQuery{MachineIds: machineIDs}, coreResp, site.ID.String()); apiErr != nil {
		logAPIError(logger, apiErr, "Failed to read Machine placement via Core gRPC proxy")
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, nil)
	}

	topology := model.NewAPIRackTopology(rackStrID, coreResp.GetMachinePositionInfo())

	logger.Info().
		Str("rack_id", rackStrID).Str("site_id", site.ID.String()).
		Int("hosts", topology.HostCount).Int("switch_groups", topology.SwitchGroupCount).
		Msg("Read Rack topology")

	return c.JSON(http.StatusOK, topology)
}
