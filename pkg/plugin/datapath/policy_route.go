// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package datapath wires a pod's network namespace to a network interface.
//
// The implementation here is source-based policy routing: each pod gets a veth
// pair, and traffic is split by source address into a routing table per
// interface.
//
// SetupDataPath and TeardownDataPath are what CNI ADD and DEL call. Two more
// entry points exist for the cases where no pod is on the other end:
// GenericTearDown unwires from inside a namespace that is still present, and
// CleanIPRules is what the daemon calls to collect rules left behind by
// interfaces that are already gone.
//
// Teardown is best-effort by design, running every step before reporting the
// errors it collected, because a step that fails must not strand the ones
// after it.
package datapath

import (
	"fmt"
	"net"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	k8sErr "k8s.io/apimachinery/pkg/util/errors"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/plugin/driver/nic"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/plugin/driver/veth"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/plugin/types"
	linuxUtils "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/utils/linux"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// PolicyRouteDriver wires a pod to its ENI with a veth pair plus policy
// routing. It is the only datapath.
//
// Traffic is steered per source address: every ENI owns a route table
// (GetRouteTableID = 1000 + ifindex) holding that ENI's default route, and a
// `from <pod-ip> lookup <eni-table>` rule sends the pod's egress into it. This
// is what allows several ENIs to coexist on one node.
//
// There is no tc redirect for the service CIDR, and none is needed: the veth
// reaches the host stack directly, so kube-proxy (or an eBPF replacement) sees
// both directions of the traffic without help.
type PolicyRouteDriver struct{}

func NewPolicyRouteDriver() *PolicyRouteDriver {
	return &PolicyRouteDriver{}
}

func (d *PolicyRouteDriver) Name() string {
	return "policyRoute"
}

// validateSetupConfig rejects the configs the generate* helpers cannot build
// from: the nil fields they would dereference, and a container with no address
// of either family.
func validateSetupConfig(cfg *types.SetupConfig) error {
	if cfg.Netns == nil {
		return fmt.Errorf("empty netns")
	}
	if cfg.ContainerIPNet == nil {
		return fmt.Errorf("empty container IPNet")
	}
	if cfg.GatewayIP == nil {
		return fmt.Errorf("empty gateway IP")
	}
	if cfg.ContainerIPNet.IPv4 != nil && cfg.GatewayIP.IPv4 == nil {
		return fmt.Errorf("container has an IPv4 address but no IPv4 gateway")
	}
	if cfg.ContainerIPNet.IPv6 != nil && cfg.GatewayIP.IPv6 == nil {
		return fmt.Errorf("container has an IPv6 address but no IPv6 gateway")
	}
	if cfg.ContainerIPNet.IPv4 == nil && cfg.ContainerIPNet.IPv6 == nil {
		return fmt.Errorf("container has no address at all")
	}
	return nil
}

func (d *PolicyRouteDriver) SetupNetwork(logger zlog.Logger, cfg *types.SetupConfig) (err error) {
	if err = validateSetupConfig(cfg); err != nil {
		return err
	}

	eni, err := netlink.LinkByIndex(cfg.ENIIndex)
	if err != nil {
		return fmt.Errorf("could not found parent device [index %d], %w", cfg.ENIIndex, err)
	}
	if eni.Attrs().OperState != netlink.OperUp {
		if err = netlink.LinkSetUp(eni); err != nil {
			return fmt.Errorf("failed to bring link %s up: %w", eni.Attrs().Name, err)
		}
	}

	// 1. create the veth pair, container end moved into the pod netns
	err = veth.Setup(&veth.Veth{
		IfName:   cfg.ContainerIfName,
		PeerName: cfg.HostVETHName,
		MTU:      cfg.MTU,
	}, cfg.Netns)
	if err != nil {
		return fmt.Errorf("error setup veth pair, %w", err)
	}

	hostVETH, err := netlink.LinkByName(cfg.HostVETHName)
	if err != nil {
		return fmt.Errorf("error find host veth %s, %w", cfg.HostVETHName, err)
	}

	// 2. address, default route and the pinned neighbor inside the container
	err = cfg.Netns.Do(func(ns.NetNS) error {
		contLink, innerErr := netlink.LinkByName(cfg.ContainerIfName)
		if innerErr != nil {
			return fmt.Errorf("error find link %s in container, %w", cfg.ContainerIfName, innerErr)
		}
		return nic.Setup(contLink, generateContCfgForPolicy(cfg, contLink, hostVETH.Attrs().HardwareAddr))
	})
	if err != nil {
		return fmt.Errorf("error set container link/address/route, %w", err)
	}

	// 3. the ENI's own route table
	table := linuxUtils.GetRouteTableID(eni.Attrs().Index)
	if err = nic.Setup(eni, generateENICfgForPolicy(logger, cfg, eni, table)); err != nil {
		return fmt.Errorf("error setup eni config, %w", err)
	}

	// 4. host end: route to the pod plus the two policy rules
	if err = nic.Setup(hostVETH, generateHostPeerCfgForPolicy(cfg, hostVETH, table)); err != nil {
		return fmt.Errorf("error setup host veth config, %w", err)
	}

	logger.Infof("PolicyRoute setup done, eni %s table %d host veth %s",
		eni.Attrs().Name, table, cfg.HostVETHName)
	return nil
}

func (d *PolicyRouteDriver) Teardown(logger zlog.Logger, cfg *types.TeardownCfg) error {
	if cfg.ContainerIPNet == nil {
		return nil
	}

	// CNI DEL is best effort: bailing out on the first failure would leave the
	// remaining state behind. The ip rules in particular are keyed on the pod
	// address rather than on the link, so nothing removes them implicitly; the
	// daemon's reclaimStaleRules sweeps the same two priorities, but only later,
	// when the pod's resources are reclaimed. So every step runs and the errors
	// are aggregated.
	var errList []error

	// Dropping the host end takes the container end with it, and with it every
	// route that pointed at either.
	if cfg.HostVETHName != "" {
		if err := linuxUtils.DelLinkByName(cfg.HostVETHName); err != nil {
			errList = append(errList, fmt.Errorf("del host veth %s: %w", cfg.HostVETHName, err))
		}
	}

	// The ip rules are not tied to the link, so they have to go explicitly.
	// FindIPRule filters on src/dst plus priority, and the pod address is a
	// single host route, so this cannot reach another pod's rules.
	extender := linuxUtils.NewIPNet(cfg.ContainerIPNet)
	delRules := func(rule *netlink.Rule) {
		rules, err := linuxUtils.FindIPRule(rule)
		if err != nil {
			errList = append(errList, fmt.Errorf("find ip rule: %w", err))
			return
		}
		for i := range rules {
			if err = linuxUtils.RuleDel(&rules[i]); err != nil {
				errList = append(errList, fmt.Errorf("del ip rule %s: %w", rules[i].String(), err))
			}
		}
	}

	for _, ipNet := range []*net.IPNet{extender.IPv4, extender.IPv6} {
		if ipNet == nil {
			continue
		}
		fromRule := netlink.NewRule()
		fromRule.Priority = fromContainerPriority
		fromRule.Src = ipNet
		delRules(fromRule)

		toRule := netlink.NewRule()
		toRule.Priority = toContainerPriority
		toRule.Dst = ipNet
		delRules(toRule)
	}

	if len(errList) > 0 {
		return k8sErr.NewAggregate(errList)
	}
	logger.Infof("PolicyRoute teardown done, pod %s host veth %q",
		cfg.ContainerIPNet.String(), cfg.HostVETHName)
	return nil
}

// generateContCfgForPolicy builds the in-netns config: the pod address, a
// default route via the link-local gateway and the permanent neighbor that
// makes that gateway resolvable without ARP.
func generateContCfgForPolicy(cfg *types.SetupConfig, link netlink.Link, hostMAC net.HardwareAddr) *nic.Conf {
	var (
		routes []*netlink.Route
		rules  []*netlink.Rule
		neighs []*netlink.Neigh
		sysctl map[string][]string
	)

	// With several networks attached the container needs its own table too, so
	// replies leave through the interface they arrived on.
	//
	// The rules below deliberately use toContainerPriority even for the
	// from-<addr> ones. The 512/2048 split exists on the host to order one
	// pod's rules against every other pod's; inside a netns there is nothing to
	// order against, since these are the only rules there. All that matters is
	// that both sit ahead of the kernel's main lookup at 32766, which either
	// constant does. Only the host side keeps the to/from split.
	//
	// The table id is derived from the in-netns ifindex, which is independent
	// of the host's ENI table ids -- route tables are per network namespace.
	table := linuxUtils.GetRouteTableID(link.Attrs().Index)
	if cfg.MultiNetwork {
		ruleIf := netlink.NewRule()
		ruleIf.OifName = cfg.ContainerIfName
		ruleIf.Table = table
		ruleIf.Priority = toContainerPriority
		rules = append(rules, ruleIf)
	}

	if cfg.ContainerIPNet.IPv4 != nil {
		if cfg.DefaultRoute {
			routes = append(routes, &netlink.Route{
				LinkIndex: link.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
				Dst:       defaultRoute,
				Gw:        LinkIPNet.IP,
				Flags:     int(netlink.FLAG_ONLINK),
			})
		}
		if len(cfg.ExtraRoutes) > 0 {
			routes = append(routes, &netlink.Route{
				LinkIndex: link.Attrs().Index,
				Scope:     netlink.SCOPE_LINK,
				Dst:       LinkIPNet,
			})
		}

		neighs = append(neighs, &netlink.Neigh{
			LinkIndex:    link.Attrs().Index,
			IP:           LinkIPNet.IP,
			HardwareAddr: hostMAC,
			State:        netlink.NUD_PERMANENT,
		})

		if cfg.MultiNetwork {
			fromContainerRule := netlink.NewRule()
			fromContainerRule.Src = linuxUtils.NewIPNetWithMaxMask(cfg.ContainerIPNet.IPv4)
			fromContainerRule.Table = table
			fromContainerRule.Priority = toContainerPriority
			rules = append(rules, fromContainerRule)

			routes = append(routes, &netlink.Route{
				LinkIndex: link.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
				Dst:       defaultRoute,
				Gw:        cfg.GatewayIP.IPv4,
				Flags:     int(netlink.FLAG_ONLINK),
				Table:     table,
			})
		}
	}

	if cfg.ContainerIPNet.IPv6 != nil {
		if cfg.DefaultRoute {
			routes = append(routes, &netlink.Route{
				LinkIndex: link.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
				Dst:       defaultRouteIPv6,
				Gw:        LinkIPNetv6.IP,
				Flags:     int(netlink.FLAG_ONLINK),
			})
		}
		if len(cfg.ExtraRoutes) > 0 {
			routes = append(routes, &netlink.Route{
				LinkIndex: link.Attrs().Index,
				Dst:       LinkIPNetv6,
			})
		}

		neighs = append(neighs, &netlink.Neigh{
			LinkIndex:    link.Attrs().Index,
			IP:           LinkIPNetv6.IP,
			HardwareAddr: hostMAC,
			State:        netlink.NUD_PERMANENT,
		})

		if cfg.MultiNetwork {
			fromContainerRule := netlink.NewRule()
			fromContainerRule.Src = linuxUtils.NewIPNetWithMaxMask(cfg.ContainerIPNet.IPv6)
			fromContainerRule.Table = table
			fromContainerRule.Priority = toContainerPriority
			rules = append(rules, fromContainerRule)

			routes = append(routes, &netlink.Route{
				LinkIndex: link.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
				Dst:       defaultRouteIPv6,
				Gw:        cfg.GatewayIP.IPv6,
				Flags:     int(netlink.FLAG_ONLINK),
				Table:     table,
			})
		}

		sysctl = linuxUtils.GenerateIPv6Sysctl(cfg.ContainerIfName, linuxUtils.AcceptRAOff, false)
	}

	// The extra routes themselves, not just the on-link route that reaches
	// their gateway.
	for i := range cfg.ExtraRoutes {
		if cfg.ExtraRoutes[i].GW != nil {
			routes = append(routes, &netlink.Route{
				LinkIndex: link.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
				Flags:     int(netlink.FLAG_ONLINK),
				Dst:       &cfg.ExtraRoutes[i].Dst,
				Gw:        cfg.ExtraRoutes[i].GW,
			})
		} else {
			routes = append(routes, &netlink.Route{
				LinkIndex: link.Attrs().Index,
				Scope:     netlink.SCOPE_LINK,
				Dst:       &cfg.ExtraRoutes[i].Dst,
			})
		}
	}

	// IFA_F_NODAD on the pod's IPv6. DAD has nothing to find -- the CNI is the
	// only allocator of these addresses -- and it costs the pod's first second,
	// during which a container binding the address gets EADDRNOTAVAIL.
	//
	// The flag rather than accept_dad=0: the kernel takes the maximum of
	// conf/all and conf/<if>, and a fresh netns starts with all/accept_dad=1
	// from the kernel template, so the per-interface knob alone leaves DAD on.
	contAddrs := linuxUtils.NewIPNetToMaxMask(cfg.ContainerIPNet)
	for _, a := range contAddrs {
		if a.IP.To4() == nil {
			a.Flags |= unix.IFA_F_NODAD
		}
	}

	return &nic.Conf{
		IfName: cfg.ContainerIfName,
		MTU:    cfg.MTU,
		Addrs:  contAddrs,
		Routes: routes,
		Rules:  rules,
		Neighs: neighs,
		SysCtl: sysctl,
	}
}

// generateHostPeerCfgForPolicy builds the host end: a link-scoped route to the
// pod and the pair of rules that split traffic by direction -- inbound resolves
// in the main table, outbound in the ENI's table.
func generateHostPeerCfgForPolicy(cfg *types.SetupConfig, link netlink.Link, table int) *nic.Conf {
	var (
		addrs  []*netlink.Addr
		routes []*netlink.Route
		rules  []*netlink.Rule
		sysctl map[string][]string
	)

	appendFor := func(podIP *net.IPNet, gwIPNet *net.IPNet) {
		if len(cfg.ExtraRoutes) > 0 {
			addrs = append(addrs, &netlink.Addr{IPNet: gwIPNet})
		}

		maxMask := linuxUtils.NewIPNetWithMaxMask(podIP)
		routes = append(routes, &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Scope:     netlink.SCOPE_LINK,
			Dst:       maxMask,
		})

		toContainerRule := netlink.NewRule()
		toContainerRule.Dst = maxMask
		toContainerRule.Table = unix.RT_TABLE_MAIN
		toContainerRule.Priority = toContainerPriority

		fromContainerRule := netlink.NewRule()
		fromContainerRule.Src = maxMask
		fromContainerRule.Table = table
		fromContainerRule.Priority = fromContainerPriority

		rules = append(rules, toContainerRule, fromContainerRule)
	}

	if cfg.ContainerIPNet.IPv4 != nil {
		appendFor(cfg.ContainerIPNet.IPv4, LinkIPNet)
	}
	if cfg.ContainerIPNet.IPv6 != nil {
		appendFor(cfg.ContainerIPNet.IPv6, LinkIPNetv6)
		sysctl = linuxUtils.GenerateIPv6Sysctl(link.Attrs().Name, linuxUtils.AcceptRAOff, true)
	}

	return &nic.Conf{
		MTU:    cfg.MTU,
		Addrs:  addrs,
		Routes: routes,
		Rules:  rules,
		SysCtl: sysctl,
	}
}

