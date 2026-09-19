// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

// An NVL72 rack is one NVLink domain, so the rack is the unit this is reported
// for: the state of the fabric manager on each NVLink switch in it, as the
// control plane last observed it.
//
// Nothing here identifies the management network. The Switch record carries
// its BMC details and its management addresses; those are dropped, because a
// caller that could read them could reach the switch, and the switches are
// not addressable through this API.

// APINVLinkSwitchState is one NVLink switch in the rack.
type APINVLinkSwitchState struct {
	// Name is the switch's own name, which is how an operator refers to it in
	// a conversation about the rack.
	Name *string `json:"name"`
	// TrayIndex and SlotNumber place the switch in the rack.
	TrayIndex  *int32 `json:"trayIndex"`
	SlotNumber *int32 `json:"slotNumber"`
	// IsPrimary marks the switch running the primary fabric manager.
	IsPrimary *bool `json:"isPrimary"`
	// FabricManagerState is the fabric manager's own state: Ok, NotOk or
	// Unknown. Unknown means the control plane has not been able to read it,
	// which is not the same as the fabric being unhealthy.
	FabricManagerState string `json:"fabricManagerState"`
	// FabricManagerReason and FabricManagerError carry why, when the fabric
	// manager is not Ok.
	FabricManagerReason *string `json:"fabricManagerReason"`
	FabricManagerError  *string `json:"fabricManagerError"`
	// PowerState is "on", "off" or "standby".
	PowerState *string `json:"powerState"`
	// HealthStatus is the switch's aggregate health: "ok", "warning" or
	// "critical".
	HealthStatus *string `json:"healthStatus"`
}

// APINVLinkFabric is the fabric manager state across a rack's NVLink switches.
type APINVLinkFabric struct {
	RackID string `json:"rackId"`
	// Switches is every NVLink switch the rack holds, in the order the Site
	// returned them.
	Switches []APINVLinkSwitchState `json:"switches"`
	// SwitchCount, and how many of those report a fabric manager that is not
	// Ok, so a caller can decide whether to look closer without walking the
	// list.
	SwitchCount int `json:"switchCount"`
	NotOkCount  int `json:"notOkCount"`
}

// NVLink fabric manager states, as this API names them.
const (
	NVLinkFabricManagerOk      = "Ok"
	NVLinkFabricManagerNotOk   = "NotOk"
	NVLinkFabricManagerUnknown = "Unknown"
)

// NewAPINVLinkFabric converts the Site's Switch records for one rack.
//
// A switch whose fabric manager state the control plane could not read is
// reported as Unknown rather than omitted: an NVLink switch that has gone
// quiet is exactly what a caller needs to see.
func NewAPINVLinkFabric(rackID string, switches []*cwssaws.Switch) APINVLinkFabric {
	out := APINVLinkFabric{
		RackID:   rackID,
		Switches: make([]APINVLinkSwitchState, 0, len(switches)),
	}

	for _, sw := range switches {
		if sw == nil {
			continue
		}
		state := APINVLinkSwitchState{
			FabricManagerState: NVLinkFabricManagerUnknown,
		}

		if status := sw.GetStatus(); status != nil {
			state.Name = optionalString(status.GetSwitchName())
			state.PowerState = optionalString(status.GetPowerState())
			state.HealthStatus = optionalString(status.GetHealthStatus())
			state.FabricManagerState = nvLinkFabricManagerState(status)

			if details := status.GetFabricManagerStatusDetails(); details != nil {
				state.FabricManagerReason = optionalString(details.GetReason())
				state.FabricManagerError = optionalString(details.GetErrorMessage())
			}
		}

		if placement := sw.GetPlacementInRack(); placement != nil {
			state.TrayIndex = placement.TrayIndex
			state.SlotNumber = placement.SlotNumber
		}
		isPrimary := sw.GetIsPrimary()
		state.IsPrimary = &isPrimary

		if state.FabricManagerState == NVLinkFabricManagerNotOk {
			out.NotOkCount++
		}
		out.Switches = append(out.Switches, state)
	}

	out.SwitchCount = len(out.Switches)
	return out
}

// nvLinkFabricManagerState prefers the structured state and falls back to the
// free-text field, which is what older Sites populate.
func nvLinkFabricManagerState(status *cwssaws.SwitchStatus) string {
	if details := status.GetFabricManagerStatusDetails(); details != nil {
		switch details.GetFabricManagerState() {
		case cwssaws.FabricManagerState_FABRIC_MANAGER_STATE_OK:
			return NVLinkFabricManagerOk
		case cwssaws.FabricManagerState_FABRIC_MANAGER_STATE_NOT_OK:
			return NVLinkFabricManagerNotOk
		}
	}

	switch status.GetFabricManagerStatus() {
	case "":
		return NVLinkFabricManagerUnknown
	case "ok", "OK", "Ok":
		return NVLinkFabricManagerOk
	default:
		return NVLinkFabricManagerNotOk
	}
}

// optionalString returns nil for an empty value, so that "not reported" and
// "reported as empty" are not the same thing in the response.
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
