// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"net"
	"strings"
	"testing"

	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// carveENI builds an ENI with the prefix exactly as given, bypassing ParseCIDR
// so a test can hand the carve a prefix no parser would produce.
func carveENI(id string, prefix *net.IPNet) *ENI {
	return &ENI{ENI: &zecTypes.ENI{ID: id, IPv6Net: prefix}}
}

func carveErr(t *testing.T, e *ENI, count int) error {
	t.Helper()
	e.Lock()
	defer e.Unlock()
	got, err := e.AllocateIPv6AddressLocked(count)
	if err == nil {
		t.Fatalf("AllocateIPv6AddressLocked(%d) = %v, want an error", count, got)
	}
	if got != nil {
		t.Errorf("AllocateIPv6AddressLocked(%d) returned %v alongside its error; the slice must be nil", count, got)
	}
	return err
}

// TestAllocateIPv6AddressLockedRejectsNonPositiveCount asserts a miscounted
// caller gets an error rather than an empty slice: an empty slice looks like
// success, and the caller goes on to pair it with IPv4 addresses that then have
// no counterpart.
func TestAllocateIPv6AddressLockedRejectsNonPositiveCount(t *testing.T) {
	_, prefix, err := net.ParseCIDR("2001:db8::/96")
	if err != nil {
		t.Fatalf("parse prefix: %v", err)
	}
	for _, count := range []int{0, -1} {
		e := carveENI("eni-count", prefix)
		if got := carveErr(t, e, count).Error(); !strings.Contains(got, "count must be > 0") {
			t.Errorf("AllocateIPv6AddressLocked(%d) = %q, want it to mention \"count must be > 0\"", count, got)
		}
	}
}

// TestAllocateIPv6AddressLockedRejectsMalformedPrefix checks each malformed
// prefix against the message of its own branch. Two of those messages differ
// only in the case of "IPv6"/"ipv6", so every assertion here is case sensitive
// on purpose: matched case-insensitively, each subtest would also pass on the
// other's message.
func TestAllocateIPv6AddressLockedRejectsMalformedPrefix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix *net.IPNet
		want   string
	}{
		{
			// No prefix at all: an interface whose IPv6Net was never
			// recorded.
			name:   "nil prefix",
			prefix: nil,
			want:   "invalid IPv6 cidr",
		},
		{
			// A mask that is not 128 bits wide, i.e. not an IPv6 mask.
			// Note the lower-case "ipv6" -- see the note above.
			name:   "mask is not 128 bits wide",
			prefix: &net.IPNet{IP: net.ParseIP("2001:db8::"), Mask: net.CIDRMask(24, 32)},
			want:   "invalid ipv6 cidr",
		},
		{
			// A base address that cannot be widened to 16 bytes. Its own
			// branch with its own message, not a variant of the one above.
			name:   "base address is not 16 bytes",
			prefix: &net.IPNet{IP: net.IP{1, 2, 3}, Mask: net.CIDRMask(96, 128)},
			want:   "invalid IPv6 address of ipv6 cidr",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := carveErr(t, carveENI("eni-malformed", tc.prefix), 1).Error()
			if !strings.Contains(got, tc.want) {
				t.Errorf("error = %q, want it to contain %q (case sensitive)", got, tc.want)
			}
		})
	}
}

// TestAllocateIPv6AddressLockedRejectsMalformedPrefixIsCaseSensitive fails if
// the two nearly-identical messages are ever made identical, which collapses two
// distinct branches into one and makes the subtests above pass for the wrong
// reason.
func TestAllocateIPv6AddressLockedRejectsMalformedPrefixIsCaseSensitive(t *testing.T) {
	nilErr := carveErr(t, carveENI("a", nil), 1).Error()
	badMask := &net.IPNet{IP: net.ParseIP("2001:db8::"), Mask: net.CIDRMask(24, 32)}
	maskErr := carveErr(t, carveENI("a", badMask), 1).Error()

	if nilErr == maskErr {
		t.Fatalf("the nil-prefix and bad-mask branches now produce the same message %q; "+
			"the subtests above can no longer tell them apart", nilErr)
	}
	if !strings.Contains(nilErr, "invalid IPv6 cidr") || !strings.Contains(maskErr, "invalid ipv6 cidr") {
		t.Errorf("the two branches no longer differ only by case: %q vs %q", nilErr, maskErr)
	}
}

