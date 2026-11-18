// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package ipamonly

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"

	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

func hostIP(t *testing.T, s string) *net.IPNet {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("bad test address %q", s)
	}
	bits := 32
	if ip.To4() == nil {
		bits = 128
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
}

// TestPodRulesDirectionAndPriority pins which table each direction selects.
//
// Getting the pair backwards is silent: the pod still reaches off-node
// destinations, and only same-node pod-to-pod breaks, because a packet aimed at
// a neighbor would be matched by the source rule and sent out of the ENI's
// default route instead of staying on the node.
func TestPodRulesDirectionAndPriority(t *testing.T) {
	const table = 1196
	rules := podRules([]*net.IPNet{hostIP(t, "10.0.0.12")}, table)

	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2 (one per direction)", len(rules))
	}

	to, from := rules[0], rules[1]

	if to.Priority != toContainerPriority {
		t.Errorf("to-container priority = %d, want %d", to.Priority, toContainerPriority)
	}
	if to.Table != unix.RT_TABLE_MAIN {
		t.Errorf("to-container table = %d, want main (%d) -- traffic aimed at a local pod must "+
			"resolve in main before the source rule can divert it", to.Table, unix.RT_TABLE_MAIN)
	}
	if to.Dst == nil || to.Src != nil {
		t.Errorf("the to-container rule must match on destination only, got src=%v dst=%v", to.Src, to.Dst)
	}

	if from.Priority != fromContainerPriority {
		t.Errorf("from-container priority = %d, want %d", from.Priority, fromContainerPriority)
	}
	if from.Table != table {
		t.Errorf("from-container table = %d, want the eni table %d", from.Table, table)
	}
	if from.Src == nil || from.Dst != nil {
		t.Errorf("the from-container rule must match on source only, got src=%v dst=%v", from.Src, from.Dst)
	}

	if to.Priority >= from.Priority {
		t.Errorf("the to-container rule must be evaluated first: %d is not below %d",
			to.Priority, from.Priority)
	}
}

// TestPodRulesUseTheSweptPriorities keeps the two priorities equal to the ones
// the daemon reclaims.
//
// reclaimStaleRules matches on priority plus pod address and is the only thing
// that cleans up when a plugin invocation dies between ADD and DEL. Changing
// either number here opts this path out of that recovery without any other
// symptom.
func TestPodRulesUseTheSweptPriorities(t *testing.T) {
	if toContainerPriority != 512 {
		t.Errorf("toContainerPriority = %d, want 512", toContainerPriority)
	}
	if fromContainerPriority != 2048 {
		t.Errorf("fromContainerPriority = %d, want 2048", fromContainerPriority)
	}
}

// TestPodRulesDualStack pins that both families get both directions, and that
// no rule pairs one family's address with the other's.
func TestPodRulesDualStack(t *testing.T) {
	v4, v6 := hostIP(t, "10.0.0.12"), hostIP(t, "2001:db8::c")
	rules := podRules([]*net.IPNet{v4, v6}, 1196)

	if len(rules) != 4 {
		t.Fatalf("got %d rules, want 4 (two directions for each of two families)", len(rules))
	}
	for _, r := range rules {
		addr := r.Src
		if addr == nil {
			addr = r.Dst
		}
		if addr == nil {
			t.Fatalf("rule %s matches on neither source nor destination", r.String())
		}
		ones, bits := addr.Mask.Size()
		if ones != bits {
			t.Errorf("rule %s uses a prefix, not a host address: a wider mask would capture "+
				"other pods on the same subnet", r.String())
		}
	}
}

// TestPodRulesSkipNilAddresses keeps a single-stack pod from producing a rule
// with a nil address, which would match everything rather than nothing.
func TestPodRulesSkipNilAddresses(t *testing.T) {
	rules := podRules([]*net.IPNet{hostIP(t, "10.0.0.12"), nil}, 1196)
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2: a nil address must contribute none", len(rules))
	}
}

// TestENITableRoutesAreOnlinkAndScoped pins the two properties without which
// the default route is either refused by the kernel or written to the wrong
// table.
func TestENITableRoutesAreOnlinkAndScoped(t *testing.T) {
	const (
		linkIndex = 196
		table     = 1196
	)
	hr := &hostRouting{
		eniIndex: linkIndex,
		podIPs:   []*net.IPNet{hostIP(t, "10.0.0.12")},
		gateway:  &zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.1")},
	}
	routes := eniTableRoutes(hr, linkIndex, table)

	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1 for a v4-only pod", len(routes))
	}
	r := routes[0]
	if r.Table != table {
		t.Errorf("route table = %d, want %d -- a default route in main would compete with the "+
			"node's own", r.Table, table)
	}
	if r.Flags&int(netlink.FLAG_ONLINK) == 0 {
		t.Error("the default route must be onlink: the interface carries only a host route for " +
			"its own address, so the kernel would otherwise refuse the gateway as unreachable")
	}
	if r.LinkIndex != linkIndex {
		t.Errorf("route link index = %d, want %d", r.LinkIndex, linkIndex)
	}
}

