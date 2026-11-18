// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/gdexlab/go-render/render"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/config"
	helper "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/factory"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/k8s"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/pool"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/signal"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/tracing"
	pkgUtils "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/utils"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

const (
	// defaultOrderCnt caps how many addresses one Create asks a new interface
	// for, and sizes both the per-interface order channel and the shared result
	// channel.
	defaultOrderCnt = 10
	// defaultEniPending bounds how many interfaces may be created or destroyed
	// at once; f.eniPending is the semaphore.
	defaultEniPending = 2
)

// ErrLegacy marks a recorded address set that does not cover every family this
// node runs. List files such an entry under Legacy, so the pool moves it to
// invalid and releases it rather than serving it to a pod.
var ErrLegacy = errors.New("legacy resource")

// eniIPResourceManager adapts the ENI-IP pool to the daemon's resource manager
// interface.
type eniIPResourceManager struct {
	pool pool.ResourcePool
	// factory is the direct-mode factory, for hooks the daemon wires after
	// construction; nil in crd mode.
	factory *eniIPFactory
}

func (m *eniIPResourceManager) Name() string {
	return "eniIP resource manager"
}

func (m *eniIPResourceManager) Allocate(ctx *netContext, prefer string) (zecTypes.NetResource, error) {
	res, err := m.pool.Allocate(ctx, prefer, zecTypes.PodKey(ctx.pod.Namespace, ctx.pod.Name))
	if err != nil {
		ctx.Log().Errorf("Allocate failed, %v", err)
		return nil, err
	}
	ctx.Log().Infof("Allocate succeed: %v", res)
	return res, nil
}

func (m *eniIPResourceManager) Release(ctx *netContext, resource *zecTypes.ResourceItem) error {
	err := m.pool.Release(resource.ID)
	if errors.Is(err, pool.ErrResourceInvalid) {
		// Callers treat it as already released: an address the pool never
		// held, e.g. one on an interface this node no longer manages.
		ctx.Log().Infof("Release %v: not held by the pool, nothing to give back", resource)
		return err
	}
	if err != nil {
		ctx.Log().Errorf("Release %v failed, %v", resource, err)
		return err
	}
	ctx.Log().Infof("Release %v succeed", resource)
	return nil
}

// InuseByOwner returns what the pool still attributes to owner, for the case
// where no stored record names the pod's addresses.
func (m *eniIPResourceManager) InuseByOwner(owner string) []zecTypes.ResourceItem {
	var items []zecTypes.ResourceItem
	for _, res := range m.pool.InuseByOwner(owner) {
		items = append(items, res.GetResourceItem())
	}
	return items
}

func (m *eniIPResourceManager) GetSnapshot() (pool.ResourcePoolSnapshot, error) {
	return m.pool.GetSnapshot()
}

func (m *eniIPResourceManager) GetPoolStatus() pool.Status {
	return m.pool.Status()
}

func (m *eniIPResourceManager) GetResourceLimit() int {
	return m.pool.GetResourceLimit()
}

func generateIPPoolCfg(cfg *config.Config, limits helper.InstanceLimits) pool.Config {
	totalMax := int(cfg.PoolTargetMax)
	if cfg.PoolTargetCapProbe {
		totalMax = limits.AllocatableIPs()
	}
	target := min(int(cfg.PoolTarget), int(math.Floor(cfg.PoolCapRatio*float64(totalMax))))
	targetMin := min(int(cfg.PoolTargetMin), int(math.Floor(cfg.PoolCapRatio*float64(totalMax))))
	return pool.Config{
		Name:            "eniIP",
		Type:            zecTypes.NetResourceTypeEniIp,
		TargetMin:       targetMin,
		Target:          target,
		MaxCap:          totalMax,
		MaxCapProbe:     cfg.PoolTargetCapProbe,
		MonitorInterval: time.Duration(cfg.PoolMonitorIntervalSec) * time.Second,
		GCProtectPeriod: time.Duration(cfg.PoolGCPeriodSec) * time.Second,
	}
}

// podTerminalGrace is how long a finished pod keeps its address attributed to
// it: the gap between a terminal phase and the CNI DEL completing, where the
// address is still configured and still the pod's own.
//
// Well past daemonRPCTimeout, the budget one CNI operation gets. Erring long
// costs a little capacity after a restart; erring short hands a second pod an
// address the first is still using, which does not recover.
const podTerminalGrace = 5 * time.Minute

// finishedLongerThan reports whether every container of a pod in a terminal
// phase has been stopped for at least d.
//
// The phase alone is not enough: it turns when the containers exit, which is
// before kubelet tears the sandbox down and runs the DEL. In that gap the
// address is still the pod's, and the index this feeds is consulted only for
// addresses no stored record claims -- so nothing else would catch the mistake.
//
// A container the status cannot account for keeps the pod: an unexplained
// container is not evidence that the sandbox is gone.
func finishedLongerThan(p *corev1.Pod, d time.Duration) bool {
	if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
		return false
	}
	var last time.Time
	for _, group := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for _, cs := range group {
			term := cs.State.Terminated
			if term == nil {
				return false
			}
			if term.FinishedAt.After(last) {
				last = term.FinishedAt.Time
			}
		}
	}
	// Nothing reported a finish time, so there is no window to measure.
	return !last.IsZero() && time.Since(last) > d
}

// ownerAliveAPITimeout bounds the direct API-server read below.
//
// That read happens inside pool.GC, which holds pause() for its whole duration:
// every Allocate on the node waits behind it. The client carries no timeout of
// its own, so an API server that accepts the connection and then stops
// answering stalled GC for as long as it stayed that way, and each pod waiting
// behind it failed its CNI ADD at daemonRPCTimeout with nothing in the error
// about why.
//
// Short rather than generous, because GC asks once per resource whose record is
// missing, and those calls are serial. A healthy in-cluster read is
// milliseconds; the only cost of cutting one short is answering "still there",
// which is what this function already answers for every inconclusive reply.
const ownerAliveAPITimeout = 2 * time.Second

// ownerStillExists is the second opinion GC needs when a stored record is
// missing but its pod may not be.
//
// The informer cache answers first because it costs nothing and needs no
// network, which also means an API server that has stopped answering does not
// reach the read below for any pod the cache still holds. A direct read follows
// only on a cache miss -- the cache lags, and acting on a lagging "gone"
// reclaims a running pod's address.
//
// Anything that is not a definite NotFound counts as "still there", a deadline
// included, so neither a broken connection nor a slow one can read as every pod
// on this node having disappeared at once.
//
// Existing is not the same as holding: the Pod object outlives the sandbox, and
// a Job with no ttlSecondsAfterFinished keeps one for ever. PreStart declines to
// attribute an address to a pod already past podTerminalGrace, and this is what
// reclaims one attributed by a restart that landed inside the window.
func ownerStillExists(k8s k8s.K8sService, owner string) bool {
	namespace, name, found := strings.Cut(owner, "/")
	if !found || namespace == "" || name == "" {
		// Not a pod key: available and invalid entries carry an empty owner,
		// and nothing else legitimately owns a resource.
		return false
	}
	if pod, err := k8s.GetLocalCachedPod(namespace, name); err == nil {
		return !finishedLongerThan(pod, podTerminalGrace)
	}

	ctx, cancel := context.WithTimeout(context.Background(), ownerAliveAPITimeout)
	defer cancel()
	pod, err := k8s.GetPod(ctx, namespace, name)
	if err == nil {
		return !finishedLongerThan(pod, podTerminalGrace)
	}
	if !k8sErrors.IsNotFound(err) {
		log.Warnf("Cannot confirm whether pod[%s] still exists, keeping its resource in use: %v", owner, err)
		return true
	}
	return false
}

func newEniIPResourceManager(cfg *config.Config, subnet helper.SubnetManager, secManager helper.SecurityGroupManager, zecAPI helper.ZecAPI, allocatedResource map[string]zecTypes.NetResourceAllocated, k8s k8s.K8sService) (*eniIPResourceManager, error) {
	log.Infof("Creating EniIPResourceManager")
	limit, err := helper.NewInstanceLimitManager(zecAPI)
	if err != nil {
		return nil, err
	}
	m := &eniIPResourceManager{}
	poolConfig := generateIPPoolCfg(cfg, limit.GetLimit())

	created, err := zecAPI.GetAttachedENIs()
	if err != nil {
		return nil, fmt.Errorf("get attached enis failed while init, %w", err)
	}

	eniFact, err := newEniFactory(secManager, subnet, zecAPI, limit, types.IPStack(cfg.IPStack))
	if err != nil {
		return nil, fmt.Errorf("create eni factory failed, %w", err)
	}

	// instanceLimitPeriod, not the reconcile interval: see that constant for why
	// the instance quota is polled on its own, shorter tick.
	eniFact.monitor(time.Duration(cfg.SubnetStatUpdateIntervalSec)*time.Second, instanceLimitPeriod)

	factory := &eniIPFactory{
		eniFactory:    eniFact,
		RWMutex:       sync.RWMutex{},
		enis:          []*ENI{},
		zecAPI:        zecAPI,
		eniIpReceiver: make(chan *ENIIPRes, defaultOrderCnt),
		eniPending:    make(chan struct{}, defaultEniPending),
		ipStack:       types.IPStack(cfg.IPStack),
		loggedLimit:   -1,
	}
	m.factory = factory
	poolConfig.Factory = factory
	// The second opinion GC needs when a stored record is missing but its pod is
	// not. The informer cache answers first because it costs nothing, and a
	// direct read follows only when the cache does not find the pod -- the cache
	// lags, and acting on a lagging "gone" reclaims a running pod's address. An
	// answer that is not a definite NotFound is treated as "still there", so a
	// broken connection to the API server cannot read as every pod on this node
	// having disappeared at once.
	poolConfig.OwnerAlive = func(owner string) bool {
		return ownerStillExists(k8s, owner)
	}
	poolConfig.PreStart = func(pool pool.ResourcePoolOp) error {
		livePodByIP, livePodV6, err := livePodAddresses(k8s)
		if err != nil {
			return err
		}

		for _, eni := range created {
			// Allocatable, not every address: each interface's own primary
			// must never enter the pool.
			v4s, v6Net, inErr := zecAPI.GetENIAllocatableIPs(eni)
			if inErr != nil {
				return fmt.Errorf("get ip list on eni %s failed, %w", eni.Mac.String(), inErr)
			}
			// An excluded primary serves no pod, so it needs no prefix -- and
			// asking for one bills it, or fails the start outright when its
			// subnet has no IPv6.
			if factory.ipStack.EnableIPv6() && v6Net == nil && !factory.excludedENI(eni) {
				var primaryV6 net.IP
				v6Net, primaryV6, inErr = zecAPI.GetOrAssignIPv6Block(eni.ID, eni.Mac.String())
				if inErr != nil {
					return fmt.Errorf("get or assign IPv6 block on eni %s failed, %w", eni.Mac.String(), inErr)
				}
				// The describe that produced the prefix also names the
				// interface's own primary IPv6, which falls inside it. Record it
				// or the carve has nothing to exclude and the first pod gets the
				// address the interface itself uses.
				if primaryV6 != nil {
					eni.PrimaryIP.IPv6 = primaryV6
				}
			}
			eni.IPv6Net = v6Net
			fEni := &ENI{
				Mutex:      sync.Mutex{},
				ENI:        eni,
				ips:        []*ENIIPRes{},
				order:      make(chan struct{}, defaultOrderCnt),
				stopWorker: make(chan struct{}, 1),
				zecApi:     zecAPI,
			}
			factory.appendEniLocked(fEni)

			if err := seedENIAddresses(pool, fEni, v4s, factory.ipStack, allocatedResource, livePodByIP, livePodV6, poolConfig.MaxCap); err != nil {
				return err
			}
			go fEni.worker(factory.eniIpReceiver)
		}
		return nil
	}
	p, err := pool.NewResourcePool(poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create resource pool %s failed, %w", poolConfig.Name, err)
	}
	// Started only once the pool exists, because NewResourcePool is what runs
	// PreStart, and PreStart builds f.enis without holding f.Lock -- correctly,
	// since nothing else exists yet to contend with it. Starting the monitor
	// first broke that: its goroutine ranges f.enis under f.Lock while PreStart
	// is appending to the slice, which is an unsynchronised slice-header write
	// against a concurrent read. On a dual node PreStart also calls
	// GetOrAssignIPv6Block per interface, which widens that window further.
	factory.subnetMonitor(time.Duration(cfg.SubnetStatUpdateIntervalSec)*time.Second,
		time.Duration(cfg.SubnetStatAgingSec)*time.Second)
	m.pool = p

	return m, nil
}

// ENIIPRes is an address set plus the error that disqualifies it. On the result
// channel that error means the assign failed; once recorded in ENI.ips it means
// the entry must not be served (ErrLegacy, ErrNotFound), which is what Valid
// reports and List classifies on.
type ENIIPRes struct {
	*zecTypes.ENIIP
	err error
}

