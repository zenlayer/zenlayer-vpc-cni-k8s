// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package linux

import (
	"fmt"
	"net"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
)

var log = zlog.GetLogger().WithFields(zlog.Fields{"subsys": "CNI"})

func NetlinkFamily(ip net.IP) int {
	if ip.To4() == nil {
		return netlink.FAMILY_V6
	}
	return netlink.FAMILY_V4
}

func LinkSetName(link netlink.Link, name string) error {
	cmd := fmt.Sprintf("ip link set %s name %s", link.Attrs().Name, name)
	log.Info(cmd)
	err := netlink.LinkSetName(link, name)
	if err != nil {
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}

func LinkSetHardwareAddr(link netlink.Link, addr net.HardwareAddr) error {
	cmd := fmt.Sprintf("ip link set %s address %s", link.Attrs().Name, addr)
	log.Info(cmd)
	err := netlink.LinkSetHardwareAddr(link, addr)
	if err != nil {
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}

func LinkAdd(link netlink.Link) error {
	cmd := fmt.Sprintf("ip link add %s type %s", link.Attrs().Name, link.Type())
	log.Info(cmd)
	err := netlink.LinkAdd(link)
	if err != nil {
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}

func LinkSetUp(link netlink.Link) error {
	cmd := fmt.Sprintf("ip link set %s up", link.Attrs().Name)
	log.Info(cmd)
	err := netlink.LinkSetUp(link)
	if err != nil {
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}

func LinkSetDown(link netlink.Link) error {
	cmd := fmt.Sprintf("ip link set %s down", link.Attrs().Name)
	log.Info(cmd)
	err := netlink.LinkSetDown(link)
	if err != nil {
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}

// LinkDel deletes the link. A link that is already gone is not an error, so a
// teardown that already ran can be repeated.
func LinkDel(link netlink.Link) error {
	cmd := fmt.Sprintf("ip link del %s", link.Attrs().Name)
	log.Info(cmd)
	err := netlink.LinkDel(link)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			return nil
		}
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}

func LinkSetMTU(link netlink.Link, mtu int) error {
	cmd := fmt.Sprintf("ip link set %s mtu %d", link.Attrs().Name, mtu)
	log.Info(cmd)
	err := netlink.LinkSetMTU(link, mtu)
	if err != nil {
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}

func AddrDel(link netlink.Link, addr *netlink.Addr) error {
	cmd := fmt.Sprintf("ip addr del %s dev %s", addr.IPNet.String(), link.Attrs().Name)
	log.Info(cmd)
	err := netlink.AddrDel(link, addr)
	if err != nil {
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}

func AddrReplace(link netlink.Link, addr *netlink.Addr) error {
	cmd := fmt.Sprintf("ip addr replace %s dev %s", addr.IPNet.String(), link.Attrs().Name)
	log.Info(cmd)
	err := netlink.AddrReplace(link, addr)
	if err != nil {
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}

func RouteReplace(route *netlink.Route) error {
	cmd := fmt.Sprintf("ip route replace %s", route.String())
	log.Info(cmd)
	err := netlink.RouteReplace(route)
	if err != nil {
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}

func RouteDel(route *netlink.Route) error {
	cmd := fmt.Sprintf("ip route del %s", route.String())
	log.Info(cmd)
	err := netlink.RouteDel(route)
	if err != nil {
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}

func NeighSet(neigh *netlink.Neigh) error {
	cmd := fmt.Sprintf("ip neigh replace %s", neigh.String())
	log.Info(cmd)
	err := netlink.NeighSet(neigh)
	if err != nil {
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}

func RuleAdd(rule *netlink.Rule) error {
	cmd := fmt.Sprintf("ip rule add %s", rule.String())
	log.Info(cmd)
	err := netlink.RuleAdd(rule)
	if err != nil {
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}

// RuleDel deletes the rule, retrying once with IifName and OifName cleared: a
// rule that fails to match with them set may still match without. Every caller
// passes a rule read back from the kernel, and some reach here precisely
// because the interface the rule names is already gone. The retry clears those
// fields on the caller's rule.
func RuleDel(rule *netlink.Rule) error {
	cmd := fmt.Sprintf("ip rule del %s", rule.String())
	log.Info(cmd)
	err := netlink.RuleDel(rule)
	if err != nil {
		rule.IifName = ""
		rule.OifName = ""

		err = netlink.RuleDel(rule)
		if err != nil {
			return fmt.Errorf("error %s, %w", cmd, err)
		}
	}
	return nil
}

func LinkSetNsFd(link netlink.Link, netNS ns.NetNS) error {
	cmd := fmt.Sprintf("ip link set %s netns %s", link.Attrs().Name, netNS.Path())
	log.Info(cmd)
	err := netlink.LinkSetNsFd(link, int(netNS.Fd()))
	if err != nil {
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}

func QdiscReplace(qdisc netlink.Qdisc) error {
	cmd := fmt.Sprintf("tc qdisc replace %s", qdisc.Attrs().String())
	log.Info(cmd)
	err := netlink.QdiscReplace(qdisc)
	if err != nil {
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}
func QdiscDel(qdisc netlink.Qdisc) error {
	cmd := fmt.Sprintf("tc qdisc del %s", qdisc.Attrs().String())
	log.Info(cmd)
	err := netlink.QdiscDel(qdisc)
	if err != nil {
		return fmt.Errorf("error %s, %w", cmd, err)
	}
	return nil
}
