// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	cwssaws "github.com/NVIDIA/infra-controller/rest-api/workflow-schema/schema/site-agent/workflows/v1"
)

func redfishResult(status, body string, at time.Time) *cwssaws.OptionalRedfishActionResult {
	return &cwssaws.OptionalRedfishActionResult{Result: &cwssaws.RedfishActionResult{
		Headers:     map[string]string{"Server": "iDRAC/9", "Set-Cookie": "session=secret"},
		Status:      status,
		Body:        body,
		CompletedAt: timestamppb.New(at),
	}}
}

// "Applied" is the Core saying it sent the change; it is not the controller
// saying it took it. The two must not look the same, which is what these
// counts are for -- this case is the one the demo actually produces, where the
// simulated controller answers 405 to a BIOS settings POST.
func TestRedfishActionAppliedButRefusedByTheControllerIsNotSuccess(t *testing.T) {
	applied := time.Date(2026, 9, 22, 23, 51, 8, 0, time.UTC)
	action := &cwssaws.RedfishAction{
		RequestId: 2,
		Requester: "tenant@example.com",
		Approvers: []string{"provider@example.com", "tenant@example.com"},
		Action:    "Bios.ChangeSettings",
		Target:    "/redfish/v1/Systems/1/Bios/Settings",
		AppliedAt: timestamppb.New(applied),
		Applier:   strPtr("provider@example.com"),
		Results: []*cwssaws.OptionalRedfishActionResult{
			redfishResult("405 Method Not Allowed", `{"error":{"code":"Base.1.0.GeneralError"}}`, applied.Add(time.Second)),
		},
	}

	out := NewAPIRedfishAction(action)

	assert.Equal(t, RedfishActionApplied, out.Status)
	require.Len(t, out.Outcomes, 1)
	assert.Equal(t, "405 Method Not Allowed", out.Outcomes[0].Status)
	assert.False(t, out.Outcomes[0].Succeeded)
	assert.Equal(t, 1, out.OutcomesFailed)
	assert.Equal(t, 0, out.OutcomesPending)
	require.NotNil(t, out.Outcomes[0].Detail)
	assert.Contains(t, *out.Outcomes[0].Detail, "GeneralError")
	require.NotNil(t, out.Outcomes[0].CompletedAt)

	// Response headers never reach the caller: they carry the controller's
	// banner and session material.
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "iDRAC")
	assert.NotContains(t, string(raw), "session=secret")
	assert.NotContains(t, string(raw), "Set-Cookie")
}

// A controller that has not answered yet is counted, not omitted, so an
// applied change with no outcomes reads as "waiting", never as "fine".
func TestRedfishActionCountsControllersThatHaveNotAnswered(t *testing.T) {
	now := time.Now()
	action := &cwssaws.RedfishAction{
		RequestId: 3,
		AppliedAt: timestamppb.New(now),
		Results: []*cwssaws.OptionalRedfishActionResult{
			redfishResult("200 OK", "", now),
			{Result: nil},
			redfishResult("not executed", "machine serial did not match original serial at time of request creation. IP address was reused", now),
		},
	}

	out := NewAPIRedfishAction(action)

	require.Len(t, out.Outcomes, 2, "the unanswered slot is counted, not listed")
	assert.True(t, out.Outcomes[0].Succeeded)
	assert.Nil(t, out.Outcomes[0].Detail, "an empty body is absent, not an empty string")
	assert.False(t, out.Outcomes[1].Succeeded, "the Core's own refusal is a failure, not a success")
	assert.Equal(t, 1, out.OutcomesPending)
	assert.Equal(t, 1, out.OutcomesFailed)
}

// A controller's response body is not allowed to make the record large.
func TestRedfishActionOutcomeDetailIsBounded(t *testing.T) {
	huge := strings.Repeat("x", RedfishOutcomeDetailLimit*3)
	action := &cwssaws.RedfishAction{
		AppliedAt: timestamppb.Now(),
		Results:   []*cwssaws.OptionalRedfishActionResult{redfishResult("500 Internal Server Error", huge, time.Now())},
	}

	out := NewAPIRedfishAction(action)

	require.Len(t, out.Outcomes, 1)
	require.NotNil(t, out.Outcomes[0].Detail)
	assert.LessOrEqual(t, len(*out.Outcomes[0].Detail), RedfishOutcomeDetailLimit+len("…"))
	assert.True(t, strings.HasSuffix(*out.Outcomes[0].Detail, "…"))
}

// Before an apply there is nothing to report, and the record says so with an
// empty list rather than a missing field.
func TestRedfishActionBeforeApplyHasNoOutcomes(t *testing.T) {
	out := NewAPIRedfishAction(&cwssaws.RedfishAction{RequestId: 1, Approvers: []string{"a"}})

	assert.NotNil(t, out.Outcomes)
	assert.Empty(t, out.Outcomes)
	assert.Equal(t, 0, out.OutcomesPending)
	assert.Equal(t, 0, out.OutcomesFailed)
	assert.Equal(t, RedfishActionAwaitingApproval, out.Status)
}

// Only a 2xx is success; a status the Core wrote in its own words, or one that
// does not parse, is not assumed to be.
func TestRedfishStatusSucceeded(t *testing.T) {
	for status, want := range map[string]bool{
		"200 OK": true, "202 Accepted": true, "204 No Content": true,
		"405 Method Not Allowed": false, "500 Internal Server Error": false,
		"not executed": false, "": false, "2": false, "2xx": false,
	} {
		assert.Equal(t, want, redfishStatusSucceeded(status), status)
	}
}
