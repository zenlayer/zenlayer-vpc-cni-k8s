// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package plugin implements the CNI ADD and DEL commands. CHECK is accepted and
// returns success without verifying anything.
//
// Each command turns into a request to the node-local daemon, which owns all
// address state, and then a datapath setup or teardown for the pod. Because
// kubelet may run several invocations at once, the parts that touch shared node
// state are serialized across processes by a file lock.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	cniTypes "github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/plugins/pkg/ns"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/plugin/datapath"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/plugin/types"
	utils "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/utils"
	linuxUtils "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/utils/linux"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/rpc"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

const (
	// defaultSocketPath must match the daemon's listen address
	// (pkg/daemon/daemon.go defaultSocketPath).
	defaultSocketPath = "/var/run/zenlayer/cni.socket"
	// defaultVethPrefix names the host end of each pod's veth pair. It must be
	// short (see maxVethPrefixLen) and must not collide with a prefix anything
	// else on the node claims, so a host veth can be attributed to this CNI on
	// sight.
	//
	// Under Cilium chaining: Cilium's device auto-detection skips only a fixed
	// prefix list (cilium_, lo, lxc, cni, docker), so interfaces named "eni…"
	// are taken for host devices and get its cil_from_netdev/cil_to_netdev
	// programs attached (cilium#30563). Pin the real uplinks there
	// (--set devices='eth+'), or set "veth_prefix" in the conflist to one of the
	// prefixes Cilium skips.
	defaultVethPrefix = "eni"
	// maxVethPrefixLen bounds the prefix so the generated name fits:
	// VethNameForPod appends 11 hex chars and linux caps interface names at 15.
	maxVethPrefixLen    = 4
	defaultDialTimeout  = 10 * time.Second
	defaultCniTimeout   = 120 * time.Second
	defaultEventTimeout = 10 * time.Second
	defaultMTU          = 1500
	// zecCNILock guards the datapath work in ADD and DEL. What it protects is the
	// state pods share rather than their own namespaces: the host's ip rule table
	// and the routing table per network interface, both of which every pod on the
	// node writes into.
	//
	// It has to be a file lock, not a mutex, because the contending parties are
	// separate processes -- kubelet runs one plugin invocation per pod and may run
	// several at once. Two of them installing or removing rules concurrently can
	// interleave into a table that routes neither pod correctly.
	//
	// The in-netns teardown is held under the same lock even though it only
	// touches one namespace, so that a DEL cannot unwire a pod while an ADD is
	// halfway through wiring another. Note the ADD region is not free of remote
	// calls: it reports events to the daemon while holding the lock.
	zecCNILock = "/var/run/eni/zec_cni.lock"
)

// DialIpamClient connects to the node-local daemon. Exported for the IPAM-only
// path in pkg/plugin/ipamonly, which talks to the same daemon over the same
// socket; the two paths differ in what they do with the reply, not in how they
// obtain it.
func DialIpamClient(ctx context.Context) (rpc.CNIIpamClient, *grpc.ClientConn, error) {
	return getCNIIpamClient(ctx)
}

// LockPath is the file lock guarding host routing state. Exported for the same
// reason as DialIpamClient: both paths write the host's ip rules and per-ENI
// route tables, so they must contend for one lock rather than two.
const LockPath = zecCNILock

// CNITimeout bounds a daemon call made from a plugin invocation.
const CNITimeout = defaultCniTimeout

