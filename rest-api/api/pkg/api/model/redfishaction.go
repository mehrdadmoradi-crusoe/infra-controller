// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"

	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

// A Redfish action is how a BIOS or BMC setting is changed through the audited
// proxy: the holder of a Machine describes the change, and it is applied only
// after someone else has approved it. Nothing here hands out a BMC credential,
// and nothing is applied by the request that created it.
//
// The Core carries the requester, the approvers and their timestamps, so the
// record of who asked and who agreed outlives this API.

// APIRedfishActionRequest is the body of a request to change a setting.
type APIRedfishActionRequest struct {
	// Action is the Redfish action name, for example
	// `ComputerSystem.SetDefaultBootOrder`.
	Action string `json:"action"`
	// Target is the Redfish URI the action is posted to, for example
	// `/redfish/v1/Systems/1/Actions/ComputerSystem.Reset`.
	Target string `json:"target"`
	// Parameters is the JSON body for the action, as a string, so that the
	// proxy passes exactly what was reviewed rather than something this API
	// re-encoded.
	Parameters string `json:"parameters"`
}

// Validate enforces the REST-layer contract before ToProto.
//
// Parameters is deliberately not parsed here. It is quoted verbatim into the
// audit record and handed to the proxy, so the bytes an approver reads are the
// bytes the BMC receives; re-encoding it would break that.
func (r *APIRedfishActionRequest) Validate() error {
	return validation.ValidateStruct(r,
		validation.Field(&r.Action,
			validation.Required.Error("a value must be specified for action"),
			validation.Length(1, 256),
		),
		validation.Field(&r.Target,
			validation.Required.Error("a value must be specified for target"),
			validation.Length(1, 1024),
		),
		validation.Field(&r.Parameters, validation.Length(0, 64*1024)),
	)
}

// ToProto converts the request to a Core gRPC RedfishCreateActionRequest.
//
// The BMC addresses are resolved by the handler rather than supplied by the
// caller: a caller that could name the address it wanted would be choosing
// which machine to reach, which is the reach check's job.
func (r *APIRedfishActionRequest) ToProto(bmcIPs []string) *cwssaws.RedfishCreateActionRequest {
	return &cwssaws.RedfishCreateActionRequest{
		Ips:        bmcIPs,
		Action:     r.Action,
		Target:     r.Target,
		Parameters: r.Parameters,
	}
}

// APIRedfishAction is one pending or applied action on a Machine.
type APIRedfishAction struct {
	// RequestID identifies the action for approve, apply and cancel.
	RequestID int64 `json:"requestId"`
	// Requester is who asked for the change.
	Requester string `json:"requester"`
	// Approvers are those who have agreed to it so far, with the time each
	// agreed in ApprovedAt at the same index.
	Approvers  []string `json:"approvers"`
	ApprovedAt []string `json:"approvedAt"`
	// Action, Target and Parameters are the change itself, as submitted.
	Action     string `json:"action"`
	Target     string `json:"target"`
	Parameters string `json:"parameters"`
	// AppliedAt and Applier are set once the change has been carried out.
	AppliedAt *string `json:"appliedAt"`
	Applier   *string `json:"applier"`
	// ApprovalsHeld and ApprovalsRequired show the arithmetic behind Status, so
	// a caller can tell "one more approval needed" from "ready to apply"
	// without knowing the threshold.
	ApprovalsHeld     int `json:"approvalsHeld"`
	ApprovalsRequired int `json:"approvalsRequired"`
	// Status is derived: Pending with no approvals, AwaitingApproval while
	// short of the threshold, Approved once it is met, Applied once the Core
	// has sent it to the Machine's management controller. Applied is the
	// Core's decision, not the controller's answer; that is in Outcomes.
	Status string `json:"status"`
	// Outcomes is what each management controller the change was sent to
	// said in reply, in the order the Core holds them, without saying which
	// controller is which: the management network is not addressable here.
	// Empty until the change is applied.
	Outcomes []APIRedfishActionOutcome `json:"outcomes"`
	// OutcomesPending is how many controllers have not answered yet, and
	// OutcomesFailed how many answered with anything other than success. They
	// exist so that "Applied" cannot be read as "the controller accepted it":
	// a change the Core dispatched and the controller refused is Applied with
	// one failed outcome, and the two must not look the same.
	OutcomesPending int `json:"outcomesPending"`
	OutcomesFailed  int `json:"outcomesFailed"`
}

// APIRedfishActionOutcome is one management controller's reply to an applied
// change. Response headers are dropped: they carry the controller's server
// banner and session material and nothing a caller needs.
type APIRedfishActionOutcome struct {
	// Status is the controller's HTTP status line, for example
	// "405 Method Not Allowed", or the Core's own "not executed" when it
	// declined to send the change because the Machine's board serial no longer
	// matched the one recorded when the change was requested.
	Status string `json:"status"`
	// CompletedAt is when the reply was recorded.
	CompletedAt *string `json:"completedAt"`
	// Detail is the start of the controller's response body, which for a
	// refusal is usually a Redfish error with the reason. Cut at
	// RedfishOutcomeDetailLimit so a controller cannot make this record large.
	Detail *string `json:"detail"`
	// Succeeded is whether Status is a 2xx, so a caller need not parse it.
	Succeeded bool `json:"succeeded"`
}

