// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/rpc"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// relClient records the ReleaseIP calls it receives.
type relClient struct {
	rpc.CNIIpamClient
	calls []*rpc.ReleaseIPRequest
	resp  *rpc.ReleaseIPResponse
	err   error
}

func (c *relClient) ReleaseIP(_ context.Context, in *rpc.ReleaseIPRequest, _ ...grpc.CallOption) (*rpc.ReleaseIPResponse, error) {
	c.calls = append(c.calls, in)
	return c.resp, c.err
}

func k8sArgs() *zecTypes.K8SArgs {
	a := &zecTypes.K8SArgs{}
	a.K8S_POD_NAME = "orphan"
	a.K8S_POD_NAMESPACE = "default"
	a.K8S_POD_INFRA_CONTAINER_ID = "sandbox-1"
	return a
}

// TestReleaseWithoutRecordAlwaysAsksTheDaemon covers the two states GetIPInfo
// answers PodNotFound for: no record at all, and a record belonging to another
// sandbox. DEL used to return success at that point without calling ReleaseIP,
// which made the daemon's lost-record recovery unreachable from a real teardown
// -- the address stayed held and its two ip rules were never removed. The
// release has to be attempted in both states.
func TestReleaseWithoutRecordAlwaysAsksTheDaemon(t *testing.T) {
	c := &relClient{resp: &rpc.ReleaseIPResponse{Code: rpc.ErrorCode_Success}}

	releaseWithoutRecord(context.Background(), zlog.GetLogger(), c, k8sArgs())

	if len(c.calls) != 1 {
		t.Fatalf("ReleaseIP called %d times, want 1", len(c.calls))
	}
	got := c.calls[0]
	if got.K8SPodName != "orphan" || got.K8SPodNamespace != "default" {
		t.Errorf("released %s/%s, want default/orphan", got.K8SPodNamespace, got.K8SPodName)
	}
	// The sandbox id has to be passed through: it is what lets the daemon tell a
	// replayed DEL from a lost record, and declining the replay is what stops a
	// same-named pod's live address from being released.
	if got.K8SPodInfraContainerId != "sandbox-1" {
		t.Errorf("sandbox id = %q, want sandbox-1", got.K8SPodInfraContainerId)
	}
}

// TestReleaseWithoutRecordSwallowsFailure pins that a failed release does not
// turn into a failed DEL. kubelet retries DEL for a long time and a
// non-idempotent one left containerd accumulating dead sandboxes; reconcile is
// the backstop for the address instead.
func TestReleaseWithoutRecordSwallowsFailure(t *testing.T) {
	for name, c := range map[string]*relClient{
		"transport error": {err: errors.New("connection refused")},
		"error code":      {resp: &rpc.ReleaseIPResponse{Code: rpc.ErrorCode_ReleaseIPFailure}},
		"nil response":    {},
	} {
		t.Run(name, func(t *testing.T) {
			// Panicking or propagating would both break DEL idempotency; the
			// call returns nothing, so reaching the next line is the assertion.
			releaseWithoutRecord(context.Background(), zlog.GetLogger(), c, k8sArgs())
			if len(c.calls) != 1 {
				t.Errorf("ReleaseIP called %d times, want 1", len(c.calls))
			}
		})
	}
}
