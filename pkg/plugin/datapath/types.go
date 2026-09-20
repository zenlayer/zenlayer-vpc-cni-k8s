// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package datapath

import (
	"net"
)

// The two ip rule priorities this datapath installs for every pod. On the host
// they carry the names they are given here: "to <podIP> lookup main" at 512, and
// "from <podIP> lookup <per-interface table>" at 2048.
//
// The order between them is what makes same-node pod-to-pod traffic work. A
// packet leaving pod A for pod B matches both rules, and the to-rule has to win:
// following it sends the packet through main, out of B's veth, and never past
// the interface. Reverse the two and the packet is handed to the per-interface
// table instead and leaves the node to come back, if it comes back at all. The
// kernel's own local table sits at priority 0 and still precedes both, which is
// what keeps traffic addressed to the host itself out of these rules.
//
// Inside the container's namespace both of the rules generated for it use 512.
// The 512/2048 split describes the host's rule table, where the two must be
// ordered against each other; in the container they are simply two bands and
// nothing else competes with them. See generateContCfgForPolicy.
//
// pkg/daemon carries its own copy of these numbers, because the daemon has to
// clean up after a plugin process that no longer exists and these are
// unexported. Changing a value here means changing it there.
const (
	toContainerPriority   = 512
	fromContainerPriority = 2048
)

// LinkIP and LinkIPv6 are the addresses the container uses as its next hop.
//
// They are link-local placeholders that exist only inside the pod's namespace:
// the datapath does not put them on any interface, it installs a permanent
// neighbor entry for each so that the pod never sends an ARP or NDP request for
// its gateway and nothing has to answer one.
//
// LinkIPv6 is deliberately not distinguished from the real thing by its value:
// the platform's metadata reports fe80::1 as the actual IPv6 gateway of a
// network interface as well. The two never meet -- one lives in the pod's
// namespace, the other on the host's interface -- but a reader comparing a route
// in the pod against one on the host will see the same literal and must not
// conclude they are the same address.
var (
	_, defaultRoute, _     = net.ParseCIDR("0.0.0.0/0")
	_, defaultRouteIPv6, _ = net.ParseCIDR("::/0")
	LinkIP                 = net.IPv4(169, 254, 1, 1)
	LinkIPv6               = net.ParseIP("fe80::1")
	LinkIPNet              = &net.IPNet{
		IP:   LinkIP,
		Mask: net.CIDRMask(32, 32),
	}
	LinkIPNetv6 = &net.IPNet{
		IP:   LinkIPv6,
		Mask: net.CIDRMask(128, 128),
	}
)
