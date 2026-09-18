// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"google.golang.org/protobuf/types/known/timestamppb"

	cutil "github.com/NVIDIA/infra-controller/rest-api/common/pkg/util"
	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

const (
	// RepairRequestSource is the health report source a repair request is
	// recorded under. It is reserved: a Tenant may only ever write this one
	// source, and only through the repair-request operation, which is what
	// keeps a Tenant out of the sources the Provider and the platform own.
	RepairRequestSource = "tenant-repair-request"

	// RepairRequestAlertID is the alert every repair request raises. Ticketing
	// keys off it, so it is stable.
	RepairRequestAlertID = "RepairRequested"

	// RepairRequestStatusSubmitted is the state a request is in once the
	// health report is written and before ticketing assigns a reference.
	RepairRequestStatusSubmitted = "Submitted"
)

// Repair request targets: what on the Machine the customer believes is faulty.
const (
	RepairTargetHost    = "HOST"
	RepairTargetPort    = "PORT"
	RepairTargetLink    = "LINK"
	RepairTargetGPU     = "GPU"
	RepairTargetStorage = "STORAGE"
)

// Repair request urgencies, in the customer's own judgement. The provider's
// service levels are a matter for the ticket, not for this field.
const (
	RepairUrgencyLow      = "LOW"
	RepairUrgencyNormal   = "NORMAL"
	RepairUrgencyHigh     = "HIGH"
	RepairUrgencyCritical = "CRITICAL"
)

var (
	// RepairTargets is the set of accepted `target` values.
	RepairTargets = []string{RepairTargetHost, RepairTargetPort, RepairTargetLink, RepairTargetGPU, RepairTargetStorage}

	// RepairUrgencies is the set of accepted `urgency` values.
	RepairUrgencies = []string{RepairUrgencyLow, RepairUrgencyNormal, RepairUrgencyHigh, RepairUrgencyCritical}

	repairTargetsAny   = toAnySlice(RepairTargets)
	repairUrgenciesAny = toAnySlice(RepairUrgencies)
)

func toAnySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

// APIMachineRepairRequest is the data structure to capture a customer's
// request that a Machine, or one part of it, be repaired.
type APIMachineRepairRequest struct {
	// Summary is what the customer observed, in its own words. It reaches the
	// operator who picks the work up, so it is required.
	Summary string `json:"summary"`
	// Target names what is believed faulty. Defaults to the whole host.
	Target *string `json:"target"`
	// Urgency is the customer's own assessment. Defaults to NORMAL.
	Urgency *string `json:"urgency"`
	// Component optionally narrows the target, for example the interface name
	// for a PORT request or the device index for a GPU.
	Component *string `json:"component"`
}

const (
	repairSummaryMin = 8
	repairSummaryMax = 1000
)

// Validate checks the request in isolation. Whether the caller may reach the
// Machine at all is the handler's business.
func (r *APIMachineRepairRequest) Validate() error {
	return validation.ValidateStruct(r,
		validation.Field(&r.Summary,
			validation.Required.Error(validationErrorValueRequired),
			validation.Length(repairSummaryMin, repairSummaryMax).Error(
				fmt.Sprintf("must be between %d and %d characters", repairSummaryMin, repairSummaryMax))),
		validation.Field(&r.Target,
			validation.In(repairTargetsAny...).Error(fmt.Sprintf("must be one of %v", RepairTargets))),
		validation.Field(&r.Urgency,
			validation.In(repairUrgenciesAny...).Error(fmt.Sprintf("must be one of %v", RepairUrgencies))),
		validation.Field(&r.Component, validation.Length(0, 255)),
	)
}

// TargetOrDefault returns the requested target, defaulted to the whole host.
func (r *APIMachineRepairRequest) TargetOrDefault() string {
	if r.Target == nil || *r.Target == "" {
		return RepairTargetHost
	}
	return *r.Target
}

// UrgencyOrDefault returns the requested urgency, defaulted to NORMAL.
func (r *APIMachineRepairRequest) UrgencyOrDefault() string {
	if r.Urgency == nil || *r.Urgency == "" {
		return RepairUrgencyNormal
	}
	return *r.Urgency
}

// alertTarget is what the alert points at: the target kind, narrowed by the
// component when one was given, so an operator reading the health report sees
// `PORT/enp1s0f0` rather than just `PORT`.
func (r *APIMachineRepairRequest) alertTarget() string {
	target := r.TargetOrDefault()
	if r.Component != nil && *r.Component != "" {
		return fmt.Sprintf("%s/%s", target, *r.Component)
	}
	return target
}

// ToProto renders the request as the health report entry Core records against
// the Machine. The entry merges, so a repair request never displaces the
// platform's own health sources; it adds one alert under its own source.
func (r *APIMachineRepairRequest) ToProto(machineID string, userID string, observedAt time.Time) *cwssaws.InsertMachineHealthReportRequest {
	return &cwssaws.InsertMachineHealthReportRequest{
		MachineId: &cwssaws.MachineId{Id: machineID},
		HealthReportEntry: &cwssaws.HealthReportEntry{
			Report: &cwssaws.HealthReport{
				Source:      RepairRequestSource,
				TriggeredBy: &userID,
				ObservedAt:  timestamppb.New(observedAt),
				Alerts: []*cwssaws.HealthProbeAlert{{
					Id:           RepairRequestAlertID,
					Target:       cutil.GetPtr(r.alertTarget()),
					InAlertSince: timestamppb.New(observedAt),
					Message:      fmt.Sprintf("repair requested (%s): %s", r.UrgencyOrDefault(), r.Summary),
				}},
			},
			Mode: cwssaws.HealthReportApplyMode_Merge,
		},
	}
}

// APIMachineRepairRequestResponse is what the customer gets back: the request
// as recorded, plus where to follow it.
type APIMachineRepairRequestResponse struct {
	MachineID string  `json:"machineId"`
	Source    string  `json:"source"`
	Target    string  `json:"target"`
	Urgency   string  `json:"urgency"`
	Summary   string  `json:"summary"`
	Component *string `json:"component"`
	Status    string  `json:"status"`
	// TicketRef is null until the ticketing integration assigns one. The
	// request is recorded on the Machine either way.
	TicketRef  *string `json:"ticketRef"`
	ObservedAt string  `json:"observedAt"`
}

// NewAPIMachineRepairRequestResponse builds the response from the request as
// it was recorded.
func NewAPIMachineRepairRequestResponse(machineID string, r *APIMachineRepairRequest, observedAt time.Time) APIMachineRepairRequestResponse {
	return APIMachineRepairRequestResponse{
		MachineID:  machineID,
		Source:     RepairRequestSource,
		Target:     r.TargetOrDefault(),
		Urgency:    r.UrgencyOrDefault(),
		Summary:    r.Summary,
		Component:  r.Component,
		Status:     RepairRequestStatusSubmitted,
		ObservedAt: observedAt.UTC().Format(time.RFC3339),
	}
}
