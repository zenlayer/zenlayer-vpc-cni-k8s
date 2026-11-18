// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/apis/network/v1alpha1"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/config"
	helper "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/factory"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/version"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/k8s"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/pool"
	zapiCom "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/common"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

const (
	crdStartPoll = 5 * time.Second
	// crdReclaimEvery spaces re-reading the identity while the controlplane
	// reports it does not match: each read walks the metadata service.
	crdReclaimEvery = 30 * time.Second
)

// reclaimIdentity reads the node's identity again and, when it differs from
// the one written, writes the new one. It reports whether it wrote.
func reclaimIdentity(ctx context.Context, f *crdIPFactory, md *helper.ZecAPIImpl, k8sSvc k8s.K8sService) (bool, error) {
	node, err := k8sSvc.GetLocalNode(ctx)
	if err != nil {
		return false, err
	}
	claim, err := identityClaim(md, node)
	if err != nil {
		return false, err
	}
	f.mu.Lock()
	same := f.claim == claim
	f.claim = claim
	f.mu.Unlock()
	if same {
		return false, nil
	}
	log.Infof("This node's identity changed to %+v; writing it to the ZecNode", claim)
	return true, f.writeSpec(ctx)
}

// identityClaim reads this node's own account of which instance it is: the
// instance ID from DMI, and the metadata service's MAC and address of the
// interface that carries the Node's InternalIP. Any read that fails is an
// error for the caller to retry: a claim missing its MAC because of one
// metadata hiccup could never be verified, and would replace a good one.
func identityClaim(md *helper.ZecAPIImpl, node *corev1.Node) (v1alpha1.IdentityClaim, error) {
	id, err := zapiCom.GetInstanceID()
	if err != nil {
		return v1alpha1.IdentityClaim{}, fmt.Errorf("read instance id from DMI: %w", err)
	}
	claim := v1alpha1.IdentityClaim{InstanceID: id}
	// The first IPv4 InternalIP, the same choice the controlplane makes: a
	// dual-stack node may list its IPv6 first.
	nodeIP := ""
	for _, a := range node.Status.Addresses {
		if ip := net.ParseIP(a.Address); a.Type == corev1.NodeInternalIP && ip != nil && ip.To4() != nil {
			nodeIP = a.Address
			break
		}
	}
	if nodeIP == "" {
		return claim, fmt.Errorf("node %s has no IPv4 InternalIP yet", node.Name)
	}
	macs, err := md.MetadataMACs()
	if err != nil {
		return claim, fmt.Errorf("list interfaces from metadata: %w", err)
	}
	for _, mac := range macs {
		ip, err := md.MetadataPrimaryIPv4(mac)
		if err != nil {
			return claim, fmt.Errorf("read the address of interface %s from metadata: %w", mac, err)
		}
		if ip != nil && ip.String() == nodeIP {
			claim.PrimaryMAC, claim.PrimaryIP = mac, nodeIP
			return claim, nil
		}
	}
	// Written anyway: the controlplane reports the mismatch on the ZecNode,
	// which is where an operator looks.
	log.Warnf("No interface in metadata carries the node's InternalIP %q; claiming instance %s alone", nodeIP, id)
	return claim, nil
}