func getCNIIpamClient(ctx context.Context) (rpc.CNIIpamClient, *grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultDialTimeout)
	defer cancel()
	// Kept on DialContext + WithBlock deliberately: grpc.NewClient has no
	// equivalent of WithBlock. It returns before the connection is up and dials
	// on the first RPC, which moves the failure out of the timeout above and
	// into the middle of a CNI operation. Migrating means reworking that timeout.
	//nolint:staticcheck // SA1019: no NewClient equivalent for WithBlock; see above
	conn, err := grpc.DialContext(ctx, defaultSocketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(
			func(ctx context.Context, s string) (net.Conn, error) {
				unixAddr, err := net.ResolveUnixAddr("unix", s)
				if err != nil {
					return nil, err
				}
				d := net.Dialer{}
				return d.DialContext(ctx, "unix", unixAddr.String())
			}),
		//nolint:staticcheck // SA1019: see the note above getCNIIpamClient's dial
		grpc.WithBlock(),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  time.Second,
				Multiplier: 1,
				MaxDelay:   time.Second,
			},
		}),
	)

	if err != nil {
		// This is the message every pod on a node gets when the daemon is not
		// answering, so it has to name the socket and say where to look: the
		// daemon owns all IPAM, and no pod can be set up until it replies.
		return nil, nil, cniTypes.NewError(cniTypes.ErrTryAgainLater,
			fmt.Sprintf("cannot reach the ipam daemon on %s", defaultSocketPath),
			fmt.Sprintf("%v -- check that the zenlayer-vpc-cni daemonset has a running pod on "+
				"this node and that it has created %s", err, defaultSocketPath))
	}

	client := rpc.NewCNIIpamClient(conn)
	return client, conn, nil
}

// GetCmdArgs parses the CNI stdin config and the kubernetes args, applies the
// defaults for the optional config fields, and opens the pod netns.
func GetCmdArgs(args *skel.CmdArgs) (*cniCmdArgs, error) {
	// A vanished netns is not fatal here. CNI DEL has to stay idempotent --
	// kubelet replays it after a restart, and containerd may have torn the
	// netns down already -- so netNS is left nil and DoCmdDel skips the
	// in-netns work. DoCmdAdd rejects a nil netNS explicitly.
	netNS, err := ns.GetNS(args.Netns)
	if err != nil && !isNSPathNotExist(err) {
		return nil, err
	}

	var conf zecTypes.CNIConf
	if err = json.Unmarshal(args.StdinData, &conf); err != nil {
		return nil, cniTypes.NewError(cniTypes.ErrDecodingFailure, "failed to parse network config", err.Error())
	}
	if conf.MTU == 0 {
		conf.MTU = defaultMTU
	}
	if conf.HostVethPrefix == "" {
		conf.HostVethPrefix = defaultVethPrefix
	}
	if len(conf.HostVethPrefix) > maxVethPrefixLen {
		return nil, cniTypes.NewError(cniTypes.ErrDecodingFailure, "invalid network config",
			fmt.Sprintf("veth_prefix %q is longer than %d characters", conf.HostVethPrefix, maxVethPrefixLen))
	}

	// Refuse an unrecognised zec_mode here rather than at the dispatch, so both
	// ADD and DEL reject it identically and neither can act on a config the
	// other would have turned away.
	if err = conf.ValidateIPAM(); err != nil {
		return nil, cniTypes.NewError(cniTypes.ErrDecodingFailure, "invalid network config", err.Error())
	}

	var k8sArgs zecTypes.K8SArgs
	if err = cniTypes.LoadArgs(args.Args, &k8sArgs); err != nil {
		return nil, cniTypes.NewError(cniTypes.ErrDecodingFailure, "failed to parse args", err.Error())
	}

	return &cniCmdArgs{
		conf:      &conf,
		netNS:     netNS,
		k8sArgs:   &k8sArgs,
		inputArgs: args,
	}, nil
}

type cniCmdArgs struct {
	conf      *zecTypes.CNIConf
	netNS     ns.NetNS
	k8sArgs   *zecTypes.K8SArgs
	inputArgs *skel.CmdArgs
}

func (args *cniCmdArgs) GetCNIConf() *zecTypes.CNIConf {
	if args == nil || args.conf == nil {
		return nil
	}
	return args.conf
}

func (args *cniCmdArgs) GetK8SConfig() *zecTypes.K8SArgs {
	if args == nil || args.k8sArgs == nil {
		return nil
	}
	return args.k8sArgs
}

func (args *cniCmdArgs) GetInputArgs() *skel.CmdArgs {
	if args == nil {
		return nil
	}
	return args.inputArgs
}