// TestAllocateIPv6AddressLockedIsAllOrNothing asserts a short carve returns
// nothing rather than a partial slice, and leaves no reservation behind. A
// reservation that outlives a failed carve takes that address out of the prefix
// for the life of the interface: nothing appends it to e.ips, so appendIPLocked
// never spends the reservation, and nothing calls releaseV6Locked for it either.
func TestAllocateIPv6AddressLockedIsAllOrNothing(t *testing.T) {
	// /126 leaves four addresses; firstPodOffsetV6 skips the first three, so
	// exactly one is carveable.
	_, prefix, err := net.ParseCIDR("2001:db8::/126")
	if err != nil {
		t.Fatalf("parse prefix: %v", err)
	}

	t.Run("asking for more than the prefix holds", func(t *testing.T) {
		e := carveENI("eni-full", prefix)
		got := carveErr(t, e, 2).Error()
		if !strings.Contains(got, "not enough available IPv6 addresses") {
			t.Errorf("error = %q, want it to mention \"not enough available IPv6 addresses\"", got)
		}
		if len(e.v6Reserved) != 0 {
			t.Errorf("v6Reserved = %v after a failed carve, want empty -- a reservation nothing "+
				"can spend removes that address from the prefix permanently", e.v6Reserved)
		}
	})

	t.Run("asking for exactly what is left", func(t *testing.T) {
		e := carveENI("eni-exact", prefix)
		e.Lock()
		got, err := e.AllocateIPv6AddressLocked(1)
		e.Unlock()
		if err != nil {
			t.Fatalf("AllocateIPv6AddressLocked(1) on a prefix with one address left: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("carved %d addresses, want 1: %v", len(got), got)
		}
		if len(e.v6Reserved) != 1 {
			t.Errorf("v6Reserved holds %d entries after a successful carve, want 1 -- "+
				"the carved address must be reserved until appendIPLocked spends it", len(e.v6Reserved))
		}
	})
}

// TestAppendIPLockedAlwaysSpendsTheV6Reservation pins the order inside
// appendIPLocked: the delete that spends the v6 reservation runs before the
// early return for an ID already in e.ips, so a duplicate append still clears
// it. Spent after that return instead, the reservation would survive with
// nothing able to consume it -- the address never reaches e.ips, so no later
// append spends it and nothing calls releaseV6Locked for it.
//
// The state is built directly rather than through the carve, which cannot
// produce it: AllocateIPv6AddressLocked excludes every v6 already in e.ips, and
// PreStart's reserve path filters against the same set. If a future path ever
// reserves without that filter, this is the shape of the leak, and it is silent
// -- the offset never comes up for reuse and the platform publishes no
// per-address IPv6 view to notice it is gone.
//
// On failure, fix appendIPLocked to spend before the early return; do not relax
// the assertion.
func TestAppendIPLockedAlwaysSpendsTheV6Reservation(t *testing.T) {
	_, prefix, err := net.ParseCIDR("2001:db8::/96")
	if err != nil {
		t.Fatalf("parse prefix: %v", err)
	}
	e := carveENI("eni-dup", prefix)
	v6 := net.ParseIP("2001:db8::3")

	res := &ENIIPRes{ENIIP: &zecTypes.ENIIP{
		ENI:   e.ENI,
		IPSet: zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.7"), IPv6: v6},
	}}

	e.Lock()
	// Already recorded and still reserved: the state the early return has to
	// handle.
	e.appendIPLocked(res)
	e.reserveV6Locked([]net.IP{v6})
	reservedBefore := len(e.v6Reserved)

	// The same ID arriving again -- the early-return path.
	e.appendIPLocked(res)
	reservedAfter := len(e.v6Reserved)
	held := len(e.ips)
	e.Unlock()

	if reservedBefore != 1 {
		t.Fatalf("setup is wrong: v6Reserved holds %d entries, want 1", reservedBefore)
	}
	if held != 1 {
		t.Errorf("e.ips holds %d entries after appending the same ID twice, want 1", held)
	}
	if reservedAfter != 0 {
		t.Errorf("v6Reserved still holds %d entries after a duplicate append: %s is now reserved "+
			"with nothing able to spend it, so it is out of the prefix for the life of the interface",
			reservedAfter, v6)
	}
}
