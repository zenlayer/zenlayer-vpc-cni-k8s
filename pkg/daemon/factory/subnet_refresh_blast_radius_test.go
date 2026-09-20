// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"testing"
	"time"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
)

// filteringEC2 answers only for the ids it is asked about, the way the platform
// does. subnetsEC2 returns its whole list whatever the request, which hides
// every question about what a describe for a specific set of ids comes back
// with.
type filteringEC2 struct {
	countingEC2
	subnets []*zecApi.SubnetInfo
}

func (c *filteringEC2) DescribeSubnets(ids []string) (*zecApi.DescribeSubnetsResponse, error) {
	var out []*zecApi.SubnetInfo
	for _, id := range ids {
		for _, sb := range c.subnets {
			if zecCom.ToString(sb.SubnetId) == id {
				out = append(out, sb)
			}
		}
	}
	return &zecApi.DescribeSubnetsResponse{
		Response: &zecApi.DescribeSubnetsResponseParams{
			TotalCount: zecCom.Integer(len(out)),
			DataSet:    out,
		},
	}, nil
}

// agedSubnet is a cache entry old enough for the refresh to pick up.
func agedSubnet(id, cidr string, available int) *PodSubnet {
	return &PodSubnet{
		RegionId:                  testRegionID,
		VpcId:                     testVpcID,
		SubnetId:                  id,
		IPv4Cidr:                  cidr,
		AvailableIPv4AddressCount: available,
		LastUpdate:                time.Now().Add(-time.Hour),
	}
}

// TestDeadLegacySubnetDoesNotStopTheRefresh pins the blast radius of a legacy
// entry the platform no longer knows.
//
// Legacy entries are the subnets an attached interface uses that the config does
// not name; nothing validates them and nothing else removes them. The platform
// answers an unknown id with silence rather than an error, so a batch holding
// nothing but a dead id comes back empty -- and it is this file's own
// empty-answer check that then fails the refresh, before the pass that would
// drop the entry. The entry survives to do the same next tick, and the failure
// is not free: the caller reads it as a reason to cordon interface creation and
// to leave forbidSubnet set, over a subnet no pod was going to use.
//
// The configured subnet is deliberately young here. With one in the batch the
// answer is non-empty, the dead id is dropped either way, and nothing is pinned.
func TestDeadLegacySubnetDoesNotStopTheRefresh(t *testing.T) {
	fake := &filteringEC2{subnets: []*zecApi.SubnetInfo{dualSubnet()}}
	m, _ := newManagerUnderTest(t, types.IPStackIPv4)
	m.apiClient = fake

	configured := agedSubnet(dualSubnetID, "10.0.0.0/20", 0)
	configured.LastUpdate = time.Now()
	m.podSubnets[dualSubnetID] = configured
	m.legacyPodSubnet["subnet-deleted"] = agedSubnet("subnet-deleted", "10.9.0.0/20", 0)

	// An explicit window: with the default of zero every entry counts as aged,
	// however recently it was written.
	if err := m.UpdateSubnetsStatus(WithAging(time.Minute)); err != nil {
		t.Fatalf("UpdateSubnetsStatus: %v -- a legacy subnet the platform dropped must not fail "+
			"the refresh", err)
	}

	if cachedSubnet(m, "subnet-deleted") != nil {
		t.Error("the legacy entry the platform no longer reports is still cached; nothing else " +
			"removes it, so every later refresh meets it again")
	}
	if cachedSubnet(m, dualSubnetID) == nil {
		t.Error("the configured subnet vanished from the cache")
	}
}

// TestUnusableSubnetIsKeptByTheRefresh pins that "cannot take a pod" and "gone"
// get different answers.
//
// A subnet the platform still answers for has not gone away. Dropping it from
// podSubnets makes IsPodSubnet false for every interface in it, which collapses
// the managed count, takes ENIAvailable to zero and with it the whole node's
// advertised capacity -- while the pods on those interfaces are still running
// and judgmentEvictPod starts finding every one of them over the limit. Zeroing
// the count instead is what stops the subnet being selected, and it is
// reversible the moment the platform reports the CIDR again.
func TestUnusableSubnetIsKeptByTheRefresh(t *testing.T) {
	lostCidr := dualSubnet()
	lostCidr.CidrBlock = zecCom.String("")

	fake := &filteringEC2{subnets: []*zecApi.SubnetInfo{lostCidr}}
	m, _ := newManagerUnderTest(t, types.IPStackIPv4)
	m.apiClient = fake

	m.podSubnets[dualSubnetID] = agedSubnet(dualSubnetID, "10.0.0.0/20", 4093)

	if err := m.UpdateSubnetsStatus(); err != nil {
		t.Fatalf("UpdateSubnetsStatus: %v", err)
	}

	sb := cachedSubnet(m, dualSubnetID)
	if sb == nil {
		t.Fatal("the subnet was deleted from the cache although the platform still reports it; " +
			"every interface in it becomes unmanaged and the node's capacity goes to zero")
	}
	if sb.Available(types.IPStackIPv4) {
		t.Error("a subnet reporting no IPv4 cidr is still selectable; every allocation against it " +
			"would fail")
	}
	if got := sb.GetAvailableIPv4AddressCount(); got != 0 {
		t.Errorf("available count = %d, want 0 -- the stale count is what would keep it selectable", got)
	}
}
