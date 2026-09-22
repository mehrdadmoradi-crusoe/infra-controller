// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	temporalEnums "go.temporal.io/api/enums/v1"
	tClient "go.temporal.io/sdk/client"
	tp "go.temporal.io/sdk/temporal"

	"github.com/NVIDIA/infra-controller/rest-api/api/internal/config"
	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/handler/util/common"
	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/model"
	sc "github.com/NVIDIA/infra-controller/rest-api/api/pkg/client/site"
	auth "github.com/NVIDIA/infra-controller/rest-api/auth/pkg/authorization"
	cutil "github.com/NVIDIA/infra-controller/rest-api/common/pkg/util"
	cdb "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db"
	cdbm "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/model"
	cdbp "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/paginator"
	flowv1 "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/flow/protobuf/v1"
	"github.com/NVIDIA/infra-controller/rest-api/workflow/pkg/queue"
)

// ~~~~~ Rack Issues Handler ~~~~~ //

// GetRackIssuesHandler reports every unresolved issue in a Rack in one read.
//
// Acceptance of a delivered tranche needs the known issues written down, and
// today that means walking each host's health report by hand. This gathers
// them: the alerts on the health of every host the Rack's expected build
// places in it, plus any Rack component reporting a leak.
//
// Host health is read from the control plane's own records rather than by
// asking the Site once per host, so a full Rack costs one query and one Rack
// read rather than dozens of round trips.
type GetRackIssuesHandler struct {
	dbSession  *cdb.Session
	scp        *sc.ClientPool
	tracerSpan *cutil.TracerSpan
}

func NewGetRackIssuesHandler(dbSession *cdb.Session, _ tClient.Client, scp *sc.ClientPool, _ *config.Config) GetRackIssuesHandler {
	return GetRackIssuesHandler{
		dbSession:  dbSession,
		scp:        scp,
		tracerSpan: cutil.NewTracerSpan(),
	}
}

// Handle godoc
// @Summary Get open issues on a Rack
// @Description Every unresolved issue in a Rack, from its hosts' health and its components' state.
// @Tags rack
// @Produce json
// @Security ApiKeyAuth
// @Param org path string true "Name of NGC organization"
// @Param id path string true "ID of Rack"
// @Param siteId query string true "ID of the Site"
// @Success 200 {object} model.APIRackIssues
// @Router /v2/org/{org}/nico/rack/{id}/issues [get]
func (h GetRackIssuesHandler) Handle(c echo.Context) error {
	org, dbUser, ctx, logger, handlerSpan := common.SetupHandler("Rack", "GetIssues", c, h.tracerSpan)
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

	machines, apiErr := machinesInRack(ctx, logger, h.dbSession, site, rackStrID)
	if apiErr != nil {
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, apiErr.Data)
	}

	// Component state comes from the Site. A Rack read that fails should not
	// hide the host issues we already have, so the components are best effort
	// and their absence is logged rather than returned.
	components, componentsRead := h.rackComponents(ctx, logger, site, rackStrID)

	return c.JSON(http.StatusOK, model.NewAPIRackIssues(rackStrID, machines, components, componentsRead))
}

// machinesInRack returns the Machines the Site's expected build places in the
// Rack, carrying their recorded health.
//
// Placement lives on the expected Machine record, so the walk is the Rack's
// expected Machines, then the Machines they resolved to.
func machinesInRack(ctx context.Context, logger zerolog.Logger, dbSession *cdb.Session, site *cdbm.Site, rackID string) ([]cdbm.Machine, *cutil.APIError) {
	expectedMachines, _, err := cdbm.NewExpectedMachineDAO(dbSession).GetAll(ctx, nil, cdbm.ExpectedMachineFilterInput{
		SiteIDs: []uuid.UUID{site.ID},
		RackIDs: []string{rackID},
	}, cdbp.PageInput{Limit: cutil.GetPtr(cdbp.TotalLimit)}, nil)
	if err != nil {
		logger.Error().Err(err).Msg("error retrieving expected Machines for Rack")
		return nil, cutil.NewAPIError(http.StatusInternalServerError, "Failed to retrieve the Machines placed in this Rack", nil)
	}

	machineIDs := make([]string, 0, len(expectedMachines))
	for _, expectedMachine := range expectedMachines {
		if expectedMachine.MachineID != nil && *expectedMachine.MachineID != "" {
			machineIDs = append(machineIDs, *expectedMachine.MachineID)
		}
	}
	if len(machineIDs) == 0 {
		return []cdbm.Machine{}, nil
	}

	machines, _, err := cdbm.NewMachineDAO(dbSession).GetAll(ctx, nil, cdbm.MachineFilterInput{
		MachineIDs: machineIDs,
	}, cdbp.PageInput{Limit: cutil.GetPtr(cdbp.TotalLimit)}, nil)
	if err != nil {
		logger.Error().Err(err).Msg("error retrieving Machines placed in Rack")
		return nil, cutil.NewAPIError(http.StatusInternalServerError, "Failed to retrieve the Machines placed in this Rack", nil)
	}

	return machines, nil
}

// rackComponents reads the Rack's components from the Site so their own state,
// today the leak detectors, can be reported alongside host health.
//
// A failure returns no components rather than failing the whole read, because
// host issues are worth reporting on their own. The second return says whether
// the read succeeded, so that the response can distinguish a Rack with nothing
// wrong from one whose components nobody could ask about.
func (h GetRackIssuesHandler) rackComponents(ctx context.Context, logger zerolog.Logger, site *cdbm.Site, rackID string) ([]*flowv1.Component, bool) {
	stc, err := h.scp.GetClientByID(site.ID)
	if err != nil {
		logger.Warn().Err(err).Msg("no workflow client for Site; reporting host issues only")
		return nil, false
	}

	workflowID := fmt.Sprintf("rack-issues-%s", rackID)
	workflowOptions := tClient.StartWorkflowOptions{
		ID:                       workflowID,
		WorkflowIDReusePolicy:    temporalEnums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		WorkflowIDConflictPolicy: temporalEnums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowExecutionTimeout: cutil.WorkflowExecutionTimeout,
		TaskQueue:                queue.SiteTaskQueue,
	}

	ctx, cancel := context.WithTimeout(ctx, cutil.WorkflowContextTimeout)
	defer cancel()

	we, err := stc.ExecuteWorkflow(ctx, workflowOptions, "GetRack", &flowv1.GetRackInfoByIDRequest{
		Id:             &flowv1.UUID{Id: rackID},
		WithComponents: true,
	})
	if err != nil {
		logger.Warn().Err(err).Msg("failed to schedule GetRack for component state; reporting host issues only")
		return nil, false
	}

	var flowResponse flowv1.GetRackInfoResponse
	if err = we.Get(ctx, &flowResponse); err != nil {
		var timeoutErr *tp.TimeoutError
		if errors.As(err, &timeoutErr) {
			logger.Warn().Err(err).Msg("GetRack timed out reading component state; reporting host issues only")
			return nil, false
		}
		logger.Warn().Err(err).Msg("failed to read component state; reporting host issues only")
		return nil, false
	}

	return flowResponse.GetRack().GetComponents(), true
}
