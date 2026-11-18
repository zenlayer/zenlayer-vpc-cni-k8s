// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package linux

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"

	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"

	"github.com/vishvananda/netlink"
)

// GetRouteTableID returns the per-interface route table id, 1000 + link index.
// The offset keeps these tables clear of the kernel's own (local 255, main 254,
// default 253). Table ids are per network namespace, so a container table and a
// host table derived from the same ifindex do not collide.
func GetRouteTableID(linkIndex int) int {
	return 1000 + linkIndex
}

// EnsureLinkUp set link up,return changed and err
func EnsureLinkUp(link netlink.Link) (bool, error) {
	if link.Attrs().Flags&net.FlagUp != 0 {
		return false, nil
	}
	return true, LinkSetUp(link)
}

// EnsureLinkMTU set link mtu,return changed and err
func EnsureLinkMTU(link netlink.Link, mtu int) (bool, error) {
	if link.Attrs().MTU == mtu {
		return false, nil
	}
	return true, LinkSetMTU(link, mtu)
}

func EnsureLinkName(link netlink.Link, name string) (bool, error) {
	if link.Attrs().Name == name {
		return false, nil
	}
	return true, LinkSetName(link, name)
}

// DelLinkByName del by name and ignore if link not present
func DelLinkByName(ifName string) error {
	contLink, err := netlink.LinkByName(ifName)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); ok { //nolint
			return nil
		}
		// Any other error leaves contLink nil; LinkDel would deref it.
		return err
	}
	return LinkDel(contLink)
}

// EnsureAddr makes expect the only global-unicast address of its family on
// link, and reports whether anything changed. Addresses that are not global
// unicast are left alone -- on IPv6 that is the link-local the kernel keeps.
func EnsureAddr(link netlink.Link, expect *netlink.Addr) (bool, error) {
	var changed bool

	addrList, err := netlink.AddrList(link, NetlinkFamily(expect.IP))
	if err != nil {
		return false, fmt.Errorf("error list address from if %s, %w", link.Attrs().Name, err)
	}

	found := false
	for _, addr := range addrList {
		if !addr.IP.IsGlobalUnicast() {
			continue
		}

		if (addr.IPNet.String() == expect.IPNet.String()) && (addr.Scope == expect.Scope) {
			found = true
			continue
		}

		err := AddrDel(link, &addr)
		if err != nil {
			return false, err
		}
		changed = true
	}
	if found {
		return changed, nil
	}
	return true, AddrReplace(link, expect)
}

// FoundRoutes lists the routes matching expected. The filter mask is built from
// whichever fields of expected are set, so a sparsely filled Route matches
// broadly; Dst is always part of it.
//
// expected.Dst must be non-nil. The "dst in route expect not nil" error below
// does not enforce that: the family is read from expected.Dst.IP one line
// earlier, so a nil Dst panics before the check is reached.
//
// A default destination is rewritten to nil before the lookup: the kernel
// reports a default route with no destination at all, so filtering on the
// literal 0.0.0.0/0 or ::/0 would never match one.
func FoundRoutes(expected *netlink.Route) ([]netlink.Route, error) {
	family := NetlinkFamily(expected.Dst.IP)
	routeFilter := netlink.RT_FILTER_DST
	if expected.Dst == nil {
		return nil, fmt.Errorf("dst in route expect not nil")
	}
	find := *expected

	if find.Dst.String() == "::/0" || find.Dst.String() == "0.0.0.0/0" {
		find.Dst = nil
	}
	if find.LinkIndex > 0 {
		routeFilter = routeFilter | netlink.RT_FILTER_OIF
	}
	if find.Scope > 0 {
		routeFilter = routeFilter | netlink.RT_FILTER_SCOPE
	}
	if find.Gw != nil {
		routeFilter = routeFilter | netlink.RT_FILTER_GW
	}
	if find.Table > 0 {
		routeFilter = routeFilter | netlink.RT_FILTER_TABLE
	}
	return netlink.RouteListFiltered(family, &find, routeFilter)
}

// EnsureRoute will call ip route replace if route is not found
func EnsureRoute(expected *netlink.Route) (bool, error) {
	routes, err := FoundRoutes(expected)
	if err != nil {
		return false, fmt.Errorf("error list expected: %v", err)
	}
	if len(routes) > 0 {
		return false, nil
	}

	return true, RouteReplace(expected)
}

