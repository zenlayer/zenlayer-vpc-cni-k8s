// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package nic

import (
	"fmt"

	"github.com/vishvananda/netlink"

	linuxUtils "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/utils/linux"
)

// Conf is the desired state of one link. Every field is optional; a zero value
// means "leave this alone" rather than "set it to zero" -- an MTU of 0 in
// particular is what keeps the instance's own interface from being resized.
type Conf struct {
	IfName string
	MTU    int

	Addrs  []*netlink.Addr
	Routes []*netlink.Route
	Rules  []*netlink.Rule
	Neighs []*netlink.Neigh

	SysCtl map[string][]string
}

func Setup(link netlink.Link, conf *Conf) error {
	var err error
	if conf.IfName != "" {
		changed, err := linuxUtils.EnsureLinkName(link, conf.IfName)
		if err != nil {
			return err
		}
		if changed {
			link, err = netlink.LinkByIndex(link.Attrs().Index)
			if err != nil {
				return err
			}
		}
	}

	if conf.MTU > 0 {
		_, err = linuxUtils.EnsureLinkMTU(link, conf.MTU)
		if err != nil {
			return err
		}
	}

	for _, v := range conf.SysCtl {
		if len(v) != 2 {
			return fmt.Errorf("sysctl config err")
		}
		err = linuxUtils.EnsureConf(v[0], v[1])
		if err != nil {
			return err
		}
	}

	for _, addr := range conf.Addrs {
		_, err = linuxUtils.EnsureAddr(link, addr)
		if err != nil {
			return err
		}
	}

	_, err = linuxUtils.EnsureLinkUp(link)
	if err != nil {
		return err
	}

	for _, neigh := range conf.Neighs {
		_, err = linuxUtils.EnsureNeigh(neigh)
		if err != nil {
			return err
		}
	}

	for _, route := range conf.Routes {
		_, err = linuxUtils.EnsureRoute(route)
		if err != nil {
			return err
		}
	}

	for _, rule := range conf.Rules {
		_, err = linuxUtils.EnsureIPRule(rule)
		if err != nil {
			return err
		}
	}
	return nil
}
