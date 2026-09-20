// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"testing"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"

	daemonTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
)

// rollbackEC2 counts the two calls a rollback is made of and lets the test
// choose what the detach answers. Everything else falls through to countingEC2,
// which answers nil for both response and error.
type rollbackEC2 struct {
	*countingEC2

	attachErr   error
	attachCalls int
	detachErr   error
	detachCalls int
	deleteCalls int
}

func (r *rollbackEC2) CreateNetworkInterface(_, _, _ string, _ map[string]string) (*zecApi.CreateNetworkInterfaceResponse, error) {
	id := "nic-created"
	return &zecApi.CreateNetworkInterfaceResponse{
		Response: &zecApi.CreateNetworkInterfaceResponseParams{NicId: &id},
	}, nil
}

func (r *rollbackEC2) AttachNetworkInterface(string, string) (*zecApi.AttachNetworkInterfaceResponse, error) {
	r.attachCalls++
	return nil, r.attachErr
}

func (r *rollbackEC2) DetachNetworkInterface(string) (*zecApi.DetachNetworkInterfaceResponse, error) {
	r.detachCalls++
	return nil, r.detachErr
}

func (r *rollbackEC2) DeleteNetworkInterface(string) (*zecApi.DeleteNetworkInterfaceResponse, error) {
	r.deleteCalls++
	return nil, nil
}

func newRollbackAPI(fake *rollbackEC2) *ZecAPIImpl {
	return &ZecAPIImpl{
		IPStack:                daemonTypes.IPStackIPv4,
		ec2Client:              fake,
		InstanceMetadataGetter: &InstanceMetadata{InstanceId: "i-test", RegionId: "reg-test"},
	}
}

// TestAllocENIRollsBackAnAttachFailureWithADetach pins that the rollback does
// not assume the attach left the interface off the instance.
//
// INVALID_NIC_ATTACHED is the platform saying the opposite: an earlier attempt
// landed and its response was lost, so the interface this call created is on the
// instance. The status poll inside attachENI reports failure the same way when
// the write landed but the interface had not reached InUse within the budget. A
// delete cannot remove an attached interface -- and deleteENI treats
// INVALID_NIC_ATTACHED as final rather than retrying -- so rolling back with a
// delete alone stranded the interface on the instance, spending a vNIC slot the
// node still believed it had.
func TestAllocENIRollsBackAnAttachFailureWithADetach(t *testing.T) {
	fake := &rollbackEC2{
		countingEC2: &countingEC2{},
		attachErr:   zecCom.NewZenlayerCloudSdkError(apiErr.InvalidNicAttached, "already attached", "req-1"),
	}
	e := newRollbackAPI(fake)

	if _, err := e.AllocENI("subnet-1", "sg-1", 2); err == nil {
		t.Fatal("AllocENI succeeded although the attach failed")
	}

	if fake.attachCalls != 1 {
		t.Errorf("AttachNetworkInterface was called %d times, want exactly 1: an interface does not "+
			"become unattached while the backoff runs", fake.attachCalls)
	}
	if fake.detachCalls == 0 {
		t.Error("the rollback never detached the interface; a delete alone cannot remove one that is attached")
	}
	if fake.deleteCalls == 0 {
		t.Error("the rollback never deleted the interface it had created")
	}
}

// TestFreeENIDeletesAnInterfaceThatIsNoLongerAttached is the other half of the
// same rollback: when the attach really did not take effect, the interface
// exists but sits on no instance, and the detach says so with
// OPERATION_DENIED_NIC_NOT_EXIST_INSTANCE. That is the state the detach was
// trying to reach, so it counts as success and the delete has to follow --
// otherwise freeing an already-detached interface returns before the delete and
// leaks it, which is exactly the case the attach rollback cannot distinguish
// from the attached one.
func TestFreeENIDeletesAnInterfaceThatIsNoLongerAttached(t *testing.T) {
	for _, code := range []string{apiErr.NicNotExistInstance, apiErr.ResourceNotFound} {
		t.Run(code, func(t *testing.T) {
			fake := &rollbackEC2{
				countingEC2: &countingEC2{},
				detachErr:   zecCom.NewZenlayerCloudSdkError(code, "not on any instance", "req-2"),
			}
			e := newRollbackAPI(fake)

			// 0 rather than FreeENI's delay: the wait is there to let a real
			// detach settle before the delete, and there is nothing to settle
			// here.
			if err := e.freeENI("nic-created", 0); err != nil {
				t.Fatalf("freeENI() = %v, want nil: %s means the interface is already off the instance", err, code)
			}
			if fake.deleteCalls == 0 {
				t.Errorf("freeENI returned without deleting the interface after the detach reported %s", code)
			}
		})
	}
}
