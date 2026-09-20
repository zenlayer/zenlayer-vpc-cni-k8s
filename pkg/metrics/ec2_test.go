// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"errors"
	"testing"

	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/ec2"
)

// stubEC2 returns err from every call. The responses are nil throughout: these
// tests exercise the metric bookkeeping around the call, not the call itself.
type stubEC2 struct{ err error }

func (s stubEC2) DescribeInstances([]string) (*zecApi.DescribeInstancesResponse, error) {
	return nil, s.err
}
func (s stubEC2) DescribeSubnets([]string) (*zecApi.DescribeSubnetsResponse, error) {
	return nil, s.err
}
func (s stubEC2) DescribeVpcs([]string) (*zecApi.DescribeVpcsResponse, error) {
	return nil, s.err
}
func (s stubEC2) AssignPrivateIpAddress(string, []string, int) (*zecApi.BatchAssignNetworkInterfaceIpv4Response, error) {
	return nil, s.err
}
func (s stubEC2) UnAssignPrivateIpAddress(string, []string) (*zecApi.UnassignNetworkInterfaceIpv4Response, error) {
	return nil, s.err
}
func (s stubEC2) AssignIpv6Addresses(string, []string, int) (interface{}, error) {
	return nil, s.err
}
func (s stubEC2) UnassignIpv6Addresses(string, []string) (interface{}, error) {
	return nil, s.err
}
func (s stubEC2) AssignNetworkInterfaceIpv6(string, ec2.IPv6Billing) (*zecApi.AssignNetworkInterfaceIpv6Response, error) {
	return nil, s.err
}
func (s stubEC2) UnassignNetworkInterfaceIpv6(string) (*zecApi.UnassignNetworkInterfaceIpv6Response, error) {
	return nil, s.err
}
func (s stubEC2) DescribeNetworkInterfaceAttributes(*zecApi.DescribeNetworkInterfacesRequest) (*zecApi.DescribeNetworkInterfacesResponse, error) {
	return nil, s.err
}
func (s stubEC2) CreateNetworkInterface(string, string, string, map[string]string) (*zecApi.CreateNetworkInterfaceResponse, error) {
	return nil, s.err
}
func (s stubEC2) AttachNetworkInterface(string, string) (*zecApi.AttachNetworkInterfaceResponse, error) {
	return nil, s.err
}
func (s stubEC2) DetachNetworkInterface(string) (*zecApi.DetachNetworkInterfaceResponse, error) {
	return nil, s.err
}
func (s stubEC2) DeleteNetworkInterface(string) (*zecApi.DeleteNetworkInterfaceResponse, error) {
	return nil, s.err
}

// TestMetricEC2WrapperLabelCardinality guards a bug the compiler cannot see:
// WithLabelValues takes variadic strings, so a call site passing a different
// number of values than its metric declares labels compiles and panics on the
// first call. Ec2APILatency declares three, Ec2ApiReq one, Ec2APIErr two.
//
// Both outcomes are covered: err == nil takes the success path, a non-nil err
// additionally runs ZecAPIErrInc.
func TestMetricEC2WrapperLabelCardinality(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"failure", errors.New("boom")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewMetricEC2Wrapper(stubEC2{err: tc.err})

			// Every method MetricEC2Wrapper defines, since each has its own
			// WithLabelValues calls.
			calls := map[string]func(){
				"DescribeInstances":                  func() { _, _ = w.DescribeInstances(nil) },
				"DescribeSubnets":                    func() { _, _ = w.DescribeSubnets(nil) },
				"DescribeVpcs":                       func() { _, _ = w.DescribeVpcs(nil) },
				"AssignPrivateIpAddress":             func() { _, _ = w.AssignPrivateIpAddress("nic", nil, 1) },
				"UnAssignPrivateIpAddress":           func() { _, _ = w.UnAssignPrivateIpAddress("nic", nil) },
				"UnassignNetworkInterfaceIpv6":       func() { _, _ = w.UnassignNetworkInterfaceIpv6("nic") },
				"AssignNetworkInterfaceIpv6":         func() { _, _ = w.AssignNetworkInterfaceIpv6("nic", ec2.IPv6Billing{}) },
				"DescribeNetworkInterfaceAttributes": func() { _, _ = w.DescribeNetworkInterfaceAttributes(nil) },
				"CreateNetworkInterface":             func() { _, _ = w.CreateNetworkInterface("n", "subnet", "sg", nil) },
				"AttachNetworkInterface":             func() { _, _ = w.AttachNetworkInterface("nic", "inst") },
				"DetachNetworkInterface":             func() { _, _ = w.DetachNetworkInterface("nic") },
				"DeleteNetworkInterface":             func() { _, _ = w.DeleteNetworkInterface("nic") },
			}
			for name, call := range calls {
				t.Run(name, func(t *testing.T) {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("panicked: %v", r)
						}
					}()
					call()
				})
			}
		})
	}
}

// TestErrLabelIsBoundedAndNilSafe covers the two properties a metric label
// value must have. Boundedness matters because prometheus keeps one time series
// per distinct label combination, so an error message or a request id would
// grow memory without limit; the nil case is pinned so an absent error labels
// as empty rather than as the "error" constant.
func TestErrLabelIsBoundedAndNilSafe(t *testing.T) {
	if got := errLabel(nil); got != "" {
		t.Errorf("errLabel(nil) = %q, want empty", got)
	}

	// An error with no bounded form must collapse to a constant, never to its
	// own text.
	verbose := errors.New("HttpRequestStatus: 404, HttpResponse: <500 bytes of body>")
	if got := errLabel(verbose); got != "error" {
		t.Errorf("errLabel(unstructured) = %q, want %q", got, "error")
	}

	// An error that can name itself supplies the label.
	if got := errLabel(labeled{"404"}); got != "404" {
		t.Errorf("errLabel(labeled) = %q, want %q", got, "404")
	}

	// Wrapped, since callers routinely wrap before this is reached.
	if got := errLabel(wrapped{labeled{"503"}}); got != "503" {
		t.Errorf("errLabel(wrapped) = %q, want %q", got, "503")
	}
}

// ZecReqErrCode is reached on the success path: every MetricEC2Wrapper method
// passes err to it for the latency metric's label before checking it. ZecReqId
// has no caller outside tests.
func TestZecReqErrCodeNilSafe(t *testing.T) {
	if got := ZecReqErrCode(nil); got != "" {
		t.Errorf("ZecReqErrCode(nil) = %q, want empty", got)
	}
	if got := ZecReqId(nil); got != "" {
		t.Errorf("ZecReqId(nil) = %q, want empty", got)
	}
}

type labeled struct{ code string }

func (l labeled) Error() string       { return "labeled error " + l.code }
func (l labeled) MetricLabel() string { return l.code }

type wrapped struct{ inner error }

func (w wrapped) Error() string { return "wrapped: " + w.inner.Error() }
func (w wrapped) Unwrap() error { return w.inner }