// TestENITableRoutesIPv6GatewayStaysInTheENITable pins the extra host route for
// the v6 gateway, and that it is not written to main.
//
// Every ENI shares one link-local gateway address. A main-table entry for it is
// last-writer-wins and keeps pointing at an interface that may since have been
// detached, so it has to live in the table that describes that interface.
func TestENITableRoutesIPv6GatewayStaysInTheENITable(t *testing.T) {
	const table = 1196
	hr := &hostRouting{
		eniIndex: 196,
		podIPs:   []*net.IPNet{hostIP(t, "2001:db8::c")},
		gateway:  &zecTypes.IPSet{IPv6: net.ParseIP("fe80::1")},
	}
	routes := eniTableRoutes(hr, 196, table)

	if len(routes) != 2 {
		t.Fatalf("got %d routes, want 2 (the gateway host route and the default)", len(routes))
	}
	for _, r := range routes {
		if r.Table != table {
			t.Errorf("route %s landed in table %d, want %d", r.String(), r.Table, table)
		}
	}
}

// TestENITableRoutesSkipFamiliesThePodDoesNotUse keeps a v4-only pod from
// installing a v6 default route on an interface, which would attract traffic
// this pod has no address for.
func TestENITableRoutesSkipFamiliesThePodDoesNotUse(t *testing.T) {
	hr := &hostRouting{
		eniIndex: 196,
		podIPs:   []*net.IPNet{hostIP(t, "10.0.0.12")},
		gateway: &zecTypes.IPSet{
			IPv4: net.ParseIP("10.0.0.1"),
			IPv6: net.ParseIP("fe80::1"),
		},
	}
	if got := len(eniTableRoutes(hr, 196, 1196)); got != 1 {
		t.Errorf("got %d routes for a v4-only pod on a dual-stack eni, want 1", got)
	}
}

// TestSplitFamiliesClassifiesByAddress pins that classification follows the
// address, not the slice position: a v6 read out of a v4 slot would otherwise
// be handed a /32 mask and match a completely different range.
func TestSplitFamiliesClassifiesByAddress(t *testing.T) {
	v6, v4 := hostIP(t, "2001:db8::c"), hostIP(t, "10.0.0.12")
	gotV4, gotV6 := splitFamilies([]*net.IPNet{v6, v4})

	if gotV4 == nil || !gotV4.IP.Equal(v4.IP) {
		t.Errorf("v4 = %v, want %v", gotV4, v4)
	}
	if gotV6 == nil || !gotV6.IP.Equal(v6.IP) {
		t.Errorf("v6 = %v, want %v", gotV6, v6)
	}
}

// TestENIAddrsSkipTheInstanceOwnInterface pins the guard that keeps this path
// away from the node's own addresses.
//
// EnsureAddr replaces any address of the family that does not match exactly,
// mask included. One disagreement between what the platform reports and what
// configured the guest would therefore take the node off the network, with
// kubelet and the daemon on it.
func TestENIAddrsSkipTheInstanceOwnInterface(t *testing.T) {
	hr := &hostRouting{
		eniIndex:  2,
		podIPs:    []*net.IPNet{hostIP(t, "10.0.0.12")},
		gateway:   &zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.1")},
		primaryIP: &zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.2")},
		isPrimary: true,
	}
	// A nil link would panic if the guard did not return first, which is the
	// assertion: nothing downstream may run for the instance's own interface.
	if err := ensureENIAddrs(zlog.New(""), hr, nil, 1002); err != nil {
		t.Errorf("ensureENIAddrs on the primary interface returned %v, want nil and no work", err)
	}
}

// TestENIAddrsSkipWhenPlatformReportsNothing pins that a missing primary
// address is left alone rather than guessed at. A bare interface is the
// recoverable failure; an invented address is not.
func TestENIAddrsSkipWhenPlatformReportsNothing(t *testing.T) {
	hr := &hostRouting{
		eniIndex: 4,
		podIPs:   []*net.IPNet{hostIP(t, "10.0.0.12")},
		gateway:  &zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.1")},
	}
	if err := ensureENIAddrs(zlog.New(""), hr, nil, 1004); err != nil {
		t.Errorf("ensureENIAddrs with no primary ip returned %v, want nil and no work", err)
	}
}
