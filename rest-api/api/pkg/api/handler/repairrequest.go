// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/NVIDIA/infra-controller/rest-api/api/internal/config"
	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/handler/util/common"
	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/model"
	sc "github.com/NVIDIA/infra-controller/rest-api/api/pkg/client/site"
	auth "github.com/NVIDIA/infra-controller/rest-api/auth/pkg/authorization"
	cutil "github.com/NVIDIA/infra-controller/rest-api/common/pkg/util"
	cdb "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db"
	cdbm "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/model"
	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

// ~~~~~ Machine Repair Request Handler ~~~~~ //

// MachineRepairRequestHandler lets the holder of a Machine ask for it to be
// repaired.
//
// The request is recorded as a health report under the reserved
// `tenant-repair-request` source, which is the signal the ticketing
// integration watches. Recording it as health rather than as a bare message
// means the platform already knows how to surface it: it shows up in the
// Machine's health report next to the platform's own sources, and it survives
// restarts of anything in the path.
type MachineRepairRequestHandler struct {
	dbSession  *cdb.Session
	scp        *sc.ClientPool
	tracerSpan *cutil.TracerSpan
}

func NewMachineRepairRequestHandler(dbSession *cdb.Session, scp *sc.ClientPool, _ *config.Config) MachineRepairRequestHandler {
	return MachineRepairRequestHandler{
		dbSession:  dbSession,
		scp:        scp,
		tracerSpan: cutil.NewTracerSpan(),
	}
}

// Handle godoc
// @Summary Request repair for a Machine
// @Description Ask for a Machine, or one part of it, to be repaired. Provider Admins and Viewers for their Machines; Tenant Admins for Machines their Tenant holds an Instance on.
// @Tags machine
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param org path string true "Name of NGC organization"
// @Param id path string true "ID of Machine"
// @Param request body model.APIMachineRepairRequest true "Repair request"
// @Success 202 {object} model.APIMachineRepairRequestResponse
// @Router /v2/org/{org}/nico/machine/{machineId}/repair-request [post]
func (h MachineRepairRequestHandler) Handle(c echo.Context) error {
	org, dbUser, ctx, logger, handlerSpan := common.SetupHandler("MachineRepairRequest", "Create", c, h.tracerSpan)
	if handlerSpan != nil {
		defer handlerSpan.End()
	}

	if dbUser == nil {
		logger.Error().Msg("Invalid User object found in request context")
		return cutil.NewAPIErrorResponse(c, http.StatusInternalServerError, "Failed to retrieve current user", nil)
	}

	ok, err := auth.ValidateOrgMembership(dbUser, org)
	if !ok {
		if err != nil {
			logger.Error().Err(err).Msg("Error validating org membership for User in request")
		} else {
			logger.Warn().Msg("Could not validate org membership for user, access denied")
		}
		return cutil.NewAPIErrorResponse(c, http.StatusForbidden, fmt.Sprintf("Failed to validate membership for org: %s", org), nil)
	}

	machineID := c.Param("id")
	if machineID == "" {
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest, "Machine ID was not specified in URL", nil)
	}

	var apiReq model.APIMachineRepairRequest
	if err = c.Bind(&apiReq); err != nil {
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest, "Failed to parse request data, potentially invalid structure", nil)
	}

	if err = apiReq.Validate(); err != nil {
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest, err.Error(), nil)
	}

	machine, apiError := outOfBandMachineAccess(ctx, logger, h.dbSession, org, dbUser, machineID)
	if apiError != nil {
		return cutil.NewAPIErrorResponse(c, apiError.Code, apiError.Message, apiError.Data)
	}

	// A Machine that is not on its Site cannot be repaired in place, and the
	// request would have nowhere to land.
	if machine.IsMissingOnSite {
		logger.Error().Msg("Machine is missing on site, unable to record a repair request")
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest, "Machine is missing on site, unable to record a repair request", nil)
	}

	if machine.Site == nil {
		logger.Error().Msg("Related Site was not returned for Machine DB entity")
		return cutil.NewAPIErrorResponse(c, http.StatusInternalServerError, "Failed to retrieve Site details for Machine, DB error", nil)
	}

	site := machine.Site
	if site.Status != cdbm.SiteStatusRegistered {
		logger.Warn().Msg("Site for the Machine is not in Registered state")
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest, "Site for the Machine is not in Registered state, cannot record a repair request", nil)
	}

	stc, err := h.scp.GetClientByID(site.ID)
	if err != nil {
		logger.Error().Err(err).Msg("failed to retrieve Temporal client for Site")
		return cutil.NewAPIErrorResponse(c, http.StatusInternalServerError, "Failed to retrieve workflow client for Site", nil)
	}

	observedAt := time.Now()

	logger.Info().
		Str("machine_id", machineID).
		Str("site_id", site.ID.String()).
		Str("target", apiReq.TargetOrDefault()).
		Str("urgency", apiReq.UrgencyOrDefault()).
		Msg("Recording Machine repair request via Core gRPC proxy")

	protoReq := apiReq.ToProto(machineID, dbUser.ID.String(), observedAt)
	apiErr := common.ExecuteCoreGRPC(ctx, stc, cwssaws.Forge_InsertMachineHealthReport_FullMethodName, protoReq, nil, site.ID.String())
	if apiErr != nil {
		logAPIError(logger, apiErr, "Failed to record Machine repair request via Core gRPC proxy")
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, nil)
	}

	return c.JSON(http.StatusAccepted, model.NewAPIMachineRepairRequestResponse(machineID, &apiReq, observedAt))
}