func (args *cniCmdArgs) GetNetNSPath() string {
	if args == nil || args.netNS == nil {
		return ""
	}
	return args.netNS.Path()
}

func (args *cniCmdArgs) Close() error {
	if args == nil || args.netNS == nil {
		return nil
	}
	return args.netNS.Close()
}

func isNSPathNotExist(err error) bool {
	if err == nil {
		return false
	}
	var _, ok = err.(ns.NSPathNotExistErr)
	return ok
}

// DoCmdAdd handles CNI ADD. All IPAM happens in the daemon; this asks it for
// the pod's addresses, builds the datapath for every interface in the reply and
// returns the address and gateway of the one kubelet asked for. Any failure
// after the allocation releases it again.
func DoCmdAdd(logger zlog.Logger, cmdArgs *cniCmdArgs) (containerIPNet *zecTypes.IPNetSet, gatewayIPSet *zecTypes.IPSet, err error) {
	var conf, cniNetns, k8sConfig, args = cmdArgs.conf, cmdArgs.netNS, cmdArgs.k8sArgs, cmdArgs.inputArgs
	if cniNetns == nil {
		return nil, nil, fmt.Errorf("netns %s does not exist", cmdArgs.inputArgs.Netns)
	}
	var (
		ctx    context.Context
		cancel context.CancelFunc
		client rpc.CNIIpamClient
		conn   *grpc.ClientConn
	)
	ctx, cancel = context.WithTimeout(context.Background(), defaultCniTimeout)
	defer cancel()
	client, conn, err = getCNIIpamClient(ctx)
	if err != nil {
		return
	}
	defer conn.Close()

	start := time.Now()
	defer func() {
		eventCtx, cancel := context.WithTimeout(context.Background(), defaultEventTimeout)
		defer cancel()
		if err != nil {
			_, _ = client.RecordEvent(eventCtx, &rpc.RecordEventRequest{
				EventTarget:     rpc.EventTarget_EventTargetPod,
				K8SPodName:      string(k8sConfig.K8S_POD_NAME),
				K8SPodNamespace: string(k8sConfig.K8S_POD_NAMESPACE),
				EventType:       rpc.EventType_EventTypeWarning,
				Reason:          zecTypes.EventReasonAllocIPFailed,
				Message:         err.Error(),
			})
		} else {
			_, _ = client.RecordEvent(eventCtx, &rpc.RecordEventRequest{
				EventTarget:     rpc.EventTarget_EventTargetPod,
				K8SPodName:      string(k8sConfig.K8S_POD_NAME),
				K8SPodNamespace: string(k8sConfig.K8S_POD_NAMESPACE),
				EventType:       rpc.EventType_EventTypeNormal,
				Reason:          "AllocIPSucceed",
				Message:         fmt.Sprintf("Alloc IP %s took %s", containerIPNet.String(), time.Since(start).String()),
			})
		}
	}()

	allocResult, err := client.AllocIP(ctx, &rpc.AllocIPRequest{
		Netns:                  args.Netns,
		K8SPodName:             string(k8sConfig.K8S_POD_NAME),
		K8SPodNamespace:        string(k8sConfig.K8S_POD_NAMESPACE),
		K8SPodInfraContainerId: string(k8sConfig.K8S_POD_INFRA_CONTAINER_ID),
		IfName:                 args.IfName,
	})
	if err != nil {
		err = fmt.Errorf("cmdAdd: error alloc ip %w", err)
		return
	}
	if allocResult.Code != rpc.ErrorCode_Success {
		err = fmt.Errorf("cmdAdd: alloc ip return not success, code: %s", allocResult.Code)
		return
	}

	defer func() {
		if err != nil {
			logger.Error(err)
			releaseCtx, cancel := context.WithTimeout(context.Background(), defaultCniTimeout)
			defer cancel()
			_, _ = client.ReleaseIP(releaseCtx, &rpc.ReleaseIPRequest{
				K8SPodName:             string(k8sConfig.K8S_POD_NAME),
				K8SPodNamespace:        string(k8sConfig.K8S_POD_NAMESPACE),
				K8SPodInfraContainerId: string(k8sConfig.K8S_POD_INFRA_CONTAINER_ID),
			})
		}
	}()

	// Ask for the IPv6 knobs when any interface in this reply carries an IPv6
	// address, rather than off the daemon's stack setting: during an ipv4->dual
	// rollout the two disagree, and it is the reply that says what this pod
	// actually needs.
	needIPv6 := false
	for _, nc := range allocResult.NetConfs {
		if nc.GetPodIP().GetIPv6() != "" {
			needIPv6 = true
			break
		}
	}
	err = linuxUtils.EnsureHostNsConfig(needIPv6)
	if err != nil {
		err = fmt.Errorf("error setup host ns configs, %w", err)
		return
	}

	multiNetwork := len(allocResult.NetConfs) > 1

	l, err := utils.GrabFileLock(zecCNILock)
	if err != nil {
		return
	}
	defer l.Close()

	for _, netConf := range allocResult.NetConfs {
		var setupCfg *types.SetupConfig
		setupCfg, err = generateSetupConfig(args, netConf, conf, cniNetns, allocResult.IPType)
		if err != nil {
			err = fmt.Errorf("error parse config, %w", err)
			return
		}
		setupCfg.HostVETHName, err = linuxUtils.VethNameForPod(string(k8sConfig.K8S_POD_NAME),
			string(k8sConfig.K8S_POD_NAMESPACE), netConf.IfName, conf.HostVethPrefix)
		if err != nil {
			return
		}
		setupCfg.MultiNetwork = multiNetwork
		logger.Debugf("netConf: %v, setupCfg %#v", netConf, setupCfg)

		err = datapath.SetupDataPath(logger, setupCfg)
		if err != nil {
			_, _ = client.RecordEvent(ctx, &rpc.RecordEventRequest{
				EventTarget:     rpc.EventTarget_EventTargetPod,
				K8SPodName:      string(k8sConfig.K8S_POD_NAME),
				K8SPodNamespace: string(k8sConfig.K8S_POD_NAMESPACE),
				EventType:       rpc.EventType_EventTypeWarning,
				Reason:          "SetupNetworkFailed",
				Message:         fmt.Sprintf("datapath %s seems unavailable, err: %s", setupCfg.DP, err),
			})
			return
		}
		if setupCfg.ContainerIfName == args.IfName {
			containerIPNet = setupCfg.ContainerIPNet
			gatewayIPSet = setupCfg.GatewayIP
		}
	}

	if containerIPNet == nil || gatewayIPSet == nil {
		err = fmt.Errorf("eth0 config is missing")
		return
	}
	return
}

