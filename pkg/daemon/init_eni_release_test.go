// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"net"
	"testing"

	helper "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/factory"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// initAPI is a ZecAPI whose interesting calls are supplied per test and whose
// FreeENI records what was released. Everything else answers zero values --
// initENI does not reach them.
type initAPI struct {
	allocENI    func(subnetID, sg string, ipCnt int) (*zecTypes.ENI, error)
	allocatable func(eni *zecTypes.ENI) ([]net.IP, *net.IPNet, error)
	v6Block     func(eniID, mac string) (*net.IPNet, net.IP, error)

	freed []string
}

func (a *initAPI) AllocENI(subnetID, sg string, ipCnt int) (*zecTypes.ENI, error) {
	return a.allocENI(subnetID, sg, ipCnt)
}

func (a *initAPI) FreeENI(eniID string) error {
	a.freed = append(a.freed, eniID)
	return nil
}

func (a *initAPI) GetENIAllocatableIPs(eni *zecTypes.ENI) ([]net.IP, *net.IPNet, error) {
	return a.allocatable(eni)
}

func (a *initAPI) GetOrAssignIPv6Block(eniID, mac string) (*net.IPNet, net.IP, error) {
	return a.v6Block(eniID, mac)
}

func (a *initAPI) GetENI(string, string, string) (*zecTypes.ENI, error) { return nil, nil }
func (a *initAPI) GetAttachedENIs() ([]*zecTypes.ENI, error)            { return nil, nil }
func (a *initAPI) GetSecondaryENIMACs() ([]string, error)               { return nil, nil }
func (a *initAPI) GetENIIPList(string) ([]net.IP, *net.IPNet, error)    { return nil, nil, nil }
func (a *initAPI) AllocIPv4Addresses(string, string, int) ([]net.IP, error) {
	return nil, nil
}
func (a *initAPI) DeallocIPv4Addresses(string, string, []net.IP) error { return nil }
func (a *initAPI) GetInstanceLimit() (*helper.InstanceLimits, error)   { return nil, nil }
func (a *initAPI) GetTotalAttachedEniCnt() (int, error)                { return 0, nil }

// initSubnets always offers one subnet, so CreateWithIPCount gets past its
// first gate and the test can drive the failure it actually means to test.
type initSubnets struct{}

func (initSubnets) FlushSubnets(...string) error          { return nil }
func (initSubnets) GetPodSubnet(string) *helper.PodSubnet { return nil }
func (initSubnets) IsPodSubnet(string) bool               { return true }
func (initSubnets) GetUpdatedPodSubnet(string) (*helper.PodSubnet, error) {
	return nil, nil
}
func (initSubnets) SelectSubnet(types.IPStack, ...helper.UpdateSubnetsStatusOption) *helper.PodSubnet {
	return &helper.PodSubnet{SubnetId: "subnet-test"}
}
func (initSubnets) UpdateSubnetsStatus(...helper.UpdateSubnetsStatusOption) error { return nil }
func (initSubnets) DisableSubnet(string)                                          {}
func (initSubnets) Status() *helper.Status                                        { return nil }

type initSec struct{}

func (initSec) GetSecurityGroup() string         { return "sg-test" }
func (initSec) UpdateSecurityGroup(string) error { return nil }

// initLimit reports whatever quota the test sets. The zero value reports zero
// quota, which is what the tests calling initENI directly want -- they bypass
// createEniAsync, so the quota is never consulted.
type initLimit struct{ lim helper.InstanceLimits }

func (l initLimit) GetLimit() helper.InstanceLimits   { return l.lim }
func (initLimit) Update()                             {}
func (initLimit) WatchUpdate(string, chan<- struct{}) {}
func (initLimit) CordonCreate(string)                 {}
func (initLimit) UnCordonCreate(string)               {}
func (initLimit) NotifyWatcher()                      {}

