// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"encoding/json"
	"sort"
)

// The management controller keeps the record of what happened to a host below
// the operating system: power events, correctable and uncorrectable memory
// errors, thermal excursions, a chassis being opened. When a host reboots
// unexpectedly, that record is where the reason is, and it survives the reboot.
//
// The controller serves it as Redfish log services, which are untyped JSON and
// vendor-shaped: the service names, the entry fields and the severities all
// vary. These types are the normalisation, so that a caller reads one shape
// rather than learning each vendor's.

// Event log sources: which of the controller's log collections to read.
const (
	// EventLogSourceSystem is the host's own log, where POST, memory and
	// thermal events land. This is the one an unexplained reboot is explained
	// by.
	EventLogSourceSystem = "system"
	// EventLogSourceManager is the controller's own log, which records what was
	// done to the controller rather than to the host.
	EventLogSourceManager = "manager"
)

// EventLogSources is every source this API accepts, so the handler and the
// validation message cannot disagree about the list.
var EventLogSources = []string{EventLogSourceSystem, EventLogSourceManager}

// Normalised severities. Redfish uses OK, Warning and Critical; anything a
// vendor invents beyond those is reported as it came rather than forced into
// one of them.
const (
	EventLogSeverityOK       = "OK"
	EventLogSeverityWarning  = "Warning"
	EventLogSeverityCritical = "Critical"
)

// APIEventLogEntry is one entry from a controller log.
type APIEventLogEntry struct {
	// Service is the log service the entry came from, as the controller names
	// it, so entries stay attributable when several services are read together.
	Service string `json:"service"`
	// ID is the entry's identifier within its service. It is unique per
	// service, not across the host.
	ID string `json:"id"`
	// Created is when the controller recorded the entry, in RFC 3339 as the
	// controller reported it. Absent when the controller did not say.
	Created *string `json:"created"`
	// Severity is OK, Warning or Critical where the controller uses Redfish's
	// own values, and whatever it reported otherwise.
	Severity *string `json:"severity"`
	// Message is the human-readable text.
	Message string `json:"message"`
	// MessageID is the Redfish registry identifier, for example
	// `Alert.1.0.PowerSupplyFailure`, which is what to match on rather than
	// the message text.
	MessageID *string `json:"messageId"`
	// EntryType distinguishes an SEL record from an OEM or Event record.
	EntryType *string `json:"entryType"`
	// Resolved is set when the controller tracks resolution and says so.
	Resolved *bool `json:"resolved"`
}

// APIEventLog is the entries read from a host's controller.
type APIEventLog struct {
	MachineID string `json:"machineId"`
	// Source is which collection was read: system or manager.
	Source string `json:"source"`
	// Services are the log services that were found and read, so a caller can
	// see what the controller offered rather than guessing why a log is short.
	Services []string `json:"services"`
	// Entries, newest first where the controller supplied timestamps.
	Entries []APIEventLogEntry `json:"entries"`
	// Truncated is true when the controller held more entries than the limit
	// returned, so a short list is not mistaken for a quiet host.
	Truncated bool `json:"truncated"`
}

// redfishLogEntry is the subset of a Redfish LogEntry this API reads. The
// controller sends a great deal more; anything not named here is ignored
// rather than passed through, so a vendor extension cannot become part of this
// API's contract by accident.
type redfishLogEntry struct {
	ID        string `json:"Id"`
	Name      string `json:"Name"`
	Created   string `json:"Created"`
	Severity  string `json:"Severity"`
	Message   string `json:"Message"`
	MessageID string `json:"MessageId"`
	EntryType string `json:"EntryType"`
	Resolved  *bool  `json:"Resolved"`
	OdataID   string `json:"@odata.id"`
}

// redfishCollection is any Redfish collection: a list of links.
type redfishCollection struct {
	Members []struct {
		OdataID string `json:"@odata.id"`
	} `json:"Members"`
	MembersCount int `json:"Members@odata.count"`
}

// ParseRedfishCollection reads the member links out of a Redfish collection.
func ParseRedfishCollection(body []byte) ([]string, error) {
	var collection redfishCollection
	if err := json.Unmarshal(body, &collection); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(collection.Members))
	for _, member := range collection.Members {
		if member.OdataID != "" {
			out = append(out, member.OdataID)
		}
	}
	return out, nil
}

// redfishLogEntries is a log service's Entries collection, which carries the
// entries inline rather than as links.
type redfishLogEntries struct {
	Members []redfishLogEntry `json:"Members"`
	Total   int               `json:"Members@odata.count"`
}

// ParseRedfishLogEntries normalises one log service's entries.
//
// The service name is carried onto every entry, because entry identifiers only
// mean something within their service.
func ParseRedfishLogEntries(service string, body []byte) ([]APIEventLogEntry, int, error) {
	var parsed redfishLogEntries
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, 0, err
	}

	out := make([]APIEventLogEntry, 0, len(parsed.Members))
	for i := range parsed.Members {
		member := parsed.Members[i]
		entry := APIEventLogEntry{
			Service:  service,
			ID:       member.ID,
			Message:  member.Message,
			Resolved: member.Resolved,
		}
		// An entry with no Message but a Name is reported by its Name; an
		// empty message would otherwise read as an event with no content.
		if entry.Message == "" {
			entry.Message = member.Name
		}
		entry.Created = optionalString(member.Created)
		entry.Severity = optionalString(member.Severity)
		entry.MessageID = optionalString(member.MessageID)
		entry.EntryType = optionalString(member.EntryType)
		out = append(out, entry)
	}

	total := parsed.Total
	if total < len(out) {
		total = len(out)
	}
	return out, total, nil
}

// SortEventLogEntriesNewestFirst orders entries by their recorded time,
// newest first, leaving entries without a time at the end in the order the
// controller gave them.
//
// The timestamps are RFC 3339 as the controller wrote them, so they sort
// lexically; parsing them only to re-serialise would discard the controller's
// own precision and offset.
func SortEventLogEntriesNewestFirst(entries []APIEventLogEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		left, right := entries[i].Created, entries[j].Created
		switch {
		case left == nil && right == nil:
			return false
		case left == nil:
			return false
		case right == nil:
			return true
		default:
			return *left > *right
		}
	})
}