// RedfishOutcomeDetailLimit is the most of a controller's response body
// carried in an outcome.
const RedfishOutcomeDetailLimit = 1024

// Redfish action statuses.
const (
	RedfishActionPending          = "Pending"
	RedfishActionAwaitingApproval = "AwaitingApproval"
	RedfishActionApproved         = "Approved"
	RedfishActionApplied          = "Applied"
)

// RedfishActionRequiredApprovals is how many approvals the Core requires
// before a change may be applied. It mirrors NUM_REQUIRED_APPROVALS in the
// Core (crates/api-core/src/handlers/redfish.rs).
//
// The Core counts the request itself as the requester's approval: it inserts
// the requester as the first entry in approvers. So 2 means the requester and
// one other person -- which is what four-eyes means -- not two approvals on
// top of the request. ApprovalsHeld therefore includes the requester, and a
// freshly requested change already holds 1.
const RedfishActionRequiredApprovals = 2

// APIRedfishActionCreated is the response to a create.
type APIRedfishActionCreated struct {
	RequestID int64  `json:"requestId"`
	Status    string `json:"status"`
}

// NewAPIRedfishAction converts one Core action record.
//
// The Machine's BMC addresses and board serials are deliberately dropped. They
// identify the management network, which this API does not expose, and the
// caller already addressed the Machine by its own ID.
func NewAPIRedfishAction(action *cwssaws.RedfishAction) APIRedfishAction {
	out := APIRedfishAction{
		RequestID:  action.GetRequestId(),
		Requester:  action.GetRequester(),
		Approvers:  append([]string{}, action.GetApprovers()...),
		ApprovedAt: make([]string, 0, len(action.GetApproverDates())),
		Action:     action.GetAction(),
		Target:     action.GetTarget(),
		Parameters: action.GetParameters(),
		Status:     RedfishActionPending,
	}

	for _, at := range action.GetApproverDates() {
		if at != nil {
			out.ApprovedAt = append(out.ApprovedAt, at.AsTime().Format(time.RFC3339))
		}
	}

	out.ApprovalsHeld = len(out.Approvers)
	out.ApprovalsRequired = RedfishActionRequiredApprovals
	switch {
	case out.ApprovalsHeld >= RedfishActionRequiredApprovals:
		out.Status = RedfishActionApproved
	case out.ApprovalsHeld > 0:
		out.Status = RedfishActionAwaitingApproval
	}

	if applied := action.GetAppliedAt(); applied != nil {
		when := applied.AsTime().Format(time.RFC3339)
		out.AppliedAt = &when
		out.Status = RedfishActionApplied
	}
	if applier := action.GetApplier(); applier != "" {
		who := applier
		out.Applier = &who
	}

	out.Outcomes = make([]APIRedfishActionOutcome, 0, len(action.GetResults()))
	for _, wrapped := range action.GetResults() {
		result := wrapped.GetResult()
		if result == nil {
			// The Core has a slot for this controller but no reply yet.
			out.OutcomesPending++
			continue
		}
		outcome := APIRedfishActionOutcome{
			Status:    result.GetStatus(),
			Succeeded: redfishStatusSucceeded(result.GetStatus()),
		}
		if at := result.GetCompletedAt(); at != nil {
			when := at.AsTime().Format(time.RFC3339)
			outcome.CompletedAt = &when
		}
		if body := result.GetBody(); body != "" {
			detail := body
			if len(detail) > RedfishOutcomeDetailLimit {
				detail = detail[:RedfishOutcomeDetailLimit] + "\u2026"
			}
			outcome.Detail = &detail
		}
		if !outcome.Succeeded {
			out.OutcomesFailed++
		}
		out.Outcomes = append(out.Outcomes, outcome)
	}

	return out
}

// redfishStatusSucceeded reads the Core's status string for one controller
// reply. The Core records an HTTP status line ("200 OK", "405 Method Not
// Allowed") or its own "not executed"; only a 2xx is success, and anything
// unparseable is not assumed to be.
func redfishStatusSucceeded(status string) bool {
	return len(status) >= 3 && status[0] == '2' && status[1] >= '0' && status[1] <= '9' && status[2] >= '0' && status[2] <= '9'
}

// NewAPIRedfishActions converts a Core action list, preserving its order.
func NewAPIRedfishActions(actions []*cwssaws.RedfishAction) []APIRedfishAction {
	out := make([]APIRedfishAction, 0, len(actions))
	for _, action := range actions {
		if action == nil {
			continue
		}
		out = append(out, NewAPIRedfishAction(action))
	}
	return out
}
