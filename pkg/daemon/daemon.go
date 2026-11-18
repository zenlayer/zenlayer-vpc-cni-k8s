// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package daemon implements the node-local IPAM service behind the CNI plugin.
//
// Its center is a factory that manages every network interface attached to this
// node, one goroutine per interface, feeding a warm pool of addresses. On top of
// that sit the gRPC surface the plugin calls, the persistent record of which pod
// holds which address, and a periodic reconcile that settles that record against
// what the platform actually reports.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"google.golang.org/grpc"

	v1 "k8s.io/api/core/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	k8sErr "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/config"
	helper "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/factory"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/version"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/deviceplugin"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/k8s"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/metrics"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/pool"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/signal"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/tracing"
	linuxUtils "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/utils/linux"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/ec2"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/rpc"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

var (
	log = zlog.GetLogger().WithFields(zlog.Fields{"subsys": "ipam"})
)

const (
	envNodeName     = "NODE_NAME"
	envPodNamespace = "POD_NAMESPACE"

	defaultSocketPath = "/var/run/zenlayer/cni.socket"
	resourceDBPath    = "/var/lib/cni/zenlayer/Resource.db"
)

// ipamDaemon manages the network resources consumed by the pods on this node.
type ipamDaemon struct {
	networkMode      string
	namespace        string
	nodeName         string
	cfg              *config.Config
	apiListenAddress string
	pendingPods      sync.Map

	instanceMeta          helper.InstanceMetadataGetter
	instanceLimit         helper.InstanceLimitManager
	zecClient             ec2.EC2
	k8s                   k8s.K8sService
	podPersistenceManager *podPersistenceManager
	subnetManager         helper.SubnetManager
	securityGroupManager  helper.SecurityGroupManager
	// eniManager is reserved for the exclusive-ENI mode; see eni.go.
	//nolint:unused // reserved for the exclusive-ENI network mode
	eniManager          *eniResourceManager
	eniIPManager        *eniIPResourceManager
	managers            map[string]ResourceManager // NetResourceType - ResourceManager
	devicePluginManager deviceplugin.Manager
	// capacityNudge asks the device plugin watcher to recount now. A pod that
	// holds an address without requesting the resource changes the advertised
	// figure the moment it starts or goes; the 5-minute tick alone left it
	// that stale. Nil when the device plugin is disabled.
	capacityNudge chan struct{}

	// gcMu serializes gc: the periodic tick and the WakeGC signal can fire at
	// the same time, and two concurrent GCs over the same pool would race.
	gcMu   sync.Mutex
	lastGC time.Time

	// staleSince records when a stored pod record was first seen without a
	// live pod behind it. Kept in memory on purpose: losing it on restart
	// restarts the grace period, which errs towards keeping a record too long
	// rather than reclaiming an address a container is still using.
	//
	// Only syncPodPersistence touches this, and its two callers -- startup and
	// gc under gcMu -- never overlap.
	staleSince map[string]time.Time

	rpc.UnimplementedCNIIpamServer
}

const (
	// gcDebounce keeps a burst of WakeGC signals from turning into a burst of GCs.
	gcDebounce = time.Minute
	// gcSlowThreshold is when a GC has paused the pool long enough to be worth
	// warning about: a quarter of the deadline a CNI ADD's RPC runs under.
	gcSlowThreshold = daemonRPCTimeout / 4
	// The two rule priorities the veth datapath installs per pod. They are
	// duplicated from pkg/plugin/datapath because that package's copies are
	// unexported and the daemon must be able to clean up after a plugin process
	// that is long gone.
	staleToPodRulePriority   = 512
	staleFromPodRulePriority = 2048
	// staleRecordGrace is how long a record outlives the pod it belongs to
	// before the daemon reclaims it.
	//
	// The window has to cover the gap between a pod leaving the API and kubelet
	// getting round to CNI DEL, because reclaiming early hands a live
	// container's address to someone else. Seconds would do for that; an hour
	// is chosen so that a stuck kubelet, a paused node or a slow eviction never
	// races it. Reclaiming late costs one address for an hour, reclaiming early
	// costs two pods the same address.
	staleRecordGrace = time.Hour
)

// gc reconciles the eni-ip pool against the resources pods actually hold.
//
// This is the only caller of pool.GC, so without it the reclaim logic in
// eniIPFactory.GC never runs and leaked ENIs and secondary IPs accumulate until
// the pool reaches its maximum capacity.
func (d *ipamDaemon) gc() {
	// Logs and re-panics (HandleCrash's ReallyCrash defaults to true). GC walks
	// pool internals and calls the OpenAPI, so when it does panic the log should
	// name it before the pod restarts.
	defer runtime.HandleCrash(func(r interface{}) {
		log.Errorf("Daemon GC panicked: %v", r)
	})

	d.gcMu.Lock()
	defer d.gcMu.Unlock()

	if time.Since(d.lastGC) < gcDebounce {
		return
	}

	// Silence WakeGC for the duration, so that what this GC does cannot queue up
	// another round behind it. The instance limit manager signals WakeGC when
	// the managed interface count drops -- which GC itself causes -- and it
	// copes with being muted here by retrying (wakeGCPending in
	// factory/instance.go).
	signal.MuteChannel(signal.WakeGC)
	defer signal.UnmuteChannel(signal.WakeGC)

	var err error
	log.Info("Daemon GC start")
	// pool.GC holds the pool paused for its whole duration -- including the
	// OpenAPI round trips inside factory.GC -- so every Allocate on this node
	// waits behind it. The RPC a CNI ADD makes is canceled after
	// daemonRPCTimeout (cniInterceptor), so a GC that runs long enough is
	// indistinguishable from an outage to a starting pod.
	start := time.Now()
	defer func() {
		// GC frees drained interfaces, which moves the limit; addresses the
		// pool gives back afterwards from its invalid queue are recounted as
		// each goes (eniIPFactory.onCapacity).
		d.nudgeCapacity()
		elapsed := time.Since(start)
		// The warning is emitted before branching on err on purpose. A GC only
		// gets slow because something upstream is timing out, so the failure
		// path is where the long pauses happen; checking the threshold only on
		// success meant the warning never fired in the case that needs it --
		// failures that held the pool for 130s, 256s and 261s produced none.
		if elapsed > gcSlowThreshold {
			log.Warnf("Daemon GC took %s, the pool was paused that whole time "+
				"(pod setup blocks on it, and CNI ADD times out after %s)", elapsed, daemonRPCTimeout)
		}
		if err != nil {
			log.Errorf("Daemon gc failed after %s, %v", elapsed, err)
			return
		}
		d.lastGC = time.Now()
		if elapsed <= gcSlowThreshold {
			log.Infof("Daemon GC finished in %s", elapsed)
		}
	}()

	// Re-synced on every run on purpose: GC decides what to reclaim from this
	// map, so a stale one would drop addresses that are still in use.
	getAllocatedResMap := func(resType string) func() (map[string]zecTypes.NetResourceAllocated, error) {
		return func() (map[string]zecTypes.NetResourceAllocated, error) {
			if inErr := d.syncPodPersistence(); inErr != nil {
				return nil, fmt.Errorf("sync pod persistence failed, %v", inErr)
			}
			oldPods, inErr := d.podPersistenceManager.List()
			if inErr != nil {
				return nil, fmt.Errorf("list pod from persistence failed, %v", inErr)
			}
			return zecTypes.GetNetResourceAllocatedFromPods(oldPods)[resType], nil
		}
	}

	var ipGCErr error
	if d.eniIPManager != nil {
		if ipGCErr = d.eniIPManager.pool.GC(getAllocatedResMap(zecTypes.NetResourceTypeEniIp)); ipGCErr != nil {
			ipGCErr = fmt.Errorf("pool of eni-ip gc failed, %v", ipGCErr)
		}
	}
	err = k8sErr.NewAggregate([]error{ipGCErr})
}

