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
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/metadata"
)

// assignEC2 counts the assign calls and answers each with a chosen error.
type assignEC2 struct {
	*countingEC2
	assignErr error
	// failFirst is how many calls answer with assignErr before one succeeds;
	// -1 means every call fails.
	failFirst    int
	assigned     []string
	assignCalls  int
	unassignCall int
}

func (a *assignEC2) AssignPrivateIpAddress(string, []string, int) (*zecApi.BatchAssignNetworkInterfaceIpv4Response, error) {
	a.assignCalls++
	if a.failFirst < 0 || a.assignCalls <= a.failFirst {
		return nil, a.assignErr
	}
	return &zecApi.BatchAssignNetworkInterfaceIpv4Response{
		Response: &zecApi.BatchAssignNetworkInterfaceIpv4ResponseParams{IpAddresses: a.assigned},
	}, nil
}

func (a *assignEC2) UnAssignPrivateIpAddress(string, []string) (*zecApi.UnassignNetworkInterfaceIpv4Response, error) {
	a.unassignCall++
	return nil, nil
}

func assignAPI(fake *assignEC2, meta pathMeta) *ZecAPIImpl {
	return &ZecAPIImpl{
		IPStack:                daemonTypes.IPStackIPv4,
		ec2Client:              fake,
		metadataSvc:            metadata.NewEC2MetadataWrapper(meta),
		InstanceMetadataGetter: &InstanceMetadata{InstanceId: "i-test", RegionId: "reg-test"},
	}
}

// TestAssignIsNotRetriedWhenThePlatformNeverAnswered is the regression this file
// exists for.
//
// The assign asks for "v4Cnt more addresses" with no ip list, so it is not
// idempotent: a request that landed while its response was lost has already
// taken effect, and a retry asks for another v4Cnt on top. APIWriteOps allows
// six attempts, so one bad connection could assign six times what was wanted --
// and only GC would find them, much later.
//
// A transport error carries no platform error code, which is exactly the signal
// that the platform never answered.
func TestAssignIsNotRetriedWhenThePlatformNeverAnswered(t *testing.T) {
	fake := &assignEC2{countingEC2: &countingEC2{}, failFirst: -1, assignErr: errors.New("connection reset by peer")}
	e := assignAPI(fake, pathMeta{})

	start := time.Now()
	if _, err := e.AllocIPv4Addresses("eni-1", testMac, 2); err == nil {
		t.Fatal("AllocIPv4Addresses succeeded although every assign failed")
	}

	if fake.assignCalls != 1 {
		t.Errorf("AssignPrivateIpAddress was called %d times for an error with no platform code, "+
			"want exactly 1: the call is not idempotent, so a retry adds addresses on top of any "+
			"the lost request already applied", fake.assignCalls)
	}
	// And it gave up promptly rather than burning the write budget.
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("took %v to give up", elapsed)
	}
}

// TestAssignIsRetriedWhenThePlatformRejectedIt is the control. A platform error
// code means the request arrived and was refused, so nothing was applied and a
// retry is both safe and useful -- without this the assertion above would pass
// for an implementation that never retries anything.
func TestAssignIsRetriedWhenThePlatformRejectedIt(t *testing.T) {
	// Rate limiting: the platform answered, so nothing was applied, and it is
	// not in the terminal list -- exactly the case a retry is for. (The public
	// codes, InternalServerError and RequestTimedOut among them, are terminal
	// here precisely because they are ambiguous.)
	//
	// One failure then success, so this costs a single backoff step rather than
	// the whole write budget.
	assigned := []string{"10.0.0.5", "10.0.0.6"}
	fake := &assignEC2{
		countingEC2: &countingEC2{},
		failFirst:   1,
		assigned:    assigned,
		assignErr:   zecCom.NewZenlayerCloudSdkError(apiErr.FlowLimitExceeded, "slow down", "req-1"),
	}
	meta := pathMeta{vals: map[string]string{
		p("network/interfaces/macs/%s/private-ipv4s"): `["10.0.0.5","10.0.0.6"]`,
	}}
	e := assignAPI(fake, meta)

	got, err := e.AllocIPv4Addresses("eni-1", testMac, 2)
	if err != nil {
		t.Fatalf("AllocIPv4Addresses: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("returned %d addresses, want 2", len(got))
	}
	if fake.assignCalls < 2 {
		t.Errorf("AssignPrivateIpAddress was called %d times for a platform refusal, want more than "+
			"one: the platform answered, so nothing was applied and retrying is safe", fake.assignCalls)
	}
}

// TestMetadataConfirmationFitsInsideTheCNIBudget pins the wait that follows a
// successful assign.
//
// It polls the node's own metadata, not the platform, and it used to be given
// APIStatusWait -- ~246s, twice the whole daemonRPCTimeout a CNI ADD gets, on
// top of the assign's own backoff and with privateIPMutex held throughout. The
// dealloc path has always used MetaStatusWait for the same wait.
//
// The rollback is the second half: on timeout the deferred handler gives back
// addresses the platform did assign, so the longer the wait, the wider the
// window in which slow metadata costs a good address.
func TestMetadataConfirmationFitsInsideTheCNIBudget(t *testing.T) {
	fake := &assignEC2{
		countingEC2: &countingEC2{},
		failFirst:   0,
		assigned:    []string{"10.0.0.5", "10.0.0.6"},
	}
	// Metadata never catches up: the path answers 404 for the whole poll.
	e := assignAPI(fake, pathMeta{})

	start := time.Now()
	_, err := e.AllocIPv4Addresses("eni-1", testMac, 2)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("AllocIPv4Addresses succeeded although metadata never showed the addresses")
	}
	// daemonRPCTimeout is 118s and the assign's own backoff has already spent
	// from it, so this alone has to be far shorter.
	if elapsed > 60*time.Second {
		t.Errorf("the confirmation waited %v; the CNI ADD it runs inside gives up at 118s, and "+
			"privateIPMutex is held for all of it", elapsed)
	}
	if fake.unassignCall == 0 {
		t.Error("the addresses were not rolled back after the confirmation timed out")
	}
}
