// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"sort"

	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

// Where a host physically sits, for the hosts in one rack.
//
// A scheduler placing a job needs to know which hosts share a failure domain
// and which share locality, and an acceptance needs to know that the rack was
// populated as designed. Both are questions about placement, not about the
// network.
//
// The control plane records placement alongside the switch and power shelf
// each host is attached to. Those are our inventory identifiers, and handing
// them out would let a caller correlate our infrastructure across racks, so
// they are replaced by labels that are meaningful only within one response:
// two hosts carrying the same label share that switch or that shelf. The
// grouping is the useful part; the identifier is ours.

// APIRackTopologyHost is one host's place in the rack.
type APIRackTopologyHost struct {
	MachineID string `json:"machineId"`
	// SlotNumber is the host's physical slot in the rack, counting from the
	// bottom, and ComputeTrayIndex is its tray.
	SlotNumber       *int32 `json:"slotNumber"`
	ComputeTrayIndex *int32 `json:"computeTrayIndex"`
	// SwitchGroup labels the switch this host is attached to. Hosts sharing a
	// label share a switch, which is what matters for locality and for a
	// shared failure. The label means nothing outside this response.
	SwitchGroup *string `json:"switchGroup"`
	// PowerGroup labels the power shelf feeding this host, on the same terms.
	PowerGroup *string `json:"powerGroup"`
}

// APIRackTopology is the placement of every host the rack holds.
type APIRackTopology struct {
	RackID string `json:"rackId"`
	// Hosts, ordered by slot so that reading the list reads the rack from the
	// bottom up.
	Hosts     []APIRackTopologyHost `json:"hosts"`
	HostCount int                   `json:"hostCount"`
	// HostsNotReported is how many hosts the rack's expected build places in it
	// that the Site returned no placement record for at all. The Site omits a
	// host it holds no management interface for, so without this a rack that is
	// half discovered would read as a smaller rack that is fully discovered.
	HostsNotReported int `json:"hostsNotReported"`
	// SwitchGroupCount and PowerGroupCount are how many distinct switches and
	// shelves the rack's hosts are spread across, which answers "is this one
	// failure domain or several" without walking the list.
	SwitchGroupCount int `json:"switchGroupCount"`
	PowerGroupCount  int `json:"powerGroupCount"`
}

// NewAPIRackTopology converts the Site's placement records for one rack.
//
// A host the Site returned with no placement is still listed, with its
// placement absent, because leaving it out would make a partially discovered
// rack look smaller than it is, which is the opposite of what an acceptance
// needs.
//
// The Site can also omit a host entirely: it reports placement by management
// address, so a host it holds no management interface for does not come back at
// all. Those cannot be listed, so expectedHosts -- how many the rack's expected
// build places in it -- is taken as well, and the difference is reported. A
// caller comparing hostCount against the rack's design would otherwise read a
// half-discovered rack as a complete smaller one.
func NewAPIRackTopology(rackID string, positions []*cwssaws.MachinePositionInfo, expectedHosts int) APIRackTopology {
	out := APIRackTopology{
		RackID: rackID,
		Hosts:  make([]APIRackTopologyHost, 0, len(positions)),
	}

	// Labels are assigned in slot order, so the same rack reads the same way
	// on every request rather than following whatever order the Site replied
	// in.
	ordered := make([]*cwssaws.MachinePositionInfo, 0, len(positions))
	for _, position := range positions {
		if position != nil {
			ordered = append(ordered, position)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return positionSortKey(ordered[i]) < positionSortKey(ordered[j])
	})

	switchLabels := newGroupLabeller("switch")
	powerLabels := newGroupLabeller("power")

	for _, position := range ordered {
		host := APIRackTopologyHost{
			MachineID:        position.GetMachineId().GetId(),
			SlotNumber:       position.PhysicalSlotNumber,
			ComputeTrayIndex: position.ComputeTrayIndex,
		}
		if id := position.GetSwitchId().GetId(); id != "" {
			host.SwitchGroup = switchLabels.labelFor(id)
		}
		if id := position.GetPowerShelfId().GetId(); id != "" {
			host.PowerGroup = powerLabels.labelFor(id)
		}
		out.Hosts = append(out.Hosts, host)
	}

	out.HostCount = len(out.Hosts)
	if missing := expectedHosts - out.HostCount; missing > 0 {
		out.HostsNotReported = missing
	}
	out.SwitchGroupCount = switchLabels.count()
	out.PowerGroupCount = powerLabels.count()
	return out
}

// positionSortKey orders hosts by slot, then tray, so that a host with no
// recorded slot sorts after those that have one rather than to the front.
func positionSortKey(position *cwssaws.MachinePositionInfo) string {
	slot := int32(1 << 20)
	if position.PhysicalSlotNumber != nil {
		slot = position.GetPhysicalSlotNumber()
	}
	tray := int32(1 << 20)
	if position.ComputeTrayIndex != nil {
		tray = position.GetComputeTrayIndex()
	}
	return fmt.Sprintf("%08d-%08d-%s", slot, tray, position.GetMachineId().GetId())
}

// groupLabeller turns our identifiers into labels that are stable within one
// response and meaningless outside it.
type groupLabeller struct {
	prefix string
	byID   map[string]string
}

func newGroupLabeller(prefix string) *groupLabeller {
	return &groupLabeller{prefix: prefix, byID: map[string]string{}}
}

func (g *groupLabeller) labelFor(id string) *string {
	if existing, ok := g.byID[id]; ok {
		return &existing
	}
	label := fmt.Sprintf("%s-%d", g.prefix, len(g.byID)+1)
	g.byID[id] = label
	return &label
}

func (g *groupLabeller) count() int {
	return len(g.byID)
}
