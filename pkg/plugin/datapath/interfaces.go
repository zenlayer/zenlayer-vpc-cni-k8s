// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package datapath

import (
	"fmt"
	"net"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/plugin/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"

	k8sErr "k8s.io/apimachinery/pkg/util/errors"

	linuxUtils "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/utils/linux"
)

// DataPath is the datapath driver interface: one implementation per value
// eniip_virtual_type may take in the conflist. newDataPathDriver is what maps
// the two.
type DataPath interface {
	Name() string

	SetupNetwork(logger zlog.Logger, config *types.SetupConfig) (err error)

	Teardown(logger zlog.Logger, cfg *types.TeardownCfg) (err error)
}

// newDataPathDriver builds the driver for a datapath.
//
// This is the only place datapaths are mapped to drivers: SetupDataPath and
// TeardownDataPath go through it instead of switching themselves, so the two
// can never disagree about which driver a datapath gets. A new datapath needs a
// case here as well as a name in types.dataPathNames.
//
// A datapath with no case is a programming mistake, not bad input: the conflist
// value reaches here through types.ParseDataPath, which errors on anything it
// does not recognize. TestEveryDataPathHasADriver catches a missing case before
// it ships.
func newDataPathDriver(dp types.DataPathType) (DataPath, error) {
	switch dp {
	case types.PolicyRoute:
		return NewPolicyRouteDriver(), nil
	default:
		return nil, fmt.Errorf("no driver for datapath %s", dp)
	}
}

// SetupDataPath wires a pod's network namespace up according to setupConfig,
// using the driver registered for the datapath it names. It is the entry point
// CNI ADD calls; the driver decides what "wired up" means.
func SetupDataPath(logger zlog.Logger, setupConfig *types.SetupConfig) error {
	dataPathDriver, err := newDataPathDriver(setupConfig.DP)
	if err != nil {
		return err
	}
	logger = logger.WithFields(zlog.Fields{"dp": dataPathDriver.Name()})
	logger.Infof("DataPath driver %s setup network, %s", dataPathDriver.Name(), setupConfig.String())
	return dataPathDriver.SetupNetwork(logger, setupConfig)
}

// TeardownDataPath undoes SetupDataPath. It is called from CNI DEL, which
// kubelet may retry and may call for a sandbox that is already gone, so drivers
// treat a missing device as success and run every step before reporting the
// errors they collected -- a step that fails must not strand the ones after it.
func TeardownDataPath(logger zlog.Logger, teardownConfig *types.TeardownCfg) error {
	dataPathDriver, err := newDataPathDriver(teardownConfig.DP)
	if err != nil {
		return err
	}
	logger = logger.WithFields(zlog.Fields{"dp": dataPathDriver.Name()})
	logger.Infof("DataPath driver %s teardown network, %s", dataPathDriver.Name(), teardownConfig.String())
	return dataPathDriver.Teardown(logger, teardownConfig)
}

// CleanIPRules deletes the ip rules left behind by interfaces that are gone.
// Deleting a link does not take its rules with it, and nothing else on the node
// knows they belonged to a pod.
//
// Only priorities 512 and 2048 are considered -- toContainerPriority and
// fromContainerPriority, the two this datapath installs per pod -- so rules
// belonging to anything else on the node are left alone. The first pass deletes
// rules naming an iif or oif that no longer resolves, remembering the address
// each selected, and returns as soon as a lookup or a delete fails. The second
// pass deletes, at those same priorities, the rules selecting one of those
// addresses without naming an interface: the address is what ties them to the
// link that vanished. It runs from a defer so it still happens when the first
// pass returned early, and it ignores delete failures.
func CleanIPRules() (err error) {
	var rules []netlink.Rule
	rules, err = netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		return err
	}

	var ipNets []*net.IPNet
	defer func() {
		for _, r := range rules {
			if r.Priority != 512 && r.Priority != 2048 {
				continue
			}
			if r.IifName != "" || r.OifName != "" {
				continue
			}
			found := false

			for _, ipNet := range ipNets {
				if r.Dst != nil {
					if r.Dst.String() == ipNet.String() {
						found = true
						break
					}
				}
				if r.Src != nil {
					if r.Src.String() == ipNet.String() {
						found = true
						break
					}
				}
			}
			if !found {
				continue
			}
			_ = linuxUtils.RuleDel(&r)
		}
	}()
	for _, r := range rules {
		if r.Priority != 512 && r.Priority != 2048 {
			continue
		}
		name := r.IifName
		if name == "" {
			name = r.OifName
		}
		if name == "" {
			continue
		}
		_, err = netlink.LinkByName(name)
		if err != nil {
			if _, ok := err.(netlink.LinkNotFoundError); !ok {
				return err
			}
			err = linuxUtils.RuleDel(&r)
			if err != nil {
				return err
			}
			var ipNet *net.IPNet
			if r.Dst != nil {
				ipNet = r.Dst
			}
			if r.Src != nil {
				ipNet = r.Src
			}
			if ipNet != nil {
				ipNets = append(ipNets, ipNet)
			}
		}
	}

	return nil
}

// GenericTearDown reclaims what can be reclaimed from the pod's namespace,
// without knowing which datapath set it up.
//
// Best effort: errors are collected and aggregated at the end instead of
// returned at the first failure, and CleanIPRules runs even when the in-netns
// work failed. An ns.NSPathNotExistErr is dropped rather than reported: DEL is
// replayed, as the CNI spec requires, so the runtime can tear the netns down
// between the plugin opening the path and this running. DoCmdDel skips the call
// altogether when the path was already gone.
//
// Inside the namespace every link but lo is set down, with the set-down error
// discarded; only ipvlan links are then also deleted, and it is that delete
// whose error is collected. The only datapath here builds a veth pair, not an
// ipvlan link, so nothing this plugin set up reaches that branch -- and
// deleting it would be moot anyway, since destroying the namespace takes the
// container end with it. CleanIPRules is the step that always matters: ip rules
// live in the host's tables, so nothing about the namespace's fate reaches
// them.
func GenericTearDown(netNS ns.NetNS) error {
	var errList []error
	err := netNS.Do(func(netNS ns.NetNS) error {
		linkList, err := netlink.LinkList()
		if err != nil {
			return fmt.Errorf("error get link list from netlink, %w", err)
		}
		for _, l := range linkList {
			if l.Attrs().Name == "lo" {
				continue
			}
			_ = linuxUtils.LinkSetDown(l)
			switch l.(type) {
			case *netlink.IPVlan:
				errList = append(errList, linuxUtils.LinkDel(l))
			default:
				continue
			}
		}
		return nil
	})
	if err != nil {
		if _, ok := err.(ns.NSPathNotExistErr); !ok {
			errList = append(errList, err)
		}
	}
	errList = append(errList, CleanIPRules())
	return k8sErr.NewAggregate(errList)
}
