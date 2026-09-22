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
	"github.com/NVIDIA/infra-controller/rest-api/common/pkg/coreproxy"
	cutil "github.com/NVIDIA/infra-controller/rest-api/common/pkg/util"
	cdb "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db"
	cdbm "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/model"
	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

// ~~~~~ Redfish Action Handlers ~~~~~ //
//
// Changing a BIOS or BMC setting is a request, not a command. The holder of a
// Machine describes the change; the Provider approves it; only then is it
// applied. Core keeps the requester, the approvers and their timestamps, so
// the record of who asked and who agreed is not this API's to lose.
//
// The customer never holds a BMC credential. It names a Machine, and the
// Machine's management addresses are resolved here, so a caller cannot choose
// which address to reach by writing one into the request.

// redfishActionNotFoundMessage is returned for an action ID that does not
// exist on the addressed Machine, so that action IDs from other Machines are
// not confirmed to exist.
const redfishActionNotFoundMessage = "Could not find Redfish action with specified ID"

// Core takes the requester, the approver and the applier from the subject of
// the TLS client certificate the caller presents to it, and rejects the call
// outright when that certificate carries no user. The Site proxy this API
// reaches Core through presents the Site's own service certificate and carries
// no field for an end user, so these three steps cannot be attributed and Core
// refuses them. Recording, listing and cancelling are unaffected.
//
// Closing this needs an actor carried from here, through the proxy request, to
// a Core that will accept a Site's assertion of who asked. That is a decision
// about how much a Site is trusted to speak for its users, so it is not
// something to work around here: a request that cannot be attributed must fail
// rather than be applied anonymously, which is the whole point of the
// four-eyes record.
const (
	coreMissingCertUserFragment = "Client certificate presented has missing information"
	redfishActionUnattributable = "Approval-gated Redfish changes are unavailable through this API: " +
		"the Core records the requester and approvers from a client certificate, and requests " +
		"proxied via the Site do not carry the calling user's identity"
)

// asUnattributable reports the Core's refusal to attribute a change as what it
// is. Without this the caller sees a bare 500, which reads as a fault that
// might clear on retry; this one never will.
func asUnattributable(apiErr *cutil.APIError) *cutil.APIError {
	if apiErr == nil {
		return nil
	}
	// Either party can be the one unable to name the caller: the Core, when it
	// is handed a certificate with no user in it, or the site, when it holds no
	// CA to mint one from. The caller's situation is the same in both cases.
	if !strings.Contains(apiErr.Message, coreMissingCertUserFragment) &&
		!strings.Contains(apiErr.Message, coreproxy.ActorUnsupportedMessage) {
		return apiErr
	}
	return cutil.NewAPIError(http.StatusNotImplemented, redfishActionUnattributable, nil)
}

// redfishActor names the caller for the Core's record of a change.
//
// Core compares approvers by this string to refuse a second approval from the
// same person, so it has to be stable for one person and distinct between two.
// The email is preferred as the name a human recognises in an audit trail; the
// Starfleet ID is the fallback for a principal with none, and the row ID is
// last, since it is always present and unique even if it means nothing to a
// reader. The group is the role the caller acted in, which is what the record
// needs alongside the name.
func redfishActor(dbUser *cdbm.User, org string, isProvider bool) *coreproxy.Actor {
	user := dbUser.ID.String()
	switch {
	case dbUser.Email != nil && *dbUser.Email != "":
		user = *dbUser.Email
	case dbUser.StarfleetID != nil && *dbUser.StarfleetID != "":
		user = *dbUser.StarfleetID
	}
	group := auth.TenantAdminRole
	if isProvider {
		group = auth.ProviderAdminRole
	}
	return &coreproxy.Actor{User: user, Org: org, Group: group}
}

// RedfishActionHandler serves the whole action lifecycle. Which operation runs
// is decided by the route, so the reach check and the address resolution are
// written once.
type RedfishActionHandler struct {
	dbSession  *cdb.Session
	scp        *sc.ClientPool
	tracerSpan *cutil.TracerSpan
	// op is the lifecycle step this instance serves.
	op redfishActionOp
}

type redfishActionOp int