// ENI is the factory's view of one attached interface: the addresses it holds,
// the orders outstanding against it, and the channels its worker runs on.
type ENI struct {
	sync.Mutex
	*zecTypes.ENI
	ips        []*ENIIPRes
	pending    int
	order      chan struct{}
	stopWorker chan struct{}
	// stopOnce guards stopWorker against a second close, which panics. Two
	// paths retire an interface -- the pool giving up its last address, and GC
	// finding it gone from the instance -- and they cannot see each other's
	// work once the interface has left f.enis.
	stopOnce sync.Once
	zecApi   helper.ZecAPI
	ipStack  types.IPStack
	// forbidAssign says why this interface is not eligible for new orders, and
	// therefore what has to change before it becomes eligible again. It used to
	// be a plain bool, and that is what made it wrong: three situations set it
	// and only one of their cures was ever checked. See forbidReason.
	forbidAssign forbidReason

	// maxIPv4 is the per-interface IPv4 quota, read fresh on each use because
	// the limit manager re-resolves it periodically.
	maxIPv4 func() int

	// v6Reserved holds IPv6 addresses carved out of this interface's prefix that
	// have not reached e.ips yet, keyed by string form. Guarded by the embedded
	// mutex, like e.ips.
	//
	// Shared rather than per-goroutine because four callers carve from the same
	// prefix (the worker, initENI, PreStart, GC). e.ips is the only other record
	// of what is taken, and it lags by however long a result spends traveling
	// the receiver channel; two carvers reading only e.ips in that window pick
	// the same address.
	v6Reserved map[string]struct{}
}

// reserveV6Locked records addresses as taken until they reach e.ips.
func (e *ENI) reserveV6Locked(ips []net.IP) {
	if len(ips) == 0 {
		return
	}
	if e.v6Reserved == nil {
		e.v6Reserved = make(map[string]struct{}, len(ips))
	}
	for _, ip := range ips {
		if ip != nil {
			e.v6Reserved[ip.String()] = struct{}{}
		}
	}
}

// releaseV6Locked gives addresses back, for the paths that carve an address and
// then find they cannot use it. Every carve needs either this or an
// appendIPLocked, or the address stays excluded for the life of the interface.
func (e *ENI) releaseV6Locked(ips []net.IP) {
	for _, ip := range ips {
		if ip != nil {
			delete(e.v6Reserved, ip.String())
		}
	}
}

func (e *ENI) appendIPLocked(ip *ENIIPRes) {
	// Spend the reservation first and unconditionally, including on the
	// duplicate path: the postcondition is "e.ips holds the address and nothing
	// reserves it". A reservation left behind can never be spent nor released,
	// so the offset leaves the prefix for the life of the interface -- silently,
	// since the platform publishes no per-address IPv6 view.
	if ip.IPSet.IPv6 != nil {
		delete(e.v6Reserved, ip.IPSet.IPv6.String())
	}
	for _, old := range e.ips {
		if old.GetID() == ip.GetID() {
			return
		}
	}
	ip.ENI = e.ENI
	e.ips = append(e.ips, ip)
}

// forbidReason is why an interface is not eligible for new orders.
//
// A set, not a single value: an interface can be out of service for more than
// one reason at once, and each reason has its own cure. With one slot the later
// reason overwrites the earlier one, and curing it re-admits an interface the
// first reason still applies to.
type forbidReason uint8

const (
	// forbidSubnet: the subnet cannot serve this order -- it is out of
	// addresses, or it does not carry every family this daemon needs. Cured by
	// the subnet itself becoming usable, which is what subnetMonitor watches.
	forbidSubnet forbidReason = 1 << iota

	// forbidInterfaceFull: this interface is at the per-NIC limit the platform
	// enforces. Nothing about the subnet cures it; only this interface giving
	// an address back does. Cleared in deleteIPLocked, the one place where that
	// actually happens.
	//
	// Note the daemon can hold this belief while its own count says there is
	// room -- the platform has refused an assign as over the per-NIC limit while
	// the daemon knew of fewer addresses than that limit. So the cure cannot be
	// "our count is below the limit"; it has to be an address actually leaving.
	forbidInterfaceFull

	// forbidRetired: the interface is being torn down (initENI failed, or
	// Release is destroying it). It must never become eligible again -- an
	// order admitted here has no worker to answer it, and the Create waiting on
	// that result blocks forever holding blockPause, which also stops GC.
	// Nothing clears this bit.
	forbidRetired

	// forbidNotPodSubnet: an interface in a subnet outside the configured pod
	// subnets, from which alone pod addresses come (see excludedENI). Fixed for
	// the daemon's run, since the subnets are read at startup. Every address on
	// it is Legacy (see excludedLocked): a pod keeps its own, the pool gives the
	// rest back. Unrecorded ones are taken IPv4 alone, nothing carved -- except
	// on the instance's primary, where they may be an operator's and are left.
	forbidNotPodSubnet
)

// forbidNone is the empty set: the interface is eligible.
const forbidNone forbidReason = 0

func (r forbidReason) String() string {
	if r == forbidNone {
		return "none"
	}
	var parts []string
	if r&forbidSubnet != 0 {
		parts = append(parts, "subnet cannot serve it")
	}
	if r&forbidInterfaceFull != 0 {
		parts = append(parts, "interface is at the per-NIC limit")
	}
	if r&forbidRetired != 0 {
		parts = append(parts, "interface is being retired")
	}
	if r&forbidNotPodSubnet != 0 {
		parts = append(parts, "interface outside the pod subnets")
	}
	return strings.Join(parts, "; ")
}

func (e *ENI) deleteIPLocked(ip *zecTypes.ENIIP) {
	for i, item := range e.ips {
		if item.GetID() == ip.GetID() {
			e.ips[len(e.ips)-1], e.ips[i] = e.ips[i], e.ips[len(e.ips)-1]
			e.ips = e.ips[:len(e.ips)-1]
			// An address just left this interface, so a per-NIC-full refusal no
			// longer describes it. This is the only event that cures that
			// reason: the subnet becoming available says nothing about how full
			// this interface is, and our own count was already wrong when the
			// refusal arrived.
			//
			// Deliberately not touching the other reasons: forbidSubnet is
			// subnetMonitor's to clear, and forbidRetired must survive
			// everything -- a retiring interface has no worker, so re-admitting
			// it strands the order and the Create waiting on it.
			e.forbidAssign &^= forbidInterfaceFull
			break
		}
	}
}

// isPrimaryLocked reports whether this is the instance's own interface, the one
// the CNI must never detach.
func (e *ENI) isPrimaryLocked() bool {
	// The embedded pointer is spelled out rather than letting IsPrimary be
	// promoted (which is what QF1008 asks for): the nil guard on the left is
	// load-bearing -- a promoted access on a nil embedded pointer panics -- and
	// naming the same field on both sides is what makes that visible.
	//nolint:staticcheck // QF1008: see above
	return e.ENI != nil && e.ENI.IsPrimary
}

func (e *ENI) getCurrentIPCountLocked() int {
	return len(e.ips) + e.pending
}

// poolCapacityLocked is how many addresses this ENI may hold in the pool:
// perENI-1 on every interface, because the platform counts an interface's
// primary against the per-NIC limit and the pool never holds a primary (the
// guest configures it on the interface; see eniAddrs).
//
// This narrows the gap rather than closing it -- the limit the platform
// actually enforces has been lower than the published figure, which is what the
// forbidAssign path handles.
func (e *ENI) poolCapacityLocked(perENI int) int {
	return max(0, perENI-1)
}

// roomForMoreLocked is how many addresses this interface can take beyond the
// ones it already holds. Orders in flight are not deducted: its only caller is
// the worker, sizing the batch those orders stand for.
func (e *ENI) roomForMoreLocked(perENI int) int {
	return max(0, e.poolCapacityLocked(perENI)-len(e.ips))
}

func (e *ENI) submitOrderLocked() error {
	select {
	case e.order <- struct{}{}:
	default:
		return fmt.Errorf("order overflow")
	}
	return nil
}

// ensureIPv6Prefix asks the platform for this interface's IPv6 prefix if it has
// none, and records the primary address that comes back with it -- the carve
// excludes that address, so recording it is not optional: handing out the one
// the interface already answers on black-holes the pod undetectably.
//
// The lock is dropped across the platform call, and taken again to publish. The
// same repair exists in PreStart and initENI; this is the one for interfaces
// that entered the factory some other way.
func (e *ENI) ensureIPv6Prefix() error {
	e.Lock()
	missing := e.IPv6Net == nil
	e.Unlock()
	if !missing {
		return nil
	}

	v6Net, primaryV6, err := e.zecApi.GetOrAssignIPv6Block(e.ID, e.Mac.String())
	if err != nil {
		// %w: receiveRes reads the platform code off this. The prefix assign
		// underneath answers a subnet that cannot carry IPv6 with
		// INVALID_SUBNET_IPSTACK_NOT_SUPPORT, and that is what wakes GC.
		return fmt.Errorf("get or assign ipv6 block on eni %s failed, %w", e.ID, err)
	}

	e.Lock()
	defer e.Unlock()
	e.IPv6Net = v6Net
	if primaryV6 != nil {
		e.PrimaryIP.IPv6 = primaryV6
	}
	return nil
}

func (e *ENI) worker(resultCache chan<- *ENIIPRes) {
	// Addresses this worker has already handed to resultCache but that the
	// receiving side may not have appended to e.ips yet. Recovery below must
	// not offer the same address twice, and e.ips alone lags behind.
	emitted := map[string]struct{}{}
	for {
		toAssign := 0
		select {
		case <-e.stopWorker:
			return
		case <-e.order:
			toAssign += 1
		}
		time.Sleep(500 * time.Millisecond)
	processOrder:
		for {
			select {
			case <-e.order:
				toAssign++
			default:
				break processOrder
			}
		}

		// Ask for no more than the interface can still hold. Overshooting is
		// not merely wasteful: the platform refuses the whole batch with
		// LAN_IP_EXCEED_LIMIT and has been observed to apply the addresses
		// anyway, which is exactly the case recoverAfterAmbiguousAssign exists
		// to clean up. Measuring here means that path is a fallback rather than
		// the normal way a full interface behaves.
		allowed := toAssign
		if e.maxIPv4 != nil {
			e.Lock()
			allowed = min(toAssign, e.roomForMoreLocked(e.maxIPv4()))
			e.Unlock()
		}
		if allowed < toAssign {
			log.Infof("Eni %s has room for %d of the %d addresses ordered; the rest go back for another interface",
				e.ID, allowed, toAssign)
		}
		if allowed == 0 {
			for i := 0; i < toAssign; i++ {
				resultCache <- &ENIIPRes{
					ENIIP: &zecTypes.ENIIP{ENI: e.ENI},
					err:   fmt.Errorf("%w: eni %s", errENIFull, e.ID),
				}
			}
			continue
		}

		log.Debugf("Begin assign %d ips on eni %s", allowed, e.ID)
		var (
			v4  []net.IP
			v6  []net.IP
			err error
		)

		if e.ipStack.EnableIPv6() {
			// An interface adopted by GC arrives without a prefix: factory.GC
			// runs with the pool paused and must not add a platform round trip,
			// so it does not ask for one. Repaired here instead, where the
			// worker is already talking to the platform. Without it every order
			// routed to that interface fails for the life of the process --
			// and nothing stops the routing, because the forbidAssign switch
			// below only reacts to platform error codes and a nil prefix is a
			// local one.
			if err = e.ensureIPv6Prefix(); err == nil {
				e.Lock()
				v6, err = e.AllocateIPv6AddressLocked(allowed)
				e.Unlock()
			}
		}
		if err == nil {
			v4, err = e.zecApi.AllocIPv4Addresses(e.ID, e.Mac.String(), allowed)
		}
		if err != nil {
			log.Errorf("Assign ip failed on eni %s, %v", e.ID, err)

			// The IPv6 side was carved before the IPv4 assign was attempted, so
			// give it back rather than leaving it reserved for an order that is
			// not happening. Without this every failed assign takes addresses
			// out of the prefix for the life of the interface.
			if len(v6) > 0 {
				e.Lock()
				e.releaseV6Locked(v6)
				e.Unlock()
				v6 = nil
			}

			// A failed assign does not mean the platform did nothing. When the
			// request times out or the connection breaks, the outcome is
			// unknown, not negative -- and the rollback inside
			// AllocIPv4Addresses can only undo the addresses it parsed out of a
			// response it never received, so it undoes nothing. The addresses
			// stay on the interface, invisible to this daemon, consuming the
			// per-NIC quota. Reconcile adopts them eventually, but that is up
			// to a full interval away and the node runs short of capacity until
			// then. Ask the platform what actually happened instead.
			recovered := e.recoverAfterAmbiguousAssign(err, toAssign, emitted)
			// recoverAfterAmbiguousAssign reports what the platform holds, which
			// is IPv4 only -- the IPv6 side never goes to the platform at all.
			// On a dual node an adopted address still needs its counterpart, or
			// the pod that receives it comes up with one family missing.
			if len(recovered) > 0 && e.ipStack.EnableIPv6() {
				e.Lock()
				pairV6, v6Err := e.AllocateIPv6AddressLocked(len(recovered))
				e.Unlock()
				if v6Err != nil {
					// Better to leave the addresses for reconcile than to hand
					// out half a pair: reconcile pairs them itself.
					log.Errorf("Could not pair the %d recovered addresses on eni %s with ipv6 (%v); leaving them for reconcile",
						len(recovered), e.ID, v6Err)
					// The recovered IPv4 addresses were already recorded in
					// emitted by recoverAfterAmbiguousAssign. Drop those records
					// too, or this worker excludes them forever and can never
					// adopt them on a later attempt.
					// No lock: emitted is created in worker() and never leaves
					// this goroutine.
					for _, ipSet := range recovered {
						if ipSet.IPv4 != nil {
							delete(emitted, ipSet.IPv4.String())
						}
					}
					recovered = nil
				} else {
					for i := range recovered {
						recovered[i].IPv6 = pairV6[i]
					}
				}
			}
			for _, ipSet := range recovered {
				resultCache <- &ENIIPRes{
					ENIIP: &zecTypes.ENIIP{
						ENI:   e.ENI,
						IPSet: ipSet,
					},
				}
			}
			for i := len(recovered); i < toAssign; i++ {
				resultCache <- &ENIIPRes{
					ENIIP: &zecTypes.ENIIP{
						ENI: e.ENI,
					},
					err: err,
				}
			}
			// Two distinct dead ends, same response: stop trying this ENI so
			// submitOrder moves on to one with room.
			//
			// By error code rather than substring -- the codes are what the
			// platform contracts on, and a substring miss is silent: the ENI
			// stays eligible and every later order retries it.
			//
			// Split by reason: these four codes are two problems with two cures,
			// and one "forbidden" flag applies the subnet's cure to the
			// interface's problem. See forbidReason.
			subnetCodes := &apiErr.OpenApiErrCodeChain{}
			nicCodes := &apiErr.OpenApiErrCodeChain{}
			switch {
			case subnetCodes.WithErrCodes(
				apiErr.InsufficientIpInSubnet,
				apiErr.OPERATION_DENIED_SUBNET_IP_INSUFFICIENT,
				apiErr.INVALID_SUBNET_IPV4_INSUFFICIENT,
			).ErrChainEqual(err):
				e.Lock()
				e.forbidAssign |= forbidSubnet
				e.Unlock()
			case nicCodes.WithErrCodes(
				apiErr.OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT,
				apiErr.INVALID_NIC_PRIVATE_IP_EXCEED_LIMIT,
			).ErrChainEqual(err):
				e.Lock()
				e.forbidAssign |= forbidInterfaceFull
				e.Unlock()
			}
		} else {
			// Into emitted before the send: until receiveRes appends an address
			// to e.ips, this set is the only record that it is already in
			// flight, and recoverAfterAmbiguousAssign would otherwise offer it
			// a second time.
			families := e.ipStack.Families()
			paired := zecTypes.PairIPs(v4, v6)
			sent := 0
			for _, ipSet := range paired {
				// PairIPs pads to the longer list, so a partial IPv4 assign
				// leaves a tail carrying only the locally carved IPv6 address.
				// Those used to go out as ordinary results, which handed a pod
				// an IPSet with no IPv4 in it. Drop them instead and let the
				// shortfall loop below report them, the same as any other
				// address the interface could not produce.
				if !ipSet.Complete(families) {
					log.Warnf("Eni %s produced an incomplete address pair (%s), dropping it: the assign returned %d of the %d ipv4 addresses ordered",
						e.ID, ipSet.String(), len(v4), allowed)
					// Nothing to leak when only the IPv6 side is present: it was
					// carved from this interface's prefix and never reached the
					// platform. An IPv4 without its pair is a different matter,
					// so say so -- reconcile is what adopts it back.
					if ipSet.IPv4 != nil {
						log.Warnf("Address %s on eni %s has no ipv6 counterpart and stays unrecorded until the next reconcile",
							ipSet.IPv4, e.ID)
					}
					// Give the carved IPv6 back: nothing will append it to
					// e.ips, so without this it stays reserved forever.
					if ipSet.IPv6 != nil {
						e.Lock()
						e.releaseV6Locked([]net.IP{ipSet.IPv6})
						e.Unlock()
					}
					continue
				}
				if ipSet.IPv4 != nil {
					emitted[ipSet.IPv4.String()] = struct{}{}
				}
				sent++
				resultCache <- &ENIIPRes{
					ENIIP: &zecTypes.ENIIP{
						ENI:   e.ENI,
						IPSet: ipSet,
					},
				}
			}
			// One result per order, always: the caller reads exactly as many as
			// it submitted and would block forever on a short count.
			for i := sent; i < toAssign; i++ {
				resultCache <- &ENIIPRes{
					ENIIP: &zecTypes.ENIIP{ENI: e.ENI},
					err:   fmt.Errorf("%w: eni %s", errENIFull, e.ID),
				}
			}
		}
	}
}

