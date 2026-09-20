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

// quotaEC2 fails one chosen call inside AllocENI -- the create, the attach, or
// either of the two address assigns -- and counts how often create, attach and
// delete were reached. What it does not override falls through to countingEC2,
// which answers nil for both response and error.
type quotaEC2 struct {
	*countingEC2

	createErr   error
	createCalls int
	attachErr   error
	attachCalls int
	deleteCalls int
	// The two address assigns createENI makes on a freshly created interface,
	// before it is attached.
	assignV4Err error
	assignV6Err error
}

func (q *quotaEC2) CreateNetworkInterface(_, _, _ string, _ map[string]string) (*zecApi.CreateNetworkInterfaceResponse, error) {
	q.createCalls++
	if q.createErr != nil {
		return nil, q.createErr
	}
	id := "nic-created"
	return &zecApi.CreateNetworkInterfaceResponse{
		Response: &zecApi.CreateNetworkInterfaceResponseParams{NicId: &id},
	}, nil
}

func (q *quotaEC2) AttachNetworkInterface(string, string) (*zecApi.AttachNetworkInterfaceResponse, error) {
	q.attachCalls++
	return nil, q.attachErr
}

func (q *quotaEC2) AssignPrivateIpAddress(string, []string, int) (*zecApi.BatchAssignNetworkInterfaceIpv4Response, error) {
	return nil, q.assignV4Err
}

func (q *quotaEC2) AssignNetworkInterfaceIpv6(string, ec2.IPv6Billing) (*zecApi.AssignNetworkInterfaceIpv6Response, error) {
	return nil, q.assignV6Err
}

func (q *quotaEC2) DeleteNetworkInterface(string) (*zecApi.DeleteNetworkInterfaceResponse, error) {
	q.deleteCalls++
	return nil, nil
}

// v6BlockEC2 refuses the IPv6 prefix assign. The describe inherited from
// countingEC2 answers no interface, so GetOrAssignIPv6Block finds no existing
// prefix and goes on to the assign.
type v6BlockEC2 struct {
	*countingEC2

	assignV6Err   error
	assignV6Calls int
}

func (v *v6BlockEC2) AssignNetworkInterfaceIpv6(string, ec2.IPv6Billing) (*zecApi.AssignNetworkInterfaceIpv6Response, error) {
	v.assignV6Calls++
	return nil, v.assignV6Err
}

// TestGetOrAssignIPv6BlockReturnsAnUnwrappableError pins the error chain of the
// prefix assign GetOrAssignIPv6Block makes; the prefix assign inside createENI
// is covered by TestAllocENIKeepsTheCodeFromTheAddressAssignsDuringCreate.
//
// receiveRes refreshes the instance limit and wakes GC on
// INVALID_SUBNET_IPSTACK_NOT_SUPPORT, and it asks by error code. Three %w wraps
// stand between here and there -- this call, initENI's IPv6 repair, and
// initENI's epilogue -- and flattening any one of them turns both reactions
// into branches that can never be taken, with silence as the only symptom.
func TestGetOrAssignIPv6BlockReturnsAnUnwrappableError(t *testing.T) {
	code := apiErr.INVALID_SUBNET_IPSTACK_NOT_SUPPORT
	fake := &v6BlockEC2{
		countingEC2: &countingEC2{},
		assignV6Err: zecCom.NewZenlayerCloudSdkError(code, "refused", "req-4"),
	}
	e := &ZecAPIImpl{
		IPStack:                daemonTypes.IPStackDual,
		ec2Client:              fake,
		InstanceMetadataGetter: &InstanceMetadata{InstanceId: "i-test", RegionId: "reg-test"},
	}

	_, _, err := e.GetOrAssignIPv6Block("eni-1", "aa:bb:cc:dd:ee:01")
	if err == nil {
		t.Fatal("GetOrAssignIPv6Block succeeded although the platform refused")
	}
	if fake.assignV6Calls != 1 {
		t.Errorf("AssignNetworkInterfaceIpv6 was called %d times, want exactly 1: a subnet does not "+
			"gain IPv6 support while the backoff runs", fake.assignV6Calls)
	}
	if !(&apiErr.OpenApiErrCodeChain{}).WithErrCodes(code).ErrChainEqual(err) {
		t.Errorf("ErrChainEqual could not find %q in %q; receiveRes refreshes the instance limit and "+
			"wakes GC on this code, and it asks by code", code, err)
	}
}

