// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package ipamonly

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	k8sErr "k8s.io/apimachinery/pkg/util/errors"

	linuxUtils "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/utils/linux"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// The two priorities are the same ones the full datapath uses, and they are not
// free to change: the daemon's reclaimStaleRules sweeps exactly these two when a
// plugin invocation dies between ADD and DEL, and it matches on priority plus
// pod address. Inventing a third priority here would silently opt this path out
// of the only cleanup that covers that crash window.
const (
	toContainerPriority   = 512
	fromContainerPriority = 2048
)

var (
	defaultRouteV4 = &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
	defaultRouteV6 = &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}
)

// hostRouting is what one pod needs on the host so its traffic leaves through
// the interface its address belongs to.
type hostRouting struct {
	// eniIndex is the ifindex of the ENI holding the pod address. The route
	// table number derives from it.
	eniIndex int
	// podIPs are the pod's addresses, already narrowed to host masks.
	podIPs []*net.IPNet
	// gateway is that ENI's gateway, per family.
	gateway *zecTypes.IPSet
	// primaryIP is the ENI's own address, the one the platform bound to this
	// port's MAC. Placed on the interface as a host route so the host has a
	// source the fabric accepts when it sends from this port.
	primaryIP *zecTypes.IPSet
	// isPrimary marks the instance's own interface, whose addresses belong to
	// whatever configures the guest and must never be rewritten here.
	isPrimary bool
}

// setupHostRouting places this pod's ip rules and the ENI's default route.
//
// Three pieces, and deliberately not a fourth:
//
//   - `from <podIP> lookup <eni table>` at prio 2048, so traffic the pod sources
//     resolves in its own ENI's table. Without it a pod on a secondary ENI
//     leaves through the instance's own interface, carrying a source address
//     that belongs to another port's MAC -- which a fabric checking source
//     addresses drops.
//   - `to <podIP> lookup main` at prio 512, so traffic aimed at a local pod
//     reaches main before the rule above can divert it. Same-node pod to pod is
//     the case that needs it: the source is the client pod, so prio 2048 would
//     otherwise send a packet destined for a neighbour out of the ENI's default
//     route.
//   - a default route in the ENI's own table, which is what prio 2048 selects.
//
// The fourth piece, a link-scope route to the pod in the main table, is the
// caller's: this path runs under a plugin that creates the pod device itself and
// installs a route to each endpoint. Installing our own would leave two routes
// to one address pointing at whatever device each of us happened to pick.
func setupHostRouting(logger zlog.Logger, hr *hostRouting) error {
	link, err := netlink.LinkByIndex(hr.eniIndex)
	if err != nil {
		return fmt.Errorf("look up eni by index %d: %w", hr.eniIndex, err)
	}

	// A freshly attached interface is not up yet, and the kernel refuses a
	// route through a down link with ENETDOWN. The daemon attaches an interface
	// the moment the pool needs one, so this is not an edge case: it is the
	// first pod placed on every new interface.
	if _, err = linuxUtils.EnsureLinkUp(link); err != nil {
		return fmt.Errorf("bring eni %s up: %w", link.Attrs().Name, err)
	}

	table := linuxUtils.GetRouteTableID(hr.eniIndex)

	if err = ensureENIAddrs(logger, hr, link, table); err != nil {
		return err
	}

	for _, route := range eniTableRoutes(hr, link.Attrs().Index, table) {
		if _, err = linuxUtils.EnsureRoute(route); err != nil {
			return fmt.Errorf("ensure route %s in table %d: %w", route.String(), table, err)
		}
	}

	for _, rule := range podRules(hr.podIPs, table) {
		if _, err = linuxUtils.EnsureIPRule(rule); err != nil {
			return fmt.Errorf("ensure ip rule %s: %w", rule.String(), err)
		}
	}

	logger.Infof("ipam-only host routing ready: eni %s table %d, %d pod address(es)",
		link.Attrs().Name, table, len(hr.podIPs))
	return nil
}