// assignDefinitelyRejected reports whether the platform certainly did not
// change anything: the request was understood and refused. These are a subset
// of the codes the assign retry loop treats as final -- the per-NIC-limit code
// the platform has been seen to return while applying the addresses anyway is
// left out on purpose. Every other failure -- a timeout, a dropped connection,
// a 500 -- leaves the outcome unknown.
func assignDefinitelyRejected(err error) bool {
	return (&apiErr.OpenApiErrCodeChain{}).WithErrCodes(
		apiErr.OPERATION_DENIED_SUBNET_TYPE_NOT_SUPPORT_IPV4,
		apiErr.INVALID_NIC_PRIVATE_IP_EXCEED_LIMIT,
		apiErr.INVALID_SUBNET_PRIVATE_IPV4_IN_USED,
		apiErr.INVALID_IP_BROADCAST_ADDRESS,
		apiErr.INVALID_IP_OUT_OF_RANGE,
		apiErr.INVALID_IP_FIRST_ADDRESS,
		apiErr.OPERATION_FAILED_RESOURCE_NOT_FOUND,
		apiErr.INVALID_IP_NETWORK_ADDRESS,
		apiErr.INVALID_NIC_STATUS,
	).ErrChainEqual(err)
}

// recoverAfterAmbiguousAssign asks the platform which addresses this interface
// actually holds, and returns the ones nothing here accounts for -- at most
// limit of them, since that is how many were ordered.
//
// It is deliberately one describe of one interface: the whole point is to run
// while the API is already unhealthy, so a full reconcile would be both slower
// and heavier than the problem it fixes. If the describe fails too, nothing is
// recovered and the address waits for reconcile exactly as before -- never
// worse than not trying.
func (e *ENI) recoverAfterAmbiguousAssign(assignErr error, limit int, emitted map[string]struct{}) []zecTypes.IPSet {
	if limit <= 0 || assignDefinitelyRejected(assignErr) {
		return nil
	}
	// Dual is handled, not skipped. This used to return early whenever IPv6 was
	// enabled, on the reasoning that a describe cannot tell us which IPv6 goes
	// with a recovered IPv4 and half a resource must not reach the pool. True
	// about the describe, wrong as a conclusion: the IPv6 side never goes to the
	// platform at all, so the caller can simply carve a counterpart for each
	// address recovered here. With the early return in place, a dual node
	// stranded every address the platform applied during an assign of unknown
	// outcome -- the exact leak this function exists to close.
	v4s, _, err := e.zecApi.GetENIAllocatableIPs(e.ENI)
	if err != nil {
		log.Warnf("Cannot check eni %s after an assign of unknown outcome, leaving it to reconcile: %v", e.ID, err)
		return nil
	}

	e.Lock()
	known := make(map[string]struct{}, len(e.ips))
	for _, ip := range e.ips {
		if ip.IPSet.IPv4 != nil {
			known[ip.IPSet.IPv4.String()] = struct{}{}
		}
	}
	e.Unlock()

	onNIC := make(map[string]struct{}, len(v4s))
	for _, ip := range v4s {
		onNIC[ip.String()] = struct{}{}
	}
	// emitted only needs to bridge the gap between handing an address over and
	// the factory recording it. Drop what the factory has caught up on, and
	// what the interface no longer carries -- otherwise a released address
	// would stay excluded forever and could never be recovered again.
	for addr := range emitted {
		_, isKnown := known[addr]
		_, stillThere := onNIC[addr]
		if isKnown || !stillThere {
			delete(emitted, addr)
		}
	}

	var recovered []zecTypes.IPSet
	for _, ip := range v4s {
		if len(recovered) >= limit {
			break
		}
		addr := ip.String()
		if _, ok := known[addr]; ok {
			continue
		}
		if _, ok := emitted[addr]; ok {
			continue
		}
		log.Warnf("Address %s appeared on eni %s from an assign that reported %v; the platform applied it after all, adopting instead of leaking it",
			addr, e.ID, assignErr)
		emitted[addr] = struct{}{}
		recovered = append(recovered, zecTypes.IPSet{IPv4: ip})
	}
	return recovered
}

// AllocateIPv6AddressLocked carves count addresses out of this interface's IPv6
// prefix. Caller holds e.Lock().
//
// Nothing here reaches the API -- the platform hands out a prefix, not
// addresses -- so the used set is only what this process knows: e.ips,
// e.v6Reserved (carved but not yet recorded), and the interface's own primary.
//
// All or nothing: on success exactly count addresses are returned and reserved,
// and the caller owns them from that moment. appendIPLocked spends the
// reservation and releaseV6Locked gives it back; a caller that does neither
// takes them out of the prefix for the life of the interface.
func (e *ENI) AllocateIPv6AddressLocked(count int) ([]net.IP, error) {
	if count <= 0 {
		return nil, errors.New("count must be > 0")
	}
	if e.IPv6Net == nil {
		return nil, fmt.Errorf("invalid IPv6 cidr %v of eni: %s", e.IPv6Net, e.GetID())
	}
	ipNet := e.IPv6Net
	prefixLen, bits := ipNet.Mask.Size()
	if bits != 128 {
		return nil, fmt.Errorf("invalid ipv6 cidr %v of eni: %s", e.IPv6Net, e.GetID())
	}
	baseIP := ipNet.IP.To16()
	if baseIP == nil {
		return nil, fmt.Errorf("invalid IPv6 address of ipv6 cidr %v of eni: %s", e.IPv6Net, e.GetID())
	}

	usedV6sMap := make(map[[16]byte]struct{})
	// Never hand out the interface's primary IPv6. There is no platform-side
	// address list to filter it against the way GetENIAllocatableIPs filters
	// PrimaryIP.IPv4, so the exclusion has to happen here -- otherwise the first
	// pod to ask is handed the address the interface itself holds, a collision
	// no reconcile can detect.
	if e.ENI != nil && e.PrimaryIP.IPv6 != nil {
		var key [16]byte
		copy(key[:], e.PrimaryIP.IPv6.To16())
		usedV6sMap[key] = struct{}{}
	}
	// The second candidate for this interface's primary, present only when the
	// platform's two sources named different addresses (see
	// GetENIAllocatableIPs). Excluded alongside the first because there is no
	// way to tell which one DHCPv6 actually configured, and the cost of being
	// wrong is a pod that is black-holed undetectably.
	if e.ENI != nil && e.PrimaryIPv6Disputed != nil {
		var key [16]byte
		copy(key[:], e.PrimaryIPv6Disputed.To16())
		usedV6sMap[key] = struct{}{}
	}
	for _, ip := range e.ips {
		if ip.IPSet.IPv6 != nil {
			var key [16]byte
			copy(key[:], ip.IPSet.IPv6.To16())
			usedV6sMap[key] = struct{}{}
		}
	}
	for addr := range e.v6Reserved {
		if ip := net.ParseIP(addr); ip != nil {
			var key [16]byte
			copy(key[:], ip.To16())
			usedV6sMap[key] = struct{}{}
		}
	}

	baseInt := big.NewInt(0).SetBytes(baseIP)
	// The number of addresses in the prefix: 2^(128 - prefixLen). Only the upper
	// bound of the scan below -- it stops as soon as it has count addresses, so
	// what it actually steps over is the used set, not the prefix.
	size := big.NewInt(0).Lsh(big.NewInt(1), uint(128-prefixLen))

	allocated := make([]net.IP, 0, count)
	candidateInt := new(big.Int)

	// Start at baseIP + firstPodOffsetV6; see that constant for the three
	// skipped offsets.
	//
	// The skip is convention, not protection: what must never be handed out is
	// the interface's primary, and the exclusions above are what withhold it.
	// Nothing outside this process could catch that mistake -- the platform
	// routes the whole prefix to the interface and publishes no per-address IPv6
	// view, so a pod holding the interface's own address looks like a pod
	// holding any other. See namesInterfacePrimary for how such a pod fails.
	for i := big.NewInt(firstPodOffsetV6); i.Cmp(size) < 0 && len(allocated) < count; i.Add(i, big.NewInt(1)) {
		candidateInt.Add(baseInt, i)

		// FillBytes, not Bytes: big.Int drops leading zeros, and a short slice
		// is not a valid net.IP.
		ipBytes := candidateInt.FillBytes(make([]byte, 16))
		candidateIP := net.IP(ipBytes)

		if !ipNet.Contains(candidateIP) {
			continue
		}

		var key [16]byte
		copy(key[:], candidateIP)

		if _, used := usedV6sMap[key]; !used {
			usedV6sMap[key] = struct{}{}
			allocated = append(allocated, candidateIP)
		}
	}

	if len(allocated) < count {
		return nil, fmt.Errorf("not enough available IPv6 addresses in IPv6 CIDR %v to allocate requested count for eni: %s", e.IPv6Net, e.GetID())
	}

	// Reserve before returning, so the next carver -- on any goroutine -- sees
	// these as taken even though nothing has reached e.ips yet.
	e.reserveV6Locked(allocated)
	return allocated, nil
}

// eniIPFactory backs the pool. It owns every managed interface on this node and
// turns pool orders into address assignments on one of them.
type eniIPFactory struct {
	eniFactory *eniFactory
	sync.RWMutex
	enis          []*ENI
	zecAPI        helper.ZecAPI
	eniIpReceiver chan *ENIIPRes
	eniPending    chan struct{}
	ipStack       types.IPStack
	// loggedLimit is the last limit GetResourceLimit logged, so a change is
	// logged once rather than on every device plugin poll; -1 until the first,
	// so a node starting at 0 logs it too. Guarded by the factory lock.
	loggedLimit int
	// onCapacity, when set, is told that the limit may have moved outside a
	// pod's own release (an address on an interface taking no new ones given
	// back by the pool's invalid queue or its shrink).
	onCapacity atomic.Pointer[func()]
}

