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

// ~~~~~ NVLink Fabric Handler ~~~~~ //

// switchLookupBatchSize is how many switch IDs are sent to Core in one lookup.
//
// Core rejects a lookup carrying more IDs than its own max_find_by_ids, which
// is a deployment setting: it defaults to 100 but is configured lower in
// practice, and this API cannot read it. 25 is comfortably below any value
// seen, and small enough that it stays below a lower one.
const switchLookupBatchSize = 25

// batchIDs splits ids into runs of at most size, so a caller can respect a
// server-side limit on how many identifiers one request may carry. The batches
// alias the input rather than copying it.
func batchIDs[T any](ids []T, size int) [][]T {
	if size < 1 {
		return [][]T{ids}
	}
	batches := make([][]T, 0, (len(ids)+size-1)/size)
	for start := 0; start < len(ids); start += size {
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		batches = append(batches, ids[start:end])
	}
	return batches
}

// GetNVLinkFabricHandler reports the fabric manager state across a Rack's
// NVLink switches.
//
// An NVL72 rack is one NVLink domain, so when a workload's collectives stall
// the question is about the rack, not about one host. The per-GPU NVLink
// interfaces are already readable; what was missing is whether the fabric
// manager driving them is up, and on which tray it is not.
//
// Switch records carry BMC details and management addresses. Those are dropped
// in the model rather than filtered here, so that a future field cannot leak
// by being added upstream of the conversion.
type GetNVLinkFabricHandler struct {
	dbSession  *cdb.Session
	scp        *sc.ClientPool
	tracerSpan *cutil.TracerSpan
}

func NewGetNVLinkFabricHandler(dbSession *cdb.Session, scp *sc.ClientPool, _ *config.Config) GetNVLinkFabricHandler {
	return GetNVLinkFabricHandler{
		dbSession:  dbSession,
		scp:        scp,
		tracerSpan: cutil.NewTracerSpan(),
	}
}

// Handle godoc
// @Summary Read the NVLink fabric manager state for a Rack
// @Description The fabric manager state of every NVLink switch in the Rack, with its tray placement. Provider Admins and Viewers for their own Racks; Tenant Admins for a Rack they hold an Instance in.
// @Tags rack
// @Produce json
// @Security ApiKeyAuth
// @Param org path string true "Name of NGC organization"
// @Param id path string true "ID of Rack"
// @Param siteId query string true "ID of the Site"
// @Success 200 {object} model.APINVLinkFabric
// @Router /v2/org/{org}/nico/rack/{id}/nvlink-fabric [get]
func (h GetNVLinkFabricHandler) Handle(c echo.Context) error {
	org, dbUser, ctx, logger, handlerSpan := common.SetupHandler("NVLinkFabric", "Get", c, h.tracerSpan)
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

	stc, err := h.scp.GetClientByID(site.ID)
	if err != nil {
		logger.Error().Err(err).Msg("failed to retrieve Temporal client for Site")
		return cutil.NewAPIErrorResponse(c, http.StatusInternalServerError, "Failed to retrieve workflow client for Site", nil)
	}

	// The switches in the Rack, then their records. Two calls, because the
	// Site's search returns identifiers and the state lives on the record.
	idResp := &cwssaws.SwitchIdList{}
	if apiErr := common.ExecuteCoreGRPC(ctx, stc, cwssaws.Forge_FindSwitchIds_FullMethodName,
		&cwssaws.SwitchSearchFilter{RackId: &cwssaws.RackId{Id: rackStrID}},
		idResp, site.ID.String()); apiErr != nil {
		logAPIError(logger, apiErr, "Failed to list the Rack's switches via Core gRPC proxy")
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, nil)
	}

	if len(idResp.GetIds()) == 0 {
		// A Rack with no switches recorded is not an error: it is a Rack whose
		// NVLink switches have not been discovered.
		return c.JSON(http.StatusOK, model.NewAPINVLinkFabric(rackStrID, nil))
	}

	// Core refuses a lookup carrying more IDs than its own max_find_by_ids, so
	// ask in batches. How many switches a Rack holds is not the caller's
	// choice and there is nothing for them to page, so a Rack wider than that
	// setting must not turn into a rejected read.
	switches := make([]*cwssaws.Switch, 0, len(idResp.GetIds()))
	for _, batch := range batchIDs(idResp.GetIds(), switchLookupBatchSize) {
		switchResp := &cwssaws.SwitchList{}
		if apiErr := common.ExecuteCoreGRPC(ctx, stc, cwssaws.Forge_FindSwitchesByIds_FullMethodName,
			&cwssaws.SwitchesByIdsRequest{SwitchIds: batch},
			switchResp, site.ID.String()); apiErr != nil {
			logAPIError(logger, apiErr, "Failed to read the Rack's switches via Core gRPC proxy")
			return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, nil)
		}
		switches = append(switches, switchResp.GetSwitches()...)
	}

	logger.Info().
		Str("rack_id", rackStrID).Str("site_id", site.ID.String()).
		Int("switches", len(switches)).
		Msg("Read NVLink fabric manager state for Rack")

	return c.JSON(http.StatusOK, model.NewAPINVLinkFabric(rackStrID, switches))
}
