// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package datapath

import (
	"fmt"
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/plugin/types"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// Both builders here are pure: a SetupConfig and a link in, a nic.Conf out.
// Nothing enters a namespace or talks to netlink -- that is nic.Setup's job --
// so these tests need no root and no interfaces.
var testHostMAC = net.HardwareAddr{0xaa, 0xbb, 0xcc, 0x00, 0x11, 0x22}

const (
	contIfIndex = 7
	hostIfName  = "zecxxxx"
)

func dualPodCfg(t *testing.T) *types.SetupConfig {
	t.Helper()
	return &types.SetupConfig{
		ContainerIfName: "eth0",
		ContainerIPNet: &zecTypes.IPNetSet{
			IPv4: mustCIDR(t, "10.0.0.31/24"),
			IPv6: mustCIDR(t, "2001:db8::3/96"),
		},
		GatewayIP: &zecTypes.IPSet{
			IPv4: net.ParseIP("10.0.0.1"),
			IPv6: net.ParseIP("2001:db8::1"),
		},
		MTU:          1500,
		DefaultRoute: true,
	}
}

func contLink() *fakeLink {
	return &fakeLink{attrs: netlink.LinkAttrs{Index: contIfIndex, Name: "eth0"}}
}

func hostLink() *fakeLink {
	return &fakeLink{attrs: netlink.LinkAttrs{Index: 8, Name: hostIfName}}
}

// ruleAddr is whichever selector the rule carries, for the assertions that do
// not care which side it is on.
func ruleAddr(r *netlink.Rule) *net.IPNet {
	if r.Src != nil {
		return r.Src
	}
	return r.Dst
}

// TestContainerRulesAllSitAtTheSamePriority is the one this file exists for.
//
// On the host the two rules must be ordered against each other, 512 before
// 2048, because every pod on the node contributes a pair and a packet to a
// same-node pod matches both. Inside the netns there is nothing to order
// against, so both bands are 512 -- and a reader who has just understood the
// host side is liable to "fix" the from-rule here to 2048. Nothing would show
// it: these rules exist only when MultiNetwork is on, so a single-network node
// behaves identically either way.
func TestContainerRulesAllSitAtTheSamePriority(t *testing.T) {
	cfg := dualPodCfg(t)
	cfg.MultiNetwork = true

	conf := generateContCfgForPolicy(cfg, contLink(), testHostMAC)

	if len(conf.Rules) != 3 {
		t.Fatalf("generated %d rules, want 3 (one oif rule, one from-rule per family)", len(conf.Rules))
	}
	wantTable := 1000 + contIfIndex
	for _, r := range conf.Rules {
		if r.Priority != toContainerPriority {
			t.Errorf("rule %s sits at priority %d, want %d: inside the netns both bands are the same, "+
				"and the 512/2048 split describes the host side only", r.String(), r.Priority, toContainerPriority)
		}
		if r.Table != wantTable {
			t.Errorf("rule %s resolves in table %d, want %d (1000+ifindex of the in-netns link): "+
				"the host's per-interface tables do not exist in this namespace",
				r.String(), r.Table, wantTable)
		}
	}
}

// TestContainerCfgWithoutMultiNetworkInstallsNoRules pins the default shape. One
// network means one table, so a rule would only send the lookup somewhere empty.
func TestContainerCfgWithoutMultiNetworkInstallsNoRules(t *testing.T) {
	cfg := dualPodCfg(t)
	cfg.MultiNetwork = false

	if conf := generateContCfgForPolicy(cfg, contLink(), testHostMAC); len(conf.Rules) != 0 {
		t.Errorf("generated %d rules with MultiNetwork off, want none", len(conf.Rules))
	}
}

// TestContainerCfgReachesItsGatewayWithoutAsking covers why the pod never sends
// an ARP or NDP request: its next hop is a link-local placeholder that sits on
// no interface, so a permanent neighbor entry is the only thing that makes it
// resolvable, and the route to it has to be onlink or the kernel refuses it.
func TestContainerCfgReachesItsGatewayWithoutAsking(t *testing.T) {
	conf := generateContCfgForPolicy(dualPodCfg(t), contLink(), testHostMAC)

	wantNeigh := map[string]bool{LinkIPNet.IP.String(): false, LinkIPNetv6.IP.String(): false}
	for _, n := range conf.Neighs {
		if _, want := wantNeigh[n.IP.String()]; !want {
			t.Errorf("permanent neighbor for %s, which is not a gateway placeholder", n.IP)
			continue
		}
		wantNeigh[n.IP.String()] = true
		if n.State != netlink.NUD_PERMANENT {
			t.Errorf("neighbor %s is in state %d, want NUD_PERMANENT: anything else is reclaimed, and "+
				"there is nobody to answer the request that follows", n.IP, n.State)
		}
		if n.HardwareAddr.String() != testHostMAC.String() {
			t.Errorf("neighbor %s points at %s, want the host veth's %s", n.IP, n.HardwareAddr, testHostMAC)
		}
		if n.LinkIndex != contIfIndex {
			t.Errorf("neighbor %s is on link %d, want %d", n.IP, n.LinkIndex, contIfIndex)
		}
	}
	for ip, seen := range wantNeigh {
		if !seen {
			t.Errorf("no permanent neighbor for the gateway placeholder %s", ip)
		}
	}

	gwSeen := map[string]bool{}
	for _, r := range conf.Routes {
		if r.Dst == nil {
			continue
		}
		if ones, _ := r.Dst.Mask.Size(); ones != 0 {
			continue
		}
		gwSeen[r.Gw.String()] = true
		if r.Flags&int(netlink.FLAG_ONLINK) == 0 {
			t.Errorf("the default route via %s is not onlink; that gateway is on no interface, so the "+
				"kernel rejects the route", r.Gw)
		}
	}
	for _, gw := range []net.IP{LinkIPNet.IP, LinkIPNetv6.IP} {
		if !gwSeen[gw.String()] {
			t.Errorf("no default route via the placeholder %s", gw)
		}
	}
}

// TestContainerCfgSuppressesDADOnTheIPv6Address covers the flag rather than the
// sysctl. The kernel takes the maximum of conf/all and conf/<if> for accept_dad
// and a fresh netns starts with all/accept_dad=1, so the per-interface knob
// alone leaves DAD on -- and DAD costs the pod its first second, during which
// binding the address returns EADDRNOTAVAIL.
func TestContainerCfgSuppressesDADOnTheIPv6Address(t *testing.T) {
	conf := generateContCfgForPolicy(dualPodCfg(t), contLink(), testHostMAC)

	var sawV6 bool
	for _, a := range conf.Addrs {
		isV6 := a.IP.To4() == nil
		sawV6 = sawV6 || isV6
		if hasNoDAD := a.Flags&unix.IFA_F_NODAD != 0; isV6 != hasNoDAD {
			t.Errorf("address %s: IFA_F_NODAD set = %v, want %v -- the flag belongs on IPv6 only",
				a.IPNet, hasNoDAD, isV6)
		}
		if ones, bits := a.Mask.Size(); ones != bits {
			t.Errorf("address %s is not a host mask; a subnet mask adds an on-link route competing "+
				"with the interface's own", a.IPNet)
		}
	}
	if !sawV6 {
		t.Fatal("no IPv6 address was configured, so the assertion above proved nothing")
	}
}

// TestContainerAndHostSysctlDifferOnForwarding pins the one asymmetry between
// the two ends, which is the thing copying gets wrong. The host veth forwards --
// it is the hop between the pod and the interface. The container end must not: a
// pod that forwards accepts traffic addressed elsewhere and can route between
// networks it was never given.
func TestContainerAndHostSysctlDifferOnForwarding(t *testing.T) {
	cfg := dualPodCfg(t)
	contConf := generateContCfgForPolicy(cfg, contLink(), testHostMAC)
	hostConf := generateHostPeerCfgForPolicy(cfg, hostLink(), 1008)

	contFwd := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/forwarding", cfg.ContainerIfName)
	if _, set := contConf.SysCtl[contFwd]; set {
		t.Errorf("the container end sets %s; a forwarding pod can route between networks it was "+
			"never given", contFwd)
	}
	hostFwd := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/forwarding", hostIfName)
	if got := hostConf.SysCtl[hostFwd]; len(got) != 2 || got[1] != "1" {
		t.Errorf("the host veth has %s = %v, want it set to 1: it is the hop between the pod and the "+
			"interface, and without it every IPv6 packet is dropped in both directions", hostFwd, got)
	}

	// Neither end accepts router advertisements: addressing here comes from the
	// platform through this CNI, and an RA would install a competing default
	// route with nothing to say which won.
	for _, tc := range []struct {
		what   string
		sysctl map[string][]string
		ifName string
	}{
		{"container end", contConf.SysCtl, cfg.ContainerIfName},
		{"host veth", hostConf.SysCtl, hostIfName},
	} {
		key := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_ra", tc.ifName)
		if got := tc.sysctl[key]; len(got) != 2 || got[1] != "0" {
			t.Errorf("%s has %s = %v, want it set to 0", tc.what, key, got)
		}
	}
}

// TestSingleStackCfgTouchesNoIPv6 covers the IPv4-only node. Writing an IPv6
// sysctl on a kernel with IPv6 compiled out fails the write, and the write is on
// the CNI ADD path, so every pod on such a node would fail to start.
func TestSingleStackCfgTouchesNoIPv6(t *testing.T) {
	cfg := dualPodCfg(t)
	cfg.ContainerIPNet.IPv6 = nil
	cfg.GatewayIP.IPv6 = nil
	cfg.MultiNetwork = true

	contConf := generateContCfgForPolicy(cfg, contLink(), testHostMAC)
	hostConf := generateHostPeerCfgForPolicy(cfg, hostLink(), 1008)

	if len(contConf.SysCtl) != 0 {
		t.Errorf("the container end sets %d sysctls for an IPv4-only pod, want none", len(contConf.SysCtl))
	}
	if len(hostConf.SysCtl) != 0 {
		t.Errorf("the host veth sets %d sysctls for an IPv4-only pod, want none", len(hostConf.SysCtl))
	}
	for _, n := range contConf.Neighs {
		if n.IP.To4() == nil {
			t.Errorf("permanent neighbor for %s on an IPv4-only pod", n.IP)
		}
	}
	// One oif rule and one from-rule, not two from-rules.
	if len(contConf.Rules) != 2 {
		t.Errorf("generated %d container rules for one family, want 2", len(contConf.Rules))
	}
	if len(hostConf.Rules) != 2 {
		t.Errorf("generated %d host rules for one family, want 2 (one to, one from)", len(hostConf.Rules))
	}
}

// TestHostPeerRulesSplitByDirection is the host-side counterpart, and the split
// it pins is what makes same-node pod-to-pod traffic work.
//
// The to-rule has to come first and resolve in main: following it sends the
// packet out of the destination pod's veth and never past the interface. Reverse
// the priorities and the packet goes to the per-interface table, leaves the
// node, and comes back only if the fabric hairpins it.
func TestHostPeerRulesSplitByDirection(t *testing.T) {
	const table = 1042

	conf := generateHostPeerCfgForPolicy(dualPodCfg(t), hostLink(), table)

	if len(conf.Rules) != 4 {
		t.Fatalf("generated %d rules, want 4 (one to and one from, per family)", len(conf.Rules))
	}
	if toContainerPriority >= fromContainerPriority {
		t.Fatalf("the to-priority %d must precede the from-priority %d, or same-node pod-to-pod "+
			"traffic leaves the node", toContainerPriority, fromContainerPriority)
	}

	var to, from int
	for _, r := range conf.Rules {
		switch r.Priority {
		case toContainerPriority:
			to++
			if r.Dst == nil || r.Src != nil {
				t.Errorf("to-rule %s selects src=%v dst=%v, want dst only", r.String(), r.Src, r.Dst)
			}
			if r.Table != unix.RT_TABLE_MAIN {
				t.Errorf("to-rule %s resolves in table %d, want main (%d): inbound has to find the "+
					"pod's own veth, and only main knows about it", r.String(), r.Table, unix.RT_TABLE_MAIN)
			}
		case fromContainerPriority:
			from++
			if r.Src == nil || r.Dst != nil {
				t.Errorf("from-rule %s selects src=%v dst=%v, want src only", r.String(), r.Src, r.Dst)
			}
			if r.Table != table {
				t.Errorf("from-rule %s resolves in table %d, want the interface's %d", r.String(), r.Table, table)
			}
		default:
			t.Errorf("rule %s sits at priority %d, which this datapath never installs", r.String(), r.Priority)
		}
	}
	if to != 2 || from != 2 {
		t.Errorf("got %d to-rules and %d from-rules, want 2 of each", to, from)
	}

	// Host masks throughout: a subnet mask would match every pod on the node, so
	// one pod's teardown would take the rules of all of them.
	for _, r := range conf.Rules {
		if n := ruleAddr(r); n != nil {
			if ones, bits := n.Mask.Size(); ones != bits {
				t.Errorf("rule %s selects %v, want a single host address", r.String(), n)
			}
		}
	}
}

// TestHostPeerAddsNoAddressWithoutExtraRoutes pins that the gateway placeholder
// only lands on the host veth when something in the pod needs it as a next hop.
// Configuring it unconditionally puts the same link-local address on every veth
// on the node.
func TestHostPeerAddsNoAddressWithoutExtraRoutes(t *testing.T) {
	link := hostLink()

	conf := generateHostPeerCfgForPolicy(dualPodCfg(t), link, 1042)

	if len(conf.Addrs) != 0 {
		t.Errorf("configured %d addresses on the host veth with no extra routes, want none: %v",
			len(conf.Addrs), conf.Addrs)
	}
	// The link-scoped host route carries the return path and is not optional.
	if len(conf.Routes) != 2 {
		t.Fatalf("generated %d routes, want one per family", len(conf.Routes))
	}
	for _, r := range conf.Routes {
		if r.Scope != netlink.SCOPE_LINK {
			t.Errorf("the route to %v has scope %d, want link", r.Dst, r.Scope)
		}
		if r.LinkIndex != link.attrs.Index {
			t.Errorf("the route to %v is on link %d, want the host veth's %d", r.Dst, r.LinkIndex, link.attrs.Index)
		}
	}
}
