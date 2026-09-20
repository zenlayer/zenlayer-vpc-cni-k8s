// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"net"
	"testing"

	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

func primaryENI() *zecTypes.ENI {
	return &zecTypes.ENI{
		ID: "eni-primary-test",
		PrimaryIP: zecTypes.IPSet{
			IPv4: net.ParseIP("10.0.0.40"),
			IPv6: net.ParseIP("2001:2:0:7775:0:1:0:2"),
		},
	}
}

// TestNamesInterfacePrimary covers the comparison itself.
//
// The address-rendering case is the one worth spelling out: metadata reports an
// interface's primary as 2001:2:0:7775:0:1::2 while describe reports
// 2001:2:0:7775:0:1:0:2. Those are the same address. A string comparison here
// would miss the match in exactly the situation the check exists for.
func TestNamesInterfacePrimary(t *testing.T) {
	eni := primaryENI()
	for _, tc := range []struct {
		name string
		set  zecTypes.IPSet
		want bool
	}{
		{"the primary IPv4", zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.40")}, true},
		{"the primary IPv6", zecTypes.IPSet{IPv6: net.ParseIP("2001:2:0:7775:0:1:0:2")}, true},
		{
			name: "the primary IPv6 written the other way",
			set:  zecTypes.IPSet{IPv6: net.ParseIP("2001:2:0:7775:0:1::2")},
			want: true,
		},
		{
			// A pair is refused if either family names the primary: the pod
			// would still end up holding an address the interface has.
			name: "a pair whose IPv6 half is the primary",
			set:  zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.55"), IPv6: net.ParseIP("2001:2:0:7775:0:1:0:2")},
			want: true,
		},
		{"an ordinary pair", zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.55"), IPv6: net.ParseIP("2001:2:0:7775:0:1:0:9")}, false},
		{"an empty set", zecTypes.IPSet{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := namesInterfacePrimary(eni, tc.set); got != tc.want {
				t.Errorf("namesInterfacePrimary(%v) = %v, want %v", tc.set, got, tc.want)
			}
		})
	}

	t.Run("a disputed primary IPv6 counts too", func(t *testing.T) {
		d := primaryENI()
		d.PrimaryIPv6Disputed = net.ParseIP("2001:2:0:7775:0:1:0:7")
		set := zecTypes.IPSet{IPv6: net.ParseIP("2001:2:0:7775:0:1::7")}
		if !namesInterfacePrimary(d, set) {
			t.Errorf("a disputed primary is not refused; when the two sources disagree about which "+
				"address the interface holds, neither candidate is safe to serve. eni=%v set=%v", d, set)
		}
	})
}

// TestValidRefusesTheInterfacesOwnPrimary is the regression test for the P0
// found on 2026-08-27.
//
// pool.Release asks Valid before deciding where a released resource goes: nil
// sends it to the available queue, the one pods are served from, and an error
// sends it to ReleaseInValid, which drops a primary's record without
// deallocating the address.
//
// Valid used to answer with whatever error the record carried, and a record
// that had not yet been through a GC cycle carried none. A corrupt stored
// record naming the interface's own primary was therefore pronounced healthy
// and queued for the next pod. Measured: 111 seconds in the queue with
// reconcileIntervalSec at 120; ~20 minutes at the default 1200.
//
// The failure it leads to is silent. The guest has that address configured on
// the interface, so the host's `local` table matches at priority 0, ahead of
// the 512 to-pod rule, and every packet for the pod is delivered to the host
// instead -- with the pod's address, routes and neighbor entries all correct.
func TestValidRefusesTheInterfacesOwnPrimary(t *testing.T) {
	build := func(set zecTypes.IPSet) (*eniIPFactory, *zecTypes.ENIIP) {
		vpcENI := primaryENI()
		res := &zecTypes.ENIIP{ENI: vpcENI, IPSet: set}
		e := &ENI{ENI: vpcENI}
		// err is nil on purpose: this is a record GC has not looked at yet,
		// which is the state the defect lived in.
		e.ips = append(e.ips, &ENIIPRes{ENIIP: res})
		return &eniIPFactory{enis: []*ENI{e}}, res
	}

	t.Run("a record naming the primary is refused even with no recorded error", func(t *testing.T) {
		f, res := build(zecTypes.IPSet{
			IPv4: net.ParseIP("10.0.0.40"), // the interface's own
			IPv6: net.ParseIP("2001:2:0:7775:0:1:0:2"),
		})
		if err := f.Valid(res); err == nil {
			t.Errorf("Valid returned nil for the interface's own primary; pool.Release would push it " +
				"onto the available queue and the next pod would be handed an address the host holds")
		}
	})

	t.Run("an ordinary record with no recorded error is still valid", func(t *testing.T) {
		// The negative control. Refusing everything would send healthy
		// addresses to ReleaseInValid, which unassigns them -- turning a
		// silent black-hole into a silent capacity leak.
		f, res := build(zecTypes.IPSet{
			IPv4: net.ParseIP("10.0.0.55"),
			IPv6: net.ParseIP("2001:2:0:7775:0:1:0:9"),
		})
		if err := f.Valid(res); err != nil {
			t.Errorf("Valid = %v for an ordinary in-use address, want nil", err)
		}
	})
}
