// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"net"
	"sync"
	"testing"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"

	helper "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/factory"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// countingLimit counts the two calls the daemon makes when the platform
// refuses: Update, which refreshes the cached instance quota, and
// NotifyWatcher. They are the observable effect the tests below check.
type countingLimit struct {
	mu      sync.Mutex
	lim     helper.InstanceLimits
	updates int
	notify  int
}

func (l *countingLimit) GetLimit() helper.InstanceLimits { return l.lim }
func (l *countingLimit) Update() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.updates++
}
func (l *countingLimit) WatchUpdate(string, chan<- struct{}) {}
func (l *countingLimit) CordonCreate(string)                 {}
func (l *countingLimit) UnCordonCreate(string)               {}
func (l *countingLimit) NotifyWatcher() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.notify++
}

func (l *countingLimit) counts() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.updates, l.notify
}

// TestCreateWithIPCountRefreshesInstanceLimitOnQuotaRefusal pins that
// CreateWithIPCount refreshes the cached vNIC quota when the platform says the
// instance may not have another interface. It decides that by error code --
// ErrChainEqual, which is errors.As underneath -- so the check holds only as
// long as AllocENI hands back an error the platform's own error is still
// reachable through.
//
// With the chain flattened the refresh never happens and the cached quota goes
// on reporting room the instance does not have: createEniAsync creates another
// interface, the attach is refused, the interface is deleted, and the next pod
// repeats it until eniFactory.monitor's own instanceLimitPeriod tick refreshes
// the quota anyway. Each round logs a create failure and leaves the cached
// quota untouched.
//
// All four codes are covered because the create call and the attach call answer
// with different ones, and an interface can be created and only then refused at
// attach.
func TestCreateWithIPCountRefreshesInstanceLimitOnQuotaRefusal(t *testing.T) {
	for _, code := range []string{
		apiErr.LimitExceededEnisPerInstance,
		apiErr.QuotaExceededEni,
		apiErr.InvalidInstanceNicLimit,
		apiErr.OPERATION_DENIED_INSTANCE_ATTACH_NIC_EXCEED_LIMIT,
	} {
		t.Run(code, func(t *testing.T) {
			// Wrapped the way AllocENI wraps it: the daemon never sees the
			// platform error bare, only under two layers of context.
			platform := zecCom.NewZenlayerCloudSdkError(code, "refused", "req-1")
			api := &initAPI{
				allocENI: func(string, string, int) (*zecTypes.ENI, error) {
					return nil, fmt.Errorf("AllocENI: failed to create ENI: %w",
						fmt.Errorf("failed to create network interface: %w", platform))
				},
			}
			limit := &countingLimit{}
			f := &eniFactory{
				ipStack:    types.IPStackIPv4,
				subnets:    initSubnets{},
				secManager: initSec{},
				zecApi:     api,
				limit:      limit,
			}

			if _, err := f.CreateWithIPCount(2); err == nil {
				t.Fatal("CreateWithIPCount succeeded although the platform refused")
			}
			if updates, _ := limit.counts(); updates != 1 {
				t.Errorf("the cached instance quota was refreshed %d times for %s, want 1: without it "+
					"createEniAsync keeps believing the instance has room and loops "+
					"create -> attach fail -> delete", updates, code)
			}
		})
	}
}

// TestCreateWithIPCountLeavesTheLimitAloneOnOtherErrors is the negative
// control: refreshing on failures that say nothing about the quota puts a
// platform round trip on a path a pod is waiting on.
func TestCreateWithIPCountLeavesTheLimitAloneOnOtherErrors(t *testing.T) {
	api := &initAPI{
		allocENI: func(string, string, int) (*zecTypes.ENI, error) {
			return nil, fmt.Errorf("AllocENI: failed to create ENI: %w",
				zecCom.NewZenlayerCloudSdkError("SOME_TRANSIENT_THING", "try again", "req-2"))
		},
	}
	limit := &countingLimit{}
	f := &eniFactory{
		ipStack:    types.IPStackIPv4,
		subnets:    initSubnets{},
		secManager: initSec{},
		zecApi:     api,
		limit:      limit,
	}

	if _, err := f.CreateWithIPCount(2); err == nil {
		t.Fatal("CreateWithIPCount succeeded although the platform refused")
	}
	if updates, _ := limit.counts(); updates != 0 {
		t.Errorf("the cached instance quota was refreshed %d times for a transient error, want 0", updates)
	}
}

