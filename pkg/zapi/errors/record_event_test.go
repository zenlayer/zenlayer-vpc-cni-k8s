// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package errors_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/tracing"
	zerrors "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
)

// captureNodeEvents redirects node events into a slice for the duration of one
// test. The recorder is process-global, so the cleanup is mandatory.
func captureNodeEvents(t *testing.T) *[]string {
	t.Helper()
	var reasons []string
	tracing.RegisterEventRecorder(func(_, reason, _ string) {
		reasons = append(reasons, reason)
	}, nil)
	t.Cleanup(func() { tracing.RegisterEventRecorder(nil, nil) })
	return &reasons
}

// TestRecordOpenAPIErrEventReachesWrappedErrors pins that the node event is
// still raised when the platform error arrives wrapped. A bare type assertion
// answers "no" for any error that has been given context, and the only symptom
// is silence: these events are what tell an operator why pods stopped starting.
// The only non-test caller, metrics.ZecAPIErrInc, forwards the error it was
// given, so this guards a property nothing depends on yet.
func TestRecordOpenAPIErrEventReachesWrappedErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{
			name: "quota, wrapped the way the assign path wraps",
			err: fmt.Errorf("failed to assign private ips on ENI eni-1, err: %w",
				sdkErr(zerrors.QuotaExceededEni)),
			want: tracing.EventVpcResourceQuotaExceeded,
		},
		{
			name: "subnet exhaustion, wrapped twice",
			err: fmt.Errorf("AllocENI: failed to create ENI: %w",
				fmt.Errorf("failed to create network interface: %w",
					sdkErr(zerrors.OPERATION_DENIED_SUBNET_IP_INSUFFICIENT))),
			want: tracing.EventInsufficientIpInSubnet,
		},
		{
			name: "the older spelling of subnet exhaustion",
			err:  fmt.Errorf("assign: %w", sdkErr(zerrors.InsufficientIpInSubnet)),
			want: tracing.EventInsufficientIpInSubnet,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reasons := captureNodeEvents(t)
			zerrors.RecordOpenAPIErrEvent(tc.err, zerrors.EventInfoField{Key: "api", Value: "test"})
			if len(*reasons) == 0 {
				t.Fatalf("no node event was recorded for %q; an operator running kubectl describe node "+
					"sees nothing at all about why the address could not be obtained", tc.err)
			}
			if (*reasons)[0] != tc.want {
				t.Errorf("event reason = %q, want %q", (*reasons)[0], tc.want)
			}
		})
	}
}

// TestRecordOpenAPIErrEventIgnoresNonPlatformErrors is the negative control for
// the errors.As walk: an error that never came from the SDK records nothing.
func TestRecordOpenAPIErrEventIgnoresNonPlatformErrors(t *testing.T) {
	reasons := captureNodeEvents(t)
	zerrors.RecordOpenAPIErrEvent(fmt.Errorf("no route to metadata service"))
	if len(*reasons) != 0 {
		t.Errorf("recorded %v for an error that never came from the platform", *reasons)
	}
}

// TestRecordOpenAPIErrEventKeepsTheCodeInTheMessage pins that the message an
// operator reads names the error code, the API the caller supplied, and the
// request id.
func TestRecordOpenAPIErrEventKeepsTheCodeInTheMessage(t *testing.T) {
	var messages []string
	tracing.RegisterEventRecorder(func(_, _, message string) {
		messages = append(messages, message)
	}, nil)
	t.Cleanup(func() { tracing.RegisterEventRecorder(nil, nil) })

	zerrors.RecordOpenAPIErrEvent(fmt.Errorf("assign: %w", sdkErr(zerrors.QuotaExceededEni)),
		zerrors.EventInfoField{Key: "api", Value: "AssignPrivateIpAddress"})
	if len(messages) == 0 {
		t.Fatal("no node event was recorded")
	}
	for _, want := range []string{zerrors.QuotaExceededEni, "AssignPrivateIpAddress", "req-1"} {
		if !strings.Contains(messages[0], want) {
			t.Errorf("event message = %q, want it to mention %q", messages[0], want)
		}
	}
}
