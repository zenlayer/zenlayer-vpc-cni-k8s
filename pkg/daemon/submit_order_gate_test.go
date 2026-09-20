// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"testing"

	helper "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/factory"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// orderSubnets answers GetPodSubnet with a real subnet. initSubnets answers nil
// there, which sends every interface down submitOrder's "subnet not found"
// continue before the forbidAssign check is ever reached.
type orderSubnets struct{ initSubnets }

func (orderSubnets) GetPodSubnet(id string) *helper.PodSubnet {
	return &helper.PodSubnet{SubnetId: id, IPv4Cidr: "10.0.0.0/24"}
}

// orderFactory is IPv4-only so the subnet above supports it: a PodSubnet with
// only an IPv4 CIDR reports IPv4, and a dual factory would be turned away by
// the subnet gate instead.
func orderFactory(perENI int, enis ...*ENI) *eniIPFactory {
	return &eniIPFactory{
		eniFactory: &eniFactory{
			ipStack:    types.IPStackIPv4,
			subnets:    orderSubnets{},
			secManager: initSec{},
			limit: initLimit{lim: helper.InstanceLimits{
				InstanceLimitsAttr: helper.InstanceLimitsAttr{IPv4MaxPerENI: perENI},
			}},
		},
		ipStack: types.IPStackIPv4,
		enis:    enis,
	}
}

func newOrderENI(id string) *ENI {
	e := newTestENI(id)
	e.Subnet = zecTypes.Subnet{ID: "subnet-1"}
	return e
}

// TestSubmitOrderSkipsARetiredInterface drives the gate itself rather than the
// bit that feeds it.
//
// An order admitted onto an interface whose worker is gone is never answered,
// and the Create waiting on that result blocks forever holding blockPause,
// which also stops GC and pool shrink. The bookkeeping half of this -- that
// forbidRetired is set and that no cure clears it -- is pinned elsewhere; what
// is asserted here is that submitOrder actually consults it, which asserting on
// forbidAssign alone can never show.
//
// The eligible case is the control. Without it the retired case also passes
// when submitOrder turns the interface away for some unrelated reason, and the
// subnet gate above is exactly such a reason.
func TestSubmitOrderSkipsARetiredInterface(t *testing.T) {
	t.Run("an eligible interface takes the order", func(t *testing.T) {
		e := newOrderENI("eni-1")
		f := orderFactory(4, e)

		if err := f.submitOrder(); err != nil {
			t.Fatalf("submitOrder() = %v, want nil -- the control case must reach the order", err)
		}
		if e.pending != 1 {
			t.Errorf("pending = %d, want 1", e.pending)
		}
		if len(e.order) != 1 {
			t.Errorf("%d orders queued, want 1", len(e.order))
		}
	})

	t.Run("a retired interface does not", func(t *testing.T) {
		e := newOrderENI("eni-1")
		e.forbidAssign |= forbidRetired
		f := orderFactory(4, e)

		if err := f.submitOrder(); err == nil {
			t.Fatal("submitOrder() = nil, want an error: the order was admitted onto a retired interface")
		}
		if e.pending != 0 {
			t.Errorf("pending = %d, want 0: a pending order GC will wait on for ever", e.pending)
		}
		if len(e.order) != 0 {
			t.Errorf("%d orders queued on a retired interface; its worker is stopping, so nothing "+
				"answers them and the Create waiting on the result blocks holding blockPause", len(e.order))
		}
	})

	// forbidRetired is the reason this gate exists, but the gate is written
	// against forbidNone, so every reason has to be turned away by it. A gate
	// that tested only for forbidRetired would let a full or wrong-subnet
	// interface through.
	t.Run("every forbid reason is turned away", func(t *testing.T) {
		for _, r := range []forbidReason{forbidRetired, forbidInterfaceFull, forbidSubnet} {
			t.Run(r.String(), func(t *testing.T) {
				e := newOrderENI("eni-1")
				e.forbidAssign |= r
				f := orderFactory(4, e)

				if err := f.submitOrder(); err == nil {
					t.Errorf("submitOrder() = nil for %s, want an error", r)
				}
				if len(e.order) != 0 {
					t.Errorf("%d orders queued for %s, want 0", len(e.order), r)
				}
			})
		}
	})
}
