// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/factory"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// TestCNIENICountExcludesPrimary guards the arithmetic in createEniAsync.
// f.enis holds every attached interface including the instance's own, because
// GetAttachedENIs returns it so its secondary addresses can serve pods.
// ENIAvailable() is the quota less that interface. Comparing len(f.enis)
// against it therefore counts the primary on one side of the inequality only,
// which costs every node exactly one interface -- and on a two-interface
// instance (ENIQuota=2, ENIAvailable=1) it means len(f.enis) is already 1
// before the CNI has created anything, so the first ENI can never be created
// and the node tops out at the primary's addresses alone: 14 pods, while
// GetResourceLimit goes on reporting the full 28.
func TestCNIENICountExcludesPrimary(t *testing.T) {
	primary := &ENI{ENI: &zecTypes.ENI{ID: "primary", IsPrimary: true}}
	cni1 := &ENI{ENI: &zecTypes.ENI{ID: "cni-1"}}
	cni2 := &ENI{ENI: &zecTypes.ENI{ID: "cni-2"}}

	for _, tc := range []struct {
		name string
		enis []*ENI
		want int
	}{
		{"empty", nil, 0},
		{"primary only", []*ENI{primary}, 0},
		{"primary plus one", []*ENI{primary, cni1}, 1},
		{"primary plus two", []*ENI{primary, cni1, cni2}, 2},
		// A nil inner ENI is a slot reserved by createEniAsync before the
		// interface exists; it is the CNI's, so it counts.
		{"pending slot counts", []*ENI{primary, {ENI: nil}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &eniIPFactory{enis: tc.enis}
			if got := f.cniENICountLocked(); got != tc.want {
				t.Errorf("cniENICountLocked() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestQuotaCheckAdmitsFirstENIOnTwoInterfaceInstance states the bug in the
// terms that made it visible on the 8 vCPU nodes: the check must admit the
// first CNI interface, and must stop at exactly ENIAvailable().
func TestQuotaCheckAdmitsFirstENIOnTwoInterfaceInstance(t *testing.T) {
	limits := factory.InstanceLimits{
		InstanceLimitsAttr: factory.InstanceLimitsAttr{ENIQuota: 2, IPv4MaxPerENI: 15},
	}
	avail := limits.ENIAvailable()
	if avail != 1 {
		t.Fatalf("ENIAvailable() = %d, want 1", avail)
	}
	// 2 * (15-1): every interface keeps its own primary address for itself, so
	// each contributes its secondaries only. See InstanceLimits.AllocatableIPs.
	if got := limits.AllocatableIPs(); got != 28 {
		t.Fatalf("AllocatableIPs() = %d, want 28", got)
	}

	f := &eniIPFactory{enis: []*ENI{{ENI: &zecTypes.ENI{ID: "primary", IsPrimary: true}}}}
	if f.cniENICountLocked() >= avail {
		t.Fatal("quota check rejects the first CNI ENI; the node can only ever " +
			"use the primary's addresses while advertising a full ENI more")
	}

	f.enis = append(f.enis, &ENI{ENI: &zecTypes.ENI{ID: "cni-1"}})
	if f.cniENICountLocked() < avail {
		t.Fatal("quota check admits a second CNI ENI beyond the instance quota")
	}
}

// TestENIsCarryTheConfiguredIPStack pins a field that decides real behavior
// and had no value. The worker reads e.ipStack to decide whether a pod also
// gets an IPv6 address, and none of the three places that build an ENI used to
// set it -- the zero value made EnableIPv6 false everywhere and a dual-stack
// cluster would have silently handed out IPv4 only. It also decides whether the
// worker carves an IPv6 counterpart for each address recoverAfterAmbiguousAssign
// brings back.
//
// The assignment lives in appendEniLocked, the one funnel every ENI passes
// through, so this exercises the real path rather than a struct literal.
func TestENIsCarryTheConfiguredIPStack(t *testing.T) {
	// The zero value is the trap: it enables neither family, and the worker
	// assigns IPv4 unconditionally and only tests EnableIPv6, so a missing
	// assignment behaves exactly like a deliberate IPv4-only choice.
	var unset types.IPStack
	if unset.EnableIPv4() || unset.EnableIPv6() {
		t.Fatalf("expected the zero IPStack to enable nothing, got v4=%v v6=%v",
			unset.EnableIPv4(), unset.EnableIPv6())
	}

	for _, stack := range []types.IPStack{types.IPStackIPv4, types.IPStackIPv6, types.IPStackDual} {
		t.Run(string(stack), func(t *testing.T) {
			f := &eniIPFactory{ipStack: stack}

			// A caller that forgot the field, which is what every call site
			// looked like before.
			eni := &ENI{ENI: &zecTypes.ENI{ID: "cni-1"}}
			got := f.appendEniLocked(eni)

			if got != eni {
				t.Fatalf("appendEniLocked returned a different ENI for a new id")
			}
			if eni.ipStack != stack {
				t.Errorf("eni.ipStack = %q, want %q", eni.ipStack, stack)
			}
			if want := stack == types.IPStackDual || stack == types.IPStackIPv6; eni.ipStack.EnableIPv6() != want {
				t.Errorf("EnableIPv6() = %v, want %v", eni.ipStack.EnableIPv6(), want)
			}
		})
	}

	// Re-appending an id the factory already holds returns the ENI it has, which
	// must keep its own stack rather than be replaced by the newcomer.
	t.Run("existing id keeps its stack", func(t *testing.T) {
		f := &eniIPFactory{ipStack: types.IPStackDual}
		first := &ENI{ENI: &zecTypes.ENI{ID: "cni-1"}}
		f.appendEniLocked(first)

		again := &ENI{ENI: &zecTypes.ENI{ID: "cni-1"}}
		got := f.appendEniLocked(again)

		if got != first {
			t.Fatal("appendEniLocked did not return the ENI it already held")
		}
		if got.ipStack != types.IPStackDual {
			t.Errorf("ipStack = %q, want %q", got.ipStack, types.IPStackDual)
		}
		if len(f.enis) != 1 {
			t.Errorf("factory holds %d enis, want 1", len(f.enis))
		}
	})
}

// stubAlloc records the batch size the worker asks for and hands back that many
// addresses. The embedded interface satisfies the rest; nothing else is called.
type stubAlloc struct {
	factory.ZecAPI
	mu       sync.Mutex
	requests []int
}

func (s *stubAlloc) AllocIPv4Addresses(_, _ string, n int) ([]net.IP, error) {
	s.mu.Lock()
	s.requests = append(s.requests, n)
	s.mu.Unlock()
	out := make([]net.IP, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, net.IPv4(10, 0, 1, byte(i+1)))
	}
	return out, nil
}

func (s *stubAlloc) got() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.requests...)
}

func eniWithIPs(id string, primary bool, held int, perENI int, api factory.ZecAPI) *ENI {
	e := &ENI{
		ENI:        &zecTypes.ENI{ID: id, IsPrimary: primary},
		order:      make(chan struct{}, defaultOrderCnt),
		stopWorker: make(chan struct{}, 1),
		zecApi:     api,
		maxIPv4:    func() int { return perENI },
	}
	for i := 0; i < held; i++ {
		e.ips = append(e.ips, &ENIIPRes{ENIIP: &zecTypes.ENIIP{
			ENI:   e.ENI,
			IPSet: zecTypes.IPSet{IPv4: net.IPv4(10, 0, 9, byte(i+1))},
		}})
	}
	return e
}

// TestRoomForMore is the arithmetic the batch size rests on. The platform counts
// an interface's own primary address against its per-NIC limit, and every
// interface keeps that address for itself -- the guest configures it so the
// interface has a source the fabric accepts -- so the pool may hold perENI-1 on
// every interface, the instance's own and CNI-created alike.
//
// The primary flag is kept in the table on purpose even though it no longer
// changes the answer: it is what makes the equality visible, and it is what
// would fail if the two kinds ever diverged again.
func TestRoomForMore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		primary bool
		held    int
		perENI  int
		want    int
	}{
		{"empty cni eni", false, 0, 30, 29},
		{"empty primary eni", true, 0, 15, 14},
		{"cni eni nearly full", false, 27, 30, 2},
		{"primary eni nearly full", true, 13, 15, 1},
		{"cni eni exactly full", false, 29, 30, 0},
		{"primary eni exactly full", true, 14, 15, 0},
		// Adoption can push an interface past what the pool thought it held;
		// a negative difference must read as no room, never as a huge batch.
		{"over full never goes negative", true, 20, 15, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := eniWithIPs("x", tc.primary, tc.held, tc.perENI, nil)
			if got := e.roomForMoreLocked(tc.perENI); got != tc.want {
				t.Errorf("roomForMoreLocked(%d) = %d, want %d", tc.perENI, got, tc.want)
			}
		})
	}
}

