// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	tmocks "go.temporal.io/sdk/mocks"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	coreproxy "github.com/NVIDIA/infra-controller/rest-api/common/pkg/coreproxy"
)

// Handler tests reach Core through a mocked proxy, and every fixture in this
// package wires that mock to succeed: WorkflowRun.Get is registered as
// .Return(nil) and has no way to fail. An endpoint whose Core call can never
// succeed in production therefore passes its whole test file.
//
// That is not hypothetical. The Redfish action endpoints were merged green and
// were incapable of working against a real Core, which only surfaced when they
// were exercised against one. See TestCreateRedfishActionReportsCoreRefusal.
//
// newCoreProxyMock is the missing half: a proxy that can be told to fail a
// named method, so a handler's behaviour on a Core refusal is covered by a
// test rather than by someone running it.
//
// Callers registering no errors get the same behaviour as the hand-rolled
// mocks, so this can replace them incrementally rather than all at once.
func newCoreProxyMock(
	t *testing.T,
	responses map[string]proto.Message,
	coreErrs map[string]error,
) (*tmocks.Client, *[]coreproxy.Request) {
	t.Helper()

	calls := &[]coreproxy.Request{}
	// The proxy is synchronous -- ExecuteWorkflow for one call is immediately
	// followed by Get for that same call -- so the method recorded on the way
	// in is the one whose response Get should serve.
	current := new(string)

	// Recording happens in Run, not in the argument matcher: testify evaluates
	// matchers while searching for an expectation, so a matcher with side
	// effects records calls that were never made.
	record := func(args mock.Arguments) {
		req, ok := args.Get(3).(coreproxy.Request)
		if !ok {
			return
		}
		*calls = append(*calls, req)
		*current = req.FullMethod
	}

	tsc := &tmocks.Client{}

	// Failing methods first, so their narrower matcher is found before the
	// catch-all below.
	for method, coreErr := range coreErrs {
		method, coreErr := method, coreErr
		failing := &tmocks.WorkflowRun{}
		failing.On("Get", mock.Anything, mock.Anything).Return(coreErr)
		tsc.On("ExecuteWorkflow", mock.Anything, mock.Anything, coreproxy.WorkflowName,
			mock.MatchedBy(func(req coreproxy.Request) bool { return req.FullMethod == method }),
		).Run(record).Return(failing, nil)
	}

	succeeding := &tmocks.WorkflowRun{}
	succeeding.On("Get", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		response, ok := responses[*current]
		if !ok || response == nil {
			return
		}
		out, ok := args.Get(1).(*coreproxy.Response)
		if !ok {
			return
		}
		respJSON, err := protojson.Marshal(response)
		require.NoError(t, err)
		out.ResponseJSON = respJSON
	}).Return(nil)

	tsc.On("ExecuteWorkflow", mock.Anything, mock.Anything, coreproxy.WorkflowName,
		mock.Anything).Run(record).Return(succeeding, nil)

	return tsc, calls
}

// coreMethodsCalled is the Core methods invoked, in order.
func coreMethodsCalled(calls *[]coreproxy.Request) []string {
	if calls == nil {
		return nil
	}
	out := make([]string, 0, len(*calls))
	for _, c := range *calls {
		out = append(out, c.FullMethod)
	}
	return out
}