// startGC runs gc periodically and on demand, and returns a cleanup func.
func (d *ipamDaemon) startGC(ctx context.Context) (func(), error) {
	period := time.Duration(d.cfg.ReconcileIntervalSec) * time.Second

	sig := make(chan signal.SigData)
	if err := signal.RegisterChannel(signal.WakeGC, sig); err != nil {
		return nil, fmt.Errorf("register %s channel failed: %w", signal.WakeGC, err)
	}

	var once sync.Once
	go wait.JitterUntil(func() {
		// Skip the immediate first tick: at startup the pool has just been
		// rebuilt from the provider and there is nothing to reclaim yet.
		once.Do(func() { time.Sleep(period) })
		d.gc()
	}, period, 0.2, true, ctx.Done())

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-sig:
				// A closed channel receives immediately, so without the ok
				// check the cleanup below would kick off one last GC on the
				// way out -- with ctx already canceled under it.
				if !ok {
					return
				}
				d.gc()
			}
		}
	}()

	// Mute before closing: NotifySignal sends under the read lock, so muting
	// first guarantees no send is in flight when the channel goes away.
	return func() {
		signal.MuteChannel(signal.WakeGC)
		close(sig)
	}, nil
}

// NewKubernetesClient creates a kubernetes client.
func NewKubernetesClient() (*kubernetes.Clientset, error) {
	c, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("create incluster config failed: %v", err)
	}
	c.UserAgent = version.UserAgent()

	return kubernetes.NewForConfig(c)
}

// startFailureEventTimeout bounds the one API call reportStartFailure makes, so
// an unreachable API server does not hold up the exit it is reporting.
const startFailureEventTimeout = 5 * time.Second

// reportStartFailure records why the daemon failed to start as a node event,
// under the OpenAPI reason when err carries one so the platform can tell a
// rejected credential from an unreachable endpoint. A stop requested while
// starting is not a failure: a rolling update landing mid-start would report one.
func reportStartFailure(runCtx context.Context, k k8s.K8sService, err error) {
	if err == nil || runCtx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	reason := startFailureReason(err)
	ctx, cancel := context.WithTimeout(context.Background(), startFailureEventTimeout)
	defer cancel()
	if evErr := k.RecordNodeEventSync(ctx, v1.EventTypeWarning, reason, err.Error()); evErr != nil {
		log.Warnf("Failed to record start failure on the node, %v", evErr)
	}
}

func startFailureReason(err error) string {
	if errors.Is(err, config.ErrNoCredentials) {
		return tracing.EventCredentialServiceAbnormal
	}
	if reason := apiErr.OpenAPIErrEventReason(err); reason != "" {
		return reason
	}
	return tracing.EventDaemonStartFailed
}

