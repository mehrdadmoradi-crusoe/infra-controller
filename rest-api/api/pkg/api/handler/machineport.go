// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"
	tClient "go.temporal.io/sdk/client"

	"github.com/NVIDIA/infra-controller/rest-api/api/internal/config"
	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/handler/util/common"
	"github.com/NVIDIA/infra-controller/rest-api/api/pkg/api/model"
	sc "github.com/NVIDIA/infra-controller/rest-api/api/pkg/client/site"
	auth "github.com/NVIDIA/infra-controller/rest-api/auth/pkg/authorization"
	cutil "github.com/NVIDIA/infra-controller/rest-api/common/pkg/util"
	cdb "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db"
	cdbm "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/model"
	cdbp "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/paginator"
	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

// ~~~~~ Machine Port Handler ~~~~~ //

// GetMachinePortsHandler reports the state of a host's own uplinks.
//
// The common failure this answers is a link that is up now but has been
// flapping, which shows up as a rising carrier-down count long before anyone
// correlates it with a slow job.
//
// What it cannot answer, and says so in the response: anything measured on the
// switch. Port speed, negotiated duplex, FEC state and error or discard
// counters are not readable through the control plane today, so they are
// absent rather than reported as zero. `switchSideAvailable` is false to make
// that explicit, because a port that looks clean here has not been checked for
// errors at all.
type GetMachinePortsHandler struct {
	dbSession  *cdb.Session
	scp        *sc.ClientPool
	tracerSpan *cutil.TracerSpan
}

func NewGetMachinePortsHandler(dbSession *cdb.Session, scp *sc.ClientPool, _ *config.Config) GetMachinePortsHandler {
	return GetMachinePortsHandler{
		dbSession:  dbSession,
		scp:        scp,
		tracerSpan: cutil.NewTracerSpan(),
	}
}

// Handle godoc
// @Summary Read the state of a Machine's uplinks
// @Description Link state, flap counts and the switch port each uplink is cabled to, observed from the host's side. Provider Admins and Viewers for their Machines; Tenant Admins for a Machine their Tenant holds an Instance on.
// @Tags machine
// @Produce json
// @Security ApiKeyAuth
// @Param org path string true "Name of NGC organization"
// @Param id path string true "ID of Machine"
// @Success 200 {object} model.APIMachinePorts
// @Router /v2/org/{org}/nico/machine/{machineId}/port [get]
func (h GetMachinePortsHandler) Handle(c echo.Context) error {
	org, dbUser, ctx, logger, handlerSpan := common.SetupHandler("MachinePorts", "Get", c, h.tracerSpan)
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

	machineID := c.Param("id")
	if machineID == "" {
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest, "Machine ID was not specified in URL", nil)
	}

	machine, apiErr := outOfBandMachineAccess(ctx, logger, h.dbSession, org, dbUser, machineID)
	if apiErr != nil {
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, apiErr.Data)
	}
	if machine.Site == nil {
		logger.Error().Msg("related Site was not returned for Machine DB entity")
		return cutil.NewAPIErrorResponse(c, http.StatusInternalServerError, "Failed to retrieve Site details for Machine, DB error", nil)
	}
	site := machine.Site

	// The uplink state is reported by the agent on the host's DPU, so the
	// host's DPUs have to be resolved first. A host with none has no uplinks
	// this can see, which is answered from the database rather than by asking
	// the Site.
	dpuMachineIDs, apiErr := h.attachedDpuMachineIDs(ctx, logger, machineID)
	if apiErr != nil {
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, apiErr.Data)
	}
	if len(dpuMachineIDs) == 0 {
		logger.Info().Str("machine_id", machineID).Msg("Machine has no attached DPUs; no uplink state to report")
		return c.JSON(http.StatusOK, model.NewAPIMachinePorts(machineID, nil, nil))
	}

	if site.Status != cdbm.SiteStatusRegistered {
		logger.Warn().Msg("Site for the addressed Machine is not in Registered state")
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest, "Site for the addressed Machine is not in Registered state, cannot execute admin operation", nil)
	}

	stc, err := h.scp.GetClientByID(site.ID)
	if err != nil {
		logger.Error().Err(err).Msg("failed to retrieve Temporal client for Site")
		return cutil.NewAPIErrorResponse(c, http.StatusInternalServerError, "Failed to retrieve workflow client for Site", nil)
	}

	// Core reports network status for the whole Site in one call: there is no
	// per-Machine filter on it. The reply is therefore narrowed here to this
	// host's own DPUs, so a caller never sees another tenant's hosts.
	statusResp := &cwssaws.ManagedHostNetworkStatusResponse{}
	if apiErr := common.ExecuteCoreGRPC(ctx, stc, cwssaws.Forge_GetAllManagedHostNetworkStatus_FullMethodName,
		&cwssaws.ManagedHostNetworkStatusRequest{}, statusResp, site.ID.String()); apiErr != nil {
		logAPIError(logger, apiErr, "Failed to read host network status via Core gRPC proxy")
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, nil)
	}

	wanted := make(map[string]bool, len(dpuMachineIDs))
	for _, id := range dpuMachineIDs {
		wanted[id] = true
	}
	mine := make([]*cwssaws.DpuNetworkStatus, 0, len(dpuMachineIDs))
	for _, status := range statusResp.GetAll() {
		if status != nil && wanted[status.GetDpuMachineId().GetId()] {
			mine = append(mine, status)
		}
	}

	// What LLDP learned about the far end of each uplink, for the switch port
	// names. Best effort: without it the host-side state is still worth
	// returning, and a Site that cannot answer should not hide a flapping link.
	connected := h.connectedDevices(ctx, logger, stc, site, dpuMachineIDs)

	ports := model.NewAPIMachinePorts(machineID, mine, connected)

	logger.Info().
		Str("machine_id", machineID).Int("dpus", len(dpuMachineIDs)).Int("ports", len(ports.Ports)).
		Msg("Read Machine uplink state")

	return c.JSON(http.StatusOK, ports)
}

