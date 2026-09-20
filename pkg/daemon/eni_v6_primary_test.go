// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"net"
	"testing"

	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// eniWithPrefix builds the least an ENI needs for AllocateIPv6AddressLocked: an
// IPv6 prefix, plus the interface's own primary when one is given.
func eniWithPrefix(t *testing.T, prefix, primaryV6 string) *ENI {
	t.Helper()
	_, v6Net, err := net.ParseCIDR(prefix)
	if err != nil {
		t.Fatalf("parse prefix %s: %v", prefix, err)
	}
	e := &ENI{ENI: &zecTypes.ENI{ID: "eni-test", IPv6Net: v6Net}}
	if primaryV6 != "" {
		e.PrimaryIP.IPv6 = net.ParseIP(primaryV6)
	}
	return e
}

func carve(t *testing.T, e *ENI, count int) []net.IP {
	t.Helper()
	e.Lock()
	defer e.Unlock()
	got, err := e.AllocateIPv6AddressLocked(count)
	if err != nil {
		t.Fatalf("AllocateIPv6AddressLocked(%d): %v", count, err)
	}
	return got
}

func contains(ips []net.IP, want string) bool {
	target := net.ParseIP(want)
	for _, ip := range ips {
		if ip.Equal(target) {
			return true
		}
	}
	return false
}

// TestCarveExcludesTheInterfacesOwnPrimary is the baseline the disputed-primary
// case builds on: wherever the primary sits, the carve steps over it.
//
// The primary sits at ::5, deliberately not ::2: firstPodOffsetV6 already skips
// ::0 through ::2, so a primary parked there would be excluded even if the code
// ignored PrimaryIP.IPv6 entirely, and the test would pass for the wrong reason.
// The primary is not pinned to that range, so an implementation leaning on the
// offset would be wrong in exactly this case.
func TestCarveExcludesTheInterfacesOwnPrimary(t *testing.T) {
	e := eniWithPrefix(t, "2001:db8::/120", "2001:db8::5")

	got := carve(t, e, 6)

	if contains(got, "2001:db8::5") {
		t.Errorf("carve returned the interface's own primary 2001:db8::5: %v", got)
	}
	// The convention offsets stay skipped too.
	for _, skipped := range []string{"2001:db8::", "2001:db8::1", "2001:db8::2"} {
		if contains(got, skipped) {
			t.Errorf("carve returned %s, which is below firstPodOffsetV6: %v", skipped, got)
		}
	}
	if len(got) != 6 {
		t.Errorf("carve returned %d addresses, want 6: %v", len(got), got)
	}
}

// TestCarveExcludesBothPrimariesWhenSourcesDisagree pins the two-source rule.
//
// The platform publishes an interface's primary IPv6 twice -- in the describe
// reply and in instance metadata -- and GetENIAllocatableIPs records the second
// answer in PrimaryIPv6Disputed when the two differ. Both have to be excluded:
// the address exists to be configured on the interface by DHCPv6, so handing out
// whichever one is really there black-holes that pod, and nothing detects it
// afterwards because the platform publishes no per-address IPv6 view.
func TestCarveExcludesBothPrimariesWhenSourcesDisagree(t *testing.T) {
	e := eniWithPrefix(t, "2001:db8::/120", "2001:db8::5")
	e.PrimaryIPv6Disputed = net.ParseIP("2001:db8::9")

	got := carve(t, e, 8)

	for _, primary := range []string{"2001:db8::5", "2001:db8::9"} {
		if contains(got, primary) {
			t.Errorf("carve returned %s; both primary candidates must be withheld while the sources disagree: %v",
				primary, got)
		}
	}
	if len(got) != 8 {
		t.Errorf("carve returned %d addresses, want 8: %v", len(got), got)
	}
}

// TestCarveIgnoresDisputedPrimaryWhenUnset guards the normal path: with the two
// sources in agreement the field is nil and nothing extra is withheld.
func TestCarveIgnoresDisputedPrimaryWhenUnset(t *testing.T) {
	e := eniWithPrefix(t, "2001:db8::/120", "2001:db8::5")

	got := carve(t, e, 8)

	// ::9 is only special when it is the disputed answer; here it must be
	// handed out like any other address, or the exclusion is unconditional.
	if !contains(got, "2001:db8::9") {
		t.Errorf("carve withheld 2001:db8::9 with no dispute recorded: %v", got)
	}
}