// teardownHostRouting removes the rules this pod's ADD installed.
//
// The ENI's table routes are left alone: they belong to the interface, not to
// the pod, and every other pod on that ENI still selects them.
//
// Best effort throughout, matching CNI DEL semantics -- returning on the first
// failure would strand the rest, and ip rules are keyed on the pod address
// rather than on a link, so nothing removes them implicitly.
func teardownHostRouting(logger zlog.Logger, podIPs []*net.IPNet) error {
	var errList []error

	del := func(rule *netlink.Rule) {
		found, err := linuxUtils.FindIPRule(rule)
		if err != nil {
			errList = append(errList, fmt.Errorf("find ip rule %s: %w", rule.String(), err))
			return
		}
		for i := range found {
			if err = linuxUtils.RuleDel(&found[i]); err != nil {
				errList = append(errList, fmt.Errorf("del ip rule %s: %w", found[i].String(), err))
			}
		}
	}

	for _, podIP := range podIPs {
		if podIP == nil {
			continue
		}
		fromRule := netlink.NewRule()
		fromRule.Priority = fromContainerPriority
		fromRule.Src = podIP
		del(fromRule)

		toRule := netlink.NewRule()
		toRule.Priority = toContainerPriority
		toRule.Dst = podIP
		del(toRule)
	}

	if len(errList) > 0 {
		return k8sErr.NewAggregate(errList)
	}
	logger.Infof("ipam-only host routing removed for %d pod address(es)", len(podIPs))
	return nil
}

// podRules builds the two rules per pod address. Separated from the netlink
// calls so the rule set can be asserted on without a host to write to.
func podRules(podIPs []*net.IPNet, table int) []*netlink.Rule {
	rules := make([]*netlink.Rule, 0, 2*len(podIPs))
	for _, podIP := range podIPs {
		if podIP == nil {
			continue
		}
		toContainer := netlink.NewRule()
		toContainer.Dst = podIP
		toContainer.Table = unix.RT_TABLE_MAIN
		toContainer.Priority = toContainerPriority

		fromContainer := netlink.NewRule()
		fromContainer.Src = podIP
		fromContainer.Table = table
		fromContainer.Priority = fromContainerPriority

		rules = append(rules, toContainer, fromContainer)
	}
	return rules
}

// eniTableRoutes builds the default route -- one per family the pod uses -- in
// the ENI's own table.
//
// FLAG_ONLINK because the gateway need not be reachable by an on-link prefix on
// this interface: the ENI carries only a host route for its own primary address
// (see eniAddrs in pkg/plugin/datapath), so without ONLINK the kernel refuses
// the route as unreachable.
//
// The IPv6 half also gets a host route to the gateway inside the same table.
// Redundant given ONLINK, but it has to be in this table rather than main:
// every ENI shares one link-local gateway address, so a main-table entry is
// last-writer-wins and outlives the interface it points at.
func eniTableRoutes(hr *hostRouting, linkIndex, table int) []*netlink.Route {
	var routes []*netlink.Route
	v4, v6 := splitFamilies(hr.podIPs)

	if v4 != nil && hr.gateway.IPv4 != nil {
		routes = append(routes, &netlink.Route{
			LinkIndex: linkIndex,
			Scope:     netlink.SCOPE_UNIVERSE,
			Table:     table,
			Dst:       defaultRouteV4,
			Gw:        hr.gateway.IPv4,
			Flags:     int(netlink.FLAG_ONLINK),
		})
	}
	if v6 != nil && hr.gateway.IPv6 != nil {
		routes = append(routes,
			&netlink.Route{
				LinkIndex: linkIndex,
				Table:     table,
				Dst:       &net.IPNet{IP: hr.gateway.IPv6, Mask: net.CIDRMask(128, 128)},
			},
			&netlink.Route{
				LinkIndex: linkIndex,
				Scope:     netlink.SCOPE_UNIVERSE,
				Table:     table,
				Dst:       defaultRouteV6,
				Gw:        hr.gateway.IPv6,
				Flags:     int(netlink.FLAG_ONLINK),
			},
		)
	}
	return routes
}

