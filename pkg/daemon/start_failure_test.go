// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"testing"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/config"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/k8s"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/tracing"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
)

// The platform tells a credential problem from anything else by the reason, so
// a missing credential -- caught before any OpenAPI call -- must not fall
// through to the catch-all.
func TestStartFailureReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"no credential configured", fmt.Errorf("load: %w", config.ErrNoCredentials),
			tracing.EventCredentialServiceAbnormal},
		{"credential rejected", fmt.Errorf("resolve: %w",
			&zecCom.ZenlayerCloudSdkError{Code: apiErr.AuthSignatureFailure}),
			tracing.EventCredentialServiceAbnormal},
		{"endpoint unreachable", fmt.Errorf("resolve: %w",
			&zecCom.ZenlayerCloudSdkError{Code: zecCom.NetworkError}),
			tracing.EventOpenApiUnreachable},
		{"anything else", fmt.Errorf("cilium agent not ready"), tracing.EventDaemonStartFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := startFailureReason(tc.err); got != tc.want {
				t.Errorf("startFailureReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

type syncEventRecorder struct {
	k8s.K8sService
	reasons []string
}

func (r *syncEventRecorder) RecordNodeEventSync(_ context.Context, _, reason, _ string) error {
	r.reasons = append(r.reasons, reason)
	return nil
}

// A stop requested mid-start is not a failure: a rolling update landing during
// startup would otherwise leave a Warning on the node.
func TestReportStartFailureSkipsShutdown(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want int
	}{
		{"success", context.Background(), nil, 0},
		{"failure", context.Background(), fmt.Errorf("boom"), 1},
		{"stopping", canceled, fmt.Errorf("boom"), 0},
		{"canceled error", context.Background(), fmt.Errorf("wait: %w", context.Canceled), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &syncEventRecorder{}
			reportStartFailure(tc.ctx, r, tc.err)
			if len(r.reasons) != tc.want {
				t.Errorf("recorded %d events, want %d", len(r.reasons), tc.want)
			}
			if tc.want == 1 && r.reasons[0] != tracing.EventDaemonStartFailed {
				t.Errorf("reason = %q, want %q", r.reasons[0], tracing.EventDaemonStartFailed)
			}
		})
	}
}