// eniAddrs decides what address, if any, to place on an ENI: for either family,
// a /32 or /128 of the interface's own primary -- the address the platform bound
// to this port's MAC.
//
// It must not be the node's address, borrowed from the instance's own
// interface. On a fabric that checks source addresses, an ARP whose sender
// belongs to another port's MAC is dropped and every pod on the interface is
// black-holed; the IPv6 half fails differently, as a duplicate on the same
// segment that DAD marks dadfailed.
//
// A host route rather than the subnet mask: the mask would add a second on-link
// route for the subnet in the main table, competing with the instance's own
// interface. The default route in the ENI's own table is onlink, so a host
// route suffices.
func eniAddrs(logger zlog.Logger, cfg *types.SetupConfig, link netlink.Link) (toApply, desired []*netlink.Addr) {
	// Never the instance's own interface. Whatever configures the guest -- DHCP
	// here, on the primary interface only -- owns its addresses, and every
	// address this function would propose for it is one that is already there.
	// Refusing outright rather than relying on the address comparison below:
	// EnsureAddr replaces any address of the family that does not match exactly,
	// mask included, so a single disagreement between the platform's describe and
	// what DHCP actually configured takes the node off the network, with kubelet
	// and this daemon on it.
	if cfg.ENIIsPrimary {
		return nil, nil
	}
	eniPrimaryIP := cfg.ENIPrimaryIP
	if eniPrimaryIP == nil || (eniPrimaryIP.IPv4 == nil && eniPrimaryIP.IPv6 == nil) {
		// Nothing to configure and nothing to guess at. Leaving the interface
		// bare is the recoverable failure -- inventing an address is what the
		// fabric rejects -- though without a source in the subnet the gateway may
		// not resolve at all. Not expected to be reached: GetENIAllocatableIPs
		// refuses to treat any address on an interface with no primary IPv4 as
		// allocatable, so such an interface contributes nothing to the pool.
		logger.Infof("Eni %s reports no primary address; leaving its addresses alone", link.Attrs().Name)
		return nil, nil
	}
	want := eniPrimaryAddrs(logger, cfg, link.Attrs().Name)
	existing, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		// Cannot tell; leaving the link untouched is the recoverable choice.
		logger.Warnf("Cannot list addresses of %s, skipping address setup: %v", link.Attrs().Name, err)
		return nil, nil
	}
	// Per family, not all-or-nothing: an interface may already hold the address
	// for one family and not the other. Matched on the address alone, never the
	// mask, and that is load-bearing: EnsureAddr deletes every global address of
	// the family that does not match its argument exactly, mask included, so
	// proposing the host route for an address the interface already holds with
	// its subnet mask would replace it and drop the on-link route to that subnet.
	out := make([]*netlink.Addr, 0, len(want))
	for _, w := range want {
		held := false
		for _, have := range existing {
			if have.IP.Equal(w.IP) {
				held = true
				break
			}
		}
		if !held {
			out = append(out, w)
		}
	}
	// Both lists: what still has to be written, and everything that should be
	// there. The rules are built from the second -- see generateENICfgForPolicy.
	return out, want
}