func (f *eniIPFactory) Name() string {
	return zecTypes.NetResourceTypeEniIp
}

func (f *eniIPFactory) receiveRes() (ip *zecTypes.ENIIP, err error) {
	eniIP := <-f.eniIpReceiver
	resErr := eniIP.err
	if resErr != nil {
		// By error code, matching the forbidAssign check in worker(). Substring
		// tests left the per-NIC case half-covered: the platform returns
		// OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT while only the
		// INVALID_NIC_PRIVATE_IP_EXCEED_LIMIT spelling was listed, so a full
		// interface refreshed neither the instance limit nor GC.
		//
		// Comparing by code depends on the error still carrying one; see the %w
		// in AllocIPv4Addresses, and the test that pins it.
		limitCodes := &apiErr.OpenApiErrCodeChain{}
		if limitCodes.WithErrCodes(
			apiErr.INVALID_NIC_PRIVATE_IP_EXCEED_LIMIT,
			apiErr.OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT,
			apiErr.INVALID_SUBNET_IPSTACK_NOT_SUPPORT,
		).ErrChainEqual(resErr) {
			f.eniFactory.limit.Update()
			signal.NotifySignal(signal.WakeGC, signal.SigWakeGC)
		}
		watcherCodes := &apiErr.OpenApiErrCodeChain{}
		if watcherCodes.WithErrCodes(
			apiErr.OPERATION_DENIED_SUBNET_TYPE_NOT_SUPPORT_IPV4,
			apiErr.OPERATION_DENIED_INTERNET_CHARGE_TYPE_NOT_SUPPORT,
		).ErrChainEqual(resErr) {
			f.eniFactory.limit.NotifyWatcher()
		}
	}

	if eniIP.ENIIP == nil || resErr != nil {
		f.Lock()
		defer f.Unlock()
		// eniIP.ENI resolves through the embedded *ENIIP, so it may only be
		// read once ENIIP itself is known to be non-nil.
		if eniIP.ENIIP != nil && eniIP.ENI != nil {
			for _, eni := range f.enis {
				if eni.ENI == nil {
					continue
				}
				if eni.ID == eniIP.ENI.ID {
					// pending is guarded by eni's own lock; submitOrder
					// increments it under that lock.
					eni.Lock()
					eni.pending--
					eni.Unlock()
				}
			}
		}
		return nil, fmt.Errorf("ip allocated failed, %v", resErr)
	}

	f.Lock()
	defer f.Unlock()
	for _, eni := range f.enis {
		if eni.ENI != nil && eni.ID == eniIP.ENI.ID {
			eni.Lock()
			eni.appendIPLocked(eniIP)
			eni.pending--
			eni.Unlock()
			return eniIP.ENIIP, nil
		}
	}
	return nil, fmt.Errorf("expected resource not return, %s", render.Render(eniIP))
}

func (f *eniIPFactory) submitOrder() error {
	f.Lock()
	defer f.Unlock()

	for _, eni := range f.enis {
		if eni.ENI == nil {
			log.Debugf("Skip initializing eni")
			continue
		}
		eni.Lock()
		if eni.forbidAssign&forbidNotPodSubnet != 0 {
			// Outside the pod subnets: no order goes here whatever its stack,
			// and checking the stack would warn about it on every order.
			eni.Unlock()
			continue
		}
		subnet := f.eniFactory.subnets.GetPodSubnet(eni.Subnet.ID)
		if subnet == nil {
			log.Errorf("Get subnet %s of eni %s from subnetManager failed, not found", eni.Subnet.ID, eni.ID)
			eni.Unlock()
			continue
		}
		subnetIPStack := subnet.IPStack()
		if !subnetIPStack.Support(f.ipStack) {
			log.Warnf("Skip submit order to eni %s because subnet ipStack %s not support %s", eni.ID, subnetIPStack, f.ipStack)
			// A property of the subnet, so subnetMonitor's check is the right
			// cure -- Available() asks the same question for every family in use.
			eni.forbidAssign |= forbidSubnet
			eni.Unlock()
			continue
		}
		if eni.forbidAssign != forbidNone {
			log.Debugf("Skip submit order to eni %v because forbidAssign: %s", eni.GetID(), eni.forbidAssign)
			eni.Unlock()
			continue
		}

		if eni.getCurrentIPCountLocked() < eni.poolCapacityLocked(f.getLimit().IPv4MaxPerENI) {
			if err := eni.submitOrderLocked(); err != nil {
				log.Warnf("Submit order failed, %v", err)
				eni.Unlock()
				continue
			}
			eni.pending++
			eni.Unlock()
			return nil
		}
		eni.Unlock()
	}
	return fmt.Errorf("cant process order")
}

// cniENICountLocked counts only the interfaces this daemon created. f.enis also
// holds the instance's own interface, since GetAttachedENIs returns it so that
// its secondary addresses can serve pods -- but the node never created it and
// can never delete it, so it must not be counted against a quota that already
// excludes it.
func (f *eniIPFactory) cniENICountLocked() int {
	n := 0
	for _, e := range f.enis {
		// Spelled out for the same reason as isPrimaryLocked: the nil guard and
		// the field access have to name the same pointer to stay readable.
		//nolint:staticcheck // QF1008: see isPrimaryLocked
		if e.ENI == nil || !e.ENI.IsPrimary {
			n++
		}
	}
	return n
}

func (f *eniIPFactory) appendEniLocked(eni *ENI) *ENI {
	// Every interface answers to the factory's stack. The worker reads this to
	// decide whether a pod also gets an IPv6 address, and it used to be left at
	// its zero value by all three places that build an ENI -- which reports
	// neither family as enabled, so a dual-stack cluster would have quietly
	// handed out IPv4 only. Setting it at the one funnel every ENI passes
	// through means a future caller cannot forget it.
	eni.ipStack = f.ipStack
	eni.maxIPv4 = func() int { return f.getLimit().IPv4MaxPerENI }

	for _, old := range f.enis {
		id := old.GetID()
		if id != "" && id == eni.GetID() {
			return old
		}
	}
	// Marked once, as the interface joins: GC passes every attached interface
	// through here again, and would repeat the event on each pass.
	if f.excludedENI(eni.ENI) {
		eni.Lock()
		eni.forbidAssign |= forbidNotPodSubnet
		eni.Unlock()
		msg := fmt.Sprintf("Interface %s is in subnet %s, not one of the pod subnets: it serves no new pod, "+
			"and pod addresses come only from interfaces in the pod subnets", eni.ID, eni.Subnet.ID)
		log.Info(msg)
		_ = tracing.RecordNodeEvent(corev1.EventTypeNormal, tracing.EventENIExcluded, msg)
	}
	f.enis = append(f.enis, eni)
	return eni
}

// stopWorkerOnce ends the interface's worker goroutine. Safe to call twice.
func (e *ENI) stopWorkerOnce() {
	e.stopOnce.Do(func() { close(e.stopWorker) })
}

func (f *eniIPFactory) deleteEniLocked(eni *ENI) {
	if eni == nil {
		return
	}
	for i, item := range f.enis {
		if item == eni {
			f.enis[len(f.enis)-1], f.enis[i] = f.enis[i], f.enis[len(f.enis)-1]
			f.enis = f.enis[:len(f.enis)-1]
			break
		}
	}
}

// initENI creates the interface behind an ENI placeholder, answers the orders
// already promised on it, and starts its worker. Both exits -- the failure path
// and the success path -- must emit exactly as many results as were promised.
func (f *eniIPFactory) initENI(eni *ENI) {
	var ipv4s, ipv6s []net.IP
	var vpcEni zecTypes.NetResource
	// A non-nil err anywhere below means the interface is unusable and has to be
	// released again; see the epilogue at the end of this function.
	var err error

	defer func() {
		<-f.eniPending
	}()

	// The promise, captured before anything else can touch the counter.
	// submitOrder raises eni.pending too, once eni.ENI is set below, and those
	// orders are fulfilled by the worker instead. Reading the counter at the end
	// would count them twice and drive pending permanently negative, which makes
	// GC skip the interface forever and lets Release tear it down with an order
	// outstanding. Safe to read here: submitOrder skips an interface whose
	// eni.ENI is still nil.
	promised := eni.pending

	// pending+1: this count is addresses on the interface, and the platform
	// makes the first one the primary, which is withheld from pods. Asking for
	// pending yields pending-1 results and receiveRes blocks forever holding
	// blockPause. Bounded by poolCapacityLocked == perENI-1, so pending+1 never
	// exceeds the per-NIC limit.
	vpcEni, err = f.eniFactory.CreateWithIPCount(promised + 1)
	if err == nil {
		// Published under the factory lock, not the interface's.
		//
		// This store is what makes the interface visible: submitOrder,
		// receiveRes, Release, Valid, List, GC and subnetMonitor all skip one
		// whose ENI is still nil, and every one of them reads it holding
		// f.Lock/f.RLock and nothing else. Writing it under eni.Lock() would
		// leave writer and readers on disjoint locks -- still a data race, and
		// one with teeth: a reader can see the pointer non-nil while the struct
		// behind it is still zero, and submitOrder would then look up subnet ""
		// through a cloud describe while holding both locks.
		created, ok := vpcEni.(*zecTypes.ENI)
		f.Lock()
		eni.ENI = created
		f.Unlock()
		if !ok {
			err = fmt.Errorf("net resource created by factory is not expect type, get %+v, try release it", vpcEni)
			log.Error(err)
			releaseErr := f.eniFactory.Release(vpcEni)
			if releaseErr != nil {
				log.Errorf("Release unexpected resource %+v failed, %v", vpcEni, releaseErr)
			}
		} else {
			ipv4s, _, err = f.zecAPI.GetENIAllocatableIPs(eni.ENI)
			if err != nil {
				log.Errorf("Get ip list on eni failed, %v, try release it", err)
				releaseErr := f.eniFactory.Release(vpcEni)
				if releaseErr != nil {
					log.Errorf("Release eni %+v failed, %v", vpcEni, releaseErr)
				}
			}
			if err == nil && f.ipStack.EnableIPv6() {
				// Repair a missing prefix the same way PreStart does. The attach
				// response is allowed to come back without one -- the platform
				// assigns it asynchronously -- and without this the carve below
				// fails on a nil prefix, the interface is destroyed, and the
				// next pod ADD creates another one. On a two-interface instance
				// that burns the quota on the first attempt.
				if eni.IPv6Net == nil {
					v6Net, primaryV6, v6Err := f.zecAPI.GetOrAssignIPv6Block(eni.ID, eni.Mac.String())
					if v6Err != nil {
						// Release, like every other failure in this function.
						// Without it the interface stays created, attached and
						// tagged on the platform while being dropped from
						// f.enis, so cniENICountLocked falls and the next pod
						// creates another one -- on a two-interface instance
						// that burns the node's whole CNI quota on the first
						// failure, until a reconcile adopts the orphan back.
						// %w: this error leaves through initENI's failure
						// path, where receiveRes reads its code. The platform
						// answers a subnet that cannot carry IPv6 with
						// INVALID_SUBNET_IPSTACK_NOT_SUPPORT, and the prefix
						// assign underneath this call is what returns it.
						err = fmt.Errorf("get or assign ipv6 block on eni %s failed, %w", eni.ID, v6Err)
						log.Error(err)
						if releaseErr := f.eniFactory.Release(vpcEni); releaseErr != nil {
							log.Errorf("Release eni %+v failed, %v", vpcEni, releaseErr)
						}
					} else {
						eni.IPv6Net = v6Net
						// See the PreStart call: the primary falls inside the
						// prefix, so it has to be recorded with it.
						if primaryV6 != nil {
							eni.PrimaryIP.IPv6 = primaryV6
						}
					}
				}
			}
			// len(ipv4s) == 0 is not a reason to destroy the interface.
			//
			// createENI assigns its secondaries without waiting for metadata to
			// publish them (AllocIPv4Addresses does poll; this path does not),
			// so a just-attached interface can legitimately report none yet.
			// The IPv4-only arm keeps the interface and lets the shortfall loop
			// report it; carving for a count of zero instead returns an error
			// and takes the whole interface down with it -- create, attach,
			// detach, delete, then the next Create races the same window on a
			// new one. On a two-interface instance that is the only CNI
			// interface the node will ever get.
			if err == nil && f.ipStack.EnableIPv6() && len(ipv4s) > 0 {
				eni.Lock()
				ipv6s, err = eni.AllocateIPv6AddressLocked(len(ipv4s))
				eni.Unlock()
				if err != nil {
					// Release, for the reason given at the prefix repair above:
					// otherwise the interface stays on the platform while being
					// dropped from f.enis, and the node's CNI quota goes with it.
					err = fmt.Errorf("allocate ipv6 address from ipv6 cidr %v of eni %s failed, %v", eni.IPv6Net, eni.ID, err)
					log.Error(err)
					releaseErr := f.eniFactory.Release(vpcEni)
					if releaseErr != nil {
						log.Errorf("Release eni %+v failed, %v", vpcEni, releaseErr)
					}
				}
			}
			if err == nil && f.ipStack.EnableIPv4() && f.ipStack.EnableIPv6() {
				// A mismatch means the local IPv6 carve fell short of what the
				// platform handed out, which -- since the carve is all-or-nothing
				// -- only happens when the prefix is unusable. Release the whole
				// interface rather than salvage part of it: emitting fewer
				// results than promised leaves pending above zero for good, and
				// GC then skips the interface for the life of the daemon.
				if len(ipv4s) != len(ipv6s) {
					err = fmt.Errorf("the number of ipv4 and ipv6 not equal on eni %+v, try release it", vpcEni)
					log.Error(err)
					releaseErr := f.eniFactory.Release(vpcEni)
					if releaseErr != nil {
						log.Errorf("Release eni %+v failed, %v", vpcEni, releaseErr)
					}
				}
			}
		}
	}

	if err != nil {
		// Remove the interface from f.enis first, then count and answer.
		// Otherwise submitOrder can push a token into eni.order in the window
		// below, and this path starts no worker to answer it -- the Create
		// waiting on it blocks forever holding blockPause. Not a remote window:
		// createEniAsync runs only when no existing interface had room, so
		// submitOrder walking straight to this one is the expected case.
		f.Lock()
		f.deleteEniLocked(eni)
		f.Unlock()

		eni.Lock()
		owed := eni.pending
		// Retired, not merely forbidden: this path never starts a worker, so an
		// order admitted later has nothing to answer it and the Create waiting
		// on that result blocks forever holding blockPause. Nothing may clear
		// this reason.
		eni.forbidAssign |= forbidRetired
		eni.pending = 0
		eni.Unlock()

		for i := 0; i < owed; i++ {
			f.eniIpReceiver <- &ENIIPRes{
				ENIIP: &zecTypes.ENIIP{
					ENI: nil,
				},
				// %w: receiveRes is the reader of this channel, and it decides
				// by error code -- errors.As -- whether to refresh the instance
				// limit, wake GC and notify the limit watchers. This is the
				// last wrap before it, so a %v here disables all three for
				// every failure that happens during interface creation, which
				// is how OPERATION_DENIED_INTERNET_CHARGE_TYPE_NOT_SUPPORT and
				// INVALID_SUBNET_IPSTACK_NOT_SUPPORT reach receiveRes at all.
				// The symptom is the daemon quietly not reacting.
				err: fmt.Errorf("init eni failed, %w", err),
			}
		}

		return
	}

	// Exactly `promised` results, never fewer and never more: Create blocks in
	// receiveRes until it has read as many as it ordered, holding blockPause,
	// which also stops GC. A short count wedges the daemon. A partial address
	// list is reported as errors so the pool can ask again.
	//
	// Sent with no lock held. Holding eni.Lock() across a send on a full channel
	// closes a cycle: submitOrder wants it while holding f.Lock(), and the only
	// reader of the channel wants f.Lock().
	eni.Lock()
	eniRef := eni.ENI
	eni.Unlock()

	sent := 0
	for _, ipSet := range zecTypes.PairIPs(ipv4s, ipv6s) {
		if sent >= promised {
			break
		}
		f.eniIpReceiver <- &ENIIPRes{
			ENIIP: &zecTypes.ENIIP{
				ENI:   eniRef,
				IPSet: ipSet,
			},
			err: nil,
		}
		sent++
	}
	if sent < promised {
		log.Warnf("Eni %s came up with %d of the %d addresses ordered; reporting the rest as unavailable",
			eni.ID, sent, promised)
	}
	for i := sent; i < promised; i++ {
		f.eniIpReceiver <- &ENIIPRes{
			ENIIP: &zecTypes.ENIIP{ENI: eniRef},
			err:   fmt.Errorf("%w: eni %s produced %d of %d addresses", errENIFull, eni.ID, sent, promised),
		}
	}

	go eni.worker(f.eniIpReceiver)
}