// DoCmdDel handles CNI DEL: tear down the datapath, then ask the daemon to
// release the addresses. It is idempotent, as the CNI spec requires.
func DoCmdDel(logger zlog.Logger, cmdArgs *cniCmdArgs) error {
	var conf, cniNetns, k8sConfig = cmdArgs.conf, cmdArgs.netNS, cmdArgs.k8sArgs

	ctx, cancel := context.WithTimeout(context.Background(), defaultCniTimeout)
	defer cancel()
	client, conn, err := getCNIIpamClient(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if cniNetns != nil {
		l, err := utils.GrabFileLock(zecCNILock)
		if err != nil {
			return err
		}
		err = datapath.GenericTearDown(cniNetns)
		if err != nil {
			_ = l.Close()
			logger.Errorf("error teardown %s", err.Error())
			return err
		}
		_ = l.Close()
	} else {
		// Only the in-netns half is skipped. The host veth, the ip rules and the
		// address are still reclaimed below, from the stored record.
		logger.Infof("Netns already gone, skipping in-netns teardown; host-side teardown and release still run")
	}

	getResult, err := client.GetIPInfo(ctx, &rpc.GetIPInfoRequest{
		K8SPodName:             string(k8sConfig.K8S_POD_NAME),
		K8SPodNamespace:        string(k8sConfig.K8S_POD_NAMESPACE),
		K8SPodInfraContainerId: string(k8sConfig.K8S_POD_INFRA_CONTAINER_ID),
	})
	if err != nil {
		return fmt.Errorf("error get ip from ipam, pod %s/%s, %w", string(k8sConfig.K8S_POD_NAMESPACE), string(k8sConfig.K8S_POD_NAME), err)
	}

	if getResult.Code != rpc.ErrorCode_Success {
		// No record means nothing left to tear down, which is success. The CNI
		// spec requires DEL to be idempotent, and kubelet leans on that: its
		// sandbox GC calls StopPodSandbox before RemovePodSandbox, so every pod
		// gets a second DEL after the first one already removed the record.
		// Failing that second call leaves the sandbox un-removable: kubelet
		// retries forever and one dead sandbox accumulates per deleted pod.
		if getResult.Code == rpc.ErrorCode_CRDNotFound || getResult.Code == rpc.ErrorCode_PodNotFound {
			logger.Infof("No ip record for this pod, nothing to tear down (code %s)", getResult.Code)
			// Still ask the daemon to release. GetIPInfo answers PodNotFound
			// both when no record exists and when the record belongs to a
			// different sandbox. A replayed DEL hits the second case, and
			// ReleaseIP declines it on the same sandbox check. The first case is
			// what a lost record store leaves behind: the pool still attributes
			// the address to this pod, adopted from the API server by PreStart,
			// and nothing else releases it or removes the ip rules naming it.
			//
			// Failure must not propagate: DEL has to stay idempotent, and
			// reconcile remains the backstop.
			releaseWithoutRecord(ctx, logger, client, k8sConfig)
			return nil
		}
		return fmt.Errorf("cmdDel: get ip info return not success, code: %s", getResult.Code)
	}
	// Host-side teardown runs whether or not the netns is still reachable. The
	// pod's ip rules live in the host's rule table, not in the pod's namespace:
	// destroying the netns does not touch them, and deleting the host veth does
	// not take them with it either, so they have to be removed explicitly.
	// Skipping this leaks the host veth and those rules -- a replayed DEL
	// returns early at PodNotFound, so there is no second chance.
	//
	// Nothing here needs the namespace: the teardown driver never enters
	// cfg.Netns, the host veth name is recomputed from the pod identity, and the
	// addresses come from the stored record. That is why the nil-netns case
	// above can fall through to this same teardown.
	err = func() error {
		l, lockErr := utils.GrabFileLock(zecCNILock)
		if lockErr != nil {
			return lockErr
		}
		defer l.Close()
		return tearDownNetConfs(logger, getResult.NetConfs, getResult.IPType, conf, k8sConfig, cniNetns)
	}()
	if err != nil {
		return err
	}
	reply, err := client.ReleaseIP(ctx, &rpc.ReleaseIPRequest{
		K8SPodName:             string(k8sConfig.K8S_POD_NAME),
		K8SPodNamespace:        string(k8sConfig.K8S_POD_NAMESPACE),
		K8SPodInfraContainerId: string(k8sConfig.K8S_POD_INFRA_CONTAINER_ID),
	})

	if err != nil || reply.GetCode() != rpc.ErrorCode_Success {
		return fmt.Errorf("error release ip for pod, maybe cause resource leak: %v, %v", err, reply)
	}
	return nil
}

// releaseWithoutRecord asks the daemon to release a pod the record store does
// not know about, and swallows the outcome.
//
// Split out from DoCmdDel so it can be tested on its own: reaching this branch
// through DoCmdDel needs a live daemon socket, and it is the branch carrying
// the risk of an address held forever.
func releaseWithoutRecord(ctx context.Context, logger zlog.Logger, client rpc.CNIIpamClient, k8sConfig *zecTypes.K8SArgs) {
	reply, err := client.ReleaseIP(ctx, &rpc.ReleaseIPRequest{
		K8SPodName:             string(k8sConfig.K8S_POD_NAME),
		K8SPodNamespace:        string(k8sConfig.K8S_POD_NAMESPACE),
		K8SPodInfraContainerId: string(k8sConfig.K8S_POD_INFRA_CONTAINER_ID),
	})
	if err != nil || reply.GetCode() != rpc.ErrorCode_Success {
		logger.Warnf("Release without a record did not succeed, an address may stay held until reconcile: %v, %v", err, reply)
	}
}

// getDatePath picks the datapath for this pod from the conflist.
//
// The name-to-datapath mapping lives in pkg/plugin/types, so the accepted
// conflist values and their String form stay in one place; the driver for a
// datapath is picked separately, in pkg/plugin/datapath. All this adds is the
// IP-type gate, which is a property of the request rather than of the datapath.
func getDatePath(ipType rpc.IPType, conf *zecTypes.CNIConf) (types.DataPathType, error) {
	if ipType != rpc.IPType_TypeENIMultiIP {
		return types.DataPathUnknown, fmt.Errorf("unsupported ipType %s", ipType)
	}
	return types.ParseDataPath(conf.ENIIPVirtualType)
}

func generateSetupConfig(args *skel.CmdArgs, alloc *rpc.NetConf, conf *zecTypes.CNIConf, netns ns.NetNS, ipType rpc.IPType) (*types.SetupConfig, error) {
	var (
		err            error
		containerIPNet *zecTypes.IPNetSet
		gatewayIP      *zecTypes.IPSet
		eniGatewayIP   *zecTypes.IPSet
		eniPrimaryIP   *zecTypes.IPSet
		deviceID       int32

		routes []cniTypes.Route
	)

	if alloc.GetPodIP() == nil || alloc.GetENIInfo() == nil {
		return nil, fmt.Errorf("return empty pod alloc info: %v", alloc)
	}

	if alloc.GetPodIP() != nil {
		podIP := alloc.GetPodIP()
		subNet := alloc.GetENIInfo().GetSubnet()
		gw := alloc.GetENIInfo().GetGatewayIP()

		containerIPNet, err = zecTypes.BuildIPNet(podIP, subNet)
		if err != nil {
			return nil, err
		}
		gatewayIP, err = zecTypes.ToIPSet(gw)
		if err != nil {
			return nil, err
		}
	}
	if alloc.GetENIInfo() != nil {
		mac := alloc.GetENIInfo().GetMAC()
		if mac != "" {
			deviceID, err = linuxUtils.GetDeviceNumber(mac)
			if err != nil {
				return nil, err
			}
		}
		if alloc.GetENIInfo().GetGatewayIP() != nil {
			eniGatewayIP, err = zecTypes.ToIPSet(alloc.GetENIInfo().GetGatewayIP())
			if err != nil {
				return nil, err
			}
		}
		if alloc.GetENIInfo().GetPrimaryIP() != nil {
			eniPrimaryIP, err = zecTypes.ToIPSet(alloc.GetENIInfo().GetPrimaryIP())
			if err != nil {
				return nil, err
			}
		}
	}

	name := alloc.IfName
	if name == "" {
		name = args.IfName
	}
	for _, r := range alloc.GetExtraRoutes() {
		ip, n, err := net.ParseCIDR(r.Dst)
		if err != nil {
			return nil, fmt.Errorf("error parse extra routes, %w", err)
		}
		route := cniTypes.Route{Dst: *n}
		if ip.To4() != nil {
			route.GW = gatewayIP.IPv4
		} else {
			route.GW = gatewayIP.IPv6
		}
		routes = append(routes, route)
	}

	dp, err := getDatePath(ipType, conf)
	if err != nil {
		return nil, err
	}

	return &types.SetupConfig{
		DP:              dp,
		Netns:           netns,
		ContainerIfName: name,
		ContainerIPNet:  containerIPNet,
		GatewayIP:       gatewayIP,
		MTU:             conf.MTU,
		ENIIndex:        int(deviceID),
		ENIGatewayIP:    eniGatewayIP,
		ENIPrimaryIP:    eniPrimaryIP,
		ENIIsPrimary:    alloc.GetENIInfo().GetIsPrimary(),
		DefaultRoute:    alloc.GetDefaultRoute(),
		ExtraRoutes:     routes,
		RuntimeConfig:   conf.RuntimeConfig,
	}, nil
}

// tearDownNetConfs removes the host-side wiring for every network the record
// names. Split out of DoCmdDel so it can be exercised without a daemon socket --
// the same reason releaseWithoutRecord is separate -- and because what it
// returns is what decides whether DoCmdDel goes on to release the address.
//
// The node-wide CNI lock is the caller's; this does not take it.
//
// Every entry is attempted and the failures are collected. A config this pod
// could not build used to log and return nil, which had two consequences, both
// silent:
//
// The nil reached DoCmdDel as success, so the release still ran. The address
// went back into the pool while the host veth and the two ip rules naming it
// stayed on the node, and nothing on this path removes them afterwards --
// reclaimStaleRules only runs for a pod whose record is already gone. The next
// pod handed that address inherits a pair of rules pointing at a veth that no
// longer exists.
//
// And the return abandoned the remaining entries, so one unbuildable config
// took the teardown of the pod's other networks with it.
//
// Failing instead makes kubelet retry the DEL, and a config that can never be
// built leaves the pod in Terminating. That is the louder of the two failures:
// kubelet reports it on every retry, whereas a stale rule surfaces days later as
// a different pod's connectivity bug.
func tearDownNetConfs(logger zlog.Logger, netConfs []*rpc.NetConf, ipType rpc.IPType,
	conf *zecTypes.CNIConf, k8sConfig *zecTypes.K8SArgs, netns ns.NetNS) error {
	var failures []error
	for _, netConf := range netConfs {
		teardownCfg, confErr := generateTearDownConf(netConf, conf, k8sConfig, netns, ipType)
		if confErr != nil {
			logger.Errorf("Cannot build the teardown config, the address will not be released: %v", confErr)
			failures = append(failures, fmt.Errorf("build teardown config: %w", confErr))
			continue
		}
		if tdErr := datapath.TeardownDataPath(logger, teardownCfg); tdErr != nil {
			failures = append(failures, tdErr)
		}
	}
	return errors.Join(failures...)
}

func generateTearDownConf(alloc *rpc.NetConf, conf *zecTypes.CNIConf, k8sConfig *zecTypes.K8SArgs,
	netns ns.NetNS, ipType rpc.IPType) (*types.TeardownCfg, error) {
	if alloc.GetPodIP() == nil || alloc.GetENIInfo() == nil {
		return nil, fmt.Errorf("return empty pod alloc info: %v", alloc)
	}

	var (
		err            error
		containerIPNet *zecTypes.IPNetSet
		eniIndex       int32
	)

	if alloc.GetPodIP() != nil {
		podIP := alloc.GetPodIP()
		subNet := alloc.GetENIInfo().GetSubnet()

		containerIPNet, err = zecTypes.BuildIPNet(podIP, subNet)
		if err != nil {
			return nil, err
		}
	}

	if alloc.GetENIInfo() != nil {
		mac := alloc.GetENIInfo().GetMAC()
		if mac != "" {
			eniIndex, _ = linuxUtils.GetDeviceNumber(mac)
		}
	}

	dp, err := getDatePath(ipType, conf)
	if err != nil {
		return nil, err
	}
	// Recomputed rather than carried across: VethNameForPod is a pure function
	// of pod namespace, pod name, interface name and prefix, which is what lets
	// teardown find the host end again. Leaving this field unset makes teardown
	// skip the link deletion -- it only deletes a non-empty name -- and leak the
	// host veth.
	hostVETHName, err := linuxUtils.VethNameForPod(string(k8sConfig.K8S_POD_NAME),
		string(k8sConfig.K8S_POD_NAMESPACE), alloc.IfName, conf.HostVethPrefix)
	if err != nil {
		return nil, err
	}

	return &types.TeardownCfg{
		DP:             dp,
		Netns:          netns,
		HostVETHName:   hostVETHName,
		ContainerIPNet: containerIPNet,
		ENIIndex:       int(eniIndex),
	}, nil
}
