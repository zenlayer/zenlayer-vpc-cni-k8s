// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package datapath

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/plugin/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// addrCfg builds the only two SetupConfig fields eniPrimaryAddrs reads: the
// interface's reported primary, and the families the pod itself uses.
func addrCfg(primaryV4, primaryV6 net.IP, podV4, podV6 bool) *types.SetupConfig {
	container := &zecTypes.IPNetSet{}
	if podV4 {
		container.IPv4 = &net.IPNet{IP: net.ParseIP("10.0.0.5"), Mask: net.CIDRMask(32, 32)}
	}
	if podV6 {
		container.IPv6 = &net.IPNet{IP: net.ParseIP("2001:db8::5"), Mask: net.CIDRMask(128, 128)}
	}
	return &types.SetupConfig{
		ContainerIPNet: container,
		ENIPrimaryIP:   &zecTypes.IPSet{IPv4: primaryV4, IPv6: primaryV6},
	}
}

// families reports how many of the returned addresses are v4 and how many v6,
// judged by the address itself rather than by the slot it arrived in.
func families(addrs []*netlink.Addr) (v4, v6 int) {
	for _, a := range addrs {
		if a.IP.To4() != nil {
			v4++
		} else {
			v6++
		}
	}
	return v4, v6
}

// TestENIPrimaryAddrsRejectsMisfiledFamilies pins the family check in
// eniPrimaryAddrs. A misfiled address arrives intact -- ToIPSet copies the
// reply's two strings into same-named slots without looking at them -- and does
// not stay harmless: NewIPNetWithMaxMask re-derives the mask from the address,
// so a v6 in the v4 slot becomes a /128 and both entries land in one family.
// EnsureAddr replaces within a family, so the second deletes the first, leaving
// the host interface with one address instead of two and no error reported.
//
// Each case therefore asserts on the families of the result, not just its
// length: a count alone would be satisfied by exactly that failure.
func TestENIPrimaryAddrsRejectsMisfiledFamilies(t *testing.T) {
	var (
		goodV4 = net.ParseIP("10.0.0.9")
		goodV6 = net.ParseIP("2001:db8::9")
	)
	for _, tc := range []struct {
		name         string
		cfg          *types.SetupConfig
		wantV4       int
		wantV6       int
		wantAddrs    []string
		wantExcluded net.IP
	}{
		{
			// Without the family check both entries come back v6.
			name:      "IPv6 in the IPv4 slot",
			cfg:       addrCfg(net.ParseIP("2001:db8::bad"), goodV6, true, true),
			wantV4:    0,
			wantV6:    1,
			wantAddrs: []string{"2001:db8::9/128"},
		},
		{
			name:      "IPv4 in the IPv6 slot",
			cfg:       addrCfg(goodV4, net.ParseIP("10.0.0.250"), true, true),
			wantV4:    1,
			wantV6:    0,
			wantAddrs: []string{"10.0.0.9/32"},
		},
		{
			name:      "both slots misfiled",
			cfg:       addrCfg(net.ParseIP("2001:db8::bad"), net.ParseIP("10.0.0.250"), true, true),
			wantV4:    0,
			wantV6:    0,
			wantAddrs: nil,
		},
		{
			// Not a misfiling: the interface has both, the pod uses one. The
			// IPv6 must not be pushed onto the interface for an IPv4-only pod.
			name:      "IPv4-only pod on a dual-configured node",
			cfg:       addrCfg(goodV4, goodV6, true, false),
			wantV4:    1,
			wantV6:    0,
			wantAddrs: []string{"10.0.0.9/32"},
		},
		{
			name:      "IPv6-only pod",
			cfg:       addrCfg(goodV4, goodV6, false, true),
			wantV4:    0,
			wantV6:    1,
			wantAddrs: []string{"2001:db8::9/128"},
		},
		{
			// Asserts the masks: a host route per family, not the subnet
			// mask. The subnet mask would add a second on-link route for the
			// subnet, competing with the instance's own interface; see
			// eniAddrs.
			name:      "both families correct",
			cfg:       addrCfg(goodV4, goodV6, true, true),
			wantV4:    1,
			wantV6:    1,
			wantAddrs: []string{"10.0.0.9/32", "2001:db8::9/128"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := eniPrimaryAddrs(zlog.GetLogger(), tc.cfg, "eth1")

			v4, v6 := families(got)
			if v4 != tc.wantV4 || v6 != tc.wantV6 {
				t.Errorf("got %d IPv4 and %d IPv6 addresses, want %d and %d: %v",
					v4, v6, tc.wantV4, tc.wantV6, got)
			}
			if v4 > 1 || v6 > 1 {
				t.Errorf("two addresses of the same family were emitted (%v); EnsureAddr "+
					"replaces within a family, so the second would delete the first with "+
					"no error reported", got)
			}
			if len(got) != len(tc.wantAddrs) {
				t.Fatalf("got %d addresses, want %d: %v", len(got), len(tc.wantAddrs), got)
			}
			for i, want := range tc.wantAddrs {
				if got[i].IPNet.String() != want {
					t.Errorf("address %d = %s, want %s", i, got[i].IPNet, want)
				}
			}
		})
	}
}

// TestENIPrimaryAddrsHandlesAMissingPrimary covers the slots being empty rather
// than wrong. eniAddrs returns early when neither family is set, but each slot
// can also be empty on its own, and an empty slot must not produce an entry --
// NewIPNetWithMaxMask would be handed a nil address.
func TestENIPrimaryAddrsHandlesAMissingPrimary(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *types.SetupConfig
		want int
	}{
		{"no IPv4 primary on a dual pod", addrCfg(nil, net.ParseIP("2001:db8::9"), true, true), 1},
		{"no IPv6 primary on a dual pod", addrCfg(net.ParseIP("10.0.0.9"), nil, true, true), 1},
		{"neither", addrCfg(nil, nil, true, true), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eniPrimaryAddrs(zlog.GetLogger(), tc.cfg, "eth1"); len(got) != tc.want {
				t.Errorf("got %d addresses, want %d: %v", len(got), tc.want, got)
			}
		})
	}
}
