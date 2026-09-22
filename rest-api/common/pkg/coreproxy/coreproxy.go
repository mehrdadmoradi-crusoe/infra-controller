// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package coreproxy holds the contract shared between the cloud REST API and
// the on-site agent for the generic NICo Core gRPC proxy.
//
// Instead of a bespoke Temporal workflow + activity + typed request for every
// Core-backed REST operation, curated REST handlers can validate their input,
// build typed forge.Forge requests, and dispatch each Core invocation through
// this generic workflow. A single REST request may dispatch zero, one, or many
// proxied Core calls; this package defines the transport for one such call. The
// on-site site-agent worker runs a generic activity that performs the actual
// gRPC call against Core. The REST surface stays curated and designed; this is
// purely the internal cloud->site transport.
//
// The request/response payloads are carried as protojson (json.RawMessage) so
// they render as readable JSON in the Temporal UI. Secret fields (e.g. a BMC
// credential password) are redacted from that readable JSON and carried
// separately as an AES-GCM ciphertext (EncryptedSecrets) so they never appear
// in Temporal history in cleartext; the site decrypts and merges them back
// before the Core call.
package coreproxy

import (
	"encoding/json"
	"fmt"
	"maps"
)

// WorkflowName is the Temporal workflow type registered by the site-agent and
// started by the cloud REST API. It must match the workflow function name in
// site-workflow/pkg/workflow (InvokeCoreGRPC).
const WorkflowName = "InvokeCoreGRPC"

// RedactedPlaceholder is the value substituted for a redacted secret field in
// the Temporal-visible request JSON.
const RedactedPlaceholder = "[REDACTED]"

// ActorUnsupportedMessage is the error a site returns when a request names an
// Actor but the site holds no CA to mint an actor certificate from. It is a
// shared constant because the REST tier recognises it: the caller then sees
// the same answer as when the Core itself refuses to attribute a change --
// unavailable here, and retrying will not change that -- rather than a bare
// internal error.
const ActorUnsupportedMessage = "site cannot attribute the call to a user: no actor CA is configured"

// Request is the generic proxy workflow/activity input.
type Request struct {
	// FullMethod is the gRPC method, either fully qualified
	// ("/forge.Forge/CreateCredential") or bare ("CreateCredential").
	FullMethod string `json:"fullMethod"`

	// RequestJSON is the protojson-encoded forge.Forge request message with
	// secret fields redacted. Kept as json.RawMessage so it is human-readable
	// in the Temporal UI.
	RequestJSON json.RawMessage `json:"requestJson,omitempty"`

	// EncryptedSecrets is the AES-GCM ciphertext of the redacted secret fields
	// (a JSON object of fieldName -> value). It is opaque (base64) in Temporal
	// history; the site decrypts it with the shared site key and merges the
	// values back into RequestJSON before invoking Core.
	EncryptedSecrets []byte `json:"encryptedSecrets,omitempty"`

	// Actor is the human on whose behalf the call is made, for the few Core
	// methods that record one. Core takes the requester and approvers of a
	// Redfish action from the subject of the client certificate presented to
	// it, so when Actor is set the site does not use its own service
	// certificate: it mints a short-lived leaf naming this actor from a CA the
	// Core trusts for exactly that, and presents that instead. Nil means the
	// call is the site's own, which is every other method.
	//
	// This is deliberately visible in Temporal history. Who asked for a BIOS
	// change is not a secret; it is the audit record.
	Actor *Actor `json:"actor,omitempty"`
}

// Actor names the human behind a proxied Core call. The fields map onto the
// certificate subject the Core reads: User to CN, Org to O, Group to OU.
type Actor struct {
	// User is the identity Core records as requester, approver or applier.
	// It must be non-empty and stable for the same person, because Core
	// refuses a second approval from the same user by comparing it.
	User string `json:"user"`
	// Org is the caller's org, informational to Core.
	Org string `json:"org,omitempty"`
	// Group is the caller's role within the org, informational to Core.
	Group string `json:"group,omitempty"`
}

// Response is the generic proxy workflow/activity output.
type Response struct {
	// ResponseJSON is the protojson-encoded forge.Forge response message.
	ResponseJSON json.RawMessage `json:"responseJson,omitempty"`
}

// RedactSecrets removes the named top-level fields from reqJSON, replacing each
// present field with RedactedPlaceholder, and returns the redacted request plus
// a JSON object of the extracted secret fields. secretFields are protojson
// field names. When no named field is present, secretsJSON is nil and redacted
// equals the input.
func RedactSecrets(reqJSON []byte, secretFields []string) (redacted []byte, secretsJSON []byte, err error) {
	if len(secretFields) == 0 {
		return reqJSON, nil, nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(reqJSON, &m); err != nil {
		return nil, nil, fmt.Errorf("redact secrets: %w", err)
	}

	placeholder, err := json.Marshal(RedactedPlaceholder)
	if err != nil {
		return nil, nil, fmt.Errorf("redact secrets: %w", err)
	}

	secrets := make(map[string]json.RawMessage)
	for _, f := range secretFields {
		if v, ok := m[f]; ok {
			secrets[f] = v
			m[f] = placeholder
		}
	}
	if len(secrets) == 0 {
		return reqJSON, nil, nil
	}

	redacted, err = json.Marshal(m)
	if err != nil {
		return nil, nil, fmt.Errorf("redact secrets: %w", err)
	}
	secretsJSON, err = json.Marshal(secrets)
	if err != nil {
		return nil, nil, fmt.Errorf("redact secrets: %w", err)
	}
	return redacted, secretsJSON, nil
}

// MergeSecrets overlays the secret fields back into a redacted request, undoing
// RedactSecrets on the site side after decryption. An empty secretsJSON returns
// redacted unchanged.
func MergeSecrets(redacted []byte, secretsJSON []byte) ([]byte, error) {
	if len(secretsJSON) == 0 {
		return redacted, nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(redacted, &m); err != nil {
		return nil, fmt.Errorf("merge secrets: %w", err)
	}
	var secrets map[string]json.RawMessage
	if err := json.Unmarshal(secretsJSON, &secrets); err != nil {
		return nil, fmt.Errorf("merge secrets: %w", err)
	}

	maps.Copy(m, secrets)
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("merge secrets: %w", err)
	}
	return out, nil
}
