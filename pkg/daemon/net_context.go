// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// netContext carries the pod a request is for, and its logger, through the
// allocate and release paths.
type netContext struct {
	context.Context
	pod *types.Pod
	res []types.ResourceItem // granted so far, for rollback on failure
	log zlog.Logger
}

func (ctx *netContext) Log() zlog.Logger {
	return ctx.log.WithFields(zlog.Fields{
		"Namespace":          ctx.pod.Namespace,
		"Name":               ctx.pod.Name,
		"SandboxContainerId": ctx.pod.SandboxContainerId,
	})
}
