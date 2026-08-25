// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"context"
	"os"

	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	zlogadapter "logur.dev/adapter/zerolog"
	"logur.dev/logur"

	tsdkClient "go.temporal.io/sdk/client"
	tsdkConverter "go.temporal.io/sdk/converter"

	cconfig "github.com/NVIDIA/infra-controller/rest-api/common/pkg/config"
	cwq "github.com/NVIDIA/infra-controller/rest-api/workflow/pkg/queue"
)

// siteQueueClient wraps a per-site Temporal client and rewrites the generic
// SiteTaskQueue ("site") dispatch queue to the site's own task queue — its
// site ID. Handlers uniformly set TaskQueue: queue.SiteTaskQueue, but the
// site-agent that executes these workflows subscribes to a queue named after
// the site ID (TEMPORAL_SUBSCRIBE_QUEUE, set to the site ID by deployment
// config — see deploy/kustomize/base/site-agent/configmap.yaml). Nothing
// polls a queue literally named "site" inside a per-site namespace, so
// dispatching there hangs until the 20s workflow timeout. Rewriting at this
// single choke point fixes every handler dispatch without touching the ~100
// call sites individually.
type siteQueueClient struct {
	tsdkClient.Client
	siteID string
}

// ExecuteWorkflow rewrites the task queue before delegating to the real client.
func (c *siteQueueClient) ExecuteWorkflow(ctx context.Context, options tsdkClient.StartWorkflowOptions, workflow interface{}, args ...interface{}) (tsdkClient.WorkflowRun, error) {
	if options.TaskQueue == cwq.SiteTaskQueue {
		options.TaskQueue = c.siteID
	}
	return c.Client.ExecuteWorkflow(ctx, options, workflow, args...)
}

// ClientPool contains Temporal clients for different site agents
type ClientPool struct {
	tcfg        *cconfig.TemporalConfig
	IDClientMap map[string]tsdkClient.Client
	mutex       sync.RWMutex
}

// GetClientByID returns a Temporal client for given cluster ID
func (cp *ClientPool) GetClientByID(siteID uuid.UUID) (tsdkClient.Client, error) {
	cp.mutex.RLock()

	client, found := cp.IDClientMap[siteID.String()]
	if found {
		cp.mutex.RUnlock()
		return client, nil
	}

	cp.mutex.RUnlock()

	// A client for the site wasn't found in the site-cache
	// So grab a write-lock so we can create and cache a client.
	cp.mutex.Lock()
	defer cp.mutex.Unlock()

	// Now that we have our exclusive lock,
	// make sure that it wasn't after a previous
	// write-lock holder already updated the client cache.
	client, found = cp.IDClientMap[siteID.String()]
	if found {
		return client, nil
	}

	tLogger := logur.LoggerToKV(zlogadapter.New(zerolog.New(os.Stderr)))

	tc, err := tsdkClient.NewLazyClient(tsdkClient.Options{
		HostPort:  fmt.Sprintf("%v:%v", cp.tcfg.Host, cp.tcfg.Port),
		Namespace: siteID.String(),
		ConnectionOptions: tsdkClient.ConnectionOptions{
			TLS: cp.tcfg.ClientTLSCfg,
		},
		DataConverter: tsdkConverter.NewCompositeDataConverter(
			tsdkConverter.NewNilPayloadConverter(),
			tsdkConverter.NewByteSlicePayloadConverter(),
			tsdkConverter.NewProtoJSONPayloadConverterWithOptions(tsdkConverter.ProtoJSONPayloadConverterOptions{
				AllowUnknownFields: true,
			}),
			tsdkConverter.NewProtoPayloadConverter(),
			tsdkConverter.NewJSONPayloadConverter(),
		),
		Logger: tLogger,
	})

	if err != nil {
		log.Panic().Err(err).Str("Temporal Namespace", siteID.String()).
			Msg("failed to create Temporal client for site")
		return nil, err
	}

	wrapped := &siteQueueClient{Client: tc, siteID: siteID.String()}

	cp.IDClientMap[siteID.String()] = wrapped

	return wrapped, nil
}

// NewClientPool initializes and returns a new client pool
func NewClientPool(tcfg *cconfig.TemporalConfig) *ClientPool {
	return &ClientPool{
		tcfg:        tcfg,
		IDClientMap: map[string]tsdkClient.Client{},
	}
}
