// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package linux

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"

	"github.com/vishvananda/netlink"
)

// GetDeviceNumber returns the host ifindex of the ENI with the given MAC.
// See pickDeviceByMac for why a MAC may match more than one link.
func GetDeviceNumber(mac string) (int32, error) {
	linkList, err := netlink.LinkList()
	if err != nil {
		return 0, fmt.Errorf("error get link list from netlink, %w", err)
	}
	return pickDeviceByMac(linkList, mac)
}

// pickDeviceByMac resolves the ENI MAC reported by the metadata service to a
// host ifindex.
//
// A MAC does not always identify one link. Zenlayer instances are provisioned
// with virtio-net failover for live migration: eth0 (the addressed master),
// eth1 (virtio standby) and eth2 (SR-IOV VF passthrough) all report the same
// MAC, and all three are netlink.Device. Only the master carries the IP and is
// the ENI in any meaningful sense -- the ifindex returned here becomes the
// policy route table id (1000+ifindex) and the route output interface, so
// picking a slave leg silently breaks host and pod connectivity.
//
// Slaves are skipped rather than ranked: on a plain instance every candidate
// has MasterIndex 0, so this is a no-op there.
func pickDeviceByMac(linkList []netlink.Link, mac string) (int32, error) {
	for _, link := range linkList {
		// ignore virtual nic type. eg. ipvlan veth bridge
		//
		// netlink gives a link a concrete type only for a kind it recognizes,
		// so every virtual type worth excluding here -- veth, ipvlan, macvlan,
		// bridge, bond, vlan -- arrives as that type and is rejected. A link
		// the kernel reports without IFLA_LINKINFO at all decodes as Device;
		// one carrying an unrecognized kind decodes as GenericLink. Both are
		// accepted: today a failover master is a Device (virtio_net registers
		// it directly, with no rtnl_link_ops), but were a kernel to start
		// labeling it, rejecting GenericLink would drop the master while
		// MasterIndex rejects its slaves -- no candidate left, and every pod
		// on the node fails to start.
		switch link.(type) {
		case *netlink.Device, *netlink.GenericLink:
		default:
			continue
		}
		attrs := link.Attrs()
		if attrs.HardwareAddr.String() != mac {
			continue
		}
		// enslaved leg of a failover/bond group, not the ENI itself
		if attrs.MasterIndex != 0 {
			continue
		}
		return int32(attrs.Index), nil
	}
	return 0, fmt.Errorf("can't found dev by mac %s", mac)
}

// VethNameForPod derives the host side veth name for a pod interface. The
// digest is truncated to 11 characters because the kernel caps interface names
// at 15; keeping prefix within the remaining 4 is the caller's job, and nothing
// here checks it (see maxVethPrefixLen in pkg/plugin). ifName "eth0" is
// normalised to the empty string, so a pod's default interface and an unnamed
// one derive the same name.
func VethNameForPod(name, namespace, ifName, prefix string) (string, error) {
	// A SHA1 digest is 20 bytes, ample for the 11 hex characters used below.
	h := sha1.New()
	if ifName == "eth0" {
		ifName = ""
	}
	_, err := h.Write([]byte(namespace + "." + name + ifName))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%s", prefix, hex.EncodeToString(h.Sum(nil))[:11]), nil
}
