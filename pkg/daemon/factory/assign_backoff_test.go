// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"errors"
	"testing"
	"time"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"

	daemonTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/ec2"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
)

// countingEC2 answers every assign with the same platform error and counts how
// many times it was asked. Everything else is unused by this path.
type countingEC2 struct {
	assignErr   error
	assignCalls int
	unassign    int
}

func (c *countingEC2) AssignPrivateIpAddress(string, []string, int) (*zecApi.BatchAssignNetworkInterfaceIpv4Response, error) {
	c.assignCalls++
	return nil, c.assignErr
}

func (c *countingEC2) UnAssignPrivateIpAddress(string, []string) (*zecApi.UnassignNetworkInterfaceIpv4Response, error) {
	c.unassign++
	return nil, nil
}

func (c *countingEC2) DescribeInstances([]string) (*zecApi.DescribeInstancesResponse, error) {
	return nil, nil
}
func (c *countingEC2) DescribeSubnets([]string) (*zecApi.DescribeSubnetsResponse, error) {
	return nil, nil
}
func (c *countingEC2) DescribeVpcs([]string) (*zecApi.DescribeVpcsResponse, error) { return nil, nil }
func (c *countingEC2) AssignNetworkInterfaceIpv6(string, ec2.IPv6Billing) (*zecApi.AssignNetworkInterfaceIpv6Response, error) {
	return nil, nil
}
func (c *countingEC2) UnassignNetworkInterfaceIpv6(string) (*zecApi.UnassignNetworkInterfaceIpv6Response, error) {
	return nil, nil
}
func (c *countingEC2) DescribeNetworkInterfaceAttributes(*zecApi.DescribeNetworkInterfacesRequest) (*zecApi.DescribeNetworkInterfacesResponse, error) {
	return nil, nil
}
func (c *countingEC2) CreateNetworkInterface(_, _, _ string, _ map[string]string) (*zecApi.CreateNetworkInterfaceResponse, error) {
	return nil, nil
}
func (c *countingEC2) AttachNetworkInterface(string, string) (*zecApi.AttachNetworkInterfaceResponse, error) {
	return nil, nil
}
func (c *countingEC2) DetachNetworkInterface(string) (*zecApi.DetachNetworkInterfaceResponse, error) {
	return nil, nil
}
func (c *countingEC2) DeleteNetworkInterface(string) (*zecApi.DeleteNetworkInterfaceResponse, error) {
	return nil, nil
}

// TestAllocIPv4AddressesStopsOnTerminalPlatformErrors pins which refusals end
// the assign backoff after a single attempt.
//
// The backoff exists for errors a retry can clear. These cannot: nothing frees
// an address -- on the subnet or on this interface -- while the backoff itself
// is running, so every extra attempt returns the same refusal.
//
// The call is made holding blockPause, which also stops GC and pool shrink, so
// the cost is not only latency: one refused call burns the whole backoff,
// roughly a minute, against a CNI ADD budget of 118s (daemonRPCTimeout),
// leaving pods in ContainerCreating while another interface has free slots.
//
// OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT is the case that was missing.
// The forbidAssign check in eni_multi_ip.go already lists it alongside
// INVALID_NIC_PRIVATE_IP_EXCEED_LIMIT, so the interface does get marked full --
// but only after this loop has burned its budget.
func TestAllocIPv4AddressesStopsOnTerminalPlatformErrors(t *testing.T) {
	for _, code := range []string{
		apiErr.OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT, // the one that was missing
		apiErr.INVALID_NIC_PRIVATE_IP_EXCEED_LIMIT,             // its other spelling
		apiErr.OPERATION_DENIED_SUBNET_IP_INSUFFICIENT,         // the subnet equivalents
		apiErr.InsufficientIpInSubnet,
	} {
		t.Run(code, func(t *testing.T) {
			fake := &countingEC2{assignErr: zecCom.NewZenlayerCloudSdkError(code, "refused", "req-1")}
			e := &ZecAPIImpl{IPStack: daemonTypes.IPStackIPv4, ec2Client: fake}

			start := time.Now()
			_, err := e.AllocIPv4Addresses("eni-1", "aa:bb:cc:dd:ee:01", 1)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatalf("AllocIPv4Addresses succeeded on %s", code)
			}
			if fake.assignCalls != 1 {
				t.Errorf("the platform was asked %d times for %s, want exactly 1: a retry cannot "+
					"clear this refusal, and the whole backoff is spent holding blockPause, which "+
					"also stops GC and pool shrink", fake.assignCalls, code)
			}
			// A generous ceiling: it only has to separate "no backoff at all"
			// from the real backoff budget of tens of seconds.
			if elapsed > 5*time.Second {
				t.Errorf("AllocIPv4Addresses took %v for %s; a terminal error must not back off", elapsed, code)
			}
		})
	}
}