const (
	redfishActionCreate redfishActionOp = iota
	redfishActionList
	redfishActionApprove
	redfishActionApply
	redfishActionCancel
)

// NewCreateRedfishActionHandler requests a BIOS or BMC change. Reachable by
// the Tenant holding the Machine, and by the Provider.
func NewCreateRedfishActionHandler(dbSession *cdb.Session, scp *sc.ClientPool, _ *config.Config) RedfishActionHandler {
	return newRedfishActionHandler(dbSession, scp, redfishActionCreate)
}

// NewListRedfishActionsHandler lists the pending and applied changes on a
// Machine.
func NewListRedfishActionsHandler(dbSession *cdb.Session, scp *sc.ClientPool, _ *config.Config) RedfishActionHandler {
	return newRedfishActionHandler(dbSession, scp, redfishActionList)
}

// NewApproveRedfishActionHandler approves a requested change. Provider only:
// an approval by the requester would not be one.
func NewApproveRedfishActionHandler(dbSession *cdb.Session, scp *sc.ClientPool, _ *config.Config) RedfishActionHandler {
	return newRedfishActionHandler(dbSession, scp, redfishActionApprove)
}

// NewApplyRedfishActionHandler carries out an approved change. Provider only.
func NewApplyRedfishActionHandler(dbSession *cdb.Session, scp *sc.ClientPool, _ *config.Config) RedfishActionHandler {
	return newRedfishActionHandler(dbSession, scp, redfishActionApply)
}

// NewCancelRedfishActionHandler withdraws a requested change before it is
// applied. Open to whoever can reach the Machine, since withdrawing a request
// takes away a pending change rather than making one.
func NewCancelRedfishActionHandler(dbSession *cdb.Session, scp *sc.ClientPool, _ *config.Config) RedfishActionHandler {
	return newRedfishActionHandler(dbSession, scp, redfishActionCancel)
}

func newRedfishActionHandler(dbSession *cdb.Session, scp *sc.ClientPool, op redfishActionOp) RedfishActionHandler {
	return RedfishActionHandler{
		dbSession:  dbSession,
		scp:        scp,
		tracerSpan: cutil.NewTracerSpan(),
		op:         op,
	}
}

// Handle godoc
// @Summary Request, approve and apply a BIOS or BMC change
// @Description Changes to a Machine's BIOS or BMC are requested through the audited proxy and applied only after approval. Requesting and listing are open to the Tenant holding the Machine and to the Provider; approving and applying require a Provider role, so that the approver is never the requester. Requesting, approving and applying answer 501 where the deployment cannot attribute the change to the calling user: the Core records requester and approvers from a client certificate, and a request proxied via the Site does not carry one. Listing and cancelling are unaffected.
// @Failure 501 {object} util.APIError "The deployment cannot attribute the change to the calling user"
// @Tags machine
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param org path string true "Name of NGC organization"
// @Param id path string true "ID of Machine"
// @Success 200 {array} model.APIRedfishAction
// @Router /v2/org/{org}/nico/machine/{machineId}/redfish-action [get]
func (h RedfishActionHandler) Handle(c echo.Context) error {
	org, dbUser, ctx, logger, handlerSpan := common.SetupHandler("RedfishAction", h.op.name(), c, h.tracerSpan)
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

	machine, isProvider, apiErr := outOfBandMachineAccessByRole(ctx, logger, h.dbSession, org, dbUser, machineID)
	if apiErr != nil {
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, apiErr.Data)
	}

	if h.op.requiresProvider() && !isProvider {
		logger.Warn().Msg("Tenant attempted to approve or apply its own requested change")
		return cutil.NewAPIErrorResponse(c, http.StatusForbidden,
			"Approving or applying a requested change requires a Provider role", nil)
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

	// Listing is the site's own read. Everything else changes the record of
	// who asked or agreed, and is made as the caller.
	actor := redfishActor(dbUser, org, isProvider)

	switch h.op {
	case redfishActionCreate:
		return h.create(c, ctx, logger, stc, machine, site, actor)
	case redfishActionList:
		return h.list(c, ctx, logger, stc, machine, site)
	default:
		return h.byID(c, ctx, logger, stc, site, actor)
	}
}