// newCRDResourceManager builds the address pool of ipamType crd: the daemon
// asks the controlplane through its ZecNode and never calls the platform.
// It returns once the controlplane has verified the node, so the CNI
// configuration -- written after this -- is never published on a node that
// cannot get addresses.
func newCRDResourceManager(ctx context.Context, cfg *config.Config, k8sSvc k8s.K8sService, nodeName string,
	allocated map[string]zecTypes.NetResourceAllocated) (*eniIPResourceManager, *crdLimit, error) {
	stack := types.IPStack(cfg.IPStack)

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("create incluster config failed: %v", err)
	}
	restCfg.UserAgent = version.UserAgent()
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, err
	}

	md := helper.NewMetadataOnly(stack)
	var claim v1alpha1.IdentityClaim
	if err := pollUntil(ctx, "this node's identity from the metadata service", func() (bool, error) {
		node, err := k8sSvc.GetLocalNode(ctx)
		if err == nil {
			claim, err = identityClaim(md, node)
		}
		if err != nil {
			log.Warnf("Read identity: %v", err)
			return false, nil
		}
		return true, nil
	}); err != nil {
		return nil, nil, err
	}

	zn := &dynamicZecNode{res: dyn.Resource(v1alpha1.GVR), name: nodeName}
	f := newCRDIPFactory(stack, claim, md, zn)

	// The controlplane creates the ZecNode; the daemon only writes its spec.
	var obj *v1alpha1.ZecNode
	if err := pollUntil(ctx, "the controlplane to create ZecNode "+nodeName, func() (bool, error) {
		o, err := zn.Get(ctx)
		if err != nil {
			if isNotFound(err) {
				return false, nil
			}
			return false, err
		}
		obj = o
		return true, nil
	}); err != nil {
		return nil, nil, err
	}
	// Addresses this node gave back before a restart stay given back: the
	// controlplane may be releasing them right now.
	for _, ip := range obj.Spec.IPv4.Releasing {
		f.releasing[ip] = true
	}
	f.onStatus(obj)
	if err := watchZecNode(ctx, dyn, nodeName, f.onStatus); err != nil {
		return nil, nil, err
	}
	// Rewrites the spec whenever it is lost -- the object recreated during the
	// waits below included.
	go f.runSync(ctx)

	// Only a changed identity is written and waited on. A restart with the
	// same identity goes on with the existing verification, so it does not
	// need the controlplane to be up -- the pods already here keep working
	// and new ones get addresses from what the node holds.
	// The wait is for the generation of the last spec write, read anew on each
	// poll: a ZecNode deleted and recreated meanwhile (the documented way to
	// rebind a node name) starts its generations over, and runSync writes the
	// identity to it again.
	waitWrite := obj.Spec.Identity != claim
	if waitWrite {
		if err := f.writeSpec(ctx); err != nil {
			return nil, nil, fmt.Errorf("write identity to ZecNode %s: %w", nodeName, err)
		}
	}

	var stackErr error
	var reclaimedAt time.Time
	if err := pollUntil(ctx, "the controlplane to verify this node", func() (bool, error) {
		f.mu.RLock()
		c := meta.FindStatusCondition(f.status.Conditions, v1alpha1.ConditionInstanceVerified)
		pending := (waitWrite && f.status.ObservedGeneration < f.specGen) || c == nil || c.Status != metav1.ConditionTrue
		var reason string
		if c != nil {
			reason = c.Reason
			if pending {
				log.Infof("ZecNode %s: %s=%s %s: %s", nodeName, c.Type, c.Status, c.Reason, c.Message)
			}
		}
		f.mu.RUnlock()
		if pending {
			// The claim was read once, at start; an operator who has since
			// fixed the node's address should not have to restart the daemon.
			if reason == v1alpha1.ReasonIdentityMismatch && time.Since(reclaimedAt) > crdReclaimEvery {
				reclaimedAt = time.Now()
				if changed, err := reclaimIdentity(ctx, f, md, k8sSvc); err != nil {
					log.Warnf("Read identity again: %v", err)
				} else if changed {
					waitWrite = true
				}
			}
			return false, nil
		}
		f.mu.RLock()
		defer f.mu.RUnlock()
		if f.status.IPStack != "" && f.status.IPStack != string(stack) {
			stackErr = fmt.Errorf("this daemon is configured for ipStack %s but the controlplane serves %s; "+
				"set zec.ipStack the same on both", stack, f.status.IPStack)
			return true, nil
		}
		return f.status.Scaler.Target > 0 && len(f.status.ENIs) > 0, nil
	}); err != nil {
		return nil, nil, err
	}
	if stackErr != nil {
		return nil, nil, stackErr
	}

	// Every interface must be built before seeding: the stored records of one
	// that is not would be skipped, and its live addresses later adopted as
	// free. An interface the controlplane could not give an IPv6 prefix is
	// built all the same and simply offers no new pairs.
	if err := pollUntil(ctx, "every interface to be readable from the metadata service", func() (bool, error) {
		f.syncENIs()
		f.mu.RLock()
		defer f.mu.RUnlock()
		for _, es := range f.status.ENIs {
			if _, ok := f.enis[es.ID]; !ok {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		return nil, nil, err
	}

	f.mu.RLock()
	sc, capacity := f.status.Scaler, f.status.Capacity
	f.mu.RUnlock()
	maxCap := capacity.Allocatable
	poolCfg := pool.Config{
		Name:      "eniIP",
		Type:      zecTypes.NetResourceTypeEniIp,
		TargetMin: min(sc.TargetMin, maxCap),
		Target:    min(sc.Target, maxCap),
		MaxCap:    maxCap,
		// The cap follows status.capacity as the controlplane updates it.
		MaxCapProbe:     true,
		MonitorInterval: time.Duration(cfg.PoolMonitorIntervalSec) * time.Second,
		GCProtectPeriod: time.Duration(cfg.PoolGCPeriodSec) * time.Second,
		Factory:         f,
		OwnerAlive:      func(owner string) bool { return ownerStillExists(k8sSvc, owner) },
		// Create waits up to crdCreateWait for the controlplane; see the field.
		CreateInBackground: true,
	}
	poolCfg.PreStart = func(p pool.ResourcePoolOp) error {
		livePodByIP, livePodV6, err := livePodAddresses(k8sSvc)
		if err != nil {
			return err
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		// A stored record whose address this node already gave back is not
		// the pool's; one on an interface status does not report is retired,
		// so its addresses are never adopted if the interface comes back.
		kept := map[string]zecTypes.NetResourceAllocated{}
		recordedV4 := map[string]bool{}
		for k, item := range allocated {
			ri := item.Resource.GetResourceItem()
			switch {
			case f.releasing[ri.IPv4]:
				log.Warnf("Stored record of %s names %s, which this node already gave back; dropping it", item.Owner, ri.IPv4)
				continue
			case statusENI(&f.status, ri.ENIId) == nil || f.enis[ri.ENIId] == nil:
				log.Warnf("Stored record of %s is on interface %s, which is not reported for this node; retiring %s",
					item.Owner, ri.ENIId, ri.IPv4)
				if ri.IPv4 != "" {
					f.retired[ri.IPv4] = true
				}
				if v6 := net.ParseIP(ri.IPv6); v6 != nil {
					f.retiredV6[ri.ENIId] = append(f.retiredV6[ri.ENIId], v6)
				}
				continue
			}
			kept[k] = item
			recordedV4[ri.IPv4] = true
		}
		for id, e := range f.enis {
			es := statusENI(&f.status, id)
			if es == nil {
				continue
			}
			var v4s []net.IP
			for _, a := range es.IPv4 {
				ip := net.ParseIP(a.IP)
				_, unsettled := f.unsettled[a.IP]
				if ip == nil || a.Owner != "" || f.releasing[a.IP] || unsettled || f.retired[a.IP] ||
					a.IP == f.metaPrimaryV4[id] || ip.Equal(e.PrimaryIP.IPv4) {
					continue
				}
				v4s = append(v4s, ip)
			}
			if stack.EnableIPv6() && e.IPv6Net == nil {
				// No prefix: nothing new can be paired here; seed the records
				// only. A live pod's address without a record must still never
				// be adopted as free once the prefix arrives: retire it.
				for _, ip := range v4s {
					if owner, live := livePodByIP[ip.String()]; live && !recordedV4[ip.String()] {
						log.Warnf("Address %s on %s is used by %s but has no stored record; retiring it", ip, id, owner)
						f.retired[ip.String()] = true
					}
				}
				v4s = nil
			}
			if es.State == v1alpha1.ENIStateExcluded && es.Primary {
				// Only what a live pod holds; anything else on the instance's
				// primary may be an operator's. See takeExcludedLocked.
				v4s = slices.DeleteFunc(v4s, func(ip net.IP) bool { _, live := livePodByIP[ip.String()]; return !live })
			}
			if err := seedENIAddresses(p, e, v4s, stack, kept, livePodByIP, livePodV6, maxCap); err != nil {
				return err
			}
		}
		f.seeded = true
		// Mark what was seeded on an Excluded interface and have GC give the
		// idle ones back: the onStatus that would do it ran before the
		// interfaces were built.
		f.markExcludedLocked()
		f.excludedGCDue = slices.ContainsFunc(f.status.ENIs, func(es v1alpha1.ENIStatus) bool {
			return es.State == v1alpha1.ENIStateExcluded
		})
		// The spec still asks for what it did before the restart; write what
		// the seeded pool now holds.
		f.specDirty = true
		return nil
	}
	p, err := pool.NewResourcePool(poolCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create resource pool %s failed, %w", poolCfg.Name, err)
	}
	limit := &crdLimit{f: f}
	var onScaler func(v1alpha1.Scaler)
	if rc, ok := p.(interface{ ReCfgCache(target, targetMin int) }); ok {
		onScaler = func(s v1alpha1.Scaler) {
			l := f.GetResourceLimit()
			rc.ReCfgCache(min(s.Target, l), min(s.TargetMin, l))
		}
	}
	f.setCallbacks(onScaler, limit.NotifyWatcher)
	// A scaler or capacity change that arrived while the pool was being
	// built found no callback, and later statuses compare equal to it:
	// apply the current state once now.
	f.mu.RLock()
	scaler := f.status.Scaler
	f.mu.RUnlock()
	if onScaler != nil && scaler.Target > 0 {
		onScaler(scaler)
	}
	limit.NotifyWatcher()
	log.Infof("ipamType crd: node verified as instance %s, pool target %d/%d, cap %d",
		claim.InstanceID, poolCfg.TargetMin, poolCfg.Target, maxCap)
	return &eniIPResourceManager{pool: p}, limit, nil
}

// pollUntil retries cond every crdStartPoll, logging what it waits for once a
// minute, until it is true, fails, or ctx ends.
func pollUntil(ctx context.Context, what string, cond func() (bool, error)) error {
	last := time.Time{}
	for {
		ok, err := cond()
		if err != nil {
			return fmt.Errorf("waiting for %s: %w", what, err)
		}
		if ok {
			return nil
		}
		if time.Since(last) > time.Minute {
			log.Infof("Waiting for %s", what)
			last = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(crdStartPoll):
		}
	}
}

// crdLimit is the InstanceLimitManager of ipamType crd: the limits are the
// controlplane's status.capacity, and watchers are told when it changes.
type crdLimit struct {
	f        *crdIPFactory
	mu       sync.Mutex
	watchers []chan<- struct{}
}

var _ helper.InstanceLimitManager = (*crdLimit)(nil)

func (l *crdLimit) GetLimit() helper.InstanceLimits {
	l.f.mu.RLock()
	defer l.f.mu.RUnlock()
	c := l.f.status.Capacity
	lim := helper.InstanceLimits{}
	lim.ENIQuota, lim.IPv4MaxPerENI, lim.ENIUnmanaged = c.ENIQuota, c.IPv4PerENI, c.ENIUnmanaged
	return lim
}

func (l *crdLimit) Update()               {}
func (l *crdLimit) CordonCreate(string)   {}
func (l *crdLimit) UnCordonCreate(string) {}
func (l *crdLimit) WatchUpdate(_ string, w chan<- struct{}) {
	l.mu.Lock()
	l.watchers = append(l.watchers, w)
	l.mu.Unlock()
}

func (l *crdLimit) NotifyWatcher() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.watchers {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}