// TestAllocIPv4AddressesRetriesRecoverableErrors is the negative control.
//
// Without it the test above is satisfied by a backoff that gives up on
// everything, which would turn a transient platform hiccup into a failed pod.
func TestAllocIPv4AddressesRetriesRecoverableErrors(t *testing.T) {
	// This one runs the backoff to exhaustion: backoff.APIWriteOps is six steps
	// from 4s with factor 1.5 and jitter 0.5, so it costs ~67s, against no wait
	// at all for the terminal cases above.
	if testing.Short() {
		t.Skip("runs the assign backoff to exhaustion (~67s)")
	}
	fake := &countingEC2{assignErr: zecCom.NewZenlayerCloudSdkError("SOME_TRANSIENT_THING", "try again", "req-2")}
	e := &ZecAPIImpl{IPStack: daemonTypes.IPStackIPv4, ec2Client: fake}

	if _, err := e.AllocIPv4Addresses("eni-1", "aa:bb:cc:dd:ee:01", 1); err == nil {
		t.Fatalf("AllocIPv4Addresses succeeded although every attempt failed")
	}
	if fake.assignCalls < 2 {
		t.Errorf("the platform was asked %d times for a non-terminal error, want more than 1: "+
			"giving up on the first hiccup turns a transient failure into a failed pod", fake.assignCalls)
	}
}

// TestAllocIPv4AddressesReturnsAnUnwrappableError pins that the platform's
// error code survives the trip back to the caller.
//
// Two callers in eni_multi_ip.go decide what to do about a refused assign by
// looking at the code:
//
//   - worker() sets forbidAssign via ErrChainEqual, which is errors.As under
//     the hood. If the chain is broken it silently matches nothing, the
//     interface is never marked full, and every later order retries it -- the
//     exact failure forbidAssign exists to prevent.
//   - receiveRes() refreshes the instance limit and wakes GC on the same two
//     interface-full codes.
//
// A wrap with %s or %v renders the code into the message and drops the error
// it came from. errors.As then fails while the text still contains the code,
// so a substring check keeps working and a code check quietly stops: the two
// callers disagree about the same error and nothing reports it.
func TestAllocIPv4AddressesReturnsAnUnwrappableError(t *testing.T) {
	code := apiErr.OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT
	fake := &countingEC2{assignErr: zecCom.NewZenlayerCloudSdkError(code, "refused", "req-3")}
	e := &ZecAPIImpl{IPStack: daemonTypes.IPStackIPv4, ec2Client: fake}

	_, err := e.AllocIPv4Addresses("eni-1", "aa:bb:cc:dd:ee:01", 1)
	if err == nil {
		t.Fatalf("AllocIPv4Addresses succeeded although the platform refused")
	}

	var sdkErr *zecCom.ZenlayerCloudSdkError
	if !errors.As(err, &sdkErr) {
		t.Fatalf("errors.As could not reach the platform error in %q; ErrChainEqual is errors.As "+
			"under the hood, so every caller that decides by error code silently matches nothing", err)
	}
	if sdkErr.GetCode() != code {
		t.Errorf("unwrapped code = %q, want %q", sdkErr.GetCode(), code)
	}
	if !(&apiErr.OpenApiErrCodeChain{}).WithErrCodes(code).ErrChainEqual(err) {
		t.Errorf("ErrChainEqual did not match %q on the returned error", code)
	}
}