// errENIQuotaReached means the instance already has every interface the CNI is
// allowed to create. It is a sentinel because the caller has to tell it apart
// from a real failure: a node running at capacity keeps asking for warm
// addresses, and every one of those asks reaches this. Logging each at error
// level buried the node in messages that say nothing is wrong.
var errENIQuotaReached = errors.New("the number of eni that can be created in this instance has reached the upper limit")

// errENIFull means the interface an order was queued against has no room left.
// Orders are admitted while the interface has room, but addresses can arrive
// between admission and assignment -- reconcile adopts what the platform
// reports, and so does the recovery after an assign of unknown outcome -- so
// the batch has to be measured again at the last moment.
var errENIFull = errors.New("no room left on this eni for more addresses")

// firstPodOffsetV6 is the first offset into an ENI's IPv6 prefix that a pod may
// use. All three skipped offsets are this CNI's own convention; the platform
// reserves none of them and forwards the whole /96.
//
//	::0  subnet-router anycast (RFC 4291)
//	::1  what an operator expects to mean "the gateway end"
//	::2  where an interface's primary IPv6 sits by default
//
// ::2 is a convention, not protection for the primary -- the primary is
// configurable and can sit anywhere in the prefix. What keeps it out of pods'
// hands is the exclusion of e.PrimaryIP.IPv6 (and PrimaryIPv6Disputed) in
// AllocateIPv6AddressLocked. Changing this constant neither creates nor closes
// that hazard.
const firstPodOffsetV6 = 3

// createEniAsync registers a placeholder for a new interface and hands it to
// initENI. It returns as soon as the placeholder is in f.enis; the withIPs
// orders are answered later through f.eniIpReceiver.
func (f *eniIPFactory) createEniAsync(withIPs int) (*ENI, error) {
	eni := &ENI{
		Mutex:        sync.Mutex{},
		ENI:          nil,
		ips:          make([]*ENIIPRes, 0),
		pending:      withIPs,
		order:        make(chan struct{}, defaultOrderCnt),
		stopWorker:   make(chan struct{}, 1),
		zecApi:       f.zecAPI,
		forbidAssign: forbidNone,
	}

	f.Lock()
	defer f.Unlock()
	// ENIAvailable() is the quota less the instance's own interface, so the
	// count compared against it must exclude that interface too.
	limit := f.getLimit().ENIAvailable()
	created := f.cniENICountLocked()
	if created < limit {
		select {
		case f.eniPending <- struct{}{}:
		default:
			return nil, fmt.Errorf("the number of eni processed at the same time has reached the upper limit %d", defaultEniPending)
		}
		f.appendEniLocked(eni)
		go f.initENI(eni)
	} else {
		return nil, fmt.Errorf("%w, limit: %d, current: %d", errENIQuotaReached, limit, created)
	}
	return eni, nil
}

// Create submits up to count orders and then reads back exactly as many results
// as it managed to submit; every order produces one result, success or failure.
// The pool calls this holding blockPause, so a result that never arrives blocks
// GC as well as this call.
func (f *eniIPFactory) Create(count int) ([]zecTypes.NetResource, error) {
	var err error
	var allocatedIP []zecTypes.NetResource
	submitted := 0

	for ; submitted < count; submitted++ {
		err = f.submitOrder()
		if err != nil {
			break
		}
	}

	lackIP := count - submitted
	// perENI-1, not perENI: an interface keeps its own primary address, so that
	// is how many pod addresses one can carry. Asking for perENI would order one
	// more than the interface can ever serve.
	lackIP = min(lackIP, max(0, f.getLimit().IPv4MaxPerENI-1))
	lackIP = min(lackIP, defaultOrderCnt)

	if lackIP > 0 {
		_, err = f.createEniAsync(lackIP)
		if errors.Is(err, errENIQuotaReached) {
			// Expected once the node is full: the pool goes on asking for warm
			// addresses and there is no interface left to put them on. Logged
			// at debug so a full node does not fill the log with a condition
			// nothing can act on; see errENIQuotaReached.
			log.Debugf("Not creating another eni, %v", err)
		} else if err != nil {
			log.Errorf("Create eni async failed, %v", err)
		} else {
			submitted += lackIP
		}
	}

	if submitted == 0 {
		return allocatedIP, fmt.Errorf("submit resource order failed, %v", err)
	}

	var eniIP *zecTypes.ENIIP
	for ; submitted > 0; submitted-- {
		eniIP, err = f.receiveRes()
		if err != nil {
			log.Errorf("Receive allocated ip address failed, %+v", err)
		} else {
			allocatedIP = append(allocatedIP, eniIP)
		}
	}

	if len(allocatedIP) == 0 {
		return nil, fmt.Errorf("receive allocated ip address failed, %+v", err)
	}
	return allocatedIP, nil
}

// ReleaseInValid unassigns the IPv4 half of a resource the pool has rejected and
// drops the local record -- except when the record names the interface's own
// primary, which is only dropped. There is no matching IPv6 call: the IPv6 is
// carved out of the interface's prefix locally, so dropping the record is the
// release.
func (f *eniIPFactory) ReleaseInValid(resource zecTypes.NetResource) (zecTypes.NetResource, error) {
	res, ok := resource.(*zecTypes.ENIIP)
	if !ok {
		return nil, fmt.Errorf("type of resource is not %s", zecTypes.NetResourceTypeEniIp)
	}
	if res.ENI == nil {
		return nil, fmt.Errorf("eni of resource is nil")
	}

	var temp *ENI
	eni := res.ENI
	ipSet := res.IPSet

	f.RLock()
	for _, e := range f.enis {
		if e.ENI == nil {
			continue
		}
		if e.ID == res.ENI.ID {
			temp = e
			break
		}
	}
	f.RUnlock()

	if temp == nil {
		log.Warnf("ENI %v not exist in this instance", eni)
		return nil, nil
	}

	if ipSet.GetIPv4() == "" || !eni.PrimaryIP.IPv4.Equal(ipSet.IPv4) {
		var v4s []net.IP
		if ipSet.IPv4 != nil {
			v4s = append(v4s, ipSet.IPv4)
		}

		err := f.zecAPI.DeallocIPv4Addresses(eni.ID, eni.Mac.String(), v4s)
		if err != nil {
			return nil, fmt.Errorf("dealloc ip address failed, %v", err)
		}
		temp.Lock()
		forbidden := temp.forbidAssign != forbidNone
		temp.deleteIPLocked(res)
		temp.Unlock()
		// On an interface taking no new addresses this lowers the limit
		// (GetResourceLimit counts what it can still contribute).
		if fn := f.onCapacity.Load(); forbidden && fn != nil {
			(*fn)()
		}
		return nil, nil
	}

	// The record names this interface's own primary, which a pod may not hold:
	// the guest configures it on the interface for a source the fabric accepts
	// (see eniAddrs), and GetENIAllocatableIPs withholds it from the pool.
	//
	// Drop the record and stop. Not deallocated -- the platform answers that
	// with INVALID_NIC_PRIMARY_IP_CANT_DETACH -- and not returned as usable:
	// handing it back would re-issue the interface's own address to a pod, which
	// black-holes it (see namesInterfacePrimary).
	log.Warnf("Record %s on eni %s names the interface's own primary address, which is not allocatable; dropping the record",
		ipSet.String(), eni.ID)
	temp.Lock()
	temp.deleteIPLocked(res)
	temp.Unlock()
	return nil, nil
}

func (f *eniIPFactory) Release(resource zecTypes.NetResource) error {
	res, ok := resource.(*zecTypes.ENIIP)
	if !ok {
		return fmt.Errorf("type of resource is not %s", zecTypes.NetResourceTypeEniIp)
	}
	if res.ENI == nil {
		return fmt.Errorf("eni of resource is nil")
	}
	var eni *ENI
	var eniIP *zecTypes.ENIIP

	f.RLock()
outLoop:
	for _, e := range f.enis {
		if e.ENI == nil {
			continue
		}
		if e.ID == res.ENI.ID {
			eni = e
			e.Lock()
			for _, ipRes := range e.ips {
				if ipRes.IPSet.String() == res.IPSet.String() {
					eniIP = ipRes.ENIIP
					e.Unlock()
					break outLoop
				}
			}
			e.Unlock()
		}
	}
	f.RUnlock()

	if eni == nil || eniIP == nil {
		return apiErr.ErrNotFound
	}

	// Giving up the last address means giving up the whole interface -- but
	// only for interfaces the CNI created. The instance's own interface holds
	// pod addresses too (its node address is filtered out upstream, so it never
	// appears in eni.ips), and draining its last one is the ordinary warm-pool
	// steady state, not a signal to free it. Taking this path for the primary
	// used to strand the node: stopWorker is closed and the ENI dropped from
	// f.enis before FreeENI is called, and FreeENI then refuses the primary --
	// leaving the interface gone from the factory with its worker dead, so no
	// order could ever be submitted to it again. The node kept its addresses
	// but could not grow, and only a daemon restart rebuilt the state.
	eni.Lock()
	if len(eni.ips) == 1 && !eni.isPrimaryLocked() {
		if eni.pending > 0 {
			eni.Unlock()
			return fmt.Errorf("allocate action on eni %s", eni.ID)
		}
		// Take the slot without holding the lock, and give up rather than wait.
		//
		// This send used to happen under eni.Lock(). f.eniPending holds
		// defaultEniPending slots and is drained by initENI's defer, so with
		// every slot held by initENI goroutines -- each of which takes f.Lock()
		// on its way out -- and a submitOrder holding f.Lock() waiting on this
		// very eni.Lock(), the three closed a cycle nothing could break.
		// Non-blocking also matches createEniAsync, which already treats a
		// full channel as "not now".
		eni.Unlock()
		select {
		case f.eniPending <- struct{}{}:
		default:
			return fmt.Errorf("eni %s not released now: %d interfaces already being processed", eni.ID, defaultEniPending)
		}
		eni.Lock()
		// Re-checked: the lock was released, so an order may have arrived.
		if eni.pending > 0 || len(eni.ips) != 1 {
			eni.Unlock()
			<-f.eniPending
			return fmt.Errorf("allocate action on eni %s", eni.ID)
		}
		// Retired: the interface is about to be destroyed.
		eni.forbidAssign |= forbidRetired
		eni.Unlock()

		f.Lock()
		eni.stopWorkerOnce()
		f.deleteEniLocked(eni)
		f.Unlock()

		err := f.eniFactory.Release(eni)
		<-f.eniPending
		if err != nil {
			return fmt.Errorf("release ENI for eniip failed, %v", err)
		}
		return nil
	}
	eni.Unlock()

	if res.ENI.PrimaryIP.IPv4.Equal(res.IPSet.IPv4) {
		return apiErr.ErrInvalidDeletionPrimaryIP
	}
	// Same for the IPv6 primary. It cannot be reached through the pool today --
	// the carve excludes it -- but a record predating that exclusion can still
	// name it, and unassigning the node's own address is not recoverable.
	if res.ENI.PrimaryIP.IPv6 != nil && res.ENI.PrimaryIP.IPv6.Equal(res.IPSet.IPv6) {
		return apiErr.ErrInvalidDeletionPrimaryIP
	}

	var v4s []net.IP
	if res.IPSet.IPv4 != nil {
		v4s = append(v4s, res.IPSet.IPv4)
	}

	// The deallocate is all-or-nothing -- one platform call covers every address
	// -- so any error means nothing was released and the local record has to
	// stay, or the address is stranded with no one left to reclaim it.
	err := f.zecAPI.DeallocIPv4Addresses(res.ENI.ID, res.ENI.Mac.String(), v4s)
	if err != nil {
		return fmt.Errorf("dealloc ipaddress failed, %v", err)
	}
	eni.Lock()
	forbidden := eni.forbidAssign != forbidNone
	eni.deleteIPLocked(eniIP)
	eni.Unlock()
	// As in ReleaseInValid: the pool's shrink lands here too.
	if fn := f.onCapacity.Load(); forbidden && fn != nil {
		(*fn)()
	}
	return nil
}