// NewIpamDaemon builds the daemon: it resolves this node's identity, loads and
// validates the configuration, opens the platform client and the record store,
// and brings up the resource manager, the warm pool and the periodic reconcile.
//
// Anything wrong with the configuration is reported here rather than deferred,
// because a node that starts and then serves addresses from the wrong subnet is
// far harder to diagnose than one that refuses to start and says why.
func NewIpamDaemon(ctx context.Context) (_ *ipamDaemon, retErr error) {
	var (
		namespace string
		nodeName  string
	)

	namespace = os.Getenv(envPodNamespace)
	if namespace == "" {
		namespace = "kube-system"
	}
	nodeName = os.Getenv(envNodeName)
	if nodeName == "" {
		return nil, fmt.Errorf("get env %s failed", envNodeName)
	}

	metrics.PrometheusRegister()

	k8sClient, err := NewKubernetesClient()
	if err != nil {
		return nil, fmt.Errorf("create kubernetes clientSet failed: %w", err)
	}
	k8s, err := k8s.NewK8sService(ctx, nodeName, k8sClient)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes service failed: %v", err)
	}

	// Wire the tracer to the API before anything can report through it.
	//
	// Without this the default tracer has no recorder: RecordNodeEvent returns
	// "no node event recorder registered" and every call site discards that
	// error, so subnet exhaustion, quota exceeded, allocate/release failures and
	// flow limits produce no node events at all. Pod events keep working through
	// a different route (the RecordEvent RPC below calls d.k8s directly).
	tracing.RegisterEventRecorder(k8s.RecordNodeEvent, k8s.RecordPodEvent)

	// A failure from here on exits the process, taking the event queue with it,
	// so the cause is written to the node synchronously. Otherwise a node stuck
	// in CrashLoopBackOff on a rejected credential or an unreachable OpenAPI
	// shows nothing outside the pod log. Run covers the steps after this returns.
	defer func() { reportStartFailure(ctx, k8s, retErr) }()

	podPersist, err := newPodPersistenceManager(resourceDBPath, "pod")
	if err != nil {
		return nil, fmt.Errorf("create persistence db failed: %w", err)
	}

	cfg, err := config.LoadConfig(ctx, k8s, namespace, nodeName)
	if err != nil {
		return nil, err
	}

	svcCIDR := &zecTypes.IPNetSet{}
	cidrs := strings.Split(cfg.ServiceCIDR, ",")

	for _, cidr := range cidrs {
		svcCIDR.SetIPNet(cidr)
	}
	err = k8s.SetSvcCIDR(svcCIDR)
	if err != nil {
		return nil, err
	}

	if cfg.IPAMType == types.IPAMTypeCRD {
		return newCRDIpamDaemon(ctx, cfg, k8s, podPersist, namespace, nodeName)
	}

	_zecClient, err := ec2.NewZecClient(cfg.CredentialAccessKeyId, cfg.CredentialAccessKeySecret, cfg.OpenAPIDomain)
	if err != nil {
		return nil, err
	}

	zecClient := metrics.NewMetricEC2Wrapper(_zecClient)

	// Must come after the client: resolving the region is an OpenAPI call, it is
	// not available from DMI or the metadata service.
	instanceMeta, err := helper.InitInstanceMetadata(zecClient)
	if err != nil {
		return nil, fmt.Errorf("resolve instance metadata failed, %w", err)
	}

	subnetManager, err := helper.NewPodSubnetManager(instanceMeta.GetRegionId(), cfg.VpcID, zecClient,
		helper.WithEventRecord(tracing.DefaultGlobalTracer()), helper.WithDefaultEventLimiter(),
		helper.WithIPStack(types.IPStack(cfg.IPStack)))
	if err != nil {
		return nil, fmt.Errorf("create pod subnets manager failed, %w", err)
	}
	err = subnetManager.FlushSubnets(cfg.Subnets...)
	if err != nil {
		return nil, fmt.Errorf("set subnets failed, %w", err)
	}

	// Every configured subnet has to carry every family the node will hand out,
	// and the daemon refuses to start otherwise. At order time a subnet that
	// cannot serve the configured stack is only skipped with a warning ("Skip
	// submit order to eni ... because subnet ipStack ... not support"), so with
	// dual against an IPv4-only subnet no interface ever gets an order: the pool
	// never fills, pods sit in ContainerCreating, and that one warning is the
	// only clue.
	if err = validateSubnetsForStack(subnetManager, types.IPStack(cfg.IPStack), cfg.Subnets); err != nil {
		return nil, err
	}

	secGrpManager := helper.NewSecurityGroupManager()
	err = secGrpManager.UpdateSecurityGroup(cfg.SecurityGroup)
	if err != nil {
		return nil, fmt.Errorf("set SecurityGroup failed, %w", err)
	}

	// Tagged on the interfaces this node creates, for the controlplane's sweep
	// should the cluster move to ipamType crd. This daemon's own sweep keeps to
	// its own instance's interfaces and needs no cluster id, so a failure only
	// leaves the tag off.
	clusterID, err := k8s.GetClusterID(ctx)
	if err != nil {
		log.Warnf("Interfaces this node creates will carry no cluster id: %v", err)
	}
	zecApi, err := helper.New(zecClient, types.IPStack(cfg.IPStack), subnetManager, instanceMeta, ec2.IPv6Billing{
		InternetChargeType: cfg.IPv6InternetChargeType,
		BandwidthMbps:      cfg.IPv6Bandwidth,
		PackageSizeTB:      cfg.IPv6PackageSize,
		ClusterId:          cfg.IPv6ClusterId,
	}, cfg.ManageUntagged(), clusterID)
	if err != nil {
		return nil, err
	}

	d := &ipamDaemon{
		networkMode:                cfg.NetworkMode,
		namespace:                  namespace,
		nodeName:                   nodeName,
		cfg:                        cfg,
		apiListenAddress:           defaultSocketPath,
		pendingPods:                sync.Map{},
		k8s:                        k8s,
		instanceMeta:               instanceMeta,
		zecClient:                  zecClient,
		podPersistenceManager:      podPersist,
		subnetManager:              subnetManager,
		securityGroupManager:       secGrpManager,
		managers:                   map[string]ResourceManager{},
		UnimplementedCNIIpamServer: rpc.UnimplementedCNIIpamServer{},
	}
	d.instanceLimit, err = helper.NewInstanceLimitManager(zecApi)
	if err != nil {
		return nil, err
	}
	err = d.syncPodPersistence()
	if err != nil {
		return nil, fmt.Errorf("sync pod persistence failed while daemon init, %w", err)
	}
	oldPods, err := podPersist.List()
	if err != nil {
		return nil, fmt.Errorf("list pod from persistence failed, %v", err)
	}
	allocatedResMap := zecTypes.GetNetResourceAllocatedFromPods(oldPods)

	// d.networkMode is the daemon mode from config (types.Mode*), not the
	// per-pod network mode (zecTypes.PodNetworkMode*).
	switch d.networkMode {
	case types.ModeENIMultiIP:
		d.eniIPManager, err = newEniIPResourceManager(cfg, subnetManager, secGrpManager,
			zecApi, allocatedResMap[zecTypes.NetResourceTypeEniIp], k8s)
		if err != nil {
			return nil, fmt.Errorf("create eniIP resource manager failed, %w", err)
		}
		d.managers[zecTypes.NetResourceTypeEniIp] = d.eniIPManager

		// Left nil when the device plugin is disabled -- see
		// Config.DisableDevicePlugin. Callers guard on nil rather than getting a
		// no-op manager, so that "this node advertises nothing" stays visible in
		// the daemon's own state instead of hiding behind a manager that accepts
		// every call and does nothing with it.
		d.startDevicePlugin()
		if f := d.eniIPManager.factory; f != nil {
			nudge := d.nudgeCapacity
			f.onCapacity.Store(&nudge)
		}
	default:
		return nil, fmt.Errorf("no support network mode %s", d.networkMode)
	}

	return d, nil
}

func isHostNetwork(pod *v1.Pod) bool {
	return pod.Spec.HostNetwork
}

func hasRequestAndLimitsFields(pod *v1.Pod) bool {
	for _, c := range pod.Spec.Containers {
		if c.Resources.Requests != nil {
			_, ok := c.Resources.Requests[deviceplugin.ZenlayerNameSpace+deviceplugin.ENIResourceName]
			if ok {
				return true
			}
			_, ok = c.Resources.Requests[deviceplugin.ZenlayerNameSpace+deviceplugin.ENIIPResourceName]
			if ok {
				return true
			}
		}
	}
	return false
}

func (d *ipamDaemon) GetStockPodCount() int {
	var count int
	pods, err := d.k8s.ListLocalCachedPods()
	if err != nil {
		log.Errorf("List local cached pods failed, %v", err)
		return count
	}
	for _, pod := range pods {
		if _, err = d.podPersistenceManager.Get(pod.Namespace, pod.Name); err == nil && !isHostNetwork(pod) && !hasRequestAndLimitsFields(pod) {
			count++
		}
	}
	return count
}

// nudgeCapacity asks for a recount without waiting: a pending request already
// covers this one.
func (d *ipamDaemon) nudgeCapacity() {
	if d.capacityNudge == nil {
		return
	}
	select {
	case d.capacityNudge <- struct{}{}:
	default:
	}
}

// countsAsStock reports whether pod is one GetStockPodCount counts: on the pod
// network without requesting the resource. A pod no longer in the cache may
// have been, so it counts.
func countsAsStock(pod *v1.Pod) bool {
	return pod == nil || (!isHostNetwork(pod) && !hasRequestAndLimitsFields(pod))
}

func watchResourceNum(ctx context.Context, pluginManger deviceplugin.Manager, resName string, lister func() int, updateSignal <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	var err error
	for {
		select {
		case <-ticker.C:
			err = pluginManger.Update(resName, lister())
		case <-updateSignal:
			err = pluginManger.Update(resName, lister())
		case <-ctx.Done():
			return
		}
		if err != nil {
			log.Errorf("update resource for %s failed, %v", resName, err)
		}
	}
}

func (d *ipamDaemon) startGrpcServer() (*grpc.Server, error) {
	// A unix socket, not a TCP port: the CNI plugin runs on this node and
	// nothing off it has any business asking the daemon to allocate addresses.
	l, err := listenOnUnixSock(d.apiListenAddress)
	if err != nil {
		return nil, err
	}

	grpcServer := grpc.NewServer(grpc.ChainUnaryInterceptor(
		cniInterceptor,
	))
	rpc.RegisterCNIIpamServer(grpcServer, d)

	go func() {
		log.Infof("Start grpc server")
		err = grpcServer.Serve(l)
		if err != nil {
			log.Warnf("Grpc server exit: %v", err)
		}
	}()
	return grpcServer, nil
}