// NewIPNetWithMaxMask returns ipNet's address with a host mask: /32 for IPv4,
// /128 for IPv6.
func NewIPNetWithMaxMask(ipNet *net.IPNet) *net.IPNet {
	if ipNet.IP.To4() == nil {
		return &net.IPNet{
			IP:   ipNet.IP,
			Mask: net.CIDRMask(128, 128),
		}
	}
	return &net.IPNet{
		IP:   ipNet.IP,
		Mask: net.CIDRMask(32, 32),
	}
}

// NewIPNet1 returns one netlink.Addr per family present in ipNet, keeping the
// prefix length each carries.
func NewIPNet1(ipNet *zecTypes.IPNetSet) []*netlink.Addr {
	var addrs []*netlink.Addr
	if ipNet.IPv4 != nil {
		addrs = append(addrs, &netlink.Addr{IPNet: ipNet.IPv4})
	}
	if ipNet.IPv6 != nil {
		addrs = append(addrs, &netlink.Addr{IPNet: ipNet.IPv6})
	}
	return addrs
}

// NewIPNetToMaxMask is NewIPNet1 with every prefix narrowed to a host mask.
func NewIPNetToMaxMask(ipNet *zecTypes.IPNetSet) []*netlink.Addr {
	var addrs []*netlink.Addr
	if ipNet.IPv4 != nil {
		addrs = append(addrs, &netlink.Addr{IPNet: NewIPNetWithMaxMask(ipNet.IPv4)})
	}
	if ipNet.IPv6 != nil {
		addrs = append(addrs, &netlink.Addr{IPNet: NewIPNetWithMaxMask(ipNet.IPv6)})
	}
	return addrs
}

// NewIPNet returns a copy of ipNet with every family narrowed to a host mask.
// Used to build the ip rule selectors, where a pod address must match itself
// and no neighbor.
func NewIPNet(ipNet *zecTypes.IPNetSet) *zecTypes.IPNetSet {
	ipNetSet := &zecTypes.IPNetSet{}
	if ipNet.IPv4 != nil {
		ipNetSet.IPv4 = NewIPNetWithMaxMask(ipNet.IPv4)
	}
	if ipNet.IPv6 != nil {
		ipNetSet.IPv6 = NewIPNetWithMaxMask(ipNet.IPv6)
	}
	return ipNetSet
}

// FindIPRule lists the ip rules matching rule.
//
// The family comes from whichever of Src or Dst is set: netlink keeps a
// separate rule table per family and a lookup in the wrong one silently returns
// nothing. A rule selected by OifName carries no address to derive a family
// from, so it is looked up in the IPv4 table.
//
// Priority joins the filter whenever it is non-negative, so a caller that set
// one gets the rules at that priority rather than anything else naming the same
// address. netlink.NewRule leaves Priority at -1, so a rule built that way and
// never assigned one matches at every priority.
func FindIPRule(rule *netlink.Rule) ([]netlink.Rule, error) {
	var filterMask uint64
	family := netlink.FAMILY_V4

	if rule.Src == nil && rule.Dst == nil && rule.OifName == "" {
		return nil, errors.New("both src and dst is nil")
	}

	if rule.Src != nil {
		filterMask = filterMask | netlink.RT_FILTER_SRC
		family = NetlinkFamily(rule.Src.IP)
	}
	if rule.Dst != nil {
		filterMask = filterMask | netlink.RT_FILTER_DST
		family = NetlinkFamily(rule.Dst.IP)
	}
	if rule.OifName != "" {
		filterMask = filterMask | netlink.RT_FILTER_OIF
		family = netlink.FAMILY_V4
	}

	if rule.Priority >= 0 {
		filterMask = filterMask | netlink.RT_FILTER_PRIORITY
	}
	return netlink.RuleListFiltered(family, rule, filterMask)
}

// EnsureIPRule makes expected present and reports whether anything changed.
// Rules FindIPRule returns for the same selector are deleted first unless their
// table, priority and iif all match expected, so a stale rule for this address
// is removed rather than left alongside the new one. OifName is not compared.
func EnsureIPRule(expected *netlink.Rule) (bool, error) {
	changed := false

	ruleList, err := FindIPRule(expected)
	if err != nil {
		return false, err
	}
	found := false
	for _, rule := range ruleList {
		del := false
		if rule.Table != expected.Table {
			del = true
		}
		if rule.Priority != expected.Priority {
			del = true
		}
		if rule.IifName != expected.IifName {
			del = true
		}
		if del {
			changed = true
			err = RuleDel(&rule)
			if err != nil {
				return changed, err
			}
		} else {
			found = true
		}
	}
	if found {
		return changed, nil
	}
	return true, RuleAdd(expected)
}