// eniPrimaryAddrs turns an ENI's reported primary into the host routes worth
// configuring on it. It decides what should be on the interface; eniAddrs then
// subtracts what already is. Split out so it can be tested without a link.
//
// Only the families this pod uses -- otherwise an IPv4-only pod on a
// dual-configured node still pushes an IPv6 onto the interface.
//
// Each slot is checked against the family it claims to be, because nothing
// upstream does: ToIPSet copies the reply's two strings into same-named slots
// without looking. A v6 in the v4 slot is not harmless -- NewIPNetWithMaxMask
// re-derives the mask, both entries land in one family, and EnsureAddr's second
// pass silently deletes what the first added.
func eniPrimaryAddrs(logger zlog.Logger, cfg *types.SetupConfig, linkName string) []*netlink.Addr {
	eniPrimaryIP := cfg.ENIPrimaryIP
	wanted := &zecTypes.IPNetSet{}
	if eniPrimaryIP.IPv4 != nil && cfg.ContainerIPNet.IPv4 != nil {
		if eniPrimaryIP.IPv4.To4() == nil {
			logger.Errorf("Eni %s reports %s as its primary IPv4, which is not an IPv4 address; skipping",
				linkName, eniPrimaryIP.IPv4)
		} else {
			wanted.IPv4 = &net.IPNet{IP: eniPrimaryIP.IPv4, Mask: net.CIDRMask(32, 32)}
		}
	}
	if eniPrimaryIP.IPv6 != nil && cfg.ContainerIPNet.IPv6 != nil {
		if eniPrimaryIP.IPv6.To4() != nil {
			logger.Errorf("Eni %s reports %s as its primary IPv6, which is an IPv4 address; skipping",
				linkName, eniPrimaryIP.IPv6)
		} else {
			wanted.IPv6 = &net.IPNet{IP: eniPrimaryIP.IPv6, Mask: net.CIDRMask(128, 128)}
		}
	}
	return linuxUtils.NewIPNetToMaxMask(wanted)
}