// startDebugServer serves /healthz, and /metrics unless metrics are disabled,
// on the configured port.
//
// There is deliberately no pprof here, or anywhere else. The pod runs with
// hostNetwork and this listener has to be reachable from off the node (kubelet
// probes it, Prometheus scrapes it), so /debug/pprof on that address would hand
// anyone who can reach the node a heap dump, a CPU profile and the process
// command line. The OpenAPI access key and secret stay in memory for the life of
// the process (see NewIpamDaemon), so that heap dump is a credential disclosure,
// not just an information leak about pod addressing.
func (d *ipamDaemon) startDebugServer() (*http.Server, error) {
	serveMux := http.NewServeMux()
	serveMux.HandleFunc("/healthz", func(rw http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(rw, "ok")
	})

	metrics.ServeMetrics(serveMux)

	addr := ":" + strconv.Itoa(int(d.cfg.HealthAndDebugPort))
	server := &http.Server{
		Addr:         addr,
		Handler:      serveMux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	go func() {
		defer runtime.HandleCrash(func(r interface{}) {
			log.Error(r)
		})
		log.Infof("Start debug server on %s", addr)
		err := server.ListenAndServe()
		if err != nil {
			if err != http.ErrServerClosed {
				log.Warnf("Debug server exit: %v", err)
			} else {
				log.Infof("Debug server shut down gracefully")
			}
		}
	}()
	return server, nil
}

// validateSubnetsForStack refuses to start when a configured subnet cannot
// serve every address family the node is set to hand out.
//
// Checked here rather than at order time because the order-time behavior is a
// warning and a skip, which is indistinguishable from "the platform is slow" --
// see the caller for what that degrades into.
func validateSubnetsForStack(subnets helper.SubnetManager, ipStack types.IPStack, ids []string) error {
	return helper.ValidateSubnetsForStack(subnets, ipStack, ids)
}

func (d *ipamDaemon) syncPodPersistence() error {
	log.Infof("Sync pod persistence")
	podMap := map[string]*v1.Pod{}

	persistPods, err := d.podPersistenceManager.List()
	if err != nil {
		return fmt.Errorf("list persist pods failed: %w", err)
	}

	localPods, err := d.k8s.ListLocalCachedPods()
	if err != nil {
		return fmt.Errorf("list local cached pods failed: %w", err)
	}

	for _, pod := range localPods {
		podMap[zecTypes.PodKey(pod.Namespace, pod.Name)] = pod
	}

	if d.staleSince == nil {
		d.staleSince = map[string]time.Time{}
	}
	seen := make(map[string]struct{}, len(persistPods))

	for _, pod := range persistPods {
		key := zecTypes.PodKey(pod.Namespace, pod.Name)
		seen[key] = struct{}{}

		if _, exist := podMap[key]; exist {
			delete(d.staleSince, key)
			continue
		}

		// The list above comes from the informer, which can lag. Ask the API
		// server directly before acting on a record, and treat any answer that
		// is not a definite NotFound as "still there" -- a broken connection
		// must not be read as every pod on this node having gone away.
		// Bounded: this runs inside pool.GC with the whole pool paused.
		getCtx, cancel := context.WithTimeout(context.Background(), ownerAliveAPITimeout)
		_, getErr := d.k8s.GetPod(getCtx, pod.Namespace, pod.Name)
		cancel()
		if getErr == nil {
			delete(d.staleSince, key)
			continue
		} else if !apiErrors.IsNotFound(getErr) {
			log.Warnf("Cannot confirm whether pod[%s] still exists, keeping its record: %v", key, getErr)
			continue
		}

		// The pod is gone. Wait out the grace period before reclaiming.
		//
		// The netns used to be consulted here instead, as proof that teardown
		// had not run yet -- but a leaked netns never goes away, so the record
		// and its address were kept forever. Time bounds the same risk without
		// depending on a signal that can get stuck.
		since, marked := d.staleSince[key]
		if !marked {
			d.staleSince[key] = time.Now()
			log.Infof("Pod[%s] is gone from the API server; holding its record for %s before reclaiming", key, staleRecordGrace)
			continue
		}
		if waited := time.Since(since); waited < staleRecordGrace {
			log.Debugf("Pod[%s] has been gone for %s, reclaiming its record after %s", key, waited, staleRecordGrace)
			continue
		}

		// Clear the host-side rules before the record that names them. They
		// outlive the netns and nothing else on the node knows which pod they
		// belonged to, so this is the last chance to find them.
		d.reclaimStaleRules(pod)

		log.Warnf("Found pod[%s/%s] in persistence not exist in k8s, it would be deleted.", pod.Namespace, pod.Name)
		err = d.podPersistenceManager.Delete(pod.Namespace, pod.Name)
		if err != nil {
			return fmt.Errorf("delete pod[%s] in persistence failed: %w", pod.Name, err)
		}
		delete(d.staleSince, key)
		d.nudgeCapacity()
	}

	// Drop marks for records that are no longer stored at all, so the map
	// cannot grow for the life of the process.
	for key := range d.staleSince {
		if _, ok := seen[key]; !ok {
			delete(d.staleSince, key)
		}
	}
	return nil
}

// reclaimStaleRules removes the policy routing rules that were installed for a
// pod whose record is about to be dropped.
//
// The rules select on the pod address and live in the host's rule table, so
// destroying a netns does not touch them: they have to be deleted explicitly,
// and once the record naming the address is gone there is nothing left to
// delete them by. Best effort by design -- a rule that cannot be removed must
// not stop the record from being reclaimed, or the address stays out of the
// pool too.
// findIPRule and ruleDel are indirected so tests can substitute fakes.
// reclaimStaleRules sits on the release path, and calling netlink directly meant
// the unit tests queried and deleted real ip rules on whatever host ran them --
// on a node where this CNI is running, its own rules for a live pod that happened
// to hold the same address.
var (
	findIPRule = linuxUtils.FindIPRule
	ruleDel    = linuxUtils.RuleDel
)

func (d *ipamDaemon) reclaimStaleRules(pod *zecTypes.Pod) {
	for _, res := range pod.Resources {
		for _, addr := range []string{res.IPv4, res.IPv6} {
			ip := net.ParseIP(addr)
			if ip == nil {
				continue
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			ipNet := &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}

			for _, r := range []*netlink.Rule{
				func() *netlink.Rule {
					n := netlink.NewRule()
					n.Priority = staleFromPodRulePriority
					n.Src = ipNet
					return n
				}(),
				func() *netlink.Rule {
					n := netlink.NewRule()
					n.Priority = staleToPodRulePriority
					n.Dst = ipNet
					return n
				}(),
			} {
				found, findErr := findIPRule(r)
				if findErr != nil {
					log.Warnf("Cannot look up leftover ip rules for %s: %v", addr, findErr)
					continue
				}
				for i := range found {
					if delErr := ruleDel(&found[i]); delErr != nil {
						log.Warnf("Cannot remove leftover ip rule %s: %v", found[i].String(), delErr)
						continue
					}
					log.Warnf("Removed leftover ip rule %s left by pod[%s/%s]", found[i].String(), pod.Namespace, pod.Name)
				}
			}
		}
	}
}

func (d *ipamDaemon) translatePod(pod *v1.Pod) *zecTypes.Pod {
	if pod == nil {
		return nil
	}
	result := &zecTypes.Pod{
		Namespace: pod.Namespace,
		Name:      pod.Name,
	}

	if vpcENI, ok := pod.Annotations[zecTypes.AnnotationPodNetworksDefinition]; ok {
		var err error
		result.VpcENI, err = strconv.ParseBool(vpcENI)
		if err != nil {
			_ = tracing.RecordPodEvent(pod.Name, pod.Namespace, v1.EventTypeWarning,
				"ParsePodFailed", fmt.Sprintf("Parse vpc eni %s failed.", vpcENI))
		}
	}

	if value, ok := pod.Annotations[zecTypes.EvictionPolicyAnnotation]; ok {
		result.AllowEviction = value == zecTypes.AllowEviction
	}

	if d.networkMode == types.ModeENIMultiIP {
		result.PodNetworkMode = zecTypes.PodNetworkModeENIShare
	}
	return result
}

func (d *ipamDaemon) verifyPodNetwork(podNetworkMode string) bool {
	return (d.networkMode == types.ModeENIMultiIP && podNetworkMode == zecTypes.PodNetworkModeENIShare)
}

func (d *ipamDaemon) judgmentEvictPod(ctx *netContext, resourceLimit int) error {
	// resourceLimit <= 0 means the node's capacity could not be worked out, not
	// that it is zero: the figure is clamped at zero rather than reported
	// negative. "len(pods) >= 0" is true for the very first pod, so without this
	// a node that cannot report its capacity would evict every pod scheduled to
	// it.
	if resourceLimit <= 0 {
		ctx.log.Warnf("Node reports no address capacity; not evicting on that basis")
		return nil
	}
	pods, err := d.podPersistenceManager.List()
	if err == nil && len(pods) >= resourceLimit {
		// The numbers and the way out both belong in this string: it is the only
		// explanation the user gets for a pod the CNI deleted, and without them
		// there is nothing to act on. The cap is not always the instance's, so
		// name the knob rather than a formula: with capacity probing off it is
		// poolTargetMax alone, and telling the user to resize the instance would
		// change nothing.
		budget := "(chart value zec.poolTargetMax, or zec.poolTargetCapProbe to take the cap from the " +
			"instance type)"
		if d.cfg != nil && d.cfg.IPAMType == types.IPAMTypeCRD {
			// No pool sizing on a crd node: the figure is the controlplane's.
			budget = "(the controlplane's capacity for it, `kubectl get zn` ALLOCATABLE: a larger instance type)"
		}
		info := fmt.Sprintf("this node can serve %d pod addresses and already holds %d pods, so "+
			"there is none left; lower this node's maxPods, give it a larger address budget "+
			budget+", or remove the %s annotation from it so the add fails and it stays in "+
			"ContainerCreating instead of being evicted",
			resourceLimit, len(pods), zecTypes.EvictionPolicyAnnotation)
		ctx.log.Infof("Pod evicted due to %s", info)

		// Record before evicting, not after. RecordPodEvent looks the pod up to
		// build the event's object reference, so once eviction has deleted the
		// pod it returns NotFound and the event is never written -- the only
		// explanation the user would get, dropped. A failure here must not stop
		// the eviction.
		if evErr := d.k8s.RecordPodEvent(ctx.pod.Name, ctx.pod.Namespace, v1.EventTypeWarning,
			zecTypes.EventReasonAllocIPFailed, fmt.Sprintf("%s, pod evicted", info)); evErr != nil {
			ctx.log.Warnf("Record eviction event failed, %v", evErr)
		}

		if err = d.k8s.EvictPod(ctx, ctx.pod.Name, ctx.pod.Namespace); err != nil {
			ctx.log.Errorf("Pod evicted failed, %v", err)
			return fmt.Errorf("%s, pod evicted but failed, %v", info, err)
		} else {
			return fmt.Errorf("%s, pod evicted", info)
		}
	}
	return nil
}

func getIfType(networkMode string) rpc.IPType {
	if networkMode == types.ModeENIMultiIP {
		return rpc.IPType_TypeENIMultiIP
	}
	return rpc.IPType_TypeENIMultiIP
}

func (d *ipamDaemon) allocateENIIP(ctx *netContext, oldPod *zecTypes.Pod) (*zecTypes.ENIIP, error) {
	oldRes := oldPod.GetNetResourceByType(zecTypes.NetResourceTypeEniIp)
	prefer := ""
	if length := len(oldRes); length > 1 {
		ctx.Log().Warnf("ENI for pod %s is more than one", zecTypes.PodKey(oldPod.Namespace, oldPod.Name))
	} else if length == 1 {
		prefer = oldRes[0].ID
	}

	if prefer == "" && ctx.pod.AllowEviction {
		err := d.judgmentEvictPod(ctx, d.eniIPManager.GetResourceLimit())
		if err != nil {
			return nil, err
		}
	}

	eniip, err := d.eniIPManager.Allocate(ctx, prefer)
	if err != nil {
		return nil, err
	}
	typed, ok := eniip.(*zecTypes.ENIIP)
	if !ok {
		// Only the eni_multi_ip manager is wired in here and it hands back
		// *zecTypes.ENIIP, so this is a wiring mistake. Reporting it fails one
		// pod's ADD; panicking would fail every pod on the node.
		return nil, fmt.Errorf("resource manager returned %T, want *zecTypes.ENIIP", eniip)
	}
	return typed, nil
}

func ParseExtraRoute(routes []zecTypes.Route) []*rpc.Route {
	if routes == nil {
		return nil
	}
	var res []*rpc.Route
	for _, r := range routes {
		res = append(res, &rpc.Route{
			Dst: r.Dst,
		})
	}
	return res
}

// AllocIP allocates network resources (ENI, IP) for a pod.
func (d *ipamDaemon) AllocIP(ctx context.Context, req *rpc.AllocIPRequest) (resp *rpc.AllocIPResponse, err error) {
	lg := log.WithFields(zlog.Fields{
		"Namespace":          req.K8SPodNamespace,
		"Name":               req.K8SPodName,
		"SandboxContainerId": req.K8SPodInfraContainerId,
		"Netns":              req.Netns,
		"IfName":             req.IfName,
	})
	lg.Infof("Handle AllocIP")

	defer runtime.HandleCrash()
	defer func() {
		if err != nil {
			lg.Warnf("Fail to handle AllocIP: %v", err)
		} else {
			lg.Infof("AllocIP result: %s", resp.String())
		}
	}()

	_, exist := d.pendingPods.LoadOrStore(zecTypes.PodKey(req.K8SPodNamespace, req.K8SPodName), struct{}{})
	if exist {
		return &rpc.AllocIPResponse{Code: rpc.ErrorCode_ConflictRequest}, fmt.Errorf("pod %s request processing", zecTypes.PodKey(req.K8SPodNamespace, req.K8SPodName))
	}
	defer func() {
		d.pendingPods.Delete(zecTypes.PodKey(req.K8SPodNamespace, req.K8SPodName))
	}()

	start := time.Now()
	defer func() {
		metrics.RpcLatency.WithLabelValues("AllocIP", fmt.Sprint(err != nil)).Observe(metrics.MsSince(start))
	}()

	k8sPod, err := d.k8s.GetLocalCachedPod(req.K8SPodNamespace, req.K8SPodName)
	if err != nil {
		return &rpc.AllocIPResponse{Code: rpc.ErrorCode_K8SAPIError}, fmt.Errorf("get pod from cache failed, %v", err)
	}
	defer func() {
		if err == nil && countsAsStock(k8sPod) {
			d.nudgeCapacity()
		}
	}()

	newPod := d.translatePod(k8sPod)
	newPod.SandboxContainerId = req.K8SPodInfraContainerId
	newPod.NetNs = req.Netns
	netCtx := &netContext{
		Context: ctx,
		log:     lg,
		pod:     newPod,
	}

	if !d.verifyPodNetwork(newPod.PodNetworkMode) {
		return &rpc.AllocIPResponse{Code: rpc.ErrorCode_NetworkModeNotSupport}, fmt.Errorf("pod network mode not match with daemon")
	}

	oldPod, err := d.podPersistenceManager.Get(req.K8SPodNamespace, req.K8SPodName)
	if err != nil {
		if !errors.Is(err, apiErr.ErrNotFound) {
			return &rpc.AllocIPResponse{Code: rpc.ErrorCode_ReadDBError}, err
		} else {
			oldPod = &zecTypes.Pod{}
		}
	}

	defer func() {
		// Rollback. Use local error variables throughout: err is the named
		// return value, so assigning to it here would overwrite the failure
		// that triggered the rollback and report the whole AllocIP as
		// successful.
		if err == nil {
			return
		}
		if delErr := d.podPersistenceManager.Delete(newPod.Namespace, newPod.Name); delErr != nil {
			lg.Errorf("Delete pod from db failed while rollback, %v", delErr)
		}
		for i, r := range netCtx.res {
			mgr := d.managers[r.Type]
			if mgr == nil {
				lg.Warnf("Find %s resource without manager", r.Type)
				continue
			}
			if relErr := mgr.Release(netCtx, &netCtx.res[i]); relErr != nil {
				lg.Errorf("Release resource %s[Type: %s] failed while rollback, %v", r.ID, r.Type, relErr)
			}
		}
	}()

	var networks []*rpc.NetConf
	switch d.networkMode {
	case types.ModeENIMultiIP:
		eniip, err := d.allocateENIIP(netCtx, oldPod)
		if err != nil {
			return &rpc.AllocIPResponse{Code: rpc.ErrorCode_AllocIPFailure}, fmt.Errorf("allocate resource failed: %v", err)
		}
		ipRes := eniip.GetResourceItem()
		newPod.PodNetworkMode = zecTypes.PodNetworkModeENIShare
		newPod.Resources = append(newPod.Resources, ipRes)
		netCtx.res = append(netCtx.res, ipRes)
		// Plain address, not a CIDR: rpc.IPSet means an address in the PodIP and
		// GatewayIP positions and a network in the Subnet and ServiceCIDR ones.
		// The plugin recombines this with ENIInfo.Subnet in BuildIPNet, which
		// parses it with net.ParseIP and therefore rejects a masked form.
		//
		// ServiceCIDR below is filled in but never read: no code in pkg/plugin
		// looks at it. This datapath does not need it -- the pod's veth reaches
		// the host stack directly, so kube-proxy or its replacement sees the
		// traffic in both directions with no redirect to install. It stays in the
		// response because the field is part of the wire format, and because the
		// daemon's own startup still insists on being able to answer the question
		// (see SetSvcCIDR): an operator who leaves serviceCidr unset has the
		// daemon go look it up, and a failure there is fatal.
		ipSet := eniip.IPSet.ToPb()
		iFace := &rpc.NetConf{
			ENIInfo:      eniip.ENI.ToPb(),
			PodIP:        ipSet,
			IfName:       req.IfName,
			DefaultRoute: true,
			ServiceCIDR:  d.k8s.GetServiceCIDR().ToPb(),
			ExtraRoutes:  ParseExtraRoute(d.cfg.ExtraRoutes),
		}
		newPod.MainInterface = iFace
		newPod.IsMainInterfaceSharedMode = true
		newPod.CreateTime = time.Now()
		err = d.podPersistenceManager.Put(newPod)
		if err != nil {
			return &rpc.AllocIPResponse{Code: rpc.ErrorCode_WriteDBError}, fmt.Errorf("put pod info into store failed, %v", err)
		}
		networks = append(networks, iFace)
	default:
		return &rpc.AllocIPResponse{Code: rpc.ErrorCode_NetworkModeNotSupport}, fmt.Errorf("no support mode %s", d.networkMode)
	}

	return &rpc.AllocIPResponse{Code: rpc.ErrorCode_Success, IPType: getIfType(d.networkMode), NetConfs: networks}, nil
}

// ReleaseIP releases the network resources used by a pod.
func (d *ipamDaemon) ReleaseIP(ctx context.Context, req *rpc.ReleaseIPRequest) (resp *rpc.ReleaseIPResponse, err error) {
	lg := log.WithFields(zlog.Fields{
		"Namespace":          req.K8SPodNamespace,
		"Name":               req.K8SPodName,
		"SandboxContainerId": req.K8SPodInfraContainerId,
	})
	lg.Infof("Handle ReleaseIP")
	defer runtime.HandleCrash()
	defer func() {
		if err != nil {
			lg.Warnf("Fail to handle ReleaseIP: %s", err.Error())
		} else {
			lg.Infof("Handle ReleaseIP succeed")
		}
	}()

	_, exist := d.pendingPods.LoadOrStore(zecTypes.PodKey(req.K8SPodNamespace, req.K8SPodName), struct{}{})
	if exist {
		return &rpc.ReleaseIPResponse{Code: rpc.ErrorCode_ConflictRequest}, fmt.Errorf("pod %s request processing", zecTypes.PodKey(req.K8SPodNamespace, req.K8SPodName))
	}
	defer func() {
		d.pendingPods.Delete(zecTypes.PodKey(req.K8SPodNamespace, req.K8SPodName))
	}()

	start := time.Now()
	defer func() {
		metrics.RpcLatency.WithLabelValues("ReleaseIP", fmt.Sprint(err != nil)).Observe(metrics.MsSince(start))
	}()

	oldPod, err := d.podPersistenceManager.Get(req.K8SPodNamespace, req.K8SPodName)
	if err != nil {
		if !errors.Is(err, apiErr.ErrNotFound) {
			return &rpc.ReleaseIPResponse{Code: rpc.ErrorCode_ReadDBError}, err
		}
		// No record: either there never was one (a DEL replayed after a
		// successful one -- CNI requires that to succeed) or the database was
		// lost while the pod kept running. The second case leaves the address
		// held with nothing that names it, so the pool is the only place left
		// to ask. Best effort: a pod that really has nothing here must still
		// get a successful DEL.
		d.releaseOrphanedResources(ctx, lg, req.K8SPodNamespace, req.K8SPodName)
		return &rpc.ReleaseIPResponse{}, nil
	}

	// Answer for the sandbox that asked. Records are keyed by (namespace, name),
	// and a StatefulSet pod keeps its name across recreations, so the name alone
	// does not identify a sandbox: a replayed DEL for an old sandbox would
	// otherwise release the addresses the current one is running on.
	if oldPod.SandboxContainerId != req.K8SPodInfraContainerId {
		return &rpc.ReleaseIPResponse{}, nil
	}

	netCtx := &netContext{
		Context: ctx,
		pod:     oldPod,
		log:     lg,
	}

	k8sPod, err := d.k8s.GetLocalCachedPod(req.K8SPodNamespace, req.K8SPodName)
	if err != nil && !apiErrors.IsNotFound(err) {
		return &rpc.ReleaseIPResponse{Code: rpc.ErrorCode_K8SAPIError}, fmt.Errorf("get pod from cache failed, %v", err)
	}

	newPod := d.translatePod(k8sPod)
	if newPod != nil && !d.verifyPodNetwork(newPod.PodNetworkMode) {
		return &rpc.ReleaseIPResponse{Code: rpc.ErrorCode_NetworkModeNotSupport}, fmt.Errorf("pod network mode not match with daemon")
	}

	for i, r := range oldPod.Resources {
		mgr := d.managers[r.Type]
		if mgr == nil {
			lg.Errorf("Find %s resource without manager", r.Type)
			continue
		}
		err = mgr.Release(netCtx, &oldPod.Resources[i])
		if err != nil && !errors.Is(err, pool.ErrResourceInvalid) {
			return &rpc.ReleaseIPResponse{Code: rpc.ErrorCode_ReleaseIPFailure}, fmt.Errorf("release resource %s[Type: %s] failed, %v", r.ID, r.Type, err)
		}
	}
	// Outside the loop: a record whose Resources list is empty still has to go,
	// or it lingers in the db and the next AllocIP for this pod picks it up as a
	// stale oldPod.
	err = d.podPersistenceManager.Delete(oldPod.Namespace, oldPod.Name)
	if err != nil {
		return &rpc.ReleaseIPResponse{Code: rpc.ErrorCode_WriteDBError}, fmt.Errorf("delete pod from db failed, %v", err)
	}
	// Any release can change the figure, not only a stock pod's: an address
	// given back from an interface outside the pod subnets lowers the limit.
	d.nudgeCapacity()
	return &rpc.ReleaseIPResponse{Code: rpc.ErrorCode_Success}, nil
}

// orphanIsReleasable reports whether a pod with no stored record has really
// finished with its addresses.
//
// The answers are deliberately asymmetric. NotFound clears a release outright. A
// pod that still exists clears it only while it is leaving -- a deletion in
// flight or a terminal phase -- which is the ordinary shape of a DEL; one that
// exists with neither is the replayed DEL this guards against. Any error other
// than NotFound counts as still there, so a lost API connection is never read as
// the pod having gone.
func (d *ipamDaemon) orphanIsReleasable(ctx context.Context, lg zlog.Logger, namespace, name string) bool {
	pod, err := d.k8s.GetPod(ctx, namespace, name)
	switch {
	case apiErrors.IsNotFound(err):
		return true
	case err != nil:
		lg.Warnf("Cannot confirm whether pod still exists; leaving its addresses for reconcile: %v", err)
		return false
	}

	if pod.DeletionTimestamp != nil {
		return true
	}
	if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
		return true
	}

	lg.Warnf("Pod is still running with no deletion in flight (phase %s); refusing to release the addresses "+
		"the pool holds for it -- this DEL names a sandbox the pod has retired, and releasing now would "+
		"take the address and the host rules away from the running one", pod.Status.Phase)
	return false
}