func newQuotaAPI(fake *quotaEC2) *ZecAPIImpl {
	return &ZecAPIImpl{
		IPStack:                daemonTypes.IPStackIPv4,
		ec2Client:              fake,
		InstanceMetadataGetter: &InstanceMetadata{InstanceId: "i-test", RegionId: "reg-test"},
	}
}

// TestAllocENIKeepsTheCodeFromTheAddressAssignsDuringCreate covers the two
// calls createENI makes on the interface it has just created, before attaching
// it: the secondary IPv4 assign and the IPv6 prefix assign.
//
// Neither code is in the quota list CreateWithIPCount matches on, but both are
// in the list receiveRes matches on --
// OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT and
// INVALID_SUBNET_IPSTACK_NOT_SUPPORT -- to refresh the instance limit and wake
// GC. The error travels createENI -> AllocENI -> CreateWithIPCount -> initENI's
// epilogue to get there, and every wrap on that route uses %w; rendering it
// into a string at any one of them defeats the match.
func TestAllocENIKeepsTheCodeFromTheAddressAssignsDuringCreate(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
		fake func(err error) *quotaEC2
	}{
		{
			name: "secondary ipv4 assign refused",
			code: apiErr.OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT,
			fake: func(err error) *quotaEC2 {
				return &quotaEC2{countingEC2: &countingEC2{}, assignV4Err: err}
			},
		},
		{
			name: "ipv6 prefix assign refused",
			code: apiErr.INVALID_SUBNET_IPSTACK_NOT_SUPPORT,
			fake: func(err error) *quotaEC2 {
				return &quotaEC2{countingEC2: &countingEC2{}, assignV6Err: err}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.fake(zecCom.NewZenlayerCloudSdkError(tc.code, "refused", "req-5"))
			e := newQuotaAPI(fake)
			// Dual with more than one address, so createENI makes both calls.
			e.IPStack = daemonTypes.IPStackDual

			_, err := e.AllocENI("subnet-1", "sg-1", 2)
			if err == nil {
				t.Fatalf("AllocENI succeeded although the platform refused with %s", tc.code)
			}
			if !(&apiErr.OpenApiErrCodeChain{}).WithErrCodes(tc.code).ErrChainEqual(err) {
				t.Errorf("ErrChainEqual could not find %q in %q; receiveRes refreshes the instance "+
					"limit and wakes GC on this code, and it asks by code", tc.code, err)
			}
		})
	}
}

// TestAllocENIReturnsAnUnwrappableError is the ENI-quota twin of
// TestAllocIPv4AddressesReturnsAnUnwrappableError.
//
// eniFactory.CreateWithIPCount decides whether the cached instance quota is
// stale by asking for the platform's error code -- ErrChainEqual, errors.As
// underneath. Two wraps stand between the refusal and that check on either
// route: createENI then AllocENI, or attachENI then AllocENI. A %s or %v at
// either one leaves the code readable to a human and unreachable to errors.As,
// so the refresh silently never happens. Hence the assertion on the error
// AllocENI returns, which is the outer one.
//
// What that costs: the cached ENIQuota keeps saying there is room, so
// createEniAsync creates another interface, the attach is refused again, the
// interface is deleted, and the next pod repeats it. The loop only ends when
// the limit manager's own 2-minute tick happens to refresh the quota -- the
// refresh this call site exists to trigger immediately.
func TestAllocENIReturnsAnUnwrappableError(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
		fake func(err error) *quotaEC2
	}{
		{
			// The create call's spelling, per the code's own documentation in
			// pkg/zapi/errors: an interface can be created and only then
			// refused at attach, so both halves have to be recognizable.
			name: "create refused",
			code: apiErr.LimitExceededEnisPerInstance,
			fake: func(err error) *quotaEC2 {
				return &quotaEC2{countingEC2: &countingEC2{}, createErr: err}
			},
		},
		{
			name: "attach refused",
			code: apiErr.OPERATION_DENIED_INSTANCE_ATTACH_NIC_EXCEED_LIMIT,
			fake: func(err error) *quotaEC2 {
				return &quotaEC2{countingEC2: &countingEC2{}, attachErr: err}
			},
		},
		{
			name: "attach refused, other spelling",
			code: apiErr.InvalidInstanceNicLimit,
			fake: func(err error) *quotaEC2 {
				return &quotaEC2{countingEC2: &countingEC2{}, attachErr: err}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.fake(zecCom.NewZenlayerCloudSdkError(tc.code, "refused", "req-1"))
			e := newQuotaAPI(fake)

			_, err := e.AllocENI("subnet-1", "sg-1", 2)
			if err == nil {
				t.Fatalf("AllocENI succeeded although the platform refused with %s", tc.code)
			}

			var sdkErr *zecCom.ZenlayerCloudSdkError
			if !errors.As(err, &sdkErr) {
				t.Fatalf("errors.As could not reach the platform error in %q; CreateWithIPCount decides "+
					"whether to refresh the instance quota by error code, so it silently matches nothing "+
					"and the daemon keeps creating interfaces the instance cannot attach", err)
			}
			if sdkErr.GetCode() != tc.code {
				t.Errorf("unwrapped code = %q, want %q", sdkErr.GetCode(), tc.code)
			}
			if !(&apiErr.OpenApiErrCodeChain{}).WithErrCodes(tc.code).ErrChainEqual(err) {
				t.Errorf("ErrChainEqual did not match %q on the returned error", tc.code)
			}
		})
	}
}