// namesInterfacePrimary reports whether an address set names, in either family,
// an address the interface itself holds. Such a set can never be a pod's: the
// host's `local` table matches at priority 0, ahead of the 512 to-pod rule, so
// the pod is black-holed while its address, routes and neighbor entries all
// look correct.
//
// Compared with net.IP.Equal, never as strings -- one IPv6 address has several
// legal renderings. PrimaryIPv6Disputed counts too: when the two sources
// disagree, neither candidate is safe to serve.
func namesInterfacePrimary(eni *zecTypes.ENI, set zecTypes.IPSet) bool {
	if eni == nil {
		return false
	}
	if set.IPv4 != nil && eni.PrimaryIP.IPv4 != nil && set.IPv4.Equal(eni.PrimaryIP.IPv4) {
		return true
	}
	if set.IPv6 != nil {
		if eni.PrimaryIP.IPv6 != nil && set.IPv6.Equal(eni.PrimaryIP.IPv6) {
			return true
		}
		if eni.PrimaryIPv6Disputed != nil && set.IPv6.Equal(eni.PrimaryIPv6Disputed) {
			return true
		}
	}
	return false
}

// Valid reports whether a resource may be served to a pod: nil sends it to the
// available queue, any error routes it through ReleaseInValid instead.
func (f *eniIPFactory) Valid(resource zecTypes.NetResource) error {
	eniIP, ok := resource.(*zecTypes.ENIIP)
	if !ok {
		return fmt.Errorf("type of resource is not %s", zecTypes.NetResourceTypeEniIp)
	}
	if eniIP.ENI == nil {
		return fmt.Errorf("eni of resource is nil")
	}

	// Refused unconditionally, ahead of the recorded error below. That answer is
	// whatever error the record carries, and only PreStart and GC ever set one
	// -- so a record naming the interface's own primary would be pronounced
	// healthy and served to a pod until a GC cycle stamped it. Other guards
	// withhold the primary earlier; this one does not depend on having
	// enumerated every entrance.
	if namesInterfacePrimary(eniIP.ENI, eniIP.IPSet) {
		return fmt.Errorf("address set %s on eni %s names the interface's own primary address; it is not allocatable",
			eniIP.IPSet.String(), eniIP.ENI.ID)
	}
	f.RLock()
	defer f.RUnlock()
	var find *ENIIPRes
	for _, eni := range f.enis {
		if eni.ENI == nil {
			continue
		}
		if eni.ID == eniIP.ENI.ID {
			eni.Lock()
			for _, addr := range eni.ips {
				if addr.IPSet.String() == eniIP.IPSet.String() {
					find = addr
					break
				}
			}
			eni.Unlock()
			if find != nil {
				break
			}
		}
	}

	if find != nil {
		if find.err == nil && f.excludedLocked(eniIP.ENI.ID) {
			return errENIExcluded
		}
		return find.err
	}

	return apiErr.ErrNotFound
}

// excludedENI reports an interface outside the configured pod subnets, from
// which alone pod addresses come: the instance's primary when its subnet is not
// one of them, or an interface created in a subnet since removed from them.
func (f *eniIPFactory) excludedENI(eni *zecTypes.ENI) bool {
	return eni != nil && f.eniFactory != nil && f.eniFactory.subnets != nil &&
		!f.eniFactory.subnets.IsPodSubnet(eni.Subnet.ID)
}

// excludedLocked reports an interface marked forbidNotPodSubnet. What it holds
// is Legacy: an idle address is given back by the pool's GC, one in use when
// its pod releases it. Caller holds f's lock, not the ENI's.
func (f *eniIPFactory) excludedLocked(eniID string) bool {
	for _, eni := range f.enis {
		if eni.ENI == nil || eni.ID != eniID {
			continue
		}
		eni.Lock()
		defer eni.Unlock()
		return eni.forbidAssign&forbidNotPodSubnet != 0
	}
	return false
}

var _ pool.UsabilityChecker = (*eniIPFactory)(nil)

// Usable is pool.UsabilityChecker: an address on an interface outside the pod
// subnets is not handed out, from startup on -- its records join the pool
// before the first GC can move them aside. Called under the pool lock; takes f's read lock and
// the ENI's.
func (f *eniIPFactory) Usable(resource zecTypes.NetResource) bool {
	res, ok := resource.(*zecTypes.ENIIP)
	if !ok || res.ENI == nil {
		return true
	}
	f.RLock()
	defer f.RUnlock()
	return !f.excludedLocked(res.ENI.ID)
}

// List classifies every address this factory records as normal, legacy or
// invalid, by comparing it against what the platform currently reports.
func (f *eniIPFactory) List() (map[zecTypes.ResStatus]map[string]zecTypes.NetResource, error) {
	f.RLock()
	defer f.RUnlock()

	list := map[zecTypes.ResStatus]map[string]zecTypes.NetResource{}
	list[zecTypes.ResStatusNormal] = map[string]zecTypes.NetResource{}
	list[zecTypes.ResStatusLegacy] = map[string]zecTypes.NetResource{}
	list[zecTypes.ResStatusInvalid] = map[string]zecTypes.NetResource{}

	// Every managed ENI, not just the secondaries: the primary contributes its
	// secondary addresses to the pool, and leaving it out here would mark each
	// of those as invalid for having no counterpart on the platform.
	enis, err := f.zecAPI.GetAttachedENIs()
	if err != nil {
		return nil, fmt.Errorf("get attached enis failed, %v", err)
	}

	eniIpMap := map[string]struct {
		v4Map map[string]net.IP
		v6Net *net.IPNet
	}{}
	for _, eni := range enis {
		v4s, v6Net, err := f.zecAPI.GetENIAllocatableIPs(eni)
		if err != nil {
			return nil, fmt.Errorf("get ip list for %s failed, %v", eni.Mac.String(), err)
		}
		eniIpMap[eni.Mac.String()] = struct {
			v4Map map[string]net.IP
			v6Net *net.IPNet
		}{v4Map: pkgUtils.NetIPToMap(v4s), v6Net: v6Net}
	}

	for _, eni := range f.enis {
		if eni.ENI == nil {
			continue
		}
		ipMap, exist := eniIpMap[eni.Mac.String()]
		if !exist {
			ipMap = struct {
				v4Map map[string]net.IP
				v6Net *net.IPNet
			}{v4Map: map[string]net.IP{}, v6Net: nil}
		}
		eni.Lock()
		excluded := eni.forbidAssign&forbidNotPodSubnet != 0
		for _, eniipRes := range eni.ips {
			valid := true
			if eniipRes.IPSet.IPv4 != nil {
				if _, ok := ipMap.v4Map[eniipRes.IPSet.IPv4.String()]; !ok {
					valid = false
				}
			}
			if eniipRes.IPSet.IPv6 != nil {
				if ipMap.v6Net == nil || !ipMap.v6Net.Contains(eniipRes.IPSet.IPv6) {
					valid = false
				}
			}
			item := &zecTypes.ENIIP{
				ENI:   eniipRes.ENI,
				IPSet: eniipRes.IPSet,
			}
			if !valid {
				list[zecTypes.ResStatusInvalid][item.GetID()] = item
			} else if eniipRes.err == nil && excluded {
				list[zecTypes.ResStatusLegacy][item.GetID()] = item
			} else if eniipRes.err == nil {
				list[zecTypes.ResStatusNormal][item.GetID()] = item
			} else if errors.Is(eniipRes.err, ErrLegacy) {
				list[zecTypes.ResStatusLegacy][item.GetID()] = item
			} else {
				list[zecTypes.ResStatusInvalid][item.GetID()] = item
			}
		}
		eni.Unlock()
	}
	return list, nil
}

func (f *eniIPFactory) subnetMonitor(updateInterval, aging time.Duration) {
	go wait.JitterUntil(func() {
		err := f.eniFactory.subnets.UpdateSubnetsStatus(helper.WithAging(aging))
		if err != nil {
			log.Errorf("SubnetMonitor reconcile failed, %v", err)
			return
		}
		f.Lock()
		for _, eni := range f.enis {
			if eni.ENI == nil {
				continue
			}
			subnet := f.eniFactory.subnets.GetPodSubnet(eni.Subnet.ID)
			if subnet == nil {
				log.Errorf("Get subnet %s of eni %s from subnetManager failed, not found", eni.Subnet.ID, eni.ID)
				continue
			}
			// Available(), not GetAvailableIPv4AddressCount(): the un-forbid
			// condition must ask the same question the admission check asks, for
			// every family in use.
			//
			// Under the ENI's own lock, which every other writer of this field
			// holds. Under f.Lock alone the two writers have disjoint lock sets
			// and a monitor tick can race away a worker's refusal.
			//
			// Clears forbidSubnet only. A subnet having addresses says nothing
			// about whether this interface is at its per-NIC limit, nor about
			// one being torn down.
			if subnet.Available(f.ipStack) {
				eni.Lock()
				eni.forbidAssign &^= forbidSubnet
				eni.Unlock()
			}
		}
		f.Unlock()
	}, updateInterval, 0.2, true, wait.NeverStop)
}

func (f *eniIPFactory) getLimit() *helper.InstanceLimits {
	limit := f.eniFactory.limit.GetLimit()
	return &limit
}

// GetResourceLimit is how many pod addresses this node can serve: the
// instance's allocatable total, less what the interfaces currently refusing
// orders can no longer contribute. Never negative.
func (f *eniIPFactory) GetResourceLimit() int {
	f.Lock()
	defer f.Unlock()

	limit := f.getLimit()
	// IPv4 alone sizes the node, in dual as well. The platform gives an
	// interface one IPv6 prefix rather than individual addresses and the CNI
	// carves them out locally, so IPv6 can never be the family that runs out
	// first -- an IPv6-only mode is what would need a per-interface v6 budget
	// here, and it is rejected at config load until then.
	perENI := limit.IPv4MaxPerENI
	nick := 0

	for _, eni := range f.enis {
		eni.Lock()
		if eni.forbidAssign != forbidNone {
			// poolCapacityLocked, not the raw per-NIC figure: what this
			// interface can still contribute is measured against what the pool
			// may hold on it, which is one fewer than the per-NIC figure on
			// every interface -- each keeps its own primary. Using the raw
			// figure deducts one address too many for each interface skipped.
			// Clamped per term. getCurrentIPCountLocked counts pending orders as
			// well as held addresses, and GC adoption can push an interface past
			// what the pool thought it held, so this difference can go negative
			// -- which would deduct nothing and instead add to the advertised
			// limit, offering slots the node cannot serve.
			nick += max(0, eni.poolCapacityLocked(perENI)-eni.getCurrentIPCountLocked())
		}
		eni.Unlock()
	}
	// AllocatableIPs, not ENIAvailable*IPv4MaxPerENI: the instance's own
	// interface contributes all but its primary address too.
	//
	// Clamped at zero. The nick subtraction can drive it negative -- nick is
	// summed over forbidden interfaces and is not bounded by AllocatableIPs.
	// A negative value is read badly by all three consumers: the device plugin
	// advertises nothing, the pool's maxCap never grows, and judgmentEvictPod's
	// "len(pods) >= limit" turns true for every pod and starts evicting.
	room := max(0, limit.AllocatableIPs()-nick)
	// The "InstanceLimit Updated" line shows the quota-based figure only; say
	// here when interfaces taking no new addresses (excluded, full) lower it.
	if room != f.loggedLimit {
		if nick > 0 {
			log.Infof("Capacity %d: %d of the quota-based %d sit on interfaces that take no new pod addresses",
				room, limit.AllocatableIPs()-room, limit.AllocatableIPs())
		} else {
			log.Infof("Capacity %d", room)
		}
		f.loggedLimit = room
	}
	return room
}