// create records a requested change against the Machine's management
// addresses. It does not apply anything.
func (h RedfishActionHandler) create(c echo.Context, ctx context.Context, logger zerolog.Logger, stc tClient.Client, machine *cdbm.Machine, site *cdbm.Site, actor *coreproxy.Actor) error {
	var apiReq model.APIRedfishActionRequest
	if err := c.Bind(&apiReq); err != nil {
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest, "Failed to parse request data, potentially invalid structure", nil)
	}
	if err := apiReq.Validate(); err != nil {
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest, err.Error(), nil)
	}

	bmcIPs, apiErr := machineBmcIPs(ctx, logger, stc, machine, site)
	if apiErr != nil {
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, apiErr.Data)
	}

	logger.Info().
		Str("machine_id", machine.ID).Str("site_id", site.ID.String()).
		Str("action", apiReq.Action).Str("target", apiReq.Target).
		Str("actor", actor.User).
		Msg("Requesting a BIOS or BMC change via Core gRPC proxy")

	coreResp := &cwssaws.RedfishCreateActionResponse{}
	if apiErr := common.ExecuteCoreGRPCAs(ctx, stc, actor, cwssaws.Forge_RedfishCreateAction_FullMethodName,
		apiReq.ToProto(bmcIPs), coreResp, site.ID.String()); apiErr != nil {
		logAPIError(logger, apiErr, "Failed to request a BIOS or BMC change via Core gRPC proxy")
		apiErr = asUnattributable(apiErr)
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, nil)
	}

	return c.JSON(http.StatusAccepted, model.APIRedfishActionCreated{
		RequestID: coreResp.GetRequestId(),
		Status:    model.RedfishActionPending,
	})
}

// list returns the actions Core holds for this Machine.
func (h RedfishActionHandler) list(c echo.Context, ctx context.Context, logger zerolog.Logger, stc tClient.Client, machine *cdbm.Machine, site *cdbm.Site) error {
	bmcIPs, apiErr := machineBmcIPs(ctx, logger, stc, machine, site)
	if apiErr != nil {
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, apiErr.Data)
	}

	// Core lists by one management address. A Machine can answer on more than
	// one, so ask for each and concatenate; an address with no actions simply
	// contributes none.
	actions := make([]*cwssaws.RedfishAction, 0)
	for i := range bmcIPs {
		ip := bmcIPs[i]
		coreResp := &cwssaws.RedfishListActionsResponse{}
		if apiErr := common.ExecuteCoreGRPC(ctx, stc, cwssaws.Forge_RedfishListActions_FullMethodName,
			&cwssaws.RedfishListActionsRequest{MachineIp: &ip}, coreResp, site.ID.String()); apiErr != nil {
			logAPIError(logger, apiErr, "Failed to list BIOS and BMC changes via Core gRPC proxy")
			return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, nil)
		}
		actions = append(actions, coreResp.GetActions()...)
	}

	return c.JSON(http.StatusOK, model.NewAPIRedfishActions(actions))
}

// byID approves, applies or cancels one action.
//
// Core addresses an action by its own identifier rather than by Machine, so the
// Machine in the path is what the reach check ran against and is not sent on.
// That is deliberate: it means a caller can only act on an action it reached a
// Machine for.
func (h RedfishActionHandler) byID(c echo.Context, ctx context.Context, logger zerolog.Logger, stc tClient.Client, site *cdbm.Site, actor *coreproxy.Actor) error {
	raw := c.Param("requestId")
	requestID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || requestID <= 0 {
		return cutil.NewAPIErrorResponse(c, http.StatusBadRequest, "Redfish action ID must be a positive integer", nil)
	}

	method, message := h.op.coreMethod()

	logger.Info().
		Int64("request_id", requestID).Str("site_id", site.ID.String()).Str("actor", actor.User).
		Msgf("%s via Core gRPC proxy", message)

	if apiErr := common.ExecuteCoreGRPCAs(ctx, stc, actor, method,
		&cwssaws.RedfishActionID{RequestId: requestID}, nil, site.ID.String()); apiErr != nil {
		// Core reports an unknown action as not found; keep that, so a caller
		// cannot use this endpoint to discover which action IDs exist.
		if apiErr.Code == http.StatusNotFound {
			return cutil.NewAPIErrorResponse(c, http.StatusNotFound, redfishActionNotFoundMessage, nil)
		}
		logAPIError(logger, apiErr, fmt.Sprintf("Failed to %s via Core gRPC proxy", message))
		apiErr = asUnattributable(apiErr)
		return cutil.NewAPIErrorResponse(c, apiErr.Code, apiErr.Message, nil)
	}

	return c.JSON(http.StatusAccepted, model.APIMessageResponse{Message: message + " was accepted"})
}