// TestWorkerClampsBatchToRoom covers the failure this prevents. Orders are
// admitted while the interface has room, but addresses can arrive between
// admission and assignment, and the platform refuses an oversized batch with
// LAN_IP_EXCEED_LIMIT -- and has been observed to apply it anyway, leaving
// addresses this daemon does not know it owns. Asking only for what fits keeps
// that off the normal path.
//
// The count matters as much as the clamp: the caller reads exactly one result
// per order it submitted, so a short count would block it forever.
func TestWorkerClampsBatchToRoom(t *testing.T) {
	for _, tc := range []struct {
		name         string
		held, perENI int
		orders       int
		wantAsked    []int
		wantOK       int
	}{
		{"room for fewer than ordered", 27, 30, 4, []int{2}, 2},
		{"no room at all", 29, 30, 4, nil, 0},
		{"room for all", 10, 30, 4, []int{4}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &stubAlloc{}
			e := eniWithIPs("cni-1", false, tc.held, tc.perENI, api)

			results := make(chan *ENIIPRes, tc.orders)
			for i := 0; i < tc.orders; i++ {
				e.order <- struct{}{}
			}
			go e.worker(results)
			defer close(e.stopWorker)

			ok, failed := 0, 0
			for i := 0; i < tc.orders; i++ {
				select {
				case r := <-results:
					if r.err != nil {
						failed++
						if !errors.Is(r.err, errENIFull) {
							t.Errorf("failure %d is %v, want errENIFull", i, r.err)
						}
					} else {
						ok++
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("only %d of %d results arrived; the caller would block here",
						ok+failed, tc.orders)
				}
			}

			if ok != tc.wantOK {
				t.Errorf("got %d addresses, want %d", ok, tc.wantOK)
			}
			if want := tc.orders - tc.wantOK; failed != want {
				t.Errorf("got %d failures, want %d", failed, want)
			}
			if asked := api.got(); !reflect.DeepEqual(asked, tc.wantAsked) {
				t.Errorf("asked the platform for %v, want %v", asked, tc.wantAsked)
			}
		})
	}
}