// GC reconciles the factory against the platform: interfaces the instance no
// longer has are dropped, recorded addresses the platform no longer reports are
// stamped ErrNotFound, and addresses nothing here accounts for are adopted onto
// the interface they sit on.
func (f *eniIPFactory) GC() error {
	f.Lock()
	defer f.Unlock()
	if len(f.eniPending) > 0 {
		log.Infof("Skip gc for eniIPFactory due to eni pending")
		return nil
	}

	log.Debugf("Start gc for eniIPFactory")
	ipKey := func(eniId, ip string) string {
		return fmt.Sprintf("%s/%s", eniId, ip)
	}

	eniMap := map[string]*struct { // key is eni-id
		skip bool
		*zecTypes.ENI
		v4s   []net.IP
		v6s   []net.IP
		v6Net *net.IPNet
	}{}
	ipMap := map[string]*struct { // key is eni-id/ip
		used  bool
		eniId string
		ip    zecTypes.IPSet
	}{}
	enis, err := f.zecAPI.GetAttachedENIs()
	if err != nil {
		return fmt.Errorf("get attachedENI failed, %v", err)
	}
	if len(enis) == 0 {
		return fmt.Errorf("pass gc due to the number of eni is 0")
	}

	for _, eni := range enis {
		// Allocatable, not the full list. Reconciliation adopts any address it
		// finds that the pool does not know about, which is what lets an address
		// survive a restart -- but an interface's own primary is configured on it
		// by the guest, and adopting that would hand a pod the address the node
		// itself answers on. See namesInterfacePrimary for why such a pod is then
		// black-holed with nothing to find.
		v4s, v6Net, err := f.zecAPI.GetENIAllocatableIPs(eni)
		if err != nil {
			return fmt.Errorf("get ip list for %s failed, %v", eni.ID, err)
		}
		eniMap[eni.ID] = &struct {
			skip bool
			*zecTypes.ENI
			v4s   []net.IP
			v6s   []net.IP
			v6Net *net.IPNet
		}{ENI: eni, v6Net: v6Net}

		for _, v4 := range v4s {
			ipMap[ipKey(eni.ID, v4.String())] = &struct {
				used  bool
				eniId string
				ip    zecTypes.IPSet
			}{
				eniId: eni.ID,
				ip: zecTypes.IPSet{
					IPv4: v4,
				},
			}
		}
	}

	if len(ipMap) == 0 {
		// Nothing is reconciled against an empty answer, but an interface
		// outside the pod subnets that the platform and the local records
		// both show empty can still be freed -- the pass below never runs
		// once the last such address is gone.
		for _, fEni := range slices.Clone(f.enis) {
			if fEni.ENI == nil || eniMap[fEni.ID] == nil {
				continue
			}
			fEni.Lock()
			f.freeDrainedLocked(fEni)
			fEni.Unlock()
		}
		return fmt.Errorf("pass gc due to the number of ip is 0")
	}

	// Interfaces this factory still holds that the instance no longer has.
	// Detaching one out of band -- from the console, or by another tool -- is
	// the case this covers.
	//
	// The addresses are handled by the marking loop below, which stamps
	// ErrNotFound on every one the platform no longer reports; the pool then
	// releases them, and ReleaseInValid takes its "eni not on this instance"
	// path, so nothing tries to unassign an address from a detached interface.
	// What that loop does not do is drop the interface itself. Left in f.enis
	// with its addresses gone, it looks completely free -- poolCapacityLocked
	// reports perENI-1 with nothing held -- so submitOrder prefers it over
	// every real interface and each order spends an API round trip failing
	// against a NIC that is not there.
	//
	// Skipped while pending != 0. An order already admitted has to be answered
	// by the worker, and stopping the worker first leaves the Create waiting on
	// it blocked forever holding blockPause, which also stops GC. Retiring it
	// costs one more GC interval and is the safe direction.
	var vanished []*ENI
	for _, fEni := range f.enis {
		if fEni.ENI == nil || fEni.ID == "" {
			continue
		}
		if _, exist := eniMap[fEni.ID]; exist {
			continue
		}
		fEni.Lock()
		pending := fEni.pending
		if pending == 0 {
			// No further orders: submitOrder skips a retired interface, and
			// this runs under f.Lock() so none can arrive between here and the
			// removal below.
			fEni.forbidAssign |= forbidRetired
		}
		fEni.Unlock()
		if pending == 0 {
			vanished = append(vanished, fEni)
		} else {
			log.Warnf("ENI %s is gone from this instance but has %d pending order(s); retiring it next cycle",
				fEni.ID, pending)
		}
	}
	for _, fEni := range vanished {
		log.Warnf("ENI %s is no longer attached to this instance; dropping it and stopping its worker", fEni.ID)
		fEni.stopWorkerOnce()
		f.deleteEniLocked(fEni)
	}

	// Mark every recorded address the platform still reports, and stamp
	// ErrNotFound on the ones it does not: those are gone from under us.
	for _, fEni := range f.enis {
		if fEni.ENI == nil {
			continue
		}
		if eni, exist := eniMap[fEni.ID]; exist && fEni.pending != 0 {
			eni.skip = true
			continue
		}

		fEni.Lock()
		for _, fIp := range fEni.ips {
			if fIp.IPSet.IPv4 != nil {
				if o, exist := ipMap[ipKey(fIp.ENI.ID, fIp.IPSet.GetIPv4())]; !exist {
					fIp.err = apiErr.ErrNotFound
				} else {
					o.used = true
				}
			}
			if fIp.IPSet.IPv6 != nil {
				v6 := fIp.IPSet.IPv6
				used := false
				// The interface may be absent from eniMap: the platform stops
				// reporting one an operator has detached, and a describe can lag
				// behind an attach. The v4 branch above and the pending check
				// both guard on exist; this one did not, and dereferenced eni.ID
				// a few lines below. Detaching an interface is ordinary operator
				// action, so on a dual-stack node that nil dereference is
				// reachable without anything being broken -- and it lands inside
				// GC, which holds the pool paused, so it takes the daemon down at
				// the moment every starting pod is waiting on it.
				eni, exist := eniMap[fEni.ID]
				if !exist {
					fIp.err = apiErr.ErrNotFound
					continue
				}
				if eni.v6Net != nil && eni.v6Net.Contains(v6) {
					used = true
				} else {
					fIp.err = apiErr.ErrNotFound
				}
				ipMap[ipKey(fEni.ID, v6.String())] = &struct {
					used  bool
					eniId string
					ip    zecTypes.IPSet
				}{
					used:  used,
					eniId: eni.ID,
					ip: zecTypes.IPSet{
						IPv6: v6,
					},
				}
			}
		}
		fEni.Unlock()
	}

	for _, o := range ipMap {
		if o.used {
			continue
		}
		e, exist := eniMap[o.eniId]
		if !exist {
			// Same reason as the v6 branch above: the interface may be gone
			// from the platform's view by now, and there is nothing to adopt
			// the address onto.
			continue
		}
		// An address on the primary ENI that this daemon has no record of is
		// not necessarily this CNI's -- an operator may have attached it for
		// something else. The platform offers no per-address tag, so ownership
		// cannot be proven after the fact; the local record is the only
		// evidence. It is adopted anyway, which is the same policy PreStart
		// applies and documents at the equivalent point: an address the daemon
		// really did lose track of is the far more likely case, and stranding
		// it costs capacity on every restart. The cost of being wrong is that
		// an operator's address on the instance's own interface can end up
		// serving a pod. Not on a primary outside the pod subnets, which
		// serves no pod: there it is left alone (see the adopt switch below).
		if o.ip.IPv4 != nil {
			e.v4s = append(e.v4s, o.ip.IPv4)
		}
		if o.ip.IPv6 != nil {
			e.v6s = append(e.v6s, o.ip.IPv6)
		}
	}

	// Adopt what is left: addresses the platform reports that nothing here
	// accounted for, onto the interface they sit on.
	for _, item := range eniMap {
		if item.skip {
			continue
		}
		temp := &ENI{
			Mutex:      sync.Mutex{},
			ENI:        item.ENI,
			ips:        []*ENIIPRes{},
			order:      make(chan struct{}, defaultOrderCnt),
			stopWorker: make(chan struct{}, 1),
			zecApi:     f.zecAPI,
		}
		fEni := f.appendEniLocked(temp)
		fEni.Lock()
		adopt := f.ipStack
		if fEni.forbidAssign&forbidNotPodSubnet != 0 {
			// On a secondary (this plugin's own): v4 alone, nothing carved;
			// List files it Legacy and the pool gives it back. On the
			// instance's primary nothing: it may be an operator's.
			adopt = types.IPStackIPv4
			if fEni.isPrimaryLocked() {
				adopt = ""
			}
		}
		if fEni == temp {
			// Not on the pass that (re)built the interface, whatever its
			// subnet: one that dropped out of a describe for a pass lost its
			// records with it, and what its pods hold would be adopted under
			// IDs their records do not match -- given back on an excluded
			// interface, handed to a second pod on any other (dual-stack
			// records are <v4>-<v6>, a fresh pairing is not).
			adopt = ""
		}
		switch adopt {
		case types.IPStackIPv4:
			for _, v4 := range item.v4s {
				fEni.appendIPLocked(&ENIIPRes{
					ENIIP: &zecTypes.ENIIP{
						ENI: item.ENI,
						IPSet: zecTypes.IPSet{
							IPv4: v4,
						},
					},
				})
			}
		case types.IPStackIPv6:
			for _, v6 := range item.v6s {
				fEni.appendIPLocked(&ENIIPRes{
					ENIIP: &zecTypes.ENIIP{
						ENI: item.ENI,
						IPSet: zecTypes.IPSet{
							IPv6: v6,
						},
					},
				})
			}
		case types.IPStackDual:
			// Adopt, exactly as the IPv4 arm above does, minting an IPv6
			// counterpart from this interface's own prefix (what PreStart does).
			// Pairing against item.v6s instead would adopt half sets: that list
			// holds only recorded IPv6 addresses that fell outside the prefix,
			// so it is normally empty, and PairIPs pads the rest with nil. List
			// files a v4-only set as Normal -- it checks a family only when the
			// family is present -- so the pool would hand it to a pod.
			//
			// If the prefix is unusable, leave the addresses for the next cycle
			// to find again rather than adopting half sets.
			if len(item.v4s) > 0 {
				v6s, v6Err := fEni.AllocateIPv6AddressLocked(len(item.v4s))
				if v6Err != nil {
					log.Errorf("Cannot pair the %d unrecorded addresses on eni %s with ipv6 (%v); leaving them alone",
						len(item.v4s), item.ID, v6Err)
				} else {
					for i, pair := range zecTypes.PairIPs(item.v4s, v6s) {
						log.Infof("Adopting unrecorded address %s on eni %s, paired with %s",
							item.v4s[i], item.ID, v6s[i])
						fEni.appendIPLocked(&ENIIPRes{
							ENIIP: &zecTypes.ENIIP{
								ENI:   item.ENI,
								IPSet: pair,
							},
						})
					}
				}
			}
		}
		if fEni != temp && f.freeDrainedLocked(fEni) {
			fEni.Unlock()
			continue
		}
		if fEni == temp {
			go fEni.worker(f.eniIpReceiver)
		}
		fEni.Unlock()
	}
	return nil
}

// freeDrainedLocked frees an interface outside the pod subnets once it holds
// no address: its pods are gone and the pool gave the rest back. Only
// secondaries get here, and only ones this plugin created (GetAttachedENIs
// manages no other secondary outside the pod subnets). Not on the GC pass that
// built the interface, whose addresses have not been looked at yet. The free
// runs in the background holding an eniPending slot, which keeps GC off until
// it is done and keeps the API call off the GC path. Caller holds f.Lock and
// e.Lock.
func (f *eniIPFactory) freeDrainedLocked(e *ENI) bool {
	if e.forbidAssign&forbidNotPodSubnet == 0 || e.isPrimaryLocked() || len(e.ips) > 0 || e.pending > 0 {
		return false
	}
	select {
	case f.eniPending <- struct{}{}:
	default:
		return false
	}
	e.forbidAssign |= forbidRetired
	e.stopWorkerOnce()
	f.deleteEniLocked(e)
	log.Infof("ENI %s is outside the pod subnets and holds no pod address; freeing it", e.ID)
	go func(eni *zecTypes.ENI) {
		defer func() { <-f.eniPending }()
		if err := f.eniFactory.Release(eni); err != nil {
			log.Errorf("Free ENI %s outside the pod subnets: %v", eni.ID, err)
		}
	}(e.ENI)
	return true
}

// livePodAddresses indexes this node's pods by the addresses the API server
// says they hold, both ways round.
//
// The stored records are the primary source but not the only authority: if
// the store is lost, every address on a managed ENI looks unclaimed and would
// be pooled as free, live pods' included. Failing here is deliberate -- a
// failed start is retried by the kubelet, a duplicated address is not
// recoverable on its own.
func livePodAddresses(k8s k8s.K8sService) (map[string]string, map[string]net.IP, error) {
	livePodByIP := map[string]string{}
	livePodV6 := map[string]net.IP{}
	localPods, listErr := k8s.ListLocalPods(context.Background())
	if listErr != nil {
		return nil, nil, fmt.Errorf("list pods of this node to identify addresses already in use: %w", listErr)
	}
	for _, p := range localPods {
		if p == nil || p.Spec.HostNetwork {
			continue
		}
		// A pod that finished long enough ago is not holding anything: its
		// sandbox is gone, its DEL has run, and Status.PodIPs is a record of
		// what it had rather than a claim on it. Leaving it in would
		// attribute the address to it again on this restart, and OwnerAlive
		// would then keep it there for as long as the Pod object exists.
		if finishedLongerThan(p, podTerminalGrace) {
			log.Infof("Pod %s finished more than %v ago; not treating %v as still held by it",
				zecTypes.PodKey(p.Namespace, p.Name), podTerminalGrace, p.Status.PodIPs)
			continue
		}
		// PodIPs, not PodIP: on a dual-stack pod PodIP is only PodIPs[0],
		// so indexing by it alone leaves the IPv6 half of every pod
		// invisible here -- and with the record store lost, this index is
		// what stops the rebuilt pool from handing a live pod's address to
		// a second pod. PodIPs is empty on older objects, so fall back to
		// PodIP.
		ips := make([]string, 0, 2)
		for _, ip := range p.Status.PodIPs {
			if ip.IP != "" {
				ips = append(ips, ip.IP)
			}
		}
		if len(ips) == 0 && p.Status.PodIP != "" {
			ips = append(ips, p.Status.PodIP)
		}
		for _, ip := range ips {
			livePodByIP[ip] = zecTypes.PodKey(p.Namespace, p.Name)
		}
		// The same index the other way round. Recovering a pod needs the
		// address that actually goes with the one found, and only the API
		// server knows the pairing -- the platform publishes no per-address
		// IPv6 view, so a locally carved counterpart is a guess.
		for _, ip := range ips {
			if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() == nil {
				livePodV6[zecTypes.PodKey(p.Namespace, p.Name)] = parsed
			}
		}
	}
	return livePodByIP, livePodV6, nil
}

