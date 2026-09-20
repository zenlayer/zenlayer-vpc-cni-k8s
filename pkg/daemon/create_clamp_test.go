// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"net"
	"sync"
	"testing"

	helper "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/factory"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// clampFactory wires a factory with no interfaces at all: submitOrder has
// nothing to queue an order against and fails, so the whole request survives as
// lackIP and reaches createEniAsync, where the count handed to AllocENI shows
// what Create decided to order.
func clampFactory(api *initAPI, perENI, eniQuota int) *eniIPFactory {
	return &eniIPFactory{
		eniFactory: &eniFactory{
			ipStack:    types.IPStackDual,
			subnets:    initSubnets{},
			secManager: initSec{},
			zecApi:     api,
			limit: initLimit{lim: helper.InstanceLimits{
				InstanceLimitsAttr: helper.InstanceLimitsAttr{
					ENIQuota:      eniQuota,
					IPv4MaxPerENI: perENI,
				},
			}},
		},
		zecAPI:        api,
		ipStack:       types.IPStackDual,
		eniIpReceiver: make(chan *ENIIPRes, 64),
		eniPending:    make(chan struct{}, 1),
	}
}

// TestCreateClampsLackIPToPerENILimit pins the two clamps Create applies to the
// shortfall before ordering a new interface:
//
//	lackIP = min(lackIP, max(0, IPv4MaxPerENI-1))   // what one interface can carry
//	lackIP = min(lackIP, defaultOrderCnt)           // 10
//
// From 4 vCPU up an interface carries 15 or 30 addresses, so defaultOrderCnt is
// the clamp that decides -- 8C is min(lackIP, 14) then min(lackIP, 10). The
// first clamp only decides anything on the 6-address tiers, which is why it
// needs a test of its own.
//
// What is observed is the count handed to AllocENI, which is `lackIP + 1`: the
// platform makes the first address the interface's primary and the primary is
// withheld from pods, so N pod addresses need N+1 on the interface.
//
// Ordering more than one interface can carry is not a shortfall: the platform
// refuses the address assign inside createENI, so the interface is deleted
// again and every order queued against it is answered with an error.
func TestCreateClampsLackIPToPerENILimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		perENI    int
		request   int
		wantIPCnt int
		why       string
	}{
		{
			// The first clamp: an interface that can hold 4 addresses carries 3
			// pod addresses, so a request for 20 must be cut to 3 -- not to
			// defaultOrderCnt's 10.
			name: "the per-interface limit bites first", perENI: 4, request: 20, wantIPCnt: 4,
			why: "perENI-1 = 3 pod addresses, +1 for the interface's own primary",
		},
		{
			// The clamp that decides on every tier from 4 vCPU up.
			name: "defaultOrderCnt bites first", perENI: 30, request: 20, wantIPCnt: 11,
			why: "defaultOrderCnt = 10, +1 for the primary",
		},
		{
			// Neither clamp applies: a small request must not be inflated to
			// either bound.
			name: "a small request is ordered as-is", perENI: 30, request: 3, wantIPCnt: 4,
			why: "3 pod addresses, +1 for the primary",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu     sync.Mutex
				ipCnts []int
			)
			api := &initAPI{
				allocENI: func(_, _ string, ipCnt int) (*zecTypes.ENI, error) {
					mu.Lock()
					ipCnts = append(ipCnts, ipCnt)
					mu.Unlock()
					return createdENI(t, "eni-clamp", mustCIDR(t, "2001:db8::/96")), nil
				},
				v6Block: func(string, string) (*net.IPNet, net.IP, error) {
					return mustCIDR(t, "2001:db8::/96"), net.ParseIP("2001:db8::2"), nil
				},
			}
			// One allocatable address per pod address ordered, so initENI pairs
			// them all and answers every order with an address instead of an
			// error.
			api.allocatable = func(*zecTypes.ENI) ([]net.IP, *net.IPNet, error) {
				mu.Lock()
				n := 0
				if len(ipCnts) > 0 {
					n = ipCnts[len(ipCnts)-1] - 1
				}
				mu.Unlock()
				ips := make([]net.IP, 0, n)
				for i := 0; i < n; i++ {
					ips = append(ips, net.ParseIP(fmt.Sprintf("10.0.0.%d", i+10)))
				}
				return ips, mustCIDR(t, "2001:db8::/96"), nil
			}

			f := clampFactory(api, tc.perENI, 4)
			if _, err := f.Create(tc.request); err != nil {
				t.Fatalf("Create(%d): %v", tc.request, err)
			}

			mu.Lock()
			got := append([]int(nil), ipCnts...)
			mu.Unlock()
			if len(got) != 1 {
				t.Fatalf("AllocENI called %d times (%v), want exactly 1", len(got), got)
			}
			if got[0] != tc.wantIPCnt {
				t.Errorf("Create(%d) with IPv4MaxPerENI=%d ordered an interface with %d addresses, "+
					"want %d (%s). Ordering more than an interface can carry deadlocks Create in "+
					"receiveRes on results that never arrive",
					tc.request, tc.perENI, got[0], tc.wantIPCnt, tc.why)
			}
		})
	}
}

// TestCreateOrdersNothingWhenAnInterfaceCanOnlyHoldItsOwnPrimary covers the
// bottom of the first clamp. IPv4MaxPerENI == 1 leaves an interface room for
// its own primary and nothing else, so perENI-1 is zero and there is no point
// creating one. The max(0, …) guard is belt and braces: the `if lackIP > 0`
// below it already refuses a negative. What this pins is that a useless
// interface is never created -- it would take one of the instance's few vNIC
// slots and serve no pod.
func TestCreateOrdersNothingWhenAnInterfaceCanOnlyHoldItsOwnPrimary(t *testing.T) {
	for _, perENI := range []int{1, 0} {
		called := false
		api := &initAPI{
			allocENI: func(string, string, int) (*zecTypes.ENI, error) {
				called = true
				return createdENI(t, "eni-should-not-exist", nil), nil
			},
			allocatable: func(*zecTypes.ENI) ([]net.IP, *net.IPNet, error) { return nil, nil, nil },
			v6Block: func(string, string) (*net.IPNet, net.IP, error) {
				return mustCIDR(t, "2001:db8::/96"), net.ParseIP("2001:db8::2"), nil
			},
		}
		f := clampFactory(api, perENI, 4)

		if _, err := f.Create(5); err == nil {
			t.Errorf("IPv4MaxPerENI=%d: Create succeeded, want an error -- no interface can serve a pod", perENI)
		}
		if called {
			t.Errorf("IPv4MaxPerENI=%d: an interface was created that can hold nothing but its own "+
				"primary; it would consume a quota slot and serve no pod", perENI)
		}
	}
}
