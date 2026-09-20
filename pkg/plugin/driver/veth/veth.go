// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package veth

import (
	"crypto/sha1"
	"fmt"
	"net"

	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	linuxUtils "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/utils/linux"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
)

var log = zlog.GetLogger().WithFields(zlog.Fields{"subsys": "CNI"})

// Veth describes the veth pair connecting a pod netns to the host.
type Veth struct {
	// IfName is the name the container end takes inside the netns.
	IfName string
	// PeerName is the name the host end keeps in the init netns.
	PeerName string
	HwAddr   net.HardwareAddr
	MTU      int
}

// Setup creates the veth pair, moving the container end into netNS.
//
// The host end keeps PeerName, which the caller derives from the pod's
// identity, so DEL recomputes the same name instead of having to remember it.
func Setup(cfg *Veth, netNS ns.NetNS) error {
	// Clear a leftover pair from a previous, failed ADD. Since PeerName is
	// deterministic, a retry lands on the same name.
	//
	// The nil case must be handled in the same chain as the error case: a nil
	// error never satisfies the LinkNotFoundError assertion, so checking it
	// separately would return nil right here and silently skip the creation
	// below, leaving the container without a nic while reporting success.
	peer, err := netlink.LinkByName(cfg.PeerName)
	if err == nil {
		if err = linuxUtils.LinkDel(peer); err != nil {
			return err
		}
	} else if _, ok := err.(netlink.LinkNotFoundError); !ok {
		return err
	}

	// Create the container end under a random name and rename it after it has
	// been moved, so it cannot collide with an interface already present in the
	// target netns.
	contLinkName, err := ip.RandomVethName()
	if err != nil {
		return err
	}

	v := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			MTU:       cfg.MTU,
			Name:      contLinkName,
			Namespace: netlink.NsFd(int(netNS.Fd())),
		},
		PeerName: cfg.PeerName,
	}
	if cfg.HwAddr != nil {
		v.HardwareAddr = cfg.HwAddr
	}
	if err = linuxUtils.LinkAdd(v); err != nil {
		return err
	}

	// Pin the host end's MAC before anyone reads it.
	//
	// systemd-udevd applies MACAddressPolicy=persistent (99-default.link) to
	// every new link, replacing the kernel's initial random address shortly
	// after the device appears. The caller reads this MAC immediately after
	// Setup returns and pins it into the container's neighbor table as the
	// next hop, so without this it pins an address that ceases to exist a
	// moment later and the pod can send nothing -- while everything else, the
	// addresses, routes and rules, looks correct.
	//
	// Assigning it explicitly both fixes the value and stops udev touching it:
	// the kernel then reports the address as administratively set, which the
	// persistent policy skips. Deriving it from the peer name keeps it stable
	// across a retry that recreates the pair.
	peerLink, err := netlink.LinkByName(cfg.PeerName)
	if err != nil {
		return fmt.Errorf("error find host veth %s after create, %w", cfg.PeerName, err)
	}
	if err = linuxUtils.LinkSetHardwareAddr(peerLink, HostMACFor(cfg.PeerName)); err != nil {
		return err
	}
	// Configure the interface we just created rather than leaving it to the
	// sweep at the start of the *next* pod's ADD. Until this, a fresh host veth
	// kept whatever systemd-sysctl replayed onto it from the host's sysctl.d
	// (rp_filter=2 on a stock Ubuntu) for as long as no other pod was created.
	//
	// Not fatal. The durable fix is the sysctl.d drop-in the init container
	// installs, which makes the host apply the right value itself; this is only
	// a backstop to it. EnsureLinkNetConf also writes IPv6 keys unconditionally,
	// so on a host with IPv6 compiled out it fails without anything being wrong.
	if cfgErr := linuxUtils.EnsureLinkNetConf(cfg.PeerName); cfgErr != nil {
		log.Warnf("Cannot apply net config to host veth %s: %v", cfg.PeerName, cfgErr)
	}

	return netNS.Do(func(ns.NetNS) error {
		contLink, innerErr := netlink.LinkByName(contLinkName)
		if innerErr != nil {
			return innerErr
		}
		_, innerErr = linuxUtils.EnsureLinkName(contLink, cfg.IfName)
		return innerErr
	})
}

// HostMACFor derives the host end's MAC from its interface name.
//
// The result is a locally administered unicast address, so it cannot collide
// with a real vendor address, and it is a pure function of the name, so a
// retry that recreates the pair arrives at the same value.
func HostMACFor(peerName string) net.HardwareAddr {
	sum := sha1.Sum([]byte(peerName))
	mac := make(net.HardwareAddr, 6)
	copy(mac, sum[:6])
	mac[0] = (mac[0] | 0x02) &^ 0x01 // locally administered, unicast
	return mac
}
