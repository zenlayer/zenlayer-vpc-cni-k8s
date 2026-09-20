// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"errors"
	"testing"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"
)

// flakyDescribeEC2 answers describe with an error until healthy is set, then
// with one primary interface. Every other call is inherited from countingEC2,
// which stubs the whole client.
type flakyDescribeEC2 struct {
	countingEC2
	healthy  bool
	nicID    string
	describe int
}

func (c *flakyDescribeEC2) DescribeNetworkInterfaceAttributes(
	*zecApi.DescribeNetworkInterfacesRequest,
) (*zecApi.DescribeNetworkInterfacesResponse, error) {
	c.describe++
	if !c.healthy {
		return nil, errors.New("SERVICE_TEMPORARY_UNAVAILABLE")
	}
	return &zecApi.DescribeNetworkInterfacesResponse{
		Response: &zecApi.DescribeNetworkInterfacesResponseParams{
			TotalCount: zecCom.Integer(1),
			DataSet: []*zecApi.NicInfo{
				{NicId: zecCom.String(c.nicID), NicType: zecCom.String(ENITypePrimary)},
			},
		},
	}, nil
}

// TestPrimaryENIIDRetriesAfterFailure pins that a failed lookup is not cached.
//
// The id cannot change for the life of the instance, so a successful answer is
// worth keeping. A failure is a different thing: the platform returning
// SERVICE_TEMPORARY_UNAVAILABLE for a few seconds is ordinary, and it lands here
// whenever it coincides with daemon start-up. Caching that answer disables the
// primary-interface check for the rest of the process -- and that check is what
// stops the node tearing down its own networking, so it must come back.
//
// The three assertions have to run in this order and all three are needed: the
// first alone passes for a client that always fails, the second alone for one
// that never fails, and without the third a version that simply never caches
// also passes.
func TestPrimaryENIIDRetriesAfterFailure(t *testing.T) {
	client := &flakyDescribeEC2{nicID: "eni-primary-1"}
	api := &ZecAPIImpl{
		ec2Client:              client,
		InstanceMetadataGetter: &InstanceMetadata{InstanceId: "i-test", RegionId: "reg-test"},
	}

	// 1. The platform is down: the lookup fails and reports why.
	if id, err := api.PrimaryENIID(); err == nil {
		t.Fatalf("PrimaryENIID() = %q, nil; want an error while the platform is unavailable", id)
	}
	if client.describe == 0 {
		t.Fatal("the platform was never asked, so the case below would prove nothing")
	}

	// 2. The platform recovers. This is the regression: an earlier version
	//    resolved this once under sync.Once, which stored the failure too and
	//    kept returning it for the life of the process.
	client.healthy = true
	id, err := api.PrimaryENIID()
	if err != nil {
		t.Fatalf("PrimaryENIID() after the platform recovered = %v; want the id, so a transient "+
			"failure at start-up does not disable the primary-interface check for the whole process", err)
	}
	if id != "eni-primary-1" {
		t.Fatalf("PrimaryENIID() = %q, want %q", id, "eni-primary-1")
	}

	// 3. The success is cached: the id cannot change for the life of the
	//    instance, so asking the platform again on every call would be waste.
	before := client.describe
	again, err := api.PrimaryENIID()
	if err != nil {
		t.Fatalf("second PrimaryENIID() = %v, want the cached id", err)
	}
	if again != id {
		t.Fatalf("second PrimaryENIID() = %q, want the cached %q", again, id)
	}
	if client.describe != before {
		t.Errorf("the platform was asked %d more times after a successful lookup; want the answer cached",
			client.describe-before)
	}
}

// TestPrimaryENIIDReportsAnInstanceWithNoPrimary pins the other failure: a
// describe that succeeds but names no primary interface is an error, not an
// empty id, and it is not cached either.
func TestPrimaryENIIDReportsAnInstanceWithNoPrimary(t *testing.T) {
	client := &emptyDescribeEC2{}
	api := &ZecAPIImpl{
		ec2Client:              client,
		InstanceMetadataGetter: &InstanceMetadata{InstanceId: "i-test", RegionId: "reg-test"},
	}

	if id, err := api.PrimaryENIID(); err == nil {
		t.Fatalf("PrimaryENIID() = %q, nil; want an error when the instance reports no primary", id)
	}
	before := client.describe
	if _, err := api.PrimaryENIID(); err == nil {
		t.Fatal("PrimaryENIID() succeeded on the second call; want the error again")
	}
	if client.describe == before {
		t.Error("the second call did not ask the platform; a failure must not be cached")
	}
}

// emptyDescribeEC2 answers describe with no interfaces at all.
type emptyDescribeEC2 struct {
	countingEC2
	describe int
}

func (c *emptyDescribeEC2) DescribeNetworkInterfaceAttributes(
	*zecApi.DescribeNetworkInterfacesRequest,
) (*zecApi.DescribeNetworkInterfacesResponse, error) {
	c.describe++
	return &zecApi.DescribeNetworkInterfacesResponse{
		Response: &zecApi.DescribeNetworkInterfacesResponseParams{TotalCount: zecCom.Integer(0)},
	}, nil
}
