// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"strings"
	"testing"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/tracing"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

const (
	v6OnlySubnetID = "subnet-v6-only"
	dualSubnetID   = "subnet-dual"
	testVpcID      = "vpc-test"
	testRegionID   = "reg-test"
)

// subnetsEC2 answers DescribeSubnets from a fixed list and stubs the rest of the
// client through countingEC2.
type subnetsEC2 struct {
	countingEC2
	subnets []*zecApi.SubnetInfo
}

func (c *subnetsEC2) DescribeSubnets([]string) (*zecApi.DescribeSubnetsResponse, error) {
	return &zecApi.DescribeSubnetsResponse{
		Response: &zecApi.DescribeSubnetsResponseParams{
			TotalCount: zecCom.Integer(len(c.subnets)),
			DataSet:    c.subnets,
		},
	}, nil
}

// v6OnlySubnet is the shape this file is about: an IPv6 CIDR and no IPv4 one.
// Such subnets exist on the platform, so the daemon meets them for real.
func v6OnlySubnet() *zecApi.SubnetInfo {
	return &zecApi.SubnetInfo{
		SubnetId:             zecCom.String(v6OnlySubnetID),
		VpcId:                zecCom.String(testVpcID),
		RegionId:             zecCom.String(testRegionID),
		CidrBlock:            zecCom.String(""),
		Ipv6CidrBlock:        zecCom.String("2001:2:0:3f20::/64"),
		Ipv6GatewayIpAddress: zecCom.String("fe80::1"),
	}
}

func dualSubnet() *zecApi.SubnetInfo {
	return &zecApi.SubnetInfo{
		SubnetId:         zecCom.String(dualSubnetID),
		VpcId:            zecCom.String(testVpcID),
		RegionId:         zecCom.String(testRegionID),
		CidrBlock:        zecCom.String("10.0.0.0/20"),
		GatewayIpAddress: zecCom.String("10.0.0.1"),
		Ipv6CidrBlock:    zecCom.String("2001:2:0:7775::/64"),
		UsageIpv4Count:   zecCom.Integer(10),
	}
}

// countingTracer records how many node events were raised, which is what tells
// "reported" apart from "passed over in silence".
type countingTracer struct {
	events int
}

func (c *countingTracer) record(_, _, _ string) { c.events++ }

func newManagerUnderTest(t *testing.T, stack types.IPStack, subnets ...*zecApi.SubnetInfo) (
	*subnetManager, *countingTracer,
) {
	t.Helper()
	counter := &countingTracer{}
	tracer := tracing.NewTracer()
	tracer.RegisterEventRecorder(counter.record, nil)

	m, err := NewPodSubnetManager(testRegionID, testVpcID, &subnetsEC2{subnets: subnets},
		WithEventRecord(tracer), WithIPStack(stack))
	if err != nil {
		t.Fatalf("NewPodSubnetManager: %v", err)
	}
	return m.(*subnetManager), counter
}

// TestSubnetWithoutIPv4CidrIsRejectedAndReported pins both halves of how a
// subnet carrying no IPv4 CIDR is handled during a refresh.
//
// Rejected while IPv4 is on, because such a subnet cannot take a pod. It never
// reaches the cache, so nothing can hand it to the pool, and the refresh that
// found it fails rather than leaving a daemon running against a subnet it can
// place nothing in. That matches the two branches around it: a subnet belonging
// to another VPC and one whose CIDR will not parse are both refused as well.
//
// Reported, because the alternative is silence: the ratio inspectSubnet computes
// divides by the total, a zero total makes it NaN, NaN compares false against
// any threshold, and the subnet would look healthy for ever while every IPv4
// allocation against it failed.
//
// A mode that does not serve IPv4 keeps it. The entry is what carries the
// subnet's IPv6 CIDR and gateway, and for that mode nothing about the subnet is
// wrong -- reporting it would raise a warning on every poll for a subnet doing
// its job.
// cachedSubnet reads the two caches directly. GetPodSubnet must not be used
// here: on a miss it falls back to GetUpdatedPodSubnet, which fetches the subnet
// from the platform and files it under legacyPodSubnet -- so it answers non-nil
// for a subnet the refresh has just refused, and would hide what these tests are
// about.
func cachedSubnet(m *subnetManager, id string) *PodSubnet {
	m.lock.RLock()
	defer m.lock.RUnlock()
	if sb, ok := m.podSubnets[id]; ok {
		return sb
	}
	return m.legacyPodSubnet[id]
}