// releaseOrphanedResources frees what the pool still attributes to a pod with no
// stored record -- what PreStart adopts at start-up, since the sandbox id a
// release is matched against is not knowable from outside the runtime -- and
// removes the host rules installed for it.
//
// With no recorded sandbox id to compare, releasing on any DEL would honor a
// replayed one for a retired sandbox and take the address and rules away from
// the running pod. So it asks the API server first and releases only once the
// pod is gone or leaving; anything else waits for GC, which applies the same
// check and reclaims when the pod object goes away.
func (d *ipamDaemon) releaseOrphanedResources(ctx context.Context, lg zlog.Logger, namespace, name string) {
	owner := zecTypes.PodKey(namespace, name)
	orphan := &zecTypes.Pod{Namespace: namespace, Name: name}

	if !d.orphanIsReleasable(ctx, lg, namespace, name) {
		return
	}

	for _, mgr := range d.managers {
		if mgr == nil {
			continue
		}
		items := mgr.InuseByOwner(owner)
		for i := range items {
			lg.Warnf("Pod has no stored record but the pool still holds %s for it; releasing it", items[i].ID)
			if relErr := mgr.Release(&netContext{Context: context.Background(), pod: orphan, log: lg}, &items[i]); relErr != nil &&
				!errors.Is(relErr, pool.ErrResourceInvalid) {
				lg.Warnf("Cannot release orphaned resource %s: %v", items[i].ID, relErr)
				continue
			}
			orphan.Resources = append(orphan.Resources, items[i])
		}
	}

	if len(orphan.Resources) == 0 {
		return
	}
	// The rules outlive the netns and nothing else on the node knows which pod
	// they belonged to. They select on the pod address, so the next pod given
	// that address would inherit them.
	d.reclaimStaleRules(orphan)
}