// TestInitENIKeepsThePlatformErrorCodeReachable pins the second consumer of the
// same chain, one hop further along.
//
// When AllocENI fails, initENI answers the orders it owes with an error of its
// own, and receiveRes reads the platform's code off that error to decide
// whether to refresh the instance limit, wake GC, or notify the limit watchers.
// initENI is therefore one more wrap the code has to survive; a %v there
// disables all three, and the only symptom is the daemon quietly not reacting.
func TestInitENIKeepsThePlatformErrorCodeReachable(t *testing.T) {
	const promised = 2

	for _, tc := range []struct {
		name string
		code string
		// api builds the stub that fails with the given platform error.
		api func(platform error) *initAPI
	}{
		{
			// Reaches receiveRes.watcherCodes, which notifies the limit
			// watchers. The platform answers this code on the create call, so
			// initENI's epilogue is the wrap it has to survive.
			name: "create refused",
			code: apiErr.OPERATION_DENIED_INTERNET_CHARGE_TYPE_NOT_SUPPORT,
			api: func(platform error) *initAPI {
				return &initAPI{
					allocENI: func(string, string, int) (*zecTypes.ENI, error) {
						return nil, fmt.Errorf("AllocENI: failed to create ENI: %w",
							fmt.Errorf("failed to create network interface: %w", platform))
					},
				}
			},
		},
		{
			// Reaches receiveRes.limitCodes, which refreshes the instance limit
			// and wakes GC. The platform answers a subnet that cannot carry
			// IPv6 with INVALID_SUBNET_IPSTACK_NOT_SUPPORT; here it comes back
			// from the IPv6 block assign inside initENI.
			name: "ipv6 block refused",
			code: apiErr.INVALID_SUBNET_IPSTACK_NOT_SUPPORT,
			api: func(platform error) *initAPI {
				return &initAPI{
					allocENI: func(string, string, int) (*zecTypes.ENI, error) {
						// No prefix, so initENI asks for one -- the repair path
						// a node upgrading from IPv4-only takes.
						return createdENI(t, "eni-created", nil), nil
					},
					allocatable: func(*zecTypes.ENI) ([]net.IP, *net.IPNet, error) {
						return []net.IP{net.ParseIP("10.0.0.7"), net.ParseIP("10.0.0.8")}, nil, nil
					},
					v6Block: func(string, string) (*net.IPNet, net.IP, error) {
						return nil, nil, fmt.Errorf("failed to assign ipv6 address on ENI %s, err: %w",
							"eni-created", platform)
					},
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			platform := zecCom.NewZenlayerCloudSdkError(tc.code, "refused", "req-3")
			f := newInitFactory(tc.api(platform))
			eni := &ENI{pending: promised}
			f.enis = append(f.enis, eni)
			f.eniPending <- struct{}{}

			f.initENI(eni)

			if got := len(f.eniIpReceiver); got != promised {
				t.Fatalf("emitted %d results for %d promised orders", got, promised)
			}
			for i := 0; i < promised; i++ {
				res := <-f.eniIpReceiver
				if res.err == nil {
					t.Fatalf("result %d carried no error although the interface could not be created", i)
				}
				if !(&apiErr.OpenApiErrCodeChain{}).WithErrCodes(tc.code).ErrChainEqual(res.err) {
					t.Errorf("result %d: ErrChainEqual could not find %q in %q; receiveRes decides by code "+
						"whether to refresh the instance limit, wake GC and notify the limit watchers, so all "+
						"three become branches that can never be taken", i, tc.code, res.err)
				}
			}
		})
	}
}
