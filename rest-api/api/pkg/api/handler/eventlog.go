// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

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
	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

// ~~~~~ Event Log Handler ~~~~~ //

// GetMachineEventLogHandler reads a host's management controller logs.
//
// When a host reboots with no explanation, the reason is in the controller's
// log rather than anywhere in the operating system, and it survives the
// reboot. Reading it meant asking an operator, because the controller is not
// reachable by the customer.
//
// Every URI sent to the controller is built here, from the source the caller
// named and the links the controller itself returned. The caller cannot supply
// a URI: the browse the Core offers is a general one, and handing that to a
// customer would be read access to the whole controller rather than to its
// logs.
type GetMachineEventLogHandler struct {
	dbSession  *cdb.Session
	scp        *sc.ClientPool
	tracerSpan *cutil.TracerSpan
}

func NewGetMachineEventLogHandler(dbSession *cdb.Session, scp *sc.ClientPool, _ *config.Config) GetMachineEventLogHandler {
	return GetMachineEventLogHandler{
		dbSession:  dbSession,
		scp:        scp,
		tracerSpan: cutil.NewTracerSpan(),
	}
}

const (
	// eventLogDefaultLimit and eventLogMaxLimit bound the response. A
	// controller can hold thousands of entries, and the whole log is rarely
	// what a caller wants.
	eventLogDefaultLimit = 100
	eventLogMaxLimit     = 1000

	// redfishRoot is the only prefix this handler will follow. A controller
	// that returned a link elsewhere would be redirecting us off its own tree.
	redfishRoot = "/redfish/v1/"
)

// eventLogCollections maps a source this API accepts to the Redfish collection
// it lives under. The caller names a source, never a path.
var eventLogCollections = map[string]string{
	model.EventLogSourceSystem:  "Systems",
	model.EventLogSourceManager: "Managers",
}

// Handle godoc
// @Summary Read a Machine's management controller event log
// @Description The host's own log, or the controller's, normalised from Redfish. Provider Admins and Viewers for their Machines; Tenant Admins for a Machine their Tenant holds an Instance on.
// @Tags machine
// @Produce json
// @Security ApiKeyAuth
// @Param org path string true "Name of NGC organization"
// @Param id path string true "ID of Machine"
// @Param source query string false "Which log to read: system (default) or manager"
// @Param limit query int false "Maximum entries to return, default 100, maximum 1000"
// @Success 200 {object} model.APIEventLog
// @Router /v2/org/{org}/nico/machine/{machineId}/event-log [get]
func (h GetMachineEventLogHandler) Handle(c echo.Context) error {
	org, dbUser, ctx, logger, handlerSpan := common.SetupHandler("MachineEventLog", "Get", c, h.tracerSpan)
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

	source := c.QueryParam("source")
	if source == "" {
		source = model.EventLogSourceSystem
	}
	collection, known := eventLogCollections[source]
	if !known {
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest,
			fmt.Sprintf("source must be one of: %s", strings.Join(model.EventLogSources, ", ")), nil)
	}

	limit := eventLogDefaultLimit
	if raw := c.QueryParam("limit"); raw != "" {
		parsed, perr := strconv.Atoi(raw)
		if perr != nil || parsed <= 0 || parsed > eventLogMaxLimit {
			return cutil.NewAPIErrorResponse(c, http.StatusBadRequest,
				fmt.Sprintf("limit must be a positive integer no greater than %d", eventLogMaxLimit), nil)
		}
		limit = parsed
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
	if site.Status != cdbm.SiteStatusRegistered {
		logger.Warn().Msg("Site for the addressed Machine is not in Registered state")
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest, "Site for the addressed Machine is not in Registered state, cannot execute admin operation", nil)
	}

	stc, err := h.scp.GetClientByID(site.ID)
	if err != nil {
		logger.Error().Err(err).Msg("failed to retrieve Temporal client for Site")
		return cutil.NewAPIErrorResponse(c, http.StatusInternalServerError, "Failed to retrieve workflow client for Site", nil)
	}

	bmcIPs, apiErr := machineBmcIPs(ctx, logger, stc, machine, site)
	if apiErr != nil {
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, apiErr.Data)
	}
	host := bmcIPs[0]

	log, apiErr := h.readEventLog(ctx, logger, stc, site, host, collection, limit)
	if apiErr != nil {
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, apiErr.Data)
	}
	log.MachineID = machine.ID
	log.Source = source

	logger.Info().
		Str("machine_id", machine.ID).Str("source", source).
		Int("entries", len(log.Entries)).Bool("truncated", log.Truncated).
		Msg("Read Machine event log via Core gRPC proxy")

	return c.JSON(http.StatusOK, log)
}