// GetIPInfo returns the network configuration recorded for a pod.
func (d *ipamDaemon) GetIPInfo(ctx context.Context, req *rpc.GetIPInfoRequest) (resp *rpc.GetIPInfoResponse, err error) {
	lg := log.WithFields(zlog.Fields{
		"Namespace":          req.K8SPodNamespace,
		"Name":               req.K8SPodName,
		"SandboxContainerId": req.K8SPodInfraContainerId,
	})
	lg.Infof("Handle GetIPInfo")
	defer runtime.HandleCrash()
	defer func() {
		if err != nil {
			lg.Warnf("Fail to handle GetIPInfo: %s", err.Error())
		} else {
			lg.Infof("Handle GetIPInfo succeed")
		}
	}()

	pod, err := d.podPersistenceManager.Get(req.K8SPodNamespace, req.K8SPodName)
	if err != nil {
		if !errors.Is(err, apiErr.ErrNotFound) {
			return &rpc.GetIPInfoResponse{Code: rpc.ErrorCode_ReadDBError}, err
		} else {
			return &rpc.GetIPInfoResponse{Code: rpc.ErrorCode_PodNotFound}, nil
		}
	}

	// Answer for the sandbox that asked, not for whatever sandbox the pod name
	// currently maps to. ReleaseIP makes the same check.
	//
	// Records are keyed by (namespace, name), so a same-named pod that has been
	// deleted and recreated resolves to the *new* record. GetIPInfo drives the
	// teardown path, and kubelet replays DEL for a sandbox it has already
	// stopped -- so a stale replay would hand back the live pod's addresses and
	// the plugin would dismantle its datapath: the host veth and both ip rules.
	// The pod stays Running with an address and no connectivity, and nothing
	// re-runs ADD for a sandbox kubelet believes is already set up.
	//
	// PodNotFound is what the caller already handles: DoCmdDel treats it as
	// "nothing left to tear down" and returns success, which is exactly right
	// for a sandbox whose state is gone.
	if pod.SandboxContainerId != req.K8SPodInfraContainerId {
		lg.Infof("Record belongs to sandbox %s, not the requesting %s; nothing to tear down",
			pod.SandboxContainerId, req.K8SPodInfraContainerId)
		return &rpc.GetIPInfoResponse{Code: rpc.ErrorCode_PodNotFound}, nil
	}

	// Tolerate the pod being gone, exactly as ReleaseIP does.
	//
	// GetIPInfo is called on the teardown path, and by then the pod object may
	// already have been removed from the API -- a forced delete does that
	// immediately. Treating that as an error made every DEL arriving after the
	// object disappeared fail permanently: the address could then only be
	// recovered by the next reconcile tick, and the kubelet kept retrying a
	// teardown that could never succeed. Nothing below needs the live pod: the
	// reply is built from the stored record, and the network mode it carries
	// was already verified when the pod was admitted.
	k8sPod, err := d.k8s.GetLocalCachedPod(req.K8SPodNamespace, req.K8SPodName)
	if err != nil && !apiErrors.IsNotFound(err) {
		return &rpc.GetIPInfoResponse{Code: rpc.ErrorCode_K8SAPIError}, fmt.Errorf("get pod from cache failed, %v", err)
	}

	newPod := d.translatePod(k8sPod)
	if newPod != nil && !d.verifyPodNetwork(newPod.PodNetworkMode) {
		return &rpc.GetIPInfoResponse{Code: rpc.ErrorCode_NetworkModeNotSupport}, fmt.Errorf("pod network mode not match with daemon")
	}

	reply := &rpc.GetIPInfoResponse{
		Code:     rpc.ErrorCode_Success,
		IPv4:     types.IPStack(d.cfg.IPStack).EnableIPv4(),
		IPv6:     types.IPStack(d.cfg.IPStack).EnableIPv6(),
		NetConfs: []*rpc.NetConf{pod.MainInterface},
	}
	switch pod.PodNetworkMode {
	case zecTypes.PodNetworkModeENIShare:
		reply.IPType = rpc.IPType_TypeENIMultiIP
	default:
		return &rpc.GetIPInfoResponse{
			Code: rpc.ErrorCode_NetworkModeNotSupport,
		}, nil
	}
	return reply, nil
}

