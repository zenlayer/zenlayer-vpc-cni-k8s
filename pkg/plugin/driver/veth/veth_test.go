// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package veth

import "testing"

// TestHostMACFor covers the properties the host end's address has to hold.
//
// It exists because the address is not cosmetic: the datapath pins it inside
// the pod as the next hop for the default route, so a value that is unstable,
// or that the kernel or udev may replace, silently costs the pod all egress.
func TestHostMACFor(t *testing.T) {
	const name = "eniefcc9486e64"
	mac := HostMACFor(name)

	if len(mac) != 6 {
		t.Fatalf("length %d, want 6: %s", len(mac), mac)
	}

	// Locally administered, so it can never collide with a vendor address.
	if mac[0]&0x02 == 0 {
		t.Errorf("%s is not locally administered", mac)
	}
	// Unicast: a multicast source address is invalid and would be dropped.
	if mac[0]&0x01 != 0 {
		t.Errorf("%s is multicast", mac)
	}

	// Deterministic: an ADD retried after a failure recreates the pair under
	// the same name and must arrive at the same address.
	if again := HostMACFor(name); again.String() != mac.String() {
		t.Errorf("not deterministic: %s then %s", mac, again)
	}

	// Distinct per interface, so two pods on one node do not share an address.
	if other := HostMACFor("eni9667fc8d00c"); other.String() == mac.String() {
		t.Errorf("collision between two interface names: both %s", mac)
	}
}
