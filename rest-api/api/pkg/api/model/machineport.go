// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"sort"

	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

// The state of a host's own uplinks: whether each is up, how often it has
// flapped, and which switch port it is cabled to.
//
// Read what this does not contain before relying on it. Everything here is
// observed from the host's side of the link, by the agent on its DPU. Port
// speed, negotiated duplex, FEC state and error or discard counters live on
// the switch, and the control plane has no read path to them today, so they
// are absent rather than zero. A link reported up with no errors here has not
// been checked for errors at all.
//
// That distinction is why this reports `carrierUp` and flap counts rather than
// a single "healthy": a flapping link is the failure this can actually see.

// APIMachinePort is one of a host's uplinks.
type APIMachinePort struct {
	// InterfaceName is the interface as the host names it, for example `p0`.
	InterfaceName string `json:"interfaceName"`
	// LinkType is the interface's kind, as the host reports it.
	LinkType *string `json:"linkType"`
	// State is the interface's administrative state from the host's side.
	State *string `json:"state"`
	// CarrierUp is whether the host currently sees carrier. This is the
	// closest thing here to "the link is up".
	CarrierUp *bool `json:"carrierUp"`
	// MTU as configured on the host's interface.
	MTU *uint32 `json:"mtu"`
	// CarrierUpCount and CarrierDownCount are how many times the host has seen
	// the link come up and go down. A link that is up now but has flapped
	// repeatedly is the case these exist for.
	CarrierUpCount   *uint32 `json:"carrierUpCount"`
	CarrierDownCount *uint32 `json:"carrierDownCount"`
	// RemotePort is the switch port this interface is cabled to, as the switch
	// announced it over LLDP. It names a port, not an address.
	RemotePort *string `json:"remotePort"`
	// SwitchGroup labels the switch at the far end. Ports sharing a label
	// reach the same switch; the label means nothing outside this response,
	// for the same reason as in the Rack topology.
	SwitchGroup *string `json:"switchGroup"`
}

// APIMachinePorts is the uplink state for one host.
type APIMachinePorts struct {
	MachineID string `json:"machineId"`
	// ObservedAt is when the host last reported, in RFC 3339. A port list with
	// an old timestamp is stale rather than wrong, and the difference matters
	// when a host has stopped reporting.
	ObservedAt *string `json:"observedAt"`
	// Ports, ordered by interface name so repeated reads match.
	Ports []APIMachinePort `json:"ports"`
	// SwitchSideAvailable is always false today, and says plainly that nothing
	// here was read from the switch: no speed, no error counters, no FEC.
	SwitchSideAvailable bool `json:"switchSideAvailable"`
}

// NewAPIMachinePorts assembles a host's uplink state from what its DPUs
// reported and what LLDP learned about the far end.
//
// connected maps a DPU's local port name to the switch port it reaches;
// switchLabels assigns the opaque per-response switch labels.
func NewAPIMachinePorts(machineID string, statuses []*cwssaws.DpuNetworkStatus, connected []*cwssaws.ConnectedDevice) APIMachinePorts {
	out := APIMachinePorts{
		MachineID: machineID,
		Ports:     make([]APIMachinePort, 0),
	}

	remoteByLocal := make(map[string]*cwssaws.ConnectedDevice, len(connected))
	for _, device := range connected {
		if device != nil && device.GetLocalPort() != "" {
			remoteByLocal[device.GetLocalPort()] = device
		}
	}

	switchLabels := newGroupLabeller("switch")

	for _, status := range statuses {
		if status == nil {
			continue
		}
		// The newest observation across this host's DPUs is the one reported,
		// so a host with more than one DPU does not appear to be as stale as
		// its quietest one.
		if observed := status.GetObservedAt(); observed != nil {
			when := observed.AsTime().Format(rfc3339)
			if out.ObservedAt == nil || when > *out.ObservedAt {
				out.ObservedAt = &when
			}
		}

		for _, fabricInterface := range status.GetFabricInterfaces() {
			if fabricInterface == nil {
				continue
			}
			port := APIMachinePort{InterfaceName: fabricInterface.GetInterfaceName()}

			if link := fabricInterface.GetLinkData(); link != nil {
				port.LinkType = link.LinkType
				port.State = link.State
				port.CarrierUp = link.CarrierUp
				port.MTU = link.Mtu
				port.CarrierUpCount = link.CarrierUpCount
				port.CarrierDownCount = link.CarrierDownCount
			}

			if device, ok := remoteByLocal[port.InterfaceName]; ok {
				if remote := device.GetRemotePort(); remote != "" {
					port.RemotePort = &remote
				}
				if id := device.GetNetworkDeviceId(); id != "" {
					port.SwitchGroup = switchLabels.labelFor(id)
				}
			}

			out.Ports = append(out.Ports, port)
		}
	}

	sort.SliceStable(out.Ports, func(i, j int) bool {
		return out.Ports[i].InterfaceName < out.Ports[j].InterfaceName
	})

	return out
}

// rfc3339 is the timestamp format used across this package's responses.
const rfc3339 = "2006-01-02T15:04:05Z07:00"
