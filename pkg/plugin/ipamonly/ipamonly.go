// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package ipamonly implements this binary's delegated-IPAM mode: it allocates
// and releases pod addresses and places the host routing that steers them, and
// creates no device at all.
//
// It exists because the full plugin path and this one overlap only in obtaining
// an address. From there they diverge completely: the full path builds a veth
// pair and writes the pod's own addresses, routes and neighbors, and reports a
// container interface; this path reports the ENI on the host and lets the
// calling plugin build whatever device it likes. The two also disagree on what
// Interfaces[0] means, which is precisely the kind of overload that is cheap to
// keep apart and expensive to debug once merged.
package ipamonly

import (
	"context"
	"fmt"
	"net"

	"github.com/containernetworking/cni/pkg/skel"
	cniTypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/vishvananda/netlink"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/plugin"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/utils"
	linuxUtils "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/utils/linux"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/rpc"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// Add allocates this pod's addresses and returns the CNI result the calling
// plugin consumes.
//
// Order matters in one place: the file lock is taken after the daemon call, not
// around it. The lock protects only host routing state, and the caller is
// blocked on us for the whole invocation -- holding it across a round trip to
// the daemon would serialize pod creation behind one remote call and entangle
// this lock with the caller's own timeout.
func Add(logger zlog.Logger, args *skel.CmdArgs, conf *zecTypes.CNIConf,
	k8sConfig *zecTypes.K8SArgs) (*current.Result, error) {

	ctx, cancel := context.WithTimeout(context.Background(), plugin.CNITimeout)
	defer cancel()

	client, conn, err := plugin.DialIpamClient(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	alloc, err := client.AllocIP(ctx, &rpc.AllocIPRequest{
		Netns:                  args.Netns,
		K8SPodName:             string(k8sConfig.K8S_POD_NAME),
		K8SPodNamespace:        string(k8sConfig.K8S_POD_NAMESPACE),
		K8SPodInfraContainerId: string(k8sConfig.K8S_POD_INFRA_CONTAINER_ID),
		IfName:                 args.IfName,
	})
	if err != nil {
		return nil, fmt.Errorf("ipam-only: alloc ip: %w", err)
	}
	if alloc.Code != rpc.ErrorCode_Success {
		return nil, fmt.Errorf("ipam-only: alloc ip returned %s", alloc.Code)
	}
	if len(alloc.NetConfs) == 0 {
		return nil, errNoAddress
	}

	// Anything that fails from here on has to give the address back. The caller
	// does call DEL on a failed ADD, but only for the invocation as a whole;
	// releasing here keeps the address from being held for a pod that will
	// never exist if that call is lost.
	defer func() {
		if err == nil {
			return
		}
		logger.WithError(err).Error("ipam-only add failed, releasing the address")
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), plugin.CNITimeout)
		defer releaseCancel()
		_, _ = client.ReleaseIP(releaseCtx, &rpc.ReleaseIPRequest{
			K8SPodName:             string(k8sConfig.K8S_POD_NAME),
			K8SPodNamespace:        string(k8sConfig.K8S_POD_NAMESPACE),
			K8SPodInfraContainerId: string(k8sConfig.K8S_POD_INFRA_CONTAINER_ID),
		})
	}()

	needIPv6 := false
	for _, nc := range alloc.NetConfs {
		if nc.GetPodIP().GetIPv6() != "" {
			needIPv6 = true
			break
		}
	}
	if err = linuxUtils.EnsureHostNsConfig(needIPv6); err != nil {
		return nil, fmt.Errorf("ipam-only: setup host ns config: %w", err)
	}

	var (
		parts   []*current.Result
		routing []*hostRouting
	)
	for _, nc := range alloc.NetConfs {
		var eniIndex int32
		eniIndex, err = eniIndexOf(nc)
		if err != nil {
			return nil, err
		}
		var link netlink.Link
		if link, err = netlink.LinkByIndex(int(eniIndex)); err != nil {
			return nil, fmt.Errorf("ipam-only: look up eni by index %d: %w", eniIndex, err)
		}

		var part *current.Result
		if part, err = buildResult(conf.CNIVersion, nc, link.Attrs().Name); err != nil {
			return nil, err
		}
		parts = append(parts, part)

		var gateway *zecTypes.IPSet
		if gateway, err = zecTypes.ToIPSet(nc.GetENIInfo().GetGatewayIP()); err != nil {
			return nil, fmt.Errorf("ipam-only: parse eni gateway: %w", err)
		}
		var primaryIP *zecTypes.IPSet
		if primaryIP, err = zecTypes.ToIPSet(nc.GetENIInfo().GetPrimaryIP()); err != nil {
			return nil, fmt.Errorf("ipam-only: parse eni primary ip: %w", err)
		}
		routing = append(routing, &hostRouting{
			eniIndex:  int(eniIndex),
			podIPs:    podHostIPs(part),
			gateway:   gateway,
			primaryIP: primaryIP,
			isPrimary: nc.GetENIInfo().GetIsPrimary(),
		})
	}

	result := mergeResults(conf.CNIVersion, parts)

	if conf.InstallHostRoutes() {
		var lock *utils.Locker
		if lock, err = utils.GrabFileLock(plugin.LockPath); err != nil {
			return nil, fmt.Errorf("ipam-only: grab %s: %w", plugin.LockPath, err)
		}
		defer func() { _ = lock.Close() }()

		for _, hr := range routing {
			if err = setupHostRouting(logger, hr); err != nil {
				return nil, fmt.Errorf("ipam-only: %w", err)
			}
		}
	} else {
		logger.Warnf("install_host_routes is off: nothing places the per-ENI ip rules for this " +
			"pod, so an address on a secondary interface will leave through the wrong one")
	}

	return result, nil
}