func (d *ipamDaemon) RecordEvent(ctx context.Context, req *rpc.RecordEventRequest) (resp *rpc.RecordEventResponse, err error) {
	lg := log.WithFields(zlog.Fields{
		"Namespace":   req.K8SPodNamespace,
		"Name":        req.K8SPodName,
		"EventTarget": req.EventTarget,
		"EventType":   req.EventType,
		"Reason":      req.Reason,
		"Message":     req.Message,
	})
	lg.Infof("Handle RecordEvent")
	defer runtime.HandleCrash()
	defer func() {
		if err != nil {
			lg.Warnf("Fail to handle RecordEvent: %s", err.Error())
		} else {
			lg.Infof("Handle RecordEvent succeed")
		}
	}()

	eventType := v1.EventTypeNormal
	if req.EventType == rpc.EventType_EventTypeWarning {
		eventType = v1.EventTypeWarning
	}

	reply := &rpc.RecordEventResponse{
		Code: rpc.ErrorCode_Success,
	}
	if req.EventTarget == rpc.EventTarget_EventTargetNode {
		d.k8s.RecordNodeEvent(eventType, req.Reason, req.Message)
		return reply, nil
	}
	// A vanished pod is not a failure to report. RecordPodEvent looks the pod up
	// to build the event's object reference, and the paths that record events
	// can run after the pod is already gone (a forced delete lands at any
	// moment). The caller has nothing to do about it, so debug beats a warning
	// that reads like a real problem. judgmentEvictPod records its explanation
	// before evicting, so that one still lands.
	err = d.k8s.RecordPodEvent(req.K8SPodName, req.K8SPodNamespace, eventType, req.Reason, req.Message)
	if err != nil {
		if apiErrors.IsNotFound(err) {
			lg.Debugf("Pod is gone, dropping the event for it: %v", err)
			err = nil
			return reply, nil
		}
		reply.Code = rpc.ErrorCode_K8SAPIError
		return reply, err
	}
	return reply, nil
}