func TestSubnetWithoutIPv4CidrIsRejectedAndReported(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stack      types.IPStack
		wantKept   bool
		wantEvents int
		why        string
	}{
		{
			name:       "ipv4 refuses it",
			stack:      types.IPStackIPv4,
			wantKept:   false,
			wantEvents: 1,
			why:        "the subnet cannot serve the only family this mode has",
		},
		{
			name:       "dual refuses it",
			stack:      types.IPStackDual,
			wantKept:   false,
			wantEvents: 1,
			why:        "dual needs IPv4 as much as ipv4-only does",
		},
		{
			name:       "ipv6-only keeps it and stays quiet",
			stack:      types.IPStackIPv6,
			wantKept:   true,
			wantEvents: 0,
			why: "a mode that does not serve IPv4 has nothing to complain about, and the entry is " +
				"what carries the subnet's IPv6 CIDR and gateway",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, counter := newManagerUnderTest(t, tc.stack, v6OnlySubnet())
			flushErr := m.FlushSubnets(v6OnlySubnetID)

			if tc.wantKept && flushErr != nil {
				t.Fatalf("FlushSubnets: %v -- %s", flushErr, tc.why)
			}
			if !tc.wantKept {
				if flushErr == nil {
					t.Fatal("FlushSubnets succeeded for a subnet this mode cannot place a pod in")
				}
				// The id has to be in the message: it is the only thing that
				// tells an operator which of the configured subnets to fix.
				if !strings.Contains(flushErr.Error(), v6OnlySubnetID) {
					t.Errorf("the error does not name the subnet: %v", flushErr)
				}
			}

			sb := cachedSubnet(m, v6OnlySubnetID)
			if kept := sb != nil; kept != tc.wantKept {
				t.Fatalf("subnet kept = %v, want %v -- %s", kept, tc.wantKept, tc.why)
			}
			if sb != nil {
				if got := sb.Cidr(zecTypes.IPFamilyV6); got != "2001:2:0:3f20::/64" {
					t.Errorf("IPv6 cidr = %q, want it recorded even though the IPv4 one is missing", got)
				}
				if got := sb.GetAvailableIPv4AddressCount(); got != 0 {
					t.Errorf("available IPv4 count = %d, want 0 -- there is no IPv4 cidr to count", got)
				}
			}
			if counter.events != tc.wantEvents {
				t.Errorf("raised %d node events, want %d -- %s", counter.events, tc.wantEvents, tc.why)
			}
		})
	}
}

// TestSubnetWithoutIPv4CidrDoesNotTakeTheOthersWithIt pins the blast radius of
// that refusal.
//
// The refusal happens per subnet inside the describe loop, so the usable subnets
// in the same response are cached as normal and only the flush as a whole
// reports failure. Refusing the whole batch instead would mean one misconfigured
// subnet in the list cost the node every other subnet it was given.
func TestSubnetWithoutIPv4CidrDoesNotTakeTheOthersWithIt(t *testing.T) {
	m, _ := newManagerUnderTest(t, types.IPStackDual, v6OnlySubnet(), dualSubnet())

	err := m.FlushSubnets(v6OnlySubnetID, dualSubnetID)
	if err == nil {
		t.Fatal("FlushSubnets succeeded although one of the subnets carries no IPv4 cidr")
	}
	if !strings.Contains(err.Error(), v6OnlySubnetID) {
		t.Errorf("the error does not name the offending subnet: %v", err)
	}
	if strings.Contains(err.Error(), dualSubnetID) {
		t.Errorf("the error blames the usable subnet as well: %v", err)
	}

	if cachedSubnet(m, v6OnlySubnetID) != nil {
		t.Error("the v6-only subnet reached the cache; with no IPv4 cidr every allocation against " +
			"it would fail")
	}

	dual := cachedSubnet(m, dualSubnetID)
	if dual == nil {
		t.Fatal("the usable subnet was dropped along with the unusable one")
	}
	if !dual.Available(types.IPStackDual) {
		t.Error("the dual subnet is not available to a dual node; without this the assertion above " +
			"passes for an implementation that rejects every subnet")
	}
}

// TestSubnetTooSmallForPodsIsReported pins the other guard on the same division:
// a prefix leaving nothing after the three addresses the platform reserves makes
// the ratio meaningless rather than merely small.
func TestSubnetTooSmallForPodsIsReported(t *testing.T) {
	tiny := dualSubnet()
	tiny.SubnetId = zecCom.String("subnet-tiny")
	tiny.CidrBlock = zecCom.String("10.9.9.0/31")
	tiny.UsageIpv4Count = zecCom.Integer(0)

	m, counter := newManagerUnderTest(t, types.IPStackIPv4, tiny)
	if err := m.FlushSubnets("subnet-tiny"); err != nil {
		t.Fatalf("FlushSubnets: %v", err)
	}
	if counter.events == 0 {
		t.Error("a /31 raised no event; two addresses minus the three reserved is negative, and " +
			"the ratio built from it says nothing")
	}
	if sb := m.GetPodSubnet("subnet-tiny"); sb != nil && sb.Available(types.IPStackIPv4) {
		t.Error("a subnet with no usable address reports itself available")
	}
}