// attachedDpuMachineIDs returns the DPU Machines attached to the host, from
// the control plane's own interface records.
func (h GetMachinePortsHandler) attachedDpuMachineIDs(ctx context.Context, logger zerolog.Logger, machineID string) ([]string, *cutil.APIError) {
	machineInterfaces, _, err := cdbm.NewMachineInterfaceDAO(h.dbSession).GetAll(ctx, nil, cdbm.MachineInterfaceFilterInput{
		MachineIDs: []string{machineID},
	}, cdbp.PageInput{Limit: cutil.GetPtr(cdbp.TotalLimit)}, nil)
	if err != nil {
		logger.Error().Err(err).Msg("error retrieving MachineInterfaces for Machine")
		return nil, cutil.NewAPIError(http.StatusInternalServerError, "Failed to retrieve Machine Interfaces, DB error", nil)
	}

	seen := map[string]bool{}
	out := make([]string, 0, len(machineInterfaces))
	for i := range machineInterfaces {
		attached := machineInterfaces[i].AttachedDPUMachineID
		if attached == nil || *attached == "" || seen[*attached] {
			continue
		}
		seen[*attached] = true
		out = append(out, *attached)
	}
	return out, nil
}

// connectedDevices asks the Site what LLDP saw at the far end of each uplink.
// A failure returns nothing rather than failing the read.
func (h GetMachinePortsHandler) connectedDevices(ctx context.Context, logger zerolog.Logger, stc tClient.Client, site *cdbm.Site, dpuMachineIDs []string) []*cwssaws.ConnectedDevice {
	ids := make([]*cwssaws.MachineId, 0, len(dpuMachineIDs))
	for _, id := range dpuMachineIDs {
		ids = append(ids, &cwssaws.MachineId{Id: id})
	}

	coreResp := &cwssaws.ConnectedDeviceList{}
	if apiErr := common.ExecuteCoreGRPC(ctx, stc, cwssaws.Forge_FindConnectedDevicesByDpuMachineIds_FullMethodName,
		&cwssaws.MachineIdList{MachineIds: ids}, coreResp, site.ID.String()); apiErr != nil {
		logger.Warn().Msg("could not read the far end of the Machine's uplinks; reporting host-side state only")
		return nil
	}
	return coreResp.GetConnectedDevices()
}