// startDevicePlugin advertises the node's eni-ip capacity to the kubelet and
// keeps it current as the instance limits change.
func (d *ipamDaemon) startDevicePlugin() {
	if !d.cfg.DisableDevicePlugin {
		d.devicePluginManager = deviceplugin.NewResourcePluginManager(context.TODO(),
			deviceplugin.NewENIDevicePlugin(deviceplugin.ENIIPResourceName,
				max(0, d.eniIPManager.GetResourceLimit()-d.GetStockPodCount())))

		sigChannel := make(chan struct{}, 1)
		d.capacityNudge = sigChannel
		d.instanceLimit.WatchUpdate("device-plugin-eniip", sigChannel)
		go watchResourceNum(context.TODO(), d.devicePluginManager, deviceplugin.ENIIPResourceName, func() int {
			return max(0, d.eniIPManager.GetResourceLimit()-d.GetStockPodCount())
		}, sigChannel)
	} else {
		log.Warn("Device plugin disabled by config: the scheduler will not see this " +
			"node's eni-ip capacity, so pods must be bounded some other way")
	}
}

// newCRDIpamDaemon is NewIpamDaemon for ipamType crd: no OpenAPI client, no
// subnet or security-group manager, no instance metadata from the platform --
// the controlplane owns all of that and reports through the node's ZecNode.
func newCRDIpamDaemon(ctx context.Context, cfg *config.Config, k8s k8s.K8sService, podPersist *podPersistenceManager,
	namespace, nodeName string) (*ipamDaemon, error) {
	d := &ipamDaemon{
		networkMode:                cfg.NetworkMode,
		namespace:                  namespace,
		nodeName:                   nodeName,
		cfg:                        cfg,
		apiListenAddress:           defaultSocketPath,
		pendingPods:                sync.Map{},
		k8s:                        k8s,
		podPersistenceManager:      podPersist,
		managers:                   map[string]ResourceManager{},
		UnimplementedCNIIpamServer: rpc.UnimplementedCNIIpamServer{},
	}
	if err := d.syncPodPersistence(); err != nil {
		return nil, fmt.Errorf("sync pod persistence failed while daemon init, %w", err)
	}
	oldPods, err := podPersist.List()
	if err != nil {
		return nil, fmt.Errorf("list pod from persistence failed, %v", err)
	}
	allocatedResMap := zecTypes.GetNetResourceAllocatedFromPods(oldPods)
	if d.networkMode != types.ModeENIMultiIP {
		return nil, fmt.Errorf("no support network mode %s", d.networkMode)
	}
	var limit *crdLimit
	d.eniIPManager, limit, err = newCRDResourceManager(ctx, cfg, k8s, nodeName, allocatedResMap[zecTypes.NetResourceTypeEniIp])
	if err != nil {
		return nil, fmt.Errorf("create eniIP resource manager failed, %w", err)
	}
	d.instanceLimit = limit
	d.managers[zecTypes.NetResourceTypeEniIp] = d.eniIPManager
	d.startDevicePlugin()
	return d, nil
}