// newInitFactory wires a dual-stack factory whose only live dependency is the
// supplied API. The receiver channel is generously buffered so initENI's
// epilogue never blocks on a test that fails to drain it -- a deadlocked test
// reports as a timeout rather than as the assertion that actually failed.
func newInitFactory(api *initAPI) *eniIPFactory {
	return &eniIPFactory{
		eniFactory: &eniFactory{
			ipStack:    types.IPStackDual,
			subnets:    initSubnets{},
			secManager: initSec{},
			zecApi:     api,
			limit:      initLimit{},
		},
		zecAPI:        api,
		ipStack:       types.IPStackDual,
		eniIpReceiver: make(chan *ENIIPRes, 64),
		eniPending:    make(chan struct{}, 1),
	}
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return n
}

// createdENI is what AllocENI hands back. The prefix belongs on the reply and
// not on the *ENI the test holds: createEniAsync builds that with a nil embedded
// pointer and initENI fills it in from the create reply, so a prefix set
// beforehand would test a state the daemon never reaches. A nil prefix models
// the reply the platform is allowed to send without one.
func createdENI(t *testing.T, id string, prefix *net.IPNet) *zecTypes.ENI {
	t.Helper()
	mac, err := net.ParseMAC("aa:bb:cc:dd:ee:02")
	if err != nil {
		t.Fatalf("parse mac: %v", err)
	}
	return &zecTypes.ENI{ID: id, Mac: mac, IPv6Net: prefix, PrimaryIP: zecTypes.IPSet{
		IPv4: net.ParseIP("10.0.0.5"),
		IPv6: net.ParseIP("2001:db8::2"),
	}}
}

// TestInitENIReleasesTheInterfaceOnEveryFailure
//
// Two invariants meet in initENI's failure epilogue:
//
//   - The interface must be released. It has been created, attached and tagged
//     on the platform by then; dropping it from f.enis without releasing leaks
//     it until some later reconcile adopts it back, and cniENICountLocked falls
//     so the next pod creates another one. On a two-interface instance the
//     first failure burns the node's whole CNI quota.
//   - Exactly as many results must be emitted as were promised. Create reads
//     precisely as many as it ordered; one short and it blocks in receiveRes
//     forever, holding blockPause, which also stops GC. The node stays wedged
//     until the daemon restarts.
//
// Each case drives one distinct failure and asserts both.
func TestInitENIReleasesTheInterfaceOnEveryFailure(t *testing.T) {
	const promised = 3

	okAlloc := func(*zecTypes.ENI) ([]net.IP, *net.IPNet, error) {
		return []net.IP{net.ParseIP("10.0.0.7"), net.ParseIP("10.0.0.8"), net.ParseIP("10.0.0.9")},
			mustCIDR(t, "2001:db8::/96"), nil
	}

	for _, tc := range []struct {
		name string
		// prefix already on the interface, nil to force GetOrAssignIPv6Block
		prefix   *net.IPNet
		api      func(a *initAPI)
		wantFree bool
		why      string
	}{
		{
			// Nothing was created, so there is nothing to free -- releasing
			// here would be a FreeENI on an id the platform never issued.
			name:   "the interface is never created",
			prefix: mustCIDR(t, "2001:db8::/96"),
			api: func(a *initAPI) {
				a.allocENI = func(string, string, int) (*zecTypes.ENI, error) {
					return nil, errors.New("quota exceeded")
				}
			},
			wantFree: false,
			why:      "nothing was created",
		},
		{
			name:   "the address list cannot be read",
			prefix: mustCIDR(t, "2001:db8::/96"),
			api: func(a *initAPI) {
				a.allocatable = func(*zecTypes.ENI) ([]net.IP, *net.IPNet, error) {
					return nil, nil, errors.New("metadata unavailable")
				}
			},
			wantFree: true,
		},
		{
			// The attach reply is allowed to come back without a prefix, so
			// this is a real path, not a defensive one.
			name:   "the IPv6 prefix cannot be assigned",
			prefix: nil,
			api: func(a *initAPI) {
				a.v6Block = func(string, string) (*net.IPNet, net.IP, error) {
					return nil, nil, errors.New("no prefix available")
				}
			},
			wantFree: true,
		},
		{
			// A /126 holds four addresses and firstPodOffsetV6 skips three, so
			// one is carveable and three were asked for.
			name:     "the prefix is too small to carve from",
			prefix:   mustCIDR(t, "2001:db8::/126"),
			wantFree: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &initAPI{
				allocENI: func(string, string, int) (*zecTypes.ENI, error) {
					return createdENI(t, "eni-created", tc.prefix), nil
				},
				allocatable: okAlloc,
				v6Block: func(string, string) (*net.IPNet, net.IP, error) {
					return mustCIDR(t, "2001:db8::/96"), net.ParseIP("2001:db8::2"), nil
				},
			}
			if tc.api != nil {
				tc.api(api)
			}
			f := newInitFactory(api)
			eni := &ENI{pending: promised}
			f.enis = append(f.enis, eni)
			f.eniPending <- struct{}{}

			f.initENI(eni)

			if got := len(api.freed) > 0; got != tc.wantFree {
				t.Errorf("FreeENI called = %v (%v), want %v -- %s", got, api.freed, tc.wantFree,
					"a created interface that is dropped from f.enis without being freed is leaked on the platform")
			}
			if len(api.freed) > 1 {
				t.Errorf("FreeENI called %d times (%v), want at most once", len(api.freed), api.freed)
			}

			// The count is the other half: one result per promise, no more and
			// no fewer, or Create never returns.
			results := len(f.eniIpReceiver)
			if results != promised {
				t.Errorf("emitted %d results for %d promised orders; Create reads exactly as many "+
					"as it ordered, so anything else either wedges it in receiveRes or leaves a "+
					"stray result for the next caller", results, promised)
			}
			for i := 0; i < results; i++ {
				if res := <-f.eniIpReceiver; res.err == nil {
					t.Errorf("result %d carries no error, but init failed", i)
				}
			}

			// Out of f.enis, so no submitOrder can push an unanswerable order
			// onto an interface whose worker was never started.
			f.RLock()
			remaining := len(f.enis)
			f.RUnlock()
			if remaining != 0 {
				t.Errorf("f.enis still holds %d interfaces after a failed init, want 0", remaining)
			}
			if eni.pending != 0 {
				t.Errorf("eni.pending = %d after a failed init, want 0: GC skips any interface "+
					"with pending != 0 for the life of the daemon", eni.pending)
			}
		})
	}
}

