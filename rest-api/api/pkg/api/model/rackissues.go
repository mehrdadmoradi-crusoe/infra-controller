// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"sort"

	cdbm "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/model"
	flowv1 "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/flow/protobuf/v1"
)

// APIRackIssue is one unresolved problem somewhere in a Rack.
//
// It comes from one of two places: an alert on a host's health report, or a
// Rack component reporting a state that needs attention, today a leak. Both
// are reported the same way so that a caller reads one list rather than
// reconciling two.
type APIRackIssue struct {
	// MachineID is set when the issue belongs to a host.
	MachineID *string `json:"machineId"`
	// Component is set when the issue belongs to a Rack component rather than
	// a host, for example `NVSwitchTray/10`.
	Component *string `json:"component"`
	// Source is the health source that reported it, or `rack-component` for a
	// component state.
	Source string `json:"source"`
	// ID is the stable alert identifier, so a caller can match an issue across
	// reads and across hosts.
	ID string `json:"id"`
	// Target is what within the host or component the alert points at.
	Target *string `json:"target"`
	// FirstObserved is when the alert was first raised, where the source
	// recorded it.
	FirstObserved *string `json:"firstObserved"`
	// Message is the operator-facing description.
	Message string `json:"message"`
}

// APIRackIssues is every unresolved issue in a Rack, gathered in one read so a
// tranche can be accepted with its known issues written down.
type APIRackIssues struct {
	RackID string `json:"rackId"`
	// OpenCount is the number of issues in the list.
	OpenCount int `json:"openCount"`
	// Hosts is the number of hosts the Rack's expected build places in it, for
	// context on how much of the Rack the issues cover.
	Hosts  int            `json:"hosts"`
	Issues []APIRackIssue `json:"issues"`
}

// rackComponentIssueSource is the source reported for an issue that comes from
// a Rack component's own state rather than from a host health report.
const rackComponentIssueSource = "rack-component"

// LeakDetectedAlertID is the issue raised for a component reporting a leak.
const LeakDetectedAlertID = "LeakDetected"

// NewAPIRackIssues gathers the unresolved issues for a Rack from the hosts
// placed in it and from the Rack's own components.
//
// Machines are expected to carry their cached health report; a host with no
// health recorded contributes nothing rather than an absence of alerts, since
// "not yet observed" is not the same as "healthy".
func NewAPIRackIssues(rackID string, machines []cdbm.Machine, components []*flowv1.Component) APIRackIssues {
	issues := make([]APIRackIssue, 0)

	for i := range machines {
		machine := machines[i]
		health, err := machine.GetHealth()
		if err != nil || health == nil {
			// Unreadable or absent health contributes nothing: "not yet
			// observed" is not the same as "healthy", and inventing an empty
			// alert list would say the wrong thing.
			continue
		}
		for _, alert := range health.Alerts {
			machineID := machine.ID
			issues = append(issues, APIRackIssue{
				MachineID:     &machineID,
				Source:        health.Source,
				ID:            alert.Id,
				Target:        alert.Target,
				FirstObserved: alert.InAlertSince,
				Message:       alert.Message,
			})
		}
	}

	for _, component := range components {
		if component.GetLeakStatus() != flowv1.LeakStatus_LEAK_STATUS_DETECTED {
			continue
		}
		name := componentIssueName(component)
		issues = append(issues, APIRackIssue{
			Component: &name,
			Source:    rackComponentIssueSource,
			ID:        LeakDetectedAlertID,
			Message:   fmt.Sprintf("component %s reports a leak", name),
		})
	}

	// Stable order: hosts before components, then by identifier, so repeated
	// reads of an unchanged Rack return the same document.
	sort.SliceStable(issues, func(i, j int) bool {
		li, lj := issueSortKey(issues[i]), issueSortKey(issues[j])
		if li != lj {
			return li < lj
		}
		return issues[i].ID < issues[j].ID
	})

	return APIRackIssues{
		RackID:    rackID,
		OpenCount: len(issues),
		Hosts:     len(machines),
		Issues:    issues,
	}
}

// componentIssueName names a component the way an operator refers to it,
// preferring type and tray index over the opaque identifier.
func componentIssueName(component *flowv1.Component) string {
	kind := component.GetType().String()
	if position := component.GetPosition(); position != nil && position.GetTrayIdx() != 0 {
		return fmt.Sprintf("%s/%d", kind, position.GetTrayIdx())
	}
	if id := component.GetComponentId(); id != "" {
		return fmt.Sprintf("%s/%s", kind, id)
	}
	return kind
}

func issueSortKey(issue APIRackIssue) string {
	if issue.MachineID != nil {
		return "0" + *issue.MachineID
	}
	if issue.Component != nil {
		return "1" + *issue.Component
	}
	return "2"
}