// splitFamilies returns the first address of each family, or nil for a family
// the pod does not use.
func splitFamilies(ips []*net.IPNet) (v4, v6 *net.IPNet) {
	for _, ip := range ips {
		if ip == nil {
			continue
		}
		if ip.IP.To4() != nil {
			if v4 == nil {
				v4 = ip
			}
			continue
		}
		if v6 == nil {
			v6 = ip
		}
	}
	return v4, v6
}

// ensureENIAddrs gives a CNI-created interface its own address, and the rule
// that keeps the host's own traffic from that address on this interface.
//
// Without the address the interface is bare: the host has no source to put in
// an ARP request for the gateway, and the route installed above cannot be used.
// Without the rule the address is worse than useless -- the host can pick it as
// a source and then resolve in the main table, sending a packet out of the
// instance's own port with a source belonging to this one, which a fabric that
// checks source addresses drops.
//
// A host route rather than the subnet mask: the mask would add a second on-link
// route for the subnet in the main table, competing with the instance's own
// interface. The default route in this interface's table is onlink, so a host
// route is enough.
//
// The instance's own interface is skipped outright. Its addresses are owned by
// whatever configures the guest, and every address this would propose for it is
// already there; writing them again risks taking the node off the network over
// a disagreement between the platform's view and what the guest configured.
func ensureENIAddrs(logger zlog.Logger, hr *hostRouting, link netlink.Link, table int) error {
	if hr.isPrimary || hr.primaryIP == nil {
		return nil
	}
	name := link.Attrs().Name
	v4, v6 := splitFamilies(hr.podIPs)

	// Only for a family this pod actually uses: an address of a family no pod on
	// the interface has attracts traffic nothing can answer.
	var wanted []*net.IPNet
	if v4 != nil && hr.primaryIP.IPv4 != nil {
		if hr.primaryIP.IPv4.To4() == nil {
			logger.Errorf("eni %s reports %s as its primary IPv4, which is not one; skipping",
				name, hr.primaryIP.IPv4)
		} else {
			wanted = append(wanted, &net.IPNet{IP: hr.primaryIP.IPv4, Mask: net.CIDRMask(32, 32)})
		}
	}
	if v6 != nil && hr.primaryIP.IPv6 != nil {
		if hr.primaryIP.IPv6.To4() != nil {
			logger.Errorf("eni %s reports %s as its primary IPv6, which is an IPv4 address; skipping",
				name, hr.primaryIP.IPv6)
		} else {
			wanted = append(wanted, &net.IPNet{IP: hr.primaryIP.IPv6, Mask: net.CIDRMask(128, 128)})
		}
	}

	for _, ipNet := range wanted {
		if _, err := linuxUtils.EnsureAddr(link, &netlink.Addr{IPNet: ipNet}); err != nil {
			return fmt.Errorf("ensure address %s on eni %s: %w", ipNet.String(), name, err)
		}
		// Derived from what was wanted rather than from what EnsureAddr changed:
		// on a retry the address is already held and nothing would be proposed,
		// leaving the interface without its rule for good. EnsureIPRule is
		// idempotent, so writing it every time is free.
		rule := netlink.NewRule()
		rule.Src = linuxUtils.NewIPNetWithMaxMask(ipNet)
		rule.Table = table
		rule.Priority = fromContainerPriority
		if _, err := linuxUtils.EnsureIPRule(rule); err != nil {
			return fmt.Errorf("ensure eni source rule %s: %w", rule.String(), err)
		}
	}

	// RA policy is ours on an interface the CNI created: nothing else in the
	// guest owns it, and an accepted RA would install a competing default route.
	if v6 != nil {
		if err := linuxUtils.EnsureLinkNetConf(name); err != nil {
			return fmt.Errorf("apply net config to eni %s: %w", name, err)
		}
	}
	return nil
}