// TestInitENIKeepsTheInterfaceWhenItSucceeds is the positive control for the
// test above. Asserting only that failures release would be satisfied by a
// function that releases unconditionally.
func TestInitENIKeepsTheInterfaceWhenItSucceeds(t *testing.T) {
	const promised = 3
	api := &initAPI{
		allocENI: func(string, string, int) (*zecTypes.ENI, error) {
			return createdENI(t, "eni-created", mustCIDR(t, "2001:db8::/96")), nil
		},
		allocatable: func(*zecTypes.ENI) ([]net.IP, *net.IPNet, error) {
			return []net.IP{net.ParseIP("10.0.0.7"), net.ParseIP("10.0.0.8"), net.ParseIP("10.0.0.9")},
				mustCIDR(t, "2001:db8::/96"), nil
		},
		v6Block: func(string, string) (*net.IPNet, net.IP, error) {
			return mustCIDR(t, "2001:db8::/96"), net.ParseIP("2001:db8::2"), nil
		},
	}
	f := newInitFactory(api)
	eni := &ENI{pending: promised}
	f.enis = append(f.enis, eni)
	f.eniPending <- struct{}{}

	f.initENI(eni)

	if len(api.freed) != 0 {
		t.Errorf("FreeENI called on a successful init: %v", api.freed)
	}
	if got := len(f.eniIpReceiver); got != promised {
		t.Errorf("emitted %d results for %d promised orders", got, promised)
	}
	for i := 0; i < promised; i++ {
		res := <-f.eniIpReceiver
		if res.err != nil {
			t.Errorf("result %d carries an error on a successful init: %v", i, res.err)
			continue
		}
		if res.IPSet.IPv4 == nil || res.IPSet.IPv6 == nil {
			t.Errorf("result %d is not a complete pair: %+v", i, res.IPSet)
		}
	}
}