// TestCreateAndAttachENIStopOnQuotaRefusals pins that a quota refusal ends the
// backoff after one attempt.
//
// Same argument as the assign backoff: nothing gives the instance another vNIC
// slot while the backoff is running, so every extra attempt is spent to reach
// the same refusal. The measured budget is tens of seconds, and AllocENI runs
// on the pool worker or on a pod's own AllocIP goroutine -- both holding
// blockPause, which also stops GC and pool shrink.
//
// The two spellings are split across the two calls on purpose: the platform
// answers the create with LimitExceeded.EnisPerInstance and the attach with
// OPERATION_DENIED_INSTANCE_ATTACH_NIC_EXCEED_LIMIT, and a list that names only
// one of them leaves the other burning the full budget.
func TestCreateAndAttachENIStopOnQuotaRefusals(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		for _, code := range []string{apiErr.LimitExceededEnisPerInstance, apiErr.QuotaExceededEni} {
			t.Run(code, func(t *testing.T) {
				fake := &quotaEC2{
					countingEC2: &countingEC2{},
					createErr:   zecCom.NewZenlayerCloudSdkError(code, "refused", "req-2"),
				}
				e := newQuotaAPI(fake)

				start := time.Now()
				if _, err := e.AllocENI("subnet-1", "sg-1", 2); err == nil {
					t.Fatal("AllocENI succeeded although the create was refused")
				}
				if fake.createCalls != 1 {
					t.Errorf("CreateNetworkInterface was called %d times for %s, want exactly 1: "+
						"the instance does not gain a vNIC slot while the backoff runs, and the whole "+
						"call is made holding blockPause, which also stops GC", fake.createCalls, code)
				}
				if elapsed := time.Since(start); elapsed > 5*time.Second {
					t.Errorf("AllocENI took %v for %s; a quota refusal must not back off", elapsed, code)
				}
			})
		}
	})

	t.Run("attach", func(t *testing.T) {
		for _, code := range []string{
			apiErr.OPERATION_DENIED_INSTANCE_ATTACH_NIC_EXCEED_LIMIT,
			apiErr.InvalidInstanceNicLimit,
		} {
			t.Run(code, func(t *testing.T) {
				fake := &quotaEC2{
					countingEC2: &countingEC2{},
					attachErr:   zecCom.NewZenlayerCloudSdkError(code, "refused", "req-3"),
				}
				e := newQuotaAPI(fake)

				start := time.Now()
				if _, err := e.AllocENI("subnet-1", "sg-1", 2); err == nil {
					t.Fatal("AllocENI succeeded although the attach was refused")
				}
				if fake.attachCalls != 1 {
					t.Errorf("AttachNetworkInterface was called %d times for %s, want exactly 1",
						fake.attachCalls, code)
				}
				// The interface exists on the platform by then. Leaving it
				// behind on every refusal is how a node runs out of quota it
				// still believes it has.
				if fake.deleteCalls == 0 {
					t.Errorf("the created interface was not deleted after the attach was refused with %s", code)
				}
				if elapsed := time.Since(start); elapsed > 5*time.Second {
					t.Errorf("AllocENI took %v for %s; a quota refusal must not back off", elapsed, code)
				}
			})
		}
	})
}
