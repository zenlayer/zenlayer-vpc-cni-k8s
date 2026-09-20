// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"testing"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
)

// foreignSubnet exists on the platform and is perfectly healthy -- it just
// belongs to another VPC and region than the one this daemon serves.
func foreignSubnet(id string) *zecApi.SubnetInfo {
	sb := dualSubnet()
	sb.SubnetId = zecCom.String(id)
	sb.VpcId = zecCom.String("vpc-somebody-else")
	sb.RegionId = zecCom.String("reg-somebody-else")
	return sb
}

// TestOnDemandLookupAppliesTheSameChecksAsTheRefresh is the regression this file
// exists for.
//
// GetPodSubnet falls back to GetUpdatedPodSubnet on a cache miss, and that
// function used to compute the counts inline rather than through inspectSubnet.
// The two had drifted: the on-demand path checked neither the vpc nor the region
// and accepted a subnet with no IPv4 CIDR, so a subnet the refresh had just
// refused came straight back in through the next pod's lookup and was filed
// under legacyPodSubnet.
func TestOnDemandLookupAppliesTheSameChecksAsTheRefresh(t *testing.T) {
	const foreignID = "subnet-foreign"

	tests := []struct {
		name   string
		stack  types.IPStack
		subnet *zecApi.SubnetInfo
		id     string
		why    string
	}{
		{
			name:   "another vpc and region",
			stack:  types.IPStackIPv4,
			subnet: foreignSubnet(foreignID),
			id:     foreignID,
			why:    "this daemon serves one vpc; a subnet in another is not its to hand out",
		},
		{
			name:   "no IPv4 cidr while IPv4 is on",
			stack:  types.IPStackIPv4,
			subnet: v6OnlySubnet(),
			id:     v6OnlySubnetID,
			why:    "the refresh refuses it, so the on-demand lookup must not put it back",
		},
		{
			name:   "no IPv4 cidr on a dual node",
			stack:  types.IPStackDual,
			subnet: v6OnlySubnet(),
			id:     v6OnlySubnetID,
			why:    "dual needs IPv4 as much as ipv4-only does",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newManagerUnderTest(t, tt.stack, tt.subnet)

			if _, err := m.GetUpdatedPodSubnet(tt.id); err == nil {
				t.Fatalf("GetUpdatedPodSubnet succeeded -- %s", tt.why)
			}
			if cachedSubnet(m, tt.id) != nil {
				t.Errorf("the subnet was cached anyway; the next lookup would hand it out -- %s", tt.why)
			}
			// The fallback is what GetPodSubnet uses, so this is the answer a
			// pod ADD actually gets.
			if m.GetPodSubnet(tt.id) != nil {
				t.Errorf("GetPodSubnet answered with a subnet this node cannot place a pod in -- %s", tt.why)
			}
		})
	}
}

// TestOnDemandLookupStillCachesAUsableSubnet is the control. Without it the
// assertions above pass for an implementation that refuses everything, and the
// on-demand path exists for a real case: an interface attached before this
// daemon started, in a subnet that is not in its configured list.
func TestOnDemandLookupStillCachesAUsableSubnet(t *testing.T) {
	m, _ := newManagerUnderTest(t, types.IPStackDual, dualSubnet())

	sb, err := m.GetUpdatedPodSubnet(dualSubnetID)
	if err != nil {
		t.Fatalf("GetUpdatedPodSubnet: %v", err)
	}
	if sb == nil {
		t.Fatal("GetUpdatedPodSubnet returned no subnet and no error")
	}
	if cachedSubnet(m, dualSubnetID) == nil {
		t.Error("the subnet was not cached, so every later lookup pays for the describe again")
	}
	if !sb.Available(types.IPStackDual) {
		t.Error("the subnet is not available to a dual node, so nothing could ever be placed in it")
	}
}
