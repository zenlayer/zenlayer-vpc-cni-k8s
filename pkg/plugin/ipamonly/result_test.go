// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package ipamonly

import (
	"testing"

	current "github.com/containernetworking/cni/pkg/types/100"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/rpc"
)

func netConf(v4, v6, subnet4, subnet6, gw4, gw6 string) *rpc.NetConf {
	return &rpc.NetConf{
		IfName: "eth0",
		PodIP:  &rpc.IPSet{IPv4: v4, IPv6: v6},
		ENIInfo: &rpc.ENIInfo{
			MAC:       "52:54:00:b9:66:f1",
			Subnet:    &rpc.IPSet{IPv4: subnet4, IPv6: subnet6},
			GatewayIP: &rpc.IPSet{IPv4: gw4, IPv6: gw6},
		},
	}
}

// TestBuildResultSandboxStaysEmpty pins the field that decides how the caller
// reads the interface we report.
//
// An empty sandbox is what marks an interface as the host-side uplink for the
// addresses that follow. Filling it in -- the obvious thing to do, since the
// full plugin path does exactly that -- makes the caller treat our ENI as a
// container interface, and it stops being able to find the uplink at all.
func TestBuildResultSandboxStaysEmpty(t *testing.T) {
	result, err := buildResult("0.4.0",
		netConf("10.0.0.12", "", "10.0.0.0/20", "", "10.0.0.1", ""), "eth3")
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if len(result.Interfaces) != 1 {
		t.Fatalf("got %d interfaces, want 1", len(result.Interfaces))
	}
	if result.Interfaces[0].Sandbox != "" {
		t.Errorf("Sandbox = %q, want empty: a non-empty sandbox makes the caller read this as a "+
			"container interface rather than the uplink", result.Interfaces[0].Sandbox)
	}
	if result.Interfaces[0].Name != "eth3" {
		t.Errorf("Name = %q, want the host eni name", result.Interfaces[0].Name)
	}
	if result.Interfaces[0].Mac != "" {
		t.Errorf("Mac = %q, want empty: on this platform a failed-over interface keeps the MAC "+
			"of the port it replaced, so a caller preferring the MAC resolves the wrong device",
			result.Interfaces[0].Mac)
	}
}

// TestBuildResultOmitsRoutes pins that no routes are reported. A delegated IPAM
// result contributes addresses and a gateway; the caller builds the pod's
// routes from the device it created, and anything set here is dropped. Setting
// them anyway would suggest a contract that is not being honoured.
func TestBuildResultOmitsRoutes(t *testing.T) {
	result, err := buildResult("0.4.0",
		netConf("10.0.0.12", "2001:db8::c", "10.0.0.0/20", "2001:db8::/96", "10.0.0.1", "fe80::1"), "eth0")
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if len(result.Routes) != 0 {
		t.Errorf("got %d routes, want none", len(result.Routes))
	}
}

// TestBuildResultDualStackKeepsBothFamilies pins that both addresses are
// reported and both point at the single interface entry.
func TestBuildResultDualStackKeepsBothFamilies(t *testing.T) {
	result, err := buildResult("0.4.0",
		netConf("10.0.0.12", "2001:db8::c", "10.0.0.0/20", "2001:db8::/96", "10.0.0.1", "fe80::1"), "eth0")
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	if len(result.IPs) != 2 {
		t.Fatalf("got %d addresses, want 2", len(result.IPs))
	}
	for _, ip := range result.IPs {
		if ip.Interface == nil || *ip.Interface != 0 {
			t.Errorf("address %s points at interface %v, want 0", ip.Address.String(), ip.Interface)
		}
		if ip.Gateway == nil {
			t.Errorf("address %s has no gateway", ip.Address.String())
		}
	}
}

// TestBuildResultRejectsAnEmptyReply keeps an address-less reply from reaching
// the caller, which would otherwise build a pod device with nothing on it.
func TestBuildResultRejectsAnEmptyReply(t *testing.T) {
	for _, tc := range []struct {
		name string
		nc   *rpc.NetConf
	}{
		{"no pod ip", &rpc.NetConf{ENIInfo: &rpc.ENIInfo{}}},
		{"no eni info", &rpc.NetConf{PodIP: &rpc.IPSet{IPv4: "10.0.0.12"}}},
		{"an address with no gateway",
			netConf("10.0.0.12", "", "10.0.0.0/20", "", "", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildResult("0.4.0", tc.nc, "eth0"); err == nil {
				t.Error("want an error, got none")
			}
		})
	}
}

// TestMergeResultsReindexesInterfaces pins that a second interface's addresses
// keep pointing at their own interface after the merge.
//
// Off-by-one here is silent in the single-interface case that covers almost
// every pod, and puts a multi-network pod's second address on the first
// interface.
func TestMergeResultsReindexesInterfaces(t *testing.T) {
	first, err := buildResult("0.4.0",
		netConf("10.0.0.12", "", "10.0.0.0/20", "", "10.0.0.1", ""), "eth0")
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	second, err := buildResult("0.4.0",
		netConf("10.0.1.30", "", "10.0.0.0/20", "", "10.0.0.1", ""), "eth3")
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}

	merged := mergeResults("0.4.0", []*current.Result{first, second})

	if len(merged.Interfaces) != 2 || len(merged.IPs) != 2 {
		t.Fatalf("got %d interfaces and %d addresses, want 2 and 2",
			len(merged.Interfaces), len(merged.IPs))
	}
	if *merged.IPs[0].Interface != 0 {
		t.Errorf("first address points at interface %d, want 0", *merged.IPs[0].Interface)
	}
	if *merged.IPs[1].Interface != 1 {
		t.Errorf("second address points at interface %d, want 1", *merged.IPs[1].Interface)
	}
	if merged.Interfaces[1].Name != "eth3" {
		t.Errorf("second interface = %q, want eth3", merged.Interfaces[1].Name)
	}
}

// TestMergeResultsDoesNotMutateItsInputs keeps the reindexing from rewriting the
// per-interface results, which the caller still holds.
func TestMergeResultsDoesNotMutateItsInputs(t *testing.T) {
	first, _ := buildResult("0.4.0", netConf("10.0.0.12", "", "10.0.0.0/20", "", "10.0.0.1", ""), "eth0")
	second, _ := buildResult("0.4.0", netConf("10.0.1.30", "", "10.0.0.0/20", "", "10.0.0.1", ""), "eth3")

	_ = mergeResults("0.4.0", []*current.Result{first, second})

	if *second.IPs[0].Interface != 0 {
		t.Errorf("merging rewrote its input: second result's address now points at interface %d",
			*second.IPs[0].Interface)
	}
}

// TestPodHostIPsNarrowsToHostMasks pins that the addresses handed to the rule
// builder are host routes.
//
// The daemon reports the pod address with its subnet mask. Using that mask in
// an ip rule would match every pod in the subnet, so one pod's DEL would remove
// rules belonging to all of them.
func TestPodHostIPsNarrowsToHostMasks(t *testing.T) {
	result, err := buildResult("0.4.0",
		netConf("10.0.0.12", "2001:db8::c", "10.0.0.0/20", "2001:db8::/96", "10.0.0.1", "fe80::1"), "eth0")
	if err != nil {
		t.Fatalf("buildResult: %v", err)
	}
	for _, ip := range podHostIPs(result) {
		ones, bits := ip.Mask.Size()
		if ones != bits {
			t.Errorf("address %s kept a /%d mask, want a host route", ip.String(), ones)
		}
	}
}
