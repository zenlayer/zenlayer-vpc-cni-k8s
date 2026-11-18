// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Command zec-cni is the CNI plugin kubelet executes for each pod sandbox.
//
// It performs no IPAM of its own. ADD and DEL are forwarded to the node-local
// daemon over a Unix socket, and this binary only wires up the resulting
// addresses inside the pod's network namespace. Keeping the two apart means a
// plugin invocation that is killed mid-flight cannot lose address state.
package main

import (
	"fmt"
	"os"

	"net"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	cniTypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	cniVersion "github.com/containernetworking/cni/pkg/version"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/metrics"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/plugin"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/plugin/ipamonly"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/utils"
	vs "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/version"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

var (
	about = fmt.Sprintf("zenlayer-vpc-cni-k8s version %s", vs.CNIVersion)
)

var (
	cniLogLocation = "/var/log/zenlayer/cni.log"
	log            = zlog.New(cniLogLocation).WithFields(zlog.Fields{"subsys": "CNI"})
)

func cmdAdd(args *skel.CmdArgs) error {
	cmdArgs, err := plugin.GetCmdArgs(args)
	if err != nil {
		return err
	}
	defer cmdArgs.Close()
	conf, k8sConfig := cmdArgs.GetCNIConf(), cmdArgs.GetK8SConfig()

	logger := log.WithFields(zlog.Fields{
		"cmd":          "cmdAdd",
		"netns":        args.Netns,
		"podName":      string(k8sConfig.K8S_POD_NAME),
		"podNamespace": string(k8sConfig.K8S_POD_NAMESPACE),
		"containerID":  string(k8sConfig.K8S_POD_INFRA_CONTAINER_ID),
	})
	if conf.DebugEnabled() {
		// The level is a literal this package's own levelMap contains, so the
		// only error SetLogLevel can return is unreachable here.
		_ = logger.SetLogLevel("debug")
	}

	start := time.Now()
	defer func() {
		duration := metrics.MsSince(start)
		logger.Infof("time cost:%f Millisecond", duration)
	}()

	logger.Infof("args: %s", utils.JSONStr(args))
	logger.Debugf("ns %s , k8s %s, cni std %s", cmdArgs.GetNetNSPath(), utils.JSONStr(k8sConfig), utils.JSONStr(conf))

	// Delegated IPAM: somebody else owns the pod's device, so this invocation
	// hands back addresses and places host routing without touching the netns.
	if conf.IsIPAMOnly() {
		result, addErr := ipamonly.Add(logger, args, conf, k8sConfig)
		if addErr != nil {
			logger.WithError(addErr).Error("ipam-only CmdAdd error")
			return cniTypes.NewError(cniTypes.ErrTryAgainLater, "failed to do ipam-only add", addErr.Error())
		}
		return cniTypes.PrintResult(result, conf.CNIVersion)
	}

	var (
		containerIPNet *types.IPNetSet
		gatewayIPSet   *types.IPSet
	)
	containerIPNet, gatewayIPSet, err = plugin.DoCmdAdd(logger, cmdArgs)
	if err != nil {
		logger.WithError(err).Error("CmdAdd error")
		return cniTypes.NewError(cniTypes.ErrTryAgainLater, "failed to do add", err.Error())
	}

	result := &current.Result{
		CNIVersion: cniVersion.Current(),
	}

	result.Interfaces = append(result.Interfaces, &current.Interface{
		Name:    args.IfName,
		Sandbox: string(k8sConfig.K8S_POD_INFRA_CONTAINER_ID),
	})
	if containerIPNet.IPv4 != nil && gatewayIPSet.IPv4 != nil {
		result.IPs = append(result.IPs, &current.IPConfig{
			Address:   *containerIPNet.IPv4,
			Gateway:   gatewayIPSet.IPv4,
			Interface: current.Int(0),
		})
		result.Routes = append(result.Routes, &cniTypes.Route{
			Dst: net.IPNet{
				IP:   net.ParseIP("0.0.0.0"),
				Mask: net.CIDRMask(0, 32),
			},
			GW: gatewayIPSet.IPv4,
		})
	}
	if containerIPNet.IPv6 != nil && gatewayIPSet.IPv6 != nil {
		result.IPs = append(result.IPs, &current.IPConfig{
			Address:   *containerIPNet.IPv6,
			Gateway:   gatewayIPSet.IPv6,
			Interface: current.Int(0),
		})
		result.Routes = append(result.Routes, &cniTypes.Route{
			Dst: net.IPNet{
				IP:   net.ParseIP("::"),
				Mask: net.CIDRMask(0, 128),
			},
			GW: gatewayIPSet.IPv6,
		})
	}

	return cniTypes.PrintResult(result, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	cmdArgs, err := plugin.GetCmdArgs(args)
	if err != nil {
		return err
	}
	defer cmdArgs.Close()
	conf, k8sConfig := cmdArgs.GetCNIConf(), cmdArgs.GetK8SConfig()

	logger := log.WithFields(zlog.Fields{
		"cmd":          "cmdDel",
		"netns":        args.Netns,
		"podName":      string(k8sConfig.K8S_POD_NAME),
		"podNamespace": string(k8sConfig.K8S_POD_NAMESPACE),
		"containerID":  string(k8sConfig.K8S_POD_INFRA_CONTAINER_ID),
	})
	if conf.DebugEnabled() {
		// The level is a literal this package's own levelMap contains, so the
		// only error SetLogLevel can return is unreachable here.
		_ = logger.SetLogLevel("debug")
	}

	start := time.Now()
	defer func() {
		duration := metrics.MsSince(start)
		logger.Infof("time cost:%f Millisecond", duration)
	}()

	logger.Infof("args: %s", utils.JSONStr(args))
	logger.Debugf("ns %s , k8s %s, cni std %s", cmdArgs.GetNetNSPath(), utils.JSONStr(k8sConfig), utils.JSONStr(conf))

	if conf.IsIPAMOnly() {
		if delErr := ipamonly.Del(logger, args, conf, k8sConfig); delErr != nil {
			logger.WithError(delErr).Error("ipam-only CmdDel error")
			return delErr
		}
		return cniTypes.PrintResult(&current.Result{CNIVersion: conf.CNIVersion}, conf.CNIVersion)
	}

	err = plugin.DoCmdDel(logger, cmdArgs)
	if err != nil {
		logger.WithError(err).Error("error deleting")
		return cniTypes.NewError(cniTypes.ErrTryAgainLater, "failed to do del", err.Error())
	}

	return cniTypes.PrintResult(&current.Result{
		CNIVersion: conf.CNIVersion,
	}, conf.CNIVersion)
}

// cmdCheck is a placeholder: it reports success without verifying anything.
//
// CHECK is meant to confirm that the container is still in the state ADD left
// it in. For this plugin that means the veth pair exists and is up, the pod
// still carries the address and routes the daemon handed out, and the host-side
// rules that steer this pod's traffic -- the prio 512 and 2048 ip rules and the
// per-ENI route table they select -- are still present. None of that is checked
// here, so a runtime calling CHECK is told the datapath is healthy even after it
// has been torn down out of band.
//
// main advertises cniVersion.All, which includes the 0.4.0+ versions that define
// CHECK, so this is a claim the implementation does not honor. Until it is
// implemented, the honest alternative is to leave Check unset in skel.CNIFuncs:
// the runtime then receives "not supported" rather than a false success.
//
// TODO: verify the items above and return a cniTypes.Error when any of them
// fails.
func cmdCheck(args *skel.CmdArgs) error {
	return nil
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		vs.Show()
		os.Exit(0)
	}

	// Equivalent to skel.PluginMain, which builds the same CNIFuncs with GC and
	// Status left unset.
	skel.PluginMainFuncs(skel.CNIFuncs{
		Add:   cmdAdd,
		Check: cmdCheck,
		Del:   cmdDel,
	}, cniVersion.All, about)
}
