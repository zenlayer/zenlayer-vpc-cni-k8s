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

// fakeLink stands in for the ENI so the config builders can be exercised
// without a real interface. Only Index and Name are read: the index lands in
// every route's LinkIndex, and the name feeds logging and the sysctl paths. The
// route table id is passed in rather than derived here -- the 1000+ifindex
// derivation lives in the production callers.
type fakeLink struct{ attrs netlink.LinkAttrs }

func (f *fakeLink) Attrs() *netlink.LinkAttrs { return &f.attrs }
func (f *fakeLink) Type() string              { return "dummy" }

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	n.IP = ip
	return n
}

// TestENIRoutesAllLandInTheInterfacesOwnTable is the regression for a gateway
// route that was written without a table id.
//
// netlink treats table 0 as "main", so that route went to the table the whole
// host resolves through instead of the one belonging to this interface. The
// IPv6 gateway is link-local, so it is the same address on every port, with
// only the device differing. Every ENI on the node then overwrote the same
// main-table entry: whichever ADD ran last won, and the entry outlived the
// interface it pointed at.
//
// The assertion covers every route rather than the one that was wrong, so a
// route added later inherits the guard.
func TestENIRoutesAllLandInTheInterfacesOwnTable(t *testing.T) {
	const ifIndex = 42
	wantTable := 1000 + ifIndex

	link := &fakeLink{attrs: netlink.LinkAttrs{Index: ifIndex, Name: "eth2"}}
	cfg := &types.SetupConfig{
		ContainerIPNet: &zecTypes.IPNetSet{
			IPv4: mustCIDR(t, "10.0.0.31/32"),
			IPv6: mustCIDR(t, "2001:db8::3/128"),
		},
		GatewayIP: &zecTypes.IPSet{
			IPv4: net.ParseIP("10.0.0.1"),
			IPv6: net.ParseIP("fe80::1"),
		},
	}

	conf := generateENICfgForPolicy(zlog.GetLogger(), cfg, link, wantTable)

	if len(conf.Routes) == 0 {
		t.Fatal("generateENICfgForPolicy produced no routes")
	}
	for _, r := range conf.Routes {
		if r.Table != wantTable {
			t.Errorf("route %v landed in table %d, want %d -- table 0 means main, "+
				"where every interface competes for the same entry", r.Dst, r.Table, wantTable)
		}
		if r.LinkIndex != ifIndex {
			t.Errorf("route %v has LinkIndex %d, want %d", r.Dst, r.LinkIndex, ifIndex)
		}
	}
}

// TestENIRoutesAreFamilyGated pins that a single-family pod gets only its own
// family's routes. Without the gate on ContainerIPNet, an IPv4-only pod on a
// dual-configured node would still push IPv6 routes onto the interface.
func TestENIRoutesAreFamilyGated(t *testing.T) {
	link := &fakeLink{attrs: netlink.LinkAttrs{Index: 7, Name: "eth1"}}

	v4Only := &types.SetupConfig{
		ContainerIPNet: &zecTypes.IPNetSet{IPv4: mustCIDR(t, "10.0.0.31/32")},
		GatewayIP: &zecTypes.IPSet{
			IPv4: net.ParseIP("10.0.0.1"),
			// Present but unused: the pod has no IPv6, so nothing may key off it.
			IPv6: net.ParseIP("fe80::1"),
		},
	}

	for _, r := range generateENICfgForPolicy(zlog.GetLogger(), v4Only, link, 1007).Routes {
		if r.Dst != nil && r.Dst.IP.To4() == nil {
			t.Errorf("an IPv4-only pod produced the IPv6 route %v", r.Dst)
		}
	}
}
