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

const legacySubnetID = "subnet-legacy"

// roomySubnet is the same subnet after addresses were freed: a /20 with only a
// handful in use.
func roomySubnet(id string) *zecApi.SubnetInfo {
	return &zecApi.SubnetInfo{
		SubnetId:         zecCom.String(id),
		VpcId:            zecCom.String(testVpcID),
		RegionId:         zecCom.String(testRegionID),
		CidrBlock:        zecCom.String("10.0.0.0/20"),
		GatewayIpAddress: zecCom.String("10.0.0.1"),
		UsageIpv4Count:   zecCom.Integer(5),
	}
}

// TestLegacySubnetsAreRefreshed is the regression this file exists for.
//
// UpdateSubnetsStatus collected the ids to refresh from podSubnets alone, and
// legacyPodSubnet holds exactly the subnets an attached interface uses that
// podSubnets does not -- the two are disjoint by construction, so a legacy entry
// was never refreshed once.
//
// The count it froze at is not cosmetic: subnetMonitor clears forbidSubnet only
// while subnet.Available() is true and Available() reads that count, so a legacy
// subnet first seen full left its interfaces forbidden for the life of the
// daemon. The addresses on them could not be handed to any pod however much room
// the subnet later had.
func TestLegacySubnetsAreRefreshed(t *testing.T) {
	m, _ := newManagerUnderTest(t, types.IPStackIPv4, roomySubnet(legacySubnetID))

	// A legacy entry as GetUpdatedPodSubnet leaves one: cached, aged, and with
	// the count it had when the subnet was full.
	m.legacyPodSubnet[legacySubnetID] = &PodSubnet{
		RegionId:                  testRegionID,
		VpcId:                     testVpcID,
		SubnetId:                  legacySubnetID,
		IPv4Cidr:                  "10.0.0.0/20",
		AvailableIPv4AddressCount: 0,
		LastUpdate:                time.Now().Add(-time.Hour),
	}

	if err := m.UpdateSubnetsStatus(); err != nil {
		t.Fatalf("UpdateSubnetsStatus: %v", err)
	}

	sb := m.GetPodSubnet(legacySubnetID)
	if sb == nil {
		t.Fatal("the legacy subnet was dropped by the refresh")
	}
	if got := sb.GetAvailableIPv4AddressCount(); got <= 0 {
		t.Fatalf("available count = %d after a refresh that reported room, want a positive number -- "+
			"a legacy subnet frozen at 0 leaves its interfaces forbidden for ever", got)
	}
	if !sb.Available(types.IPStackIPv4) {
		t.Error("the refreshed subnet still reports itself unavailable, so subnetMonitor would never " +
			"clear forbidSubnet on the interfaces using it")
	}
}

// TestLegacySubnetsAreNotRefreshedWhileYoung pins that the aging option applies
// to them as well. Without it every monitor tick would describe every legacy
// subnet, and the aging option exists precisely to keep the periodic refresh
// from becoming one API call per tick per subnet.
func TestLegacySubnetsAreNotRefreshedWhileYoung(t *testing.T) {
	fake := &subnetsEC2{subnets: []*zecApi.SubnetInfo{roomySubnet(legacySubnetID)}}
	m, _ := newManagerUnderTest(t, types.IPStackIPv4)
	m.apiClient = fake

	m.legacyPodSubnet[legacySubnetID] = &PodSubnet{
		SubnetId:                  legacySubnetID,
		IPv4Cidr:                  "10.0.0.0/20",
		AvailableIPv4AddressCount: 0,
		LastUpdate:                time.Now(),
	}

	if err := m.UpdateSubnetsStatus(WithAging(time.Hour)); err != nil {
		t.Fatalf("UpdateSubnetsStatus: %v", err)
	}

	if got := m.GetPodSubnet(legacySubnetID).GetAvailableIPv4AddressCount(); got != 0 {
		t.Errorf("available count = %d, want it left alone: the entry is younger than the aging limit", got)
	}
}