// acceptRA values for GenerateIPv6Sysctl.
const (
	// AcceptRAOff refuses router advertisements. Right for an interface nothing
	// configures by RA: the container end, the host veth, a CNI-created ENI.
	AcceptRAOff = "0"
	// AcceptRALeaveAlone does not touch the setting.
	//
	// The instance's own interface gets this, because something else owns RA
	// policy there. On the node images observed so far, systemd-networkd holds
	// the kernel knob at 0 and processes router advertisements in userspace,
	// and the node's IPv6 default route is one it installed that way --
	// "proto ra", with a finite lifetime only networkd renews. Against that,
	// writing 0 is a no-op and writing 2 is worse: the kernel would process RAs
	// in parallel and install its own routes and addresses alongside
	// networkd's, with different lifetimes. Writing 2 would only be defensible
	// if this CNI owned the node's network configuration end to end. It does
	// not: the primary interface is configured by whatever the node image ships.
	//
	// Enabling forwarding alongside it is safe: forwarding=1 on that interface,
	// and on "all", leaves the route in place. The kernel's
	// rt6_purge_dflt_routers only reaches routes it autoconfigured itself, and
	// networkd's is not one of them.
	AcceptRALeaveAlone = ""
)

// GenerateIPv6Sysctl builds the IPv6 sysctl writes for one interface:
// disable_ipv6=0 on lo, all and default, the same on ifName, plus accept_ra and
// forwarding=1 on ifName when asked. acceptRA is one of the AcceptRA* values.
// An empty ifName yields only the lo/all/default entries.
//
// Deliberately no DAD knob: the kernel takes the maximum of conf/all/accept_dad
// and conf/<if>/accept_dad, so writing the per-interface one alone does nothing
// wherever `all` is 1 -- which is what a fresh network namespace starts with.
// Where DAD has to be suppressed the address carries IFA_F_NODAD instead; see
// the container branch of the policy-route datapath.
func GenerateIPv6Sysctl(ifName string, acceptRA string, enableForward bool) map[string][]string {
	result := map[string][]string{}
	for _, name := range []string{"lo", "all", "default"} {
		result[fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/disable_ipv6", name)] = []string{fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/disable_ipv6", name), "0"}
	}

	if ifName != "" {
		result[fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/disable_ipv6", ifName)] = []string{fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/disable_ipv6", ifName), "0"}

		if acceptRA != "" {
			result[fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_ra", ifName)] = []string{fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_ra", ifName), acceptRA}
		}
		if enableForward {
			result[fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/forwarding", ifName)] = []string{fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/forwarding", ifName), "1"}
		}
	}
	return result
}

func EnsureNeigh(neigh *netlink.Neigh) (bool, error) {
	var neighs []netlink.Neigh
	var err error
	if IsIPv6(neigh.IP) {
		neighs, err = netlink.NeighList(neigh.LinkIndex, netlink.FAMILY_V6)
	} else {
		neighs, err = netlink.NeighList(neigh.LinkIndex, netlink.FAMILY_V4)
	}
	if err != nil {
		return false, err
	}
	found := false
	for _, n := range neighs {
		if n.IP.Equal(neigh.IP) && n.HardwareAddr.String() == neigh.HardwareAddr.String() {
			found = true
			break
		}
	}
	if !found {
		return true, NeighSet(neigh)
	}
	return false, err
}

var ipv4NetConfig = [][]string{
	{"/proc/sys/net/ipv4/conf/%s/forwarding", "1"},
	{"/proc/sys/net/ipv4/conf/%s/rp_filter", "0"},
}

var ipv6NetConfig = [][]string{
	{"/proc/sys/net/ipv6/conf/%s/forwarding", "1"},
	{"/proc/sys/net/ipv6/conf/%s/disable_ipv6", "0"},
}

func ensureConf(fPath string, cfg string) error {
	if content, err := os.ReadFile(fPath); err == nil {
		if bytes.Equal(bytes.TrimSpace(content), []byte(cfg)) {
			return nil
		}
	}
	return os.WriteFile(fPath, []byte(cfg), 0644)
}

