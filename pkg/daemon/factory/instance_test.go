// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"fmt"
	"strings"
	"testing"
)

// TestENIAvailableNeverNegative pins the clamp its consumers rely on.
//
// A negative "available" is not a value this function may return: AllocatableIPs
// multiplies by it, and that figure sizes the pool's ticket channel (make()
// panics on a negative) and bounds the evictor's "len(pods) >= limit". The raw
// arithmetic does go below zero -- ENIQuota comes from a spec table that rounds
// a between-tier vCPU count down, so an instance can legitimately carry more
// interfaces than the table admits.
func TestENIAvailableNeverNegative(t *testing.T) {
	for _, tt := range []struct {
		name string
		l    InstanceLimits
		want int
	}{
		{
			name: "room for one",
			l:    InstanceLimits{InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 2}},
			want: 1,
		},
		{
			name: "quota exactly consumed",
			l: InstanceLimits{
				InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 2},
				ENIUnmanaged:       1,
			},
			want: 0,
		},
		{
			name: "over budget by three",
			l: InstanceLimits{
				InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 2},
				ENIUnmanaged:       4,
			},
			want: 0,
		},
		{
			// Cordoned reports what is already created, which is never negative.
			name: "cordoned while over budget",
			l: InstanceLimits{
				InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 2},
				ENIUnmanaged:       4,
				ENIManaged:         1,
				Cordon:             true,
			},
			want: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.l.ENIAvailable(); got != tt.want {
				t.Errorf("ENIAvailable() = %d, want %d (limits %s)", got, tt.want, tt.l.String())
			}
		})
	}
}

// TestAllocatableIPs pins the capacity formula. With poolTargetCapProbe on it
// becomes the pool's max cap and the count the device plugin advertises, and so
// how many pods requesting the resource the scheduler places here.
//
// Every interface contributes all but one of its addresses: each keeps its own
// primary, because the guest configures that address on the interface so it has
// a source the fabric accepts when resolving the gateway. On the instance's own
// interface that address is the node's.
//
// That includes CNI-created interfaces. Counting their primary as allocatable
// would hand a pod the address the interface is configured with, which
// namesInterfacePrimary in pkg/daemon rejects on the way back in.
func TestAllocatableIPs(t *testing.T) {
	tests := []struct {
		name string
		l    InstanceLimits
		want int
	}{
		{
			// 8 vCPU: 2 interfaces, 15 addresses each, both usable and each
			// keeping its own primary.
			name: "two interfaces, none created yet",
			l:    InstanceLimits{InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 2, IPv4MaxPerENI: 15}},
			want: 2 * 14,
		},
		{
			// 16 vCPU: 4 interfaces, 30 addresses each.
			name: "four interfaces",
			l:    InstanceLimits{InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 4, IPv4MaxPerENI: 30}},
			want: 4 * 29,
		},
		{
			// An interface attached by someone else takes a slot from the quota.
			name: "one interface belongs to the user",
			l: InstanceLimits{
				InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 4, IPv4MaxPerENI: 30},
				ENIUnmanaged:       1,
			},
			want: 3 * 29,
		},
		{
			// Cordoned: no new interfaces, so only those already created count,
			// plus the instance's own.
			name: "cordoned with one created",
			l: InstanceLimits{
				InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 4, IPv4MaxPerENI: 30},
				ENIManaged:         1,
				Cordon:             true,
			},
			want: 2 * 29,
		},
		{
			// Even with no room for a CNI interface the instance's own offers
			// its secondaries -- the whole point of including it.
			name: "single interface instance",
			l:    InstanceLimits{InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 1, IPv4MaxPerENI: 6}},
			want: 5,
		},
		{
			name: "unknown per-eni limit yields nothing",
			l:    InstanceLimits{InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 4}},
			want: 0,
		},
		{
			// One address per interface means no pod addresses at all: that one
			// is the interface's own. Guarding this keeps the formula from
			// reporting the quota itself as capacity.
			name: "one address per interface yields nothing",
			l:    InstanceLimits{InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 4, IPv4MaxPerENI: 1}},
			want: 0,
		},
		{
			// More interfaces attached by hand than the quota allows drives the
			// raw arithmetic negative; ENIAvailable clamps it to zero, and the
			// instance's own interface still contributes -- it exists whatever
			// an operator did to the rest, and reporting zero here would drain
			// the pool and refuse every pod on a node that can still serve 14.
			name: "more user interfaces than the quota",
			l: InstanceLimits{
				InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 2, IPv4MaxPerENI: 15},
				ENIUnmanaged:       5,
			},
			want: 14,
		},
		{
			// The boundary of the same case: no room for a CNI interface, but
			// the instance's own is untouched.
			name: "quota exactly consumed by user interfaces",
			l: InstanceLimits{
				InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 2, IPv4MaxPerENI: 15},
				ENIUnmanaged:       1,
			},
			want: 14,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.l.AllocatableIPs(); got != tt.want {
				t.Errorf("AllocatableIPs() = %d, want %d (limits %s)", got, tt.want, tt.l.String())
			}
		})
	}
}

// TestInstanceLimitsStringReportsTheRealAllocatable pins the number in the log
// line to the number the node actually advertises.
//
// The line exists so an operator chasing "why is capacity N" can stop at it.
// If it printed anything other than AllocatableIPs() it would send them looking
// for a discrepancy that is not there -- worse than not printing it at all.
func TestInstanceLimitsStringReportsTheRealAllocatable(t *testing.T) {
	for _, tc := range []struct {
		name string
		l    InstanceLimits
	}{
		{"no interfaces attached beyond the primary", InstanceLimits{
			InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 4, IPv4MaxPerENI: 30},
		}},
		{"two interfaces left unmanaged", InstanceLimits{
			InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 4, IPv4MaxPerENI: 30},
			ENIUnmanaged:       2,
		}},
		{"quota fully spent by unmanaged interfaces", InstanceLimits{
			InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 2, IPv4MaxPerENI: 15},
			ENIUnmanaged:       5,
		}},
		{"an interface that cannot serve a pod at all", InstanceLimits{
			InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 2, IPv4MaxPerENI: 1},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := tc.l
			want := fmt.Sprintf("allocatable: %d}", l.AllocatableIPs())
			if got := l.String(); !strings.HasSuffix(got, want) {
				t.Errorf("String() = %q\n  does not end with %q", got, want)
			}
		})
	}
}

// TestInstanceLimitsAttrRendersAsAValue guards the node event.
//
// The event is built with %v on an InstanceLimitsAttr value. Moving String to a
// pointer receiver -- the shape InstanceLimits uses -- would take it out of the
// value's method set without any compiler complaint, and the event would start
// carrying Go's default struct formatting to operators instead.
func TestInstanceLimitsAttrRendersAsAValue(t *testing.T) {
	a := InstanceLimitsAttr{ENIQuota: 4, IPv4MaxPerENI: 30, IPv6CidrMaxPerENI: 1}
	got := fmt.Sprintf("%v", a)
	if !strings.HasPrefix(got, "{ENIQuota:") || !strings.Contains(got, "IPv4MaxPerENI: 30") {
		t.Errorf("%%v on an InstanceLimitsAttr value = %q; String() was not used", got)
	}
}