// generateENICfgForPolicy puts the ENI's default route into that ENI's own
// table, so the from-pod rule installed on the host end resolves there. It also
// carries the guards that keep the instance's own interface from being
// reconfigured -- see eniAddrs, and the MTU and accept_ra notes below.
func generateENICfgForPolicy(logger zlog.Logger, cfg *types.SetupConfig, link netlink.Link, table int) *nic.Conf {
	var (
		routes []*netlink.Route
		sysctl map[string][]string
	)

	if cfg.ContainerIPNet.IPv4 != nil {
		routes = append(routes, &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Scope:     netlink.SCOPE_UNIVERSE,
			Table:     table,
			Dst:       defaultRoute,
			Gw:        cfg.GatewayIP.IPv4,
			Flags:     int(netlink.FLAG_ONLINK),
		})
	}
	if cfg.ContainerIPNet.IPv6 != nil {
		routes = append(routes,
			// An on-link route to the gateway itself, in this interface's table.
			//
			// Table matters: without it netlink writes to main, where every
			// interface competes for the same fe80::1/128 -- last ADD wins, and
			// the entry survives pointing at a dead LinkIndex once that interface
			// is detached.
			//
			// Redundant either way (the default route below carries FLAG_ONLINK),
			// but a redundant entry belongs in the table it describes. No IPv4
			// counterpart: an IPv4 gateway is a real address in the subnet.
			&netlink.Route{
				LinkIndex: link.Attrs().Index,
				Table:     table,
				Dst: &net.IPNet{
					IP:   cfg.GatewayIP.IPv6,
					Mask: net.CIDRMask(128, 128),
				},
			},
			&netlink.Route{
				LinkIndex: link.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
				Table:     table,
				Dst:       defaultRouteIPv6,
				Gw:        cfg.GatewayIP.IPv6,
				Flags:     int(netlink.FLAG_ONLINK),
			},
		)
		// Leave RA policy alone on the instance's own interface: the guest's
		// network manager owns it there (see AcceptRALeaveAlone). A CNI-created
		// interface has no such owner, so suppressing RA on it is ours to do.
		acceptRA := linuxUtils.AcceptRAOff
		if cfg.ENIIsPrimary {
			acceptRA = linuxUtils.AcceptRALeaveAlone
		}
		sysctl = linuxUtils.GenerateIPv6Sysctl(link.Attrs().Name, acceptRA, true)
	}

	// MTU only on an interface the CNI created. EnsureLinkMTU sets the value
	// exactly -- it raises as well as lowers -- so handing the conflist's MTU to
	// the instance's own interface rewrites the node's uplink on every pod that
	// lands there, and the primary serving pods is the normal case, not an edge.
	// On a jumbo-frame node that silently takes eth0 from 9000 to 1500; on a
	// node an operator lowered for an encapsulating upstream it raises it back
	// and blackholes large frames. Zero means leave it alone (see nic.Setup).
	mtu := cfg.MTU
	if cfg.ENIIsPrimary {
		mtu = 0
	}

	addrs, desired := eniAddrs(logger, cfg, link)

	// A from-rule for each address just configured, so traffic the host sources
	// from it resolves in this interface's table and leaves through this
	// interface. Without one it resolves in the main table and leaves through
	// the instance's own interface -- a packet whose source belongs to another
	// port's MAC, which is exactly the drop this whole arrangement exists to
	// avoid. So an address configured without its rule is a source the host can
	// pick and then send out of the wrong port.
	//
	// Built from the desired addresses, not from the ones still to be written.
	// Deriving them from the difference installs them at most once: if anything
	// between the address write and the rule write fails, the retry sees the
	// address already held and proposes nothing, leaving that interface without
	// its rule for good. EnsureIPRule is idempotent.
	//
	// Nothing is emitted for the instance's own interface -- eniAddrs returns
	// nothing there.
	//
	// These rules have no reaper: teardown removes rules by the pod's address,
	// so a detached interface leaves its rule behind. Inert (the source address
	// went with the link) but it accumulates one entry per interface.
	var rules []*netlink.Rule
	for _, addr := range desired {
		fromENIRule := netlink.NewRule()
		fromENIRule.Src = linuxUtils.NewIPNetWithMaxMask(addr.IPNet)
		fromENIRule.Table = table
		fromENIRule.Priority = fromContainerPriority
		rules = append(rules, fromENIRule)
	}

	return &nic.Conf{
		MTU:    mtu,
		Addrs:  addrs,
		Routes: routes,
		Rules:  rules,
		SysCtl: sysctl,
	}
}

var _ DataPath = (*PolicyRouteDriver)(nil)