// ~~~~~ Withdraw Machine Repair Request Handler ~~~~~ //

// WithdrawMachineRepairRequestHandler removes a repair request the caller
// raised, for when the customer diagnoses the fault as its own.
//
// It removes only the reserved repair-request source, so it can never clear
// the platform's or the Provider's health sources.
type WithdrawMachineRepairRequestHandler struct {
	dbSession  *cdb.Session
	scp        *sc.ClientPool
	tracerSpan *cutil.TracerSpan
}

func NewWithdrawMachineRepairRequestHandler(dbSession *cdb.Session, scp *sc.ClientPool, _ *config.Config) WithdrawMachineRepairRequestHandler {
	return WithdrawMachineRepairRequestHandler{
		dbSession:  dbSession,
		scp:        scp,
		tracerSpan: cutil.NewTracerSpan(),
	}
}

// Handle godoc
// @Summary Withdraw a Machine repair request
// @Description Withdraw a repair request raised for a Machine. Provider Admins and Viewers for their Machines; Tenant Admins for Machines their Tenant holds an Instance on.
// @Tags machine
// @Produce json
// @Security ApiKeyAuth
// @Param org path string true "Name of NGC organization"
// @Param id path string true "ID of Machine"
// @Success 204
// @Router /v2/org/{org}/nico/machine/{machineId}/repair-request [delete]
func (h WithdrawMachineRepairRequestHandler) Handle(c echo.Context) error {
	org, dbUser, ctx, logger, handlerSpan := common.SetupHandler("MachineRepairRequest", "Withdraw", c, h.tracerSpan)
	if handlerSpan != nil {
		defer handlerSpan.End()
	}

	if dbUser == nil {
		logger.Error().Msg("Invalid User object found in request context")
		return cutil.NewAPIErrorResponse(c, http.StatusInternalServerError, "Failed to retrieve current user", nil)
	}

	ok, err := auth.ValidateOrgMembership(dbUser, org)
	if !ok {
		if err != nil {
			logger.Error().Err(err).Msg("Error validating org membership for User in request")
		} else {
			logger.Warn().Msg("Could not validate org membership for user, access denied")
		}
		return cutil.NewAPIErrorResponse(c, http.StatusForbidden, fmt.Sprintf("Failed to validate membership for org: %s", org), nil)
	}

	machineID := c.Param("id")
	if machineID == "" {
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest, "Machine ID was not specified in URL", nil)
	}

	machine, apiError := outOfBandMachineAccess(ctx, logger, h.dbSession, org, dbUser, machineID)
	if apiError != nil {
		return cutil.NewAPIErrorResponse(c, apiError.Code, apiError.Message, apiError.Data)
	}

	if machine.Site == nil {
		logger.Error().Msg("Related Site was not returned for Machine DB entity")
		return cutil.NewAPIErrorResponse(c, http.StatusInternalServerError, "Failed to retrieve Site details for Machine, DB error", nil)
	}

	site := machine.Site
	if site.Status != cdbm.SiteStatusRegistered {
		logger.Warn().Msg("Site for the Machine is not in Registered state")
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest, "Site for the Machine is not in Registered state, cannot withdraw a repair request", nil)
	}

	stc, err := h.scp.GetClientByID(site.ID)
	if err != nil {
		logger.Error().Err(err).Msg("failed to retrieve Temporal client for Site")
		return cutil.NewAPIErrorResponse(c, http.StatusInternalServerError, "Failed to retrieve workflow client for Site", nil)
	}

	logger.Info().Str("machine_id", machineID).Str("site_id", site.ID.String()).Msg("Withdrawing Machine repair request via Core gRPC proxy")

	apiErr := common.ExecuteCoreGRPC(
		ctx, stc,
		cwssaws.Forge_RemoveMachineHealthReport_FullMethodName,
		model.NewRemoveMachineHealthReportProto(machineID, model.RepairRequestSource),
		nil, site.ID.String(),
	)
	if apiErr != nil {
		logAPIError(logger, apiErr, "Failed to withdraw Machine repair request via Core gRPC proxy")
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, nil)
	}

	return c.NoContent(http.StatusNoContent)
}
