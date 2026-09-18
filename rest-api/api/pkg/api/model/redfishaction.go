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
	// Status is derived: Pending until approved, Approved until applied, then
	// Applied. It saves every caller re-deriving the same thing from the
	// timestamps.
	Status string `json:"status"`
}

// Redfish action statuses.
const (
	RedfishActionPending  = "Pending"
	RedfishActionApproved = "Approved"
	RedfishActionApplied  = "Applied"
)

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

	if len(out.Approvers) > 0 {
		out.Status = RedfishActionApproved
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

	return out
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