// EnsureHostNsConfig setup host namespace configs.
//
// ipv6 gates the `default` and `all` entries only, and it is the caller that
// knows whether the pod it is setting up has an IPv6 address:
// net.ipv6.conf.all.* is host-wide, so turning it on for an IPv4-only node
// changes the host's behavior for no benefit. It does not gate the
// per-interface pass: EnsureNetConfSet below writes ipv6NetConfig onto every
// link either way, which is harmless precisely because `all` is what decides
// whether forwarding actually happens.
//
// ip6_forward() tests net->ipv6.devconf_all->forwarding before it ever looks at
// the per-device value, so the per-interface forwarding=1 that EnsureNetConfSet
// and GenerateIPv6Sysctl already write puts a device in router mode without
// enabling forwarding at all. Without the `all` entry a dual pod comes up with a
// correct address, route, neighbor entry and both ip rules -- everything an
// operator would inspect looks right -- and every IPv6 packet is dropped in
// both directions, silently. The IPv4 side is not exposed to this:
// ipv4NetConfig is written here whatever ipv6 says.
func EnsureHostNsConfig(ipv6 bool) error {
	cfgs := ipv4NetConfig
	if ipv6 {
		cfgs = append(append([][]string{}, ipv4NetConfig...), ipv6NetConfig...)
	}
	for _, key := range []string{"default", "all"} {
		for _, cfg := range cfgs {
			err := ensureConf(fmt.Sprintf(cfg[0], key), cfg[1])
			if err != nil {
				return err
			}
		}
	}

	// The `default`/`all` pass above is this pod's own correctness and stays
	// fatal. The host-wide sweep below is not, and its failure must not fail
	// this ADD: it touches every interface on the node, so two things outside
	// this pod's control end up in its error.
	//
	// One is an interface another pod's DEL removed between LinkList and the
	// write, which yields ENOENT for a link that is legitimately gone -- and
	// this runs before the node-wide file lock is taken, so that race is
	// reachable. The other is a node booted with ipv6.disable=1 or built with
	// CONFIG_IPV6=n: /proc/sys/net/ipv6 does not exist at all, every ipv6 write
	// in the sweep returns ENOENT, and returning that error failed every CNI ADD
	// on the node -- every pod stuck in ContainerCreating, including IPv4-only
	// ones, for settings no pod on that node needs.
	if err := EnsureNetConfSet(); err != nil {
		log.Warnf("Host-wide per-interface sysctl sweep did not fully apply, continuing: %v", err)
	}
	return nil
}

// EnsureLinkNetConf applies the per-link settings to one interface.
//
// systemd-sysctl replays the host's sysctl.d onto every interface as it
// appears, so a freshly created veth starts with the host's policy rather than
// ours. Writing here states the CNI's intent on the interfaces it owns, but
// whether the replay lands before or after this write is a race; the durable
// fix is a higher-priority sysctl.d drop-in scoped to these interface names.
func EnsureLinkNetConf(name string) error {
	// Collected rather than chained: the first wrap would have had a nil err on
	// its left, producing a message that began with "<nil>, ".
	var failures []error
	for _, cfg := range ipv4NetConfig {
		if innerErr := ensureConf(fmt.Sprintf(cfg[0], name), cfg[1]); innerErr != nil {
			failures = append(failures, innerErr)
		}
	}
	for _, cfg := range ipv6NetConfig {
		if innerErr := ensureConf(fmt.Sprintf(cfg[0], name), cfg[1]); innerErr != nil {
			failures = append(failures, innerErr)
		}
	}
	return errors.Join(failures...)
}

// EnsureAcceptRAOff refuses router advertisements on one interface the CNI
// owns. Not part of ipv6NetConfig: that set is also swept over the instance's
// own interface, whose RA policy belongs to the guest (see AcceptRALeaveAlone).
func EnsureAcceptRAOff(name string) error {
	return ensureConf(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_ra", name), AcceptRAOff)
}

// EnsureNetConfSet applies both the IPv4 and the IPv6 per-link settings to every
// interface on the host, unconditionally -- no family gate here, unlike in
// EnsureHostNsConfig, which calls it.
//
// Errors do not abort the sweep: one interface that rejects a write must not
// leave the remaining interfaces unconfigured. They are chained into the return
// value instead.
func EnsureNetConfSet() error {
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}

	// Collected rather than chained onto err: err is nil at this point, and
	// wrapping it produced messages that began with "<nil>, ".
	var failures []error
	for _, link := range links {
		for _, cfg := range ipv4NetConfig {
			innerErr := ensureConf(fmt.Sprintf(cfg[0], link.Attrs().Name), cfg[1])
			if innerErr != nil {
				failures = append(failures, innerErr)
			}
		}
		for _, cfg := range ipv6NetConfig {
			innerErr := ensureConf(fmt.Sprintf(cfg[0], link.Attrs().Name), cfg[1])
			if innerErr != nil {
				failures = append(failures, innerErr)
			}
		}
	}
	return errors.Join(failures...)
}