// Del removes this pod's host routing and releases its addresses.
//
// The rules go first. Releasing the address first would let the daemon hand it
// to another pod while rules naming it are still installed, and the two pods'
// rules would then be indistinguishable by address.
//
// Every step runs even after a failure, and the errors are aggregated: DEL is
// replayed by the runtime and must be idempotent, so stopping at the first
// error strands whatever came after it.
func Del(logger zlog.Logger, args *skel.CmdArgs, conf *zecTypes.CNIConf,
	k8sConfig *zecTypes.K8SArgs) error {

	var errList []error

	if conf.InstallHostRoutes() {
		podIPs, err := podIPsFromDaemon(logger, k8sConfig)
		if err != nil {
			errList = append(errList, err)
		}
		if len(podIPs) > 0 {
			lock, lockErr := utils.GrabFileLock(plugin.LockPath)
			if lockErr != nil {
				errList = append(errList, fmt.Errorf("grab %s: %w", plugin.LockPath, lockErr))
			} else {
				if tdErr := teardownHostRouting(logger, podIPs); tdErr != nil {
					errList = append(errList, tdErr)
				}
				_ = lock.Close()
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), plugin.CNITimeout)
	defer cancel()

	client, conn, err := plugin.DialIpamClient(ctx)
	if err != nil {
		errList = append(errList, err)
		return aggregate(errList)
	}
	defer func() { _ = conn.Close() }()

	reply, err := client.ReleaseIP(ctx, &rpc.ReleaseIPRequest{
		K8SPodName:             string(k8sConfig.K8S_POD_NAME),
		K8SPodNamespace:        string(k8sConfig.K8S_POD_NAMESPACE),
		K8SPodInfraContainerId: string(k8sConfig.K8S_POD_INFRA_CONTAINER_ID),
	})
	switch {
	case err != nil:
		errList = append(errList, fmt.Errorf("release ip: %w", err))
	case reply.Code != rpc.ErrorCode_Success:
		errList = append(errList, fmt.Errorf("release ip returned %s", reply.Code))
	}

	return aggregate(errList)
}

// podIPsFromDaemon asks the daemon which addresses this pod holds, so DEL can
// remove rules naming them.
//
// A pod the daemon has no record of yields no addresses and no error: DEL is
// replayed, and the second run legitimately finds nothing. The daemon's own
// reclaimStaleRules covers the case where the record is gone but rules remain.
func podIPsFromDaemon(logger zlog.Logger, k8sConfig *zecTypes.K8SArgs) ([]*net.IPNet, error) {
	ctx, cancel := context.WithTimeout(context.Background(), plugin.CNITimeout)
	defer cancel()

	client, conn, err := plugin.DialIpamClient(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	reply, err := client.GetIPInfo(ctx, &rpc.GetIPInfoRequest{
		K8SPodName:             string(k8sConfig.K8S_POD_NAME),
		K8SPodNamespace:        string(k8sConfig.K8S_POD_NAMESPACE),
		K8SPodInfraContainerId: string(k8sConfig.K8S_POD_INFRA_CONTAINER_ID),
	})
	if err != nil {
		return nil, fmt.Errorf("get ip info: %w", err)
	}

	var ips []*net.IPNet
	for _, nc := range reply.GetNetConfs() {
		ipNet, err := zecTypes.BuildIPNet(nc.GetPodIP(), nc.GetENIInfo().GetSubnet())
		if err != nil {
			logger.WithError(err).Warnf("ipam-only del: skipping an unparsable record")
			continue
		}
		for _, addr := range []*net.IPNet{ipNet.IPv4, ipNet.IPv6} {
			if addr == nil {
				continue
			}
			ips = append(ips, linuxUtils.NewIPNetWithMaxMask(addr))
		}
	}
	return ips, nil
}

// eniIndexOf resolves the interface the daemon picked for this pod.
//
// The daemon reports the ENI by MAC, and the ifindex is what both the route
// table number and the interface name derive from.
func eniIndexOf(nc *rpc.NetConf) (int32, error) {
	mac := nc.GetENIInfo().GetMAC()
	if mac == "" {
		return 0, fmt.Errorf("ipam-only: daemon returned an eni with no mac: %v", nc.GetENIInfo())
	}
	index, err := linuxUtils.GetDeviceNumber(mac)
	if err != nil {
		return 0, fmt.Errorf("ipam-only: no interface with mac %s: %w", mac, err)
	}
	return index, nil
}

func aggregate(errList []error) error {
	if len(errList) == 0 {
		return nil
	}
	msg := ""
	for i, err := range errList {
		if i > 0 {
			msg += "; "
		}
		msg += err.Error()
	}
	return cniTypes.NewError(cniTypes.ErrTryAgainLater, "ipam-only del failed", msg)
}