// readEventLog walks the controller: the collection, each member's log
// services, and each service's entries.
//
// The walk follows the controller's own links rather than guessing paths,
// because the identifiers are vendor-shaped: one vendor's system is `1` and
// another's is `Self`. Links that leave the Redfish tree are refused.
func (h GetMachineEventLogHandler) readEventLog(ctx context.Context, logger zerolog.Logger, stc tClient.Client, site *cdbm.Site, host, collection string, limit int) (model.APIEventLog, *cutil.APIError) {
	out := model.APIEventLog{
		Services: make([]string, 0),
		Entries:  make([]model.APIEventLogEntry, 0),
	}

	members, apiErr := h.browseCollection(ctx, logger, stc, site, host, redfishRoot+collection)
	if apiErr != nil {
		return out, apiErr
	}

	total := 0
	for _, member := range members {
		services, apiErr := h.browseCollection(ctx, logger, stc, site, host, member+"/LogServices")
		if apiErr != nil {
			// A member without log services is normal; a controller exposing
			// Managers that carry none should not fail the whole read.
			logger.Debug().Str("member", member).Msg("no log services on this member")
			continue
		}

		for _, service := range services {
			body, apiErr := h.browse(ctx, logger, stc, site, host, service+"/Entries")
			if apiErr != nil {
				logger.Debug().Str("service", service).Msg("log service entries could not be read")
				continue
			}

			name := service[strings.LastIndex(service, "/")+1:]
			entries, serviceTotal, perr := model.ParseRedfishLogEntries(name, body)
			if perr != nil {
				logger.Warn().Err(perr).Str("service", service).Msg("log service returned a body this API could not read")
				continue
			}

			out.Services = append(out.Services, name)
			out.Entries = append(out.Entries, entries...)
			total += serviceTotal
		}
	}

	model.SortEventLogEntriesNewestFirst(out.Entries)
	if len(out.Entries) > limit {
		out.Entries = out.Entries[:limit]
	}
	out.Truncated = total > len(out.Entries)

	return out, nil
}

// browseCollection reads a Redfish collection and returns its member links.
func (h GetMachineEventLogHandler) browseCollection(ctx context.Context, logger zerolog.Logger, stc tClient.Client, site *cdbm.Site, host, path string) ([]string, *cutil.APIError) {
	body, apiErr := h.browse(ctx, logger, stc, site, host, path)
	if apiErr != nil {
		return nil, apiErr
	}

	members, err := model.ParseRedfishCollection(body)
	if err != nil {
		logger.Warn().Err(err).Str("path", path).Msg("controller returned a collection this API could not read")
		return nil, cutil.NewAPIError(http.StatusBadGateway,
			"The management controller returned a response this API could not read", nil)
	}

	// Only links inside the controller's own Redfish tree are followed.
	kept := make([]string, 0, len(members))
	for _, member := range members {
		if strings.HasPrefix(member, redfishRoot) {
			kept = append(kept, strings.TrimSuffix(member, "/"))
			continue
		}
		logger.Warn().Str("link", member).Msg("controller returned a link outside its Redfish tree; ignored")
	}
	return kept, nil
}

// browse issues one read against the controller through the Core proxy.
func (h GetMachineEventLogHandler) browse(ctx context.Context, logger zerolog.Logger, stc tClient.Client, site *cdbm.Site, host, path string) ([]byte, *cutil.APIError) {
	uri := fmt.Sprintf("https://%s%s", host, path)

	coreResp := &cwssaws.RedfishBrowseResponse{}
	if apiErr := common.ExecuteCoreGRPC(ctx, stc, cwssaws.Forge_RedfishBrowse_FullMethodName,
		&cwssaws.RedfishBrowseRequest{Uri: uri}, coreResp, site.ID.String()); apiErr != nil {
		return nil, apiErr
	}

	return []byte(coreResp.GetText()), nil
}
