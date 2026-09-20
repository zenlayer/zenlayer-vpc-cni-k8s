// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"net"
	"testing"

	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// TestDeleteIPLockedClearsOnlyTheInterfaceFullReason pins which reason an
// address leaving the interface is allowed to cure.
//
// An address leaving is evidence about this interface and nothing else, so it
// cures forbidInterfaceFull and must not touch the other two:
//
//   - forbidSubnet is subnetMonitor's to clear. Clearing it here would re-admit
//     an interface whose subnet is still out of addresses.
//   - forbidRetired must survive everything. A retiring interface has no worker
//     -- it either never started one or GC stopped it -- so an order admitted
//     onto it is never answered, and the Create waiting on that result blocks
//     forever holding blockPause, which also stops GC.
func TestDeleteIPLockedClearsOnlyTheInterfaceFullReason(t *testing.T) {
	res := &zecTypes.ENIIP{
		ENI:   &zecTypes.ENI{ID: "eni-1"},
		IPSet: zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.7")},
	}
	for _, tc := range []struct {
		name  string
		start forbidReason
		want  forbidReason
	}{
		{"a full interface is cured by an address leaving", forbidInterfaceFull, forbidNone},
		{"a subnet problem is not cured by this interface", forbidSubnet, forbidSubnet},
		{"a retiring interface stays out of service", forbidRetired, forbidRetired},
		{"an eligible interface stays eligible", forbidNone, forbidNone},
		// The cases a single-valued field could not represent at all. With one
		// slot the second reason overwrote the first, and curing the second
		// then re-admitted an interface the first still applied to.
		{
			"full AND subnet-starved: only the full half is cured",
			forbidInterfaceFull | forbidSubnet, forbidSubnet,
		},
		{
			"full AND retiring: the interface still must not be used",
			forbidInterfaceFull | forbidRetired, forbidRetired,
		},
		{
			"all three: curing one leaves the other two",
			forbidInterfaceFull | forbidSubnet | forbidRetired, forbidSubnet | forbidRetired,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &ENI{ENI: res.ENI, forbidAssign: tc.start}
			e.ips = append(e.ips, &ENIIPRes{ENIIP: res})

			e.Lock()
			e.deleteIPLocked(res)
			got := e.forbidAssign
			held := len(e.ips)
			e.Unlock()

			if held != 0 {
				t.Errorf("e.ips still holds %d entries; the address was not removed", held)
			}
			if got != tc.want {
				t.Errorf("forbidAssign = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDeleteIPLockedLeavesTheReasonAloneWhenNothingWasRemoved is the negative
// control for the test above.
//
// The cure is "an address left", not "deleteIPLocked was called". A call naming
// an address this interface never held removes nothing, so a full interface
// must stay forbidden -- curing on the call alone would let a release for an
// address already gone re-admit an interface still at its limit.
func TestDeleteIPLockedLeavesTheReasonAloneWhenNothingWasRemoved(t *testing.T) {
	eni := &zecTypes.ENI{ID: "eni-1"}
	held := &zecTypes.ENIIP{ENI: eni, IPSet: zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.7")}}
	stranger := &zecTypes.ENIIP{ENI: eni, IPSet: zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.99")}}

	e := &ENI{ENI: eni, forbidAssign: forbidInterfaceFull}
	e.ips = append(e.ips, &ENIIPRes{ENIIP: held})

	e.Lock()
	e.deleteIPLocked(stranger)
	got := e.forbidAssign
	remaining := len(e.ips)
	e.Unlock()

	if remaining != 1 {
		t.Errorf("e.ips holds %d entries, want 1 -- an address it never held was removed", remaining)
	}
	if got != forbidInterfaceFull {
		t.Errorf("forbidAssign = %v, want %v: no address left this interface, so nothing was cured",
			got, forbidInterfaceFull)
	}
}

// TestForbidReasonStringsAreDistinct guards the log line.
//
// The one consumer of String() today is the debug line submitOrder logs when it
// skips an interface whose forbidAssign is not empty. Two reasons rendering the
// same text would leave that line unable to separate "the subnet is out of
// addresses" from "this interface is full".
func TestForbidReasonStringsAreDistinct(t *testing.T) {
	seen := map[string]forbidReason{}
	for _, r := range []forbidReason{forbidNone, forbidSubnet, forbidInterfaceFull, forbidRetired} {
		s := r.String()
		if s == "" {
			t.Errorf("reason %d renders as the empty string", r)
		}
		if prev, dup := seen[s]; dup {
			t.Errorf("reasons %d and %d both render as %q", prev, r, s)
		}
		seen[s] = r
	}
}

// TestForbidReasonsAccumulateIndependently is the reason this is a set.
//
// Two conditions can hold at once, and each is cured by a different event. A
// single-valued field could only remember the most recent one: the earlier
// condition was silently forgotten, its cure never had to arrive, and the
// interface came back into service while it still applied.
//
// The retiring case carries the worst outcome: a retired interface has no
// worker, so an order admitted onto it is never answered and the Create waiting
// on the result blocks forever holding blockPause, which also stops GC.
func TestForbidReasonsAccumulateIndependently(t *testing.T) {
	t.Run("clearing the subnet reason leaves a full interface forbidden", func(t *testing.T) {
		r := forbidNone
		r |= forbidInterfaceFull // the interface filled up
		r |= forbidSubnet        // then the subnet ran dry

		r &^= forbidSubnet // subnetMonitor sees the subnet recover

		if r&forbidInterfaceFull == 0 {
			t.Errorf("the interface is eligible again after only the subnet recovered; "+
				"it is still at its per-NIC limit. reasons=%v", r)
		}
		if r == forbidNone {
			t.Errorf("reasons = none, want the interface-full bit to survive")
		}
	})

	t.Run("nothing clears a retiring interface", func(t *testing.T) {
		r := forbidRetired | forbidSubnet | forbidInterfaceFull
		r &^= forbidSubnet        // subnetMonitor
		r &^= forbidInterfaceFull // deleteIPLocked

		if r&forbidRetired == 0 {
			t.Errorf("a retiring interface became eligible; an order admitted onto it has no " +
				"worker to answer it, and the Create waiting on that result never returns")
		}
	})

	t.Run("an interface is eligible only when every reason is gone", func(t *testing.T) {
		r := forbidInterfaceFull | forbidSubnet
		r &^= forbidInterfaceFull
		if r == forbidNone {
			t.Fatalf("eligible with the subnet reason still set")
		}
		r &^= forbidSubnet
		if r != forbidNone {
			t.Errorf("reasons = %v after both were cured, want none", r)
		}
	})
}