// seedENIAddresses puts an interface's addresses into the pool at startup:
// the stored records as in use, the rest as available or -- when a live pod
// holds one with no record -- in use by that pod. v4s is every allocatable
// IPv4 the platform reports on the interface; fEni.IPv6Net must already be
// set for a stack with IPv6.
func seedENIAddresses(p pool.ResourcePoolOp, fEni *ENI, v4s []net.IP, stack types.IPStack,
	allocated map[string]zecTypes.NetResourceAllocated, livePodByIP map[string]string,
	livePodV6 map[string]net.IP, maxCap int) error {
	eni := fEni.ENI
	var err error
	v4sMap := pkgUtils.NetIPToMap(v4s)
	for _, item := range allocated {
		if item.Resource.GetResourceItem().ENIId != eni.ID {
			continue
		}
		ipSet := zecTypes.IPSet{
			IPv4: net.ParseIP(item.Resource.GetResourceItem().IPv4),
			IPv6: net.ParseIP(item.Resource.GetResourceItem().IPv6),
		}
		// The IPv4 leaves the "unclaimed" set on the strength of the
		// record, before any family is stripped -- the address is taken
		// whatever stack this daemon now runs.
		if ipSet.IPv4 != nil {
			delete(v4sMap, ipSet.IPv4.String())
		}

		// Strip to the families in use first, then give the same object
		// to both the pool and the factory. IPSet is a value and GetID()
		// folds the addresses in, so registering an unstripped copy
		// keys the two sides differently ("eni/<v4>-<v6>" vs
		// "eni/<v4>"). Identical on a dual node; on a node switched back
		// to ipv4 it makes pool.GC push a live pod's address into
		// available. Switching the stack is supported.
		if !stack.EnableIPv4() {
			ipSet.IPv4 = nil
		}
		if !stack.EnableIPv6() {
			ipSet.IPv6 = nil
		}
		// A record naming the interface's own primary is corrupt, and
		// registering it is worse than losing it. It cannot be a pod's
		// address -- the guest has it configured on the interface -- so
		// the pod this record belongs to is not actually using it, and
		// keeping the record only puts the address somewhere a later
		// release can hand it to a pod that then black-holes.
		//
		// Dropped here rather than marked: a marked record still enters
		// the pool, occupies a slot and inflates resource_pool_total
		// until something reclaims it, and the reclaim path for a
		// primary is a no-op by design. Nothing downstream wants it.
		if namesInterfacePrimary(eni, ipSet) {
			log.Warnf("Stored record for pod %s names eni %s's own primary address (%s); "+
				"dropping it -- an address configured on the interface cannot also be a pod's",
				item.Owner, eni.ID, ipSet.String())
			continue
		}
		var resErr error
		if (stack.EnableIPv4() && ipSet.IPv4 == nil) ||
			(stack.EnableIPv6() && ipSet.IPv6 == nil) {
			resErr = ErrLegacy
		}
		// An IPv6 outside this interface's prefix is not routed to it. The pod
		// that has it keeps it; on release the pair is reclaimed, never pooled.
		if ipSet.IPv6 != nil && fEni.IPv6Net != nil && !fEni.IPv6Net.Contains(ipSet.IPv6) {
			log.Warnf("Stored record for pod %s pairs %s with %s, outside eni %s's prefix %s; "+
				"it will be reclaimed on release", item.Owner, ipSet.IPv4, ipSet.IPv6, eni.ID, fEni.IPv6Net)
			resErr = ErrLegacy
		}
		eniIP := &zecTypes.ENIIP{
			ENI:   eni,
			IPSet: ipSet,
		}
		p.AddInuse(eniIP, item.Owner)
		fEni.appendIPLocked(&ENIIPRes{
			ENIIP: eniIP,
			err:   resErr,
		})
	}

	if stack.EnableIPv4() && !stack.EnableIPv6() {
		// What remains in v4sMap are addresses the platform reports on a
		// managed ENI that no stored pod claims. Adopting them is what
		// lets an address survive a restart -- the platform still counts
		// it against the per-NIC quota, so leaving it shrinks the node's
		// capacity on every restart.
		//
		// This includes the instance's own interface (its primary is
		// already excluded by GetENIAllocatableIPs), so an address an
		// operator attached by hand is adoptable too -- the accepted
		// cost of having no per-address ownership tag. Not when that
		// interface is outside the pod subnets: then only what a live pod
		// holds is taken.
		fEni.Lock()
		skipUnowned := fEni.forbidAssign&forbidNotPodSubnet != 0 && fEni.isPrimaryLocked()
		fEni.Unlock()
		for _, ipv4 := range v4sMap {
			if _, live := livePodByIP[ipv4.String()]; skipUnowned && !live {
				continue // an excluded primary's: may be an operator's
			}
			eniIP := &ENIIPRes{
				ENIIP: &zecTypes.ENIIP{
					ENI: eni,
					IPSet: zecTypes.IPSet{
						IPv4: ipv4,
					},
				},
			}
			fEni.appendIPLocked(eniIP)
			// A pod on this node is holding this address even though no
			// stored record mentions it -- the record was lost. Take the
			// API server's word for it and mark the address in use, or
			// the next pod scheduled here would be given an address that
			// is already carrying traffic.
			if owner, held := livePodByIP[ipv4.String()]; held {
				log.Warnf("Address %s on eni %s is held by pod %s but has no stored record; recovering it as in-use",
					ipv4, eni.ID, owner)
				p.AddInuse(eniIP.ENIIP, owner)
				continue
			}
			p.AddAvailable(eniIP.ENIIP) // no need to deal with ipv6
		}
	} else if !stack.EnableIPv4() && stack.EnableIPv6() {
		fEni.Lock()
		v6List, err := fEni.AllocateIPv6AddressLocked(maxCap)
		fEni.Unlock()
		if err != nil {
			return fmt.Errorf("allocate IPv6 address from ipv6 cidr %v of eni %s  failed, %v", fEni.IPv6Net.String(), fEni.ID, err)
		}
		for _, ipv6 := range v6List {
			eniIP := &ENIIPRes{
				ENIIP: &zecTypes.ENIIP{
					ENI: eni,
					IPSet: zecTypes.IPSet{
						IPv6: ipv6,
					},
				},
			}
			fEni.appendIPLocked(eniIP)
			p.AddAvailable(eniIP.ENIIP) // no need to deal with ipv4
		}
	} else {
		var v4List []net.IP
		for _, v4 := range v4sMap {
			v4List = append(v4List, v4)
		}
		// Nothing to pair is the common case, not an error: v4sMap holds
		// only the addresses the platform reports that no stored record
		// claims, and on a freshly started node with an intact record
		// store -- or on a primary interface carrying nothing but the
		// node's own address, which GetENIAllocatableIPs filters out --
		// that set is empty. AllocateIPv6AddressLocked rejects a count
		// of zero, so calling it unconditionally failed PreStart, and a
		// failed PreStart takes the whole daemon down. Every dual node
		// would have CrashLooped on its first start.
		if len(v4List) == 0 {
			return nil
		}
		// Pair from what the API server says, not from carve order.
		// Zipping freshly carved addresses against v4List pairs them at
		// random (v4List comes from ranging a map), and on this path --
		// a lost record store with pods still running -- the carve can
		// reproduce an address a live pod already holds. livePodV6 knows
		// the real pairing: use it, reserve the live addresses first, and
		// carve only for what no live pod claims.
		fEni.Lock()
		// Only the ones e.ips does not already carry. A live pod whose
		// stored record survived was appended above, which spent any
		// reservation for it -- reserving it again here would leave an
		// entry nothing can ever spend, which takes the address out of
		// the prefix for the life of the interface.
		held := make(map[string]struct{}, len(fEni.ips))
		for _, rec := range fEni.ips {
			if rec.IPSet.IPv6 != nil {
				held[rec.IPSet.IPv6.String()] = struct{}{}
			}
		}
		var liveV6 []net.IP
		for _, v6 := range livePodV6 {
			if fEni.IPv6Net == nil || !fEni.IPv6Net.Contains(v6) {
				continue
			}
			if _, ok := held[v6.String()]; ok {
				continue
			}
			liveV6 = append(liveV6, v6)
		}
		fEni.reserveV6Locked(liveV6)
		fEni.Unlock()

		type recovered struct {
			v4    net.IP
			v6    net.IP
			owner string
			// legacy: on an excluded interface, held by no live pod. Taken
			// as a v4-only Legacy entry for the pool to give back; nothing
			// is carved for it.
			legacy bool
		}
		items := make([]recovered, 0, len(v4List))
		carveFor := 0
		fEni.Lock()
		excluded := fEni.forbidAssign&forbidNotPodSubnet != 0
		excludedPrimary := excluded && fEni.isPrimaryLocked()
		fEni.Unlock()
		for _, v4 := range v4List {
			it := recovered{v4: v4}
			if excluded {
				// The interface serves no new pod and may have no prefix: carve
				// nothing. What a live pod holds is recovered for it; on a
				// secondary (this plugin's own) the rest is Legacy, given back by
				// the pool; on the instance's primary it is left alone.
				if owner, ok := livePodByIP[v4.String()]; ok {
					it.owner = owner
					if v6 := livePodV6[owner]; v6 != nil && fEni.IPv6Net != nil && fEni.IPv6Net.Contains(v6) {
						it.v6 = v6
					}
				} else if excludedPrimary {
					continue // may be an operator's
				} else {
					it.legacy = true
				}
				items = append(items, it)
				continue
			}
			if owner, ok := livePodByIP[v4.String()]; ok {
				it.owner = owner
				if v6 := livePodV6[owner]; v6 != nil && fEni.IPv6Net != nil && fEni.IPv6Net.Contains(v6) {
					it.v6 = v6
				}
			}
			if it.v6 == nil && it.owner == "" {
				carveFor++
			}
			items = append(items, it)
		}

		var carved []net.IP
		if carveFor > 0 {
			fEni.Lock()
			carved, err = fEni.AllocateIPv6AddressLocked(carveFor)
			fEni.Unlock()
			if err != nil {
				return fmt.Errorf("allocate IPv6 address from ipv6 cidr %v of eni %s failed, %v", fEni.IPv6Net.String(), fEni.ID, err)
			}
		}

		next := 0
		for _, it := range items {
			if it.v6 == nil && it.owner == "" && !it.legacy {
				it.v6 = carved[next]
				next++
			}
			eniIP := &ENIIPRes{
				ENIIP: &zecTypes.ENIIP{
					ENI:   eni,
					IPSet: zecTypes.IPSet{IPv4: it.v4, IPv6: it.v6},
				},
			}
			// An incomplete set is marked, exactly as the stored-record
			// arm above marks one. Without it Valid() returns nil, so
			// pool.Release skips ReleaseInValid and pushes the half set
			// onto the available queue -- and factory.List classifies it
			// Normal, because it checks a family only when that family
			// is present, so every later reconcile re-queues it too.
			// Either way the next pod on a dual node comes up with no
			// IPv6 and nothing ever repairs the entry.
			if it.v6 == nil {
				eniIP.err = ErrLegacy
			}
			if it.owner == "" {
				fEni.appendIPLocked(eniIP)
				p.AddAvailable(eniIP.ENIIP)
				continue
			}
			// A live pod claims this address. Recover it as in-use so
			// the next pod scheduled here is not given an address that
			// is already carrying traffic.
			if it.v6 == nil {
				// The pod holds this IPv4 but no IPv6 out of this
				// interface's prefix. Record what is true rather than
				// inventing a counterpart: an invented one would be
				// configured on no interface, and releasing the resource
				// instead would unassign the address the pod is using.
				// Being in use does not keep it out of the available
				// queue: pool.GC moves an in-use entry there once its
				// record is gone and its owner is not alive. The
				// ErrLegacy stamped above does -- Valid returns it and
				// List files the entry under Legacy.
				log.Warnf("Pod %s holds address %s on eni %s with no ipv6 from this interface's prefix; recovering the ipv4 alone",
					it.owner, it.v4, eni.ID)
			} else {
				log.Warnf("Address set %s on eni %s is held by pod %s but has no stored record; recovering it as in-use",
					eniIP.IPSet.String(), eni.ID, it.owner)
			}
			fEni.appendIPLocked(eniIP)
			p.AddInuse(eniIP.ENIIP, it.owner)
		}
	}
	return nil
}