// machineBmcIPs resolves the Machine's management addresses through Core.
//
// They are not stored in this database, and they are not accepted from the
// caller: the addresses belong to the management network, which this API does
// not expose, and a caller that could name one would be choosing what to
// reach.
func machineBmcIPs(ctx context.Context, logger zerolog.Logger, stc tClient.Client, machine *cdbm.Machine, site *cdbm.Site) ([]string, *cutil.APIError) {
	// Core looks a BMC up by hardware address or by chassis serial. Prefer the
	// address, which identifies the board, and fall back to the serial, which
	// is what a Machine recorded from an expected build carries before it has
	// been seen on the network.
	lookupReq, apiErr := bmcLookupFor(machine)
	if apiErr != nil {
		logger.Warn().Str("machine_id", machine.ID).Msg("Machine has neither a hardware address nor a serial; cannot resolve its BMC")
		return nil, apiErr
	}

	coreResp := &cwssaws.BmcIpList{}
	if apiErr := common.ExecuteCoreGRPC(ctx, stc, cwssaws.Forge_FindBmcIps_FullMethodName,
		lookupReq, coreResp, site.ID.String()); apiErr != nil {
		logAPIError(logger, apiErr, "Failed to resolve the Machine's BMC address via Core gRPC proxy")
		return nil, apiErr
	}

	if len(coreResp.GetBmcIps()) == 0 {
		logger.Warn().Str("machine_id", machine.ID).Msg("Core knows no BMC address for the Machine")
		return nil, cutil.NewAPIError(http.StatusConflict,
			"This Machine's management controller has not been discovered yet", nil)
	}

	return coreResp.GetBmcIps(), nil
}

// bmcLookupFor picks how Core should find the Machine's BMC.
// The oneof's interface type is unexported by protoc-gen-go, so the whole
// request is built here rather than just the branch.
func bmcLookupFor(machine *cdbm.Machine) (*cwssaws.FindBmcIpsRequest, *cutil.APIError) {
	if mac := machine.DefaultMacAddress; mac != nil && *mac != "" {
		return &cwssaws.FindBmcIpsRequest{LookupBy: &cwssaws.FindBmcIpsRequest_MacAddress{MacAddress: *mac}}, nil
	}
	if serial := machine.SerialNumber; serial != nil && *serial != "" {
		return &cwssaws.FindBmcIpsRequest{LookupBy: &cwssaws.FindBmcIpsRequest_Serial{Serial: *serial}}, nil
	}
	return nil, cutil.NewAPIError(http.StatusConflict,
		"This Machine has neither a recorded hardware address nor a serial, so its management controller cannot be addressed", nil)
}

// requiresProvider reports whether the step may only be taken by the Provider.
func (op redfishActionOp) requiresProvider() bool {
	return op == redfishActionApprove || op == redfishActionApply
}

// coreMethod returns the Core method for a by-ID step and the phrase used for
// it in logs and in the response.
func (op redfishActionOp) coreMethod() (string, string) {
	switch op {
	case redfishActionApprove:
		return cwssaws.Forge_RedfishApproveAction_FullMethodName, "Approving the requested change"
	case redfishActionApply:
		return cwssaws.Forge_RedfishApplyAction_FullMethodName, "Applying the approved change"
	default:
		return cwssaws.Forge_RedfishCancelAction_FullMethodName, "Cancelling the requested change"
	}
}

// name is the handler name used in traces and logs.
func (op redfishActionOp) name() string {
	switch op {
	case redfishActionCreate:
		return "Create"
	case redfishActionList:
		return "List"
	case redfishActionApprove:
		return "Approve"
	case redfishActionApply:
		return "Apply"
	default:
		return "Cancel"
	}
}
