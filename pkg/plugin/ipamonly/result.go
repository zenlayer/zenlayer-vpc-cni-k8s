// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package ipamonly

import (
	"fmt"
	"net"

	cniTypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/rpc"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// buildResult turns one daemon reply into the CNI Result the calling plugin
// reads back.
//
// The shape differs from the full plugin's result in a way that is easy to get
// backwards. Here Interfaces[0] is the ENI on the host, not a device inside the
// pod, and its Sandbox must stay empty -- an empty sandbox is what marks an
// interface as the host-side uplink for the address that follows. Naming the
// pod's device here instead would have the caller treat a container interface
// as the uplink.
//
// Routes are left out: a delegated IPAM result contributes addresses and a
// gateway, and the caller builds the pod's routes itself from the device it
// created. Anything set here would be dropped.
func buildResult(cniVersion string, nc *rpc.NetConf, eniName string) (*current.Result, error) {
	if nc.GetPodIP() == nil || nc.GetENIInfo() == nil {
		return nil, fmt.Errorf("daemon returned no pod address or eni info: %v", nc)
	}

	podIPNet, err := zecTypes.BuildIPNet(nc.GetPodIP(), nc.GetENIInfo().GetSubnet())
	if err != nil {
		return nil, fmt.Errorf("build pod ip net: %w", err)
	}
	gateway, err := zecTypes.ToIPSet(nc.GetENIInfo().GetGatewayIP())
	if err != nil {
		return nil, fmt.Errorf("parse eni gateway: %w", err)
	}

	result := &current.Result{CNIVersion: cniVersion}
	result.Interfaces = append(result.Interfaces, &current.Interface{
		Name: eniName,
		// Mac is left empty on purpose. It is not needed to identify the
		// uplink, and on this platform an interface that failed over keeps the
		// MAC of the port it replaced, so a caller that prefers the MAC over
		// the name would resolve to the wrong device.
		//
		// Sandbox is left empty too, and that one is load-bearing: see above.
	})

	appendIP := func(addr *net.IPNet, gw net.IP) {
		if addr == nil || gw == nil {
			return
		}
		result.IPs = append(result.IPs, &current.IPConfig{
			Address:   *addr,
			Gateway:   gw,
			Interface: current.Int(0),
		})
	}
	appendIP(podIPNet.IPv4, gateway.IPv4)
	appendIP(podIPNet.IPv6, gateway.IPv6)

	if len(result.IPs) == 0 {
		return nil, fmt.Errorf("daemon returned a pod address with no usable gateway: %v", nc)
	}
	return result, nil
}

// mergeResults folds per-interface results into the single result the caller
// expects, keeping each interface's addresses pointed at their own interface.
func mergeResults(cniVersion string, parts []*current.Result) *current.Result {
	merged := &current.Result{CNIVersion: cniVersion}
	for _, part := range parts {
		base := len(merged.Interfaces)
		merged.Interfaces = append(merged.Interfaces, part.Interfaces...)
		for _, ip := range part.IPs {
			shifted := *ip
			if ip.Interface != nil {
				shifted.Interface = current.Int(base + *ip.Interface)
			}
			merged.IPs = append(merged.IPs, &shifted)
		}
	}
	return merged
}

// podHostIPs narrows a result's addresses to host masks, which is the form both
// the ip rules and their later removal are keyed on.
func podHostIPs(result *current.Result) []*net.IPNet {
	ips := make([]*net.IPNet, 0, len(result.IPs))
	for _, ip := range result.IPs {
		addr := ip.Address
		bits := 32
		if addr.IP.To4() == nil {
			bits = 128
		}
		ips = append(ips, &net.IPNet{IP: addr.IP, Mask: net.CIDRMask(bits, bits)})
	}
	return ips
}

// errNoAddress is returned when the daemon replies successfully but with
// nothing usable, which the caller must treat as a failure rather than hand an
// empty result to a plugin that would then build a pod with no address.
var errNoAddress = cniTypes.NewError(cniTypes.ErrInternal,
	"ipam returned no usable address", "the daemon replied with no NetConf entries")
