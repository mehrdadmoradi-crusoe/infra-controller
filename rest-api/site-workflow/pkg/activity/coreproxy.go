// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activity

import (
	"context"
	"errors"
	"os"

	"github.com/NVIDIA/infra-controller/rest-api/common/pkg/coreproxy"
	cloudutils "github.com/NVIDIA/infra-controller/rest-api/common/pkg/util"
	swe "github.com/NVIDIA/infra-controller/rest-api/site-workflow/pkg/error"
	"github.com/NVIDIA/infra-controller/rest-api/site-workflow/pkg/grpc/client"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// ManageCoreProxy is the activity wrapper for the generic NICo Core gRPC proxy.
type ManageCoreProxy struct {
	coreGrpcAtomicClient *client.CoreGrpcAtomicClient
	// secretKey decrypts the redacted secret fields carried in
	// coreproxy.Request.EncryptedSecrets. It is the shared site key (the
	// site/cluster ID), matching the key the cloud used to encrypt them.
	secretKey string
	// actorCACertPath and actorCAKeyPath name the CA this site mints
	// per-actor client certificates from, for requests that carry an Actor.
	// Read on each such call rather than at startup so a renewed CA is picked
	// up without a restart. Empty means the site cannot attribute calls.
	actorCACertPath string
	actorCAKeyPath  string
}

// NewManageCoreProxy returns a new ManageCoreProxy bound to the Core gRPC client
// and the site secret key used to decrypt redacted request fields.
func NewManageCoreProxy(coreGrpcClient *client.CoreGrpcAtomicClient, secretKey string) ManageCoreProxy {
	return ManageCoreProxy{
		coreGrpcAtomicClient: coreGrpcClient,
		secretKey:            secretKey,
	}
}

// WithActorCA returns a copy able to make attributed calls: requests carrying
// an Actor are made with a certificate minted from this CA naming that actor,
// rather than with the site's own. Either path empty leaves attribution off.
func (m ManageCoreProxy) WithActorCA(certPath, keyPath string) ManageCoreProxy {
	m.actorCACertPath = certPath
	m.actorCAKeyPath = keyPath
	return m
}

// InvokeCoreGRPCOnSite proxies a single Core gRPC call described by req. Any
// redacted secret fields are decrypted and merged back into the request before
// it reaches Core. The request body is intentionally never logged because it
// may contain secrets (e.g. BMC credential passwords); only the method is.
func (m *ManageCoreProxy) InvokeCoreGRPCOnSite(ctx context.Context, req coreproxy.Request) (coreproxy.Response, error) {
	logger := log.With().Str("Activity", "InvokeCoreGRPCOnSite").Str("Method", req.FullMethod).Logger()
	logger.Info().Msg("Starting activity")

	reqJSON := req.RequestJSON
	if len(req.EncryptedSecrets) > 0 {
		secretsJSON := cloudutils.DecryptData(req.EncryptedSecrets, m.secretKey)
		merged, err := coreproxy.MergeSecrets(reqJSON, secretsJSON)
		if err != nil {
			logger.Warn().Err(err).Msg("Failed to merge request secrets")
			return coreproxy.Response{}, swe.WrapErr(err)
		}
		reqJSON = merged
	}

	// A request that names who is asking is made as that person, not as the
	// site. This is decided before anything else about the connection: a site
	// that cannot attribute must refuse, because sending the call under its own
	// certificate would have Core record the change as nobody's.
	if req.Actor != nil {
		return m.invokeAsActor(ctx, logger, req, reqJSON)
	}

	grpcClient := m.coreGrpcAtomicClient.GetClient()
	if grpcClient == nil {
		return coreproxy.Response{}, client.ErrCoreGrpcClientNotConnected
	}

	respJSON, err := grpcClient.InvokeJSON(ctx, req.FullMethod, reqJSON)
	if err != nil {
		logger.Warn().Err(err).Msg("Failed to proxy Core gRPC call")
		return coreproxy.Response{}, swe.WrapErr(err)
	}

	logger.Info().Msg("Completed activity")
	return coreproxy.Response{ResponseJSON: respJSON}, nil
}

// invokeAsActor makes one Core call presenting a freshly minted certificate
// that names req.Actor, on its own connection, which is closed afterwards.
// The actor's user is logged: it is the audit record, not a secret.
func (m *ManageCoreProxy) invokeAsActor(ctx context.Context, logger zerolog.Logger, req coreproxy.Request, reqJSON []byte) (coreproxy.Response, error) {
	logger = logger.With().Str("Actor", req.Actor.User).Str("ActorOrg", req.Actor.Org).Logger()

	if m.actorCACertPath == "" || m.actorCAKeyPath == "" {
		logger.Warn().Msg("Request names an actor but this site has no actor CA; refusing rather than calling as the site")
		return coreproxy.Response{}, swe.WrapErr(errors.New(coreproxy.ActorUnsupportedMessage))
	}
	if m.coreGrpcAtomicClient == nil || m.coreGrpcAtomicClient.Config == nil {
		return coreproxy.Response{}, client.ErrCoreGrpcClientNotConnected
	}

	caCertPEM, err := os.ReadFile(m.actorCACertPath)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to read actor CA certificate")
		return coreproxy.Response{}, swe.WrapErr(err)
	}
	caKeyPEM, err := os.ReadFile(m.actorCAKeyPath)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to read actor CA key")
		return coreproxy.Response{}, swe.WrapErr(err)
	}

	cert, err := client.MintActorCertificate(caCertPEM, caKeyPEM, *req.Actor, client.ActorCertTTL)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to mint actor certificate")
		return coreproxy.Response{}, swe.WrapErr(err)
	}

	actorClient, err := client.NewCoreGrpcClientWithCertificate(m.coreGrpcAtomicClient.Config, cert)
	if err != nil {
		logger.Warn().Err(err).Msg("Failed to connect to Core as the actor")
		return coreproxy.Response{}, swe.WrapErr(err)
	}
	defer func() {
		if cerr := actorClient.Close(); cerr != nil {
			logger.Debug().Err(cerr).Msg("Closing actor connection")
		}
	}()

	respJSON, err := actorClient.InvokeJSON(ctx, req.FullMethod, reqJSON)
	if err != nil {
		logger.Warn().Err(err).Msg("Failed to proxy Core gRPC call as the actor")
		return coreproxy.Response{}, swe.WrapErr(err)
	}

	logger.Info().Msg("Completed activity as the actor")
	return coreproxy.Response{ResponseJSON: respJSON}, nil
}
