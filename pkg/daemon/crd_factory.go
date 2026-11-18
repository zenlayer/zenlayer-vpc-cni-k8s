// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/apis/network/v1alpha1"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/pool"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/signal"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

const (
	// crdCreateWait bounds one Create: half the CNI ADD budget, so a pod
	// waiting on an empty pool still has time to be served once it returns.
	crdCreateWait = daemonRPCTimeout / 2
	// crdStatusPoll backs up the watch, which delivers nothing while it
	// reconnects.
	crdStatusPoll = 2 * time.Second
	// crdMissingGrace is how long an address or interface the pool holds may
	// be absent from status before it is treated as gone: a status one pass
	// behind, or briefly empty after the object was recreated, is normal.
	crdMissingGrace = time.Minute
	// crdExcludedWake: how often GC is asked for again while an interface that
	// just became Excluded still has idle addresses for it to give back; a
	// wake can be dropped (GC already running) or debounced.
	crdExcludedWake = 10 * time.Second
	crdENISyncRetry = 2 * time.Second
	crdSpecWrite    = 5 * time.Second
	// crdPartialQuiet: once some addresses have arrived, a Create returns
	// what it has after this long without another, rather than hold them
	// from the pool for the whole crdCreateWait.
	crdPartialQuiet = 10 * time.Second
)

// eniMetadata is what the crd factory reads from the node's metadata service.
type eniMetadata interface {
	GetENI(eniId, subnetId, mac string) (*zecTypes.ENI, error)
	MetadataPrimaryIPv4(mac string) (net.IP, error)
	MetadataPrimaryIPv6(mac string) (net.IP, error)
}

// zecNodeAPI reads and writes this node's ZecNode.
type zecNodeAPI interface {
	Get(ctx context.Context) (*v1alpha1.ZecNode, error)
	// PatchSpec replaces the whole spec and returns the object's generation
	// after it. The daemon owns all of the spec, so a full replacement cannot
	// drop another writer's field.
	PatchSpec(ctx context.Context, spec v1alpha1.ZecNodeSpec) (int64, error)
}

// crdIPFactory is the pool.ObjectFactory of ipamType crd. It never calls the
// platform: it asks for addresses through spec.ipv4 and takes what the
// controlplane reports in status.
//
// The rule that keeps one address off two pods: an address is pooled only if
// this factory took it fresh from status, or the startup seeding put it there
// from the stored records and the live pods. Nothing else is ever adopted --
// not an address given back, not one whose release is still unconfirmed, and
// not one that sat on an interface status stopped reporting.
//
// Lock order: the factory's lock, then an ENI's. Neither is held across a call
// to the API server or the metadata service.
type crdIPFactory struct {
	mu        sync.RWMutex
	enis      map[string]*ENI
	stack     types.IPStack
	claim     v1alpha1.IdentityClaim
	status    v1alpha1.ZecNodeStatus
	statusRV  int64
	uid       string
	releasing map[string]bool
	// specGen is the generation the last successful spec write produced, on
	// whichever object then held the name: a startup waiting for its identity
	// to be verified waits for status to have observed it, and a ZecNode
	// recreated meanwhile restarts the count with the rewrite.
	specGen int64
	// excludedGCDue: an interface became Excluded and no GC has run since, so
	// its idle addresses still wait to be given back -- and still count in
	// GetResourceLimit. runSync keeps waking GC until one runs.
	excludedGCDue bool
	// unsettled: dropped from releasing because status no longer lists them.
	// The value is the generation of the spec write that dropped them, 0
	// until that write succeeds. Until a status computed from that spec
	// arrives (observedGeneration reaches it) the controlplane may still act
	// on the old list, so they are not adopted.
	unsettled map[string]int64
	// retired: held by the pool when their interface left status, or stored
	// for an interface missing at startup. Never adopted again in this
	// process. retiredV6 keeps the IPv6s carved on such an interface, so a
	// rebuilt one does not carve them for someone else.
	retired   map[string]bool
	retiredV6 map[string][]net.IP
	// metaPrimaryV4 is each interface's own IPv4 according to the metadata
	// service; withheld from the pool alongside the platform's answer.
	metaPrimaryV4   map[string]string
	missingSince    map[string]time.Time
	eniMissingSince map[string]time.Time
	// buildLogAt throttles the log of an interface that is not usable yet.
	buildLogAt map[string]time.Time
	// pending is what an in-flight Create asks for beyond what is held; every
	// spec written meanwhile includes it.
	pending  int
	creating bool
	// seeded: startup seeding is done; before it nothing is adopted.
	seeded bool
	// specDirty: the spec must be written again, which runSync does off the
	// GC and allocation paths.
	specDirty bool

	onScaler   func(v1alpha1.Scaler)
	onCapacity func()

	createMu sync.Mutex
	// failedAt is when the last Create got nothing. A Create queued behind it
	// gives up at once while status still says allocation fails, so the pod's
	// error names the reason instead of an RPC deadline.
	failedAt time.Time
	// writeMu serializes spec writes: two in flight could land out of order
	// and put back a releasing list that was already settled.
	writeMu sync.Mutex
	meta    eniMetadata
	zn      zecNodeAPI

	updateMu sync.Mutex
	updated  chan struct{}
}

func newCRDIPFactory(stack types.IPStack, claim v1alpha1.IdentityClaim, meta eniMetadata, zn zecNodeAPI) *crdIPFactory {
	return &crdIPFactory{
		enis:            map[string]*ENI{},
		stack:           stack,
		claim:           claim,
		releasing:       map[string]bool{},
		unsettled:       map[string]int64{},
		retired:         map[string]bool{},
		retiredV6:       map[string][]net.IP{},
		metaPrimaryV4:   map[string]string{},
		missingSince:    map[string]time.Time{},
		eniMissingSince: map[string]time.Time{},
		buildLogAt:      map[string]time.Time{},
		meta:            meta,
		zn:              zn,
		updated:         make(chan struct{}),
	}
}

func (f *crdIPFactory) Name() string { return zecTypes.NetResourceTypeEniIp }

// setCallbacks installs the pool and device-plugin hooks; safe while the
// watch is already delivering.
func (f *crdIPFactory) setCallbacks(onScaler func(v1alpha1.Scaler), onCapacity func()) {
	f.mu.Lock()
	f.onScaler, f.onCapacity = onScaler, onCapacity
	f.mu.Unlock()
}

// broadcast wakes every Create waiting for a status change.
func (f *crdIPFactory) broadcast() {
	f.updateMu.Lock()
	close(f.updated)
	f.updated = make(chan struct{})
	f.updateMu.Unlock()
}

func (f *crdIPFactory) waitCh() <-chan struct{} {
	f.updateMu.Lock()
	defer f.updateMu.Unlock()
	return f.updated
}

// onStatus takes a ZecNode from the watch or a poll.
func (f *crdIPFactory) onStatus(zn *v1alpha1.ZecNode) {
	rv, _ := strconv.ParseInt(zn.ResourceVersion, 10, 64)
	f.mu.Lock()
	// The watch and the poll can deliver out of order; never go backwards.
	if rv != 0 && rv < f.statusRV {
		f.mu.Unlock()
		return
	}
	f.statusRV = rv
	if uid := string(zn.UID); uid != f.uid {
		// A recreated object counts generations from 1 again: a settled
		// release waiting on the old count must be written anew.
		if f.uid != "" {
			for ip := range f.unsettled {
				f.unsettled[ip] = 0
			}
			f.specDirty = true
		}
		f.uid = uid
	}
	oldScaler, oldCap, oldLimit := f.status.Scaler, f.status.Capacity, f.limitLocked()
	// A node always has its primary interface, so a status with none is one
	// the controlplane has not filled yet (a recreated object, or the first
	// look at startup). Keep the last full view rather than read it as every
	// interface gone, and settle nothing against it.
	if len(zn.Status.ENIs) > 0 || len(f.status.ENIs) == 0 {
		f.status = *zn.Status.DeepCopy()
		if len(f.status.ENIs) > 0 {
			f.settleReleasingLocked()
			f.stampMissingLocked(time.Now())
			if f.markExcludedLocked() {
				f.excludedGCDue = true
			}
		}
	} else {
		// Keep only what the unfilled status lacks. Its generation and
		// conditions are the current object's: the startup wait compares
		// them against this object's spec writes, and a recreated object's
		// verdict must not be read off the old one.
		prev := f.status
		f.status = *zn.Status.DeepCopy()
		f.status.ENIs = prev.ENIs
		if f.status.Capacity == (v1alpha1.Capacity{}) {
			f.status.Capacity = prev.Capacity
		}
		if f.status.Scaler.Target == 0 {
			f.status.Scaler = prev.Scaler
		}
	}
	f.clearSettledLocked(zn.Status.ObservedGeneration)
	// The spec is the daemon's, derived from local state; if the stored one
	// differs (a recreated object, a lost write), write it again.
	if !specEqual(zn.Spec, f.desiredSpecLocked()) {
		f.specDirty = true
	}
	scaler, capacity, limit := f.status.Scaler, f.status.Capacity, f.limitLocked()
	onScaler, onCapacity := f.onScaler, f.onCapacity
	f.mu.Unlock()

	// The pool targets are clamped to the limit, so a change in it re-applies
	// them too.
	changed := capacity != oldCap || limit != oldLimit
	if (scaler != oldScaler || changed) && scaler.Target > 0 && onScaler != nil {
		onScaler(scaler)
	}
	if changed && onCapacity != nil {
		onCapacity()
	}
	f.broadcast()
}

// settleReleasingLocked drops from releasing what status no longer lists: the
// controlplane has released it.
func (f *crdIPFactory) settleReleasingLocked() {
	onPlatform := f.onPlatformLocked()
	for ip := range f.releasing {
		if !onPlatform[ip] {
			delete(f.releasing, ip)
			f.unsettled[ip] = 0
			f.specDirty = true
		}
	}
}

// clearSettledLocked ends the wait of unsettled addresses whose spec the
// controlplane has now seen.
func (f *crdIPFactory) clearSettledLocked(observed int64) {
	for ip, gen := range f.unsettled {
		if gen > 0 && observed >= gen {
			delete(f.unsettled, ip)
		}
	}
}

func (f *crdIPFactory) onPlatformLocked() map[string]bool {
	on := map[string]bool{}
	for _, es := range f.status.ENIs {
		for _, a := range es.IPv4 {
			on[a.IP] = true
		}
	}
	return on
}

// ipv4Set is the IPv4 addresses status lists under one interface; nil-safe.
func ipv4Set(es *v1alpha1.ENIStatus) map[string]bool {
	on := map[string]bool{}
	if es == nil {
		return on
	}
	for _, a := range es.IPv4 {
		on[a.IP] = true
	}
	return on
}

func specEqual(a, b v1alpha1.ZecNodeSpec) bool {
	if a.Identity != b.Identity || a.IPv4.Requested != b.IPv4.Requested || len(a.IPv4.Releasing) != len(b.IPv4.Releasing) {
		return false
	}
	x := append([]string(nil), a.IPv4.Releasing...)
	sort.Strings(x)
	for i := range x {
		if x[i] != b.IPv4.Releasing[i] {
			return false
		}
	}
	return true
}

// usableLocked reports whether the controlplane counts the addresses of an
// interface as the node's: on a dual node, only once it has its IPv6 prefix.
func (f *crdIPFactory) usableLocked(es *v1alpha1.ENIStatus) bool {
	return es != nil && es.State != v1alpha1.ENIStateExcluded && (!f.stack.EnableIPv6() || es.IPv6 != nil)
}

// errENIExcluded marks an address held on an interface status reports
// Excluded (outside the pod subnets). As ErrLegacy, an idle one
// is given back by the pool's GC and one in use when its pod releases it.
var errENIExcluded = fmt.Errorf("%w: its interface is outside the pod subnets", ErrLegacy)

// markExcludedLocked sets errENIExcluded on the addresses held on an Excluded
// interface, and clears it once the interface is a pod interface again (its
// subnet was added to the pod subnets). It reports whether it newly marked
// any, so the pool's GC can give the idle ones back now rather than at the
// next periodic GC (see excludedGCDue). Caller holds f.mu.
func (f *crdIPFactory) markExcludedLocked() (marked bool) {
	for id, e := range f.enis {
		es := statusENI(&f.status, id)
		excluded := es != nil && es.State == v1alpha1.ENIStateExcluded
		e.Lock()
		for _, r := range e.ips {
			switch {
			case excluded && r.err == nil:
				r.err = errENIExcluded
				marked = true
			case !excluded && r.err == errENIExcluded:
				r.err = nil
				if !r.IPSet.Complete(f.stack.Families()) {
					r.err = ErrLegacy // taken IPv4 alone while Excluded: still given back
				}
			}
		}
		e.Unlock()
	}
	return marked
}

// heldLocked is how many addresses the pool holds that the controlplane also
// counts as held: listed in status, on an interface it counts. A record whose
// address status dropped is not one -- counting it would ask for one more
// address on every pass, without end.
func (f *crdIPFactory) heldLocked() int {
	n := 0
	for id, e := range f.enis {
		es := statusENI(&f.status, id)
		if !f.usableLocked(es) {
			continue
		}
		on := ipv4Set(es) // listed under the record's own interface
		e.Lock()
		for _, r := range e.ips {
			if r.IPSet.IPv4 != nil && on[r.IPSet.IPv4.String()] {
				n++
			}
		}
		e.Unlock()
	}
	return n
}

// refusedLocked counts the addresses status lists that this node will never
// take (retired, or an interface's own address by the metadata service's
// account) and is not giving back. The controlplane counts them as held, so
// they are asked for too; otherwise every fill would fall short by that many.
func (f *crdIPFactory) refusedLocked() int {
	n := 0
	for i := range f.status.ENIs {
		es := &f.status.ENIs[i]
		if !f.usableLocked(es) {
			continue
		}
		for _, a := range es.IPv4 {
			if f.releasing[a.IP] {
				continue
			}
			if f.retired[a.IP] || a.IP == f.metaPrimaryV4[es.ID] {
				n++
			}
		}
	}
	return n
}

// desiredSpecLocked is the spec the local state implies.
func (f *crdIPFactory) desiredSpecLocked() v1alpha1.ZecNodeSpec {
	rel := make([]string, 0, len(f.releasing))
	for ip := range f.releasing {
		rel = append(rel, ip)
	}
	sort.Strings(rel)
	return v1alpha1.ZecNodeSpec{
		Identity: f.claim,
		IPv4:     v1alpha1.IPDemand{Requested: f.heldLocked() + f.pending + f.refusedLocked(), Releasing: rel},
	}
}

// writeSpec writes the spec the local state implies. On success the addresses
// it no longer lists wait for a status computed from it (clearSettledLocked).
func (f *crdIPFactory) writeSpec(ctx context.Context) error {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	f.mu.Lock()
	spec := f.desiredSpecLocked()
	var settling []string
	for ip, gen := range f.unsettled {
		if gen == 0 {
			settling = append(settling, ip)
		}
	}
	f.specDirty = false
	f.mu.Unlock()
	gen, err := f.zn.PatchSpec(ctx, spec)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.specDirty = true
		return err
	}
	f.specGen = gen
	for _, ip := range settling {
		if g, ok := f.unsettled[ip]; ok && g == 0 {
			f.unsettled[ip] = gen
		}
	}
	f.clearSettledLocked(f.status.ObservedGeneration)
	return nil
}

// runSync keeps the local interfaces in step with status and rewrites the spec
// when it is dirty, until ctx ends. It is where the metadata waits happen, so
// that neither GC nor the allocation path has to make them.
func (f *crdIPFactory) runSync(ctx context.Context) {
	var wokeGC time.Time
	excludedWakes := 0 // doubles the wait while GC keeps not clearing excludedGCDue
	for {
		f.syncENIs()
		f.mu.RLock()
		dirty := f.specDirty
		due := f.gcDueLocked(time.Now())
		every := crdMissingGrace
		if f.excludedGCDue {
			every = min(crdExcludedWake<<min(excludedWakes, 6), crdMissingGrace)
		} else {
			excludedWakes = 0
		}
		f.mu.RUnlock()
		// GC is what drops what status stopped listing; its own period is
		// far longer than the grace, so it is woken once something is due.
		if due && time.Since(wokeGC) > every {
			wokeGC = time.Now()
			excludedWakes++
			signal.NotifySignal(signal.WakeGC, signal.SigWakeGC)
		}
		if dirty {
			wctx, cancel := context.WithTimeout(ctx, crdSpecWrite)
			if err := f.writeSpec(wctx); err != nil {
				log.Warnf("Write ZecNode spec: %v", err)
			}
			cancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-f.waitCh():
		case <-time.After(crdENISyncRetry):
		}
	}
}

// syncENIs builds a local interface for each one status reports that the
// metadata service can already describe -- a new interface's gateway and CIDR
// appear there with a delay -- and fills in an IPv6 prefix that arrived after
// the interface did.
func (f *crdIPFactory) syncENIs() {
	f.mu.RLock()
	var todo []v1alpha1.ENIStatus
	for _, es := range f.status.ENIs {
		e, ok := f.enis[es.ID]
		if !ok || (f.stack.EnableIPv6() && e.IPv6Net == nil && es.IPv6 != nil) {
			todo = append(todo, es)
		}
	}
	f.mu.RUnlock()

	for _, es := range todo {
		eni, metaV4, err := f.buildENI(es)
		if err != nil {
			f.mu.Lock()
			if time.Since(f.buildLogAt[es.ID]) > time.Minute {
				f.buildLogAt[es.ID] = time.Now()
				log.Infof("Interface %s (%s) is not usable yet: %v", es.ID, es.MAC, err)
			}
			f.mu.Unlock()
			continue
		}
		f.mu.Lock()
		if cur, ok := f.enis[es.ID]; ok {
			cur.Lock()
			if cur.IPv6Net == nil {
				// A copy, not in place: resources already handed out share
				// the old *zecTypes.ENI and read it without these locks.
				cp := *cur.ENI
				cp.IPv6Net = eni.IPv6Net
				cp.PrimaryIP.IPv6 = eni.PrimaryIP.IPv6
				cp.PrimaryIPv6Disputed = eni.PrimaryIPv6Disputed
				cur.ENI = &cp
			}
			cur.Unlock()
		} else {
			e := &ENI{ENI: eni, ipStack: f.stack, ips: []*ENIIPRes{}}
			e.reserveV6Locked(f.retiredV6[es.ID])
			f.enis[es.ID] = e
		}
		limitChanged := false
		var scaler v1alpha1.Scaler
		var onScaler func(v1alpha1.Scaler)
		var onCapacity func()
		if old, had := f.metaPrimaryV4[es.ID]; metaV4 != "" && (!had || old != metaV4) {
			// refusedLocked counts the metadata primary, so this moves both
			// the limit and the spec's request: say so, as onStatus does.
			oldLimit := f.limitLocked()
			f.metaPrimaryV4[es.ID] = metaV4
			f.specDirty = true
			limitChanged = f.limitLocked() != oldLimit
			scaler, onScaler, onCapacity = f.status.Scaler, f.onScaler, f.onCapacity
		}
		delete(f.buildLogAt, es.ID)
		f.mu.Unlock()
		if limitChanged {
			if onScaler != nil && scaler.Target > 0 {
				onScaler(scaler)
			}
			if onCapacity != nil {
				onCapacity()
			}
		}
		log.Infof("Interface %s (%s) is ready for pod addresses", es.ID, es.MAC)
		f.broadcast()
	}
}

// buildENI reads an interface's gateway and subnet CIDR from the metadata
// service and takes its primaries and IPv6 prefix from status.
func (f *crdIPFactory) buildENI(es v1alpha1.ENIStatus) (*zecTypes.ENI, string, error) {
	eni, err := f.meta.GetENI(es.ID, es.SubnetID, es.MAC)
	if err != nil {
		return nil, "", err
	}
	eni.IsPrimary = es.Primary
	eni.PrimaryIP.IPv4 = net.ParseIP(es.PrimaryIPv4)
	if eni.PrimaryIP.IPv4 == nil {
		return nil, "", fmt.Errorf("interface %s reports no primary address", es.ID)
	}
	metaV4 := ""
	if ip, err := f.meta.MetadataPrimaryIPv4(es.MAC); err == nil && ip != nil && !ip.Equal(eni.PrimaryIP.IPv4) {
		log.Warnf("Interface %s primary address disagrees between sources: metadata says %s, the platform %s; withholding both",
			es.ID, ip, eni.PrimaryIP.IPv4)
		metaV4 = ip.String()
	}
	if f.stack.EnableIPv6() && es.IPv6 != nil {
		_, v6Net, err := net.ParseCIDR(es.IPv6.Prefix)
		if err != nil {
			return nil, "", fmt.Errorf("interface %s ipv6 prefix %q: %w", es.ID, es.IPv6.Prefix, err)
		}
		primary := net.ParseIP(es.IPv6.Primary)
		if primary == nil {
			return nil, "", fmt.Errorf("interface %s has ipv6 prefix %s but reports no primary ipv6, refusing to carve from it",
				es.ID, v6Net)
		}
		eni.IPv6Net, eni.PrimaryIP.IPv6 = v6Net, primary
		if metaV6, err := f.meta.MetadataPrimaryIPv6(es.MAC); err == nil && metaV6 != nil && !metaV6.Equal(primary) {
			log.Warnf("Interface %s primary ipv6 disagrees between sources: metadata says %s, the platform %s; withholding both",
				es.ID, metaV6, primary)
			eni.PrimaryIPv6Disputed = metaV6
		}
	}
	return eni, metaV4, nil
}

// statusENI returns the status entry of an interface.
func statusENI(st *v1alpha1.ZecNodeStatus, id string) *v1alpha1.ENIStatus {
	for i := range st.ENIs {
		if st.ENIs[i].ID == id {
			return &st.ENIs[i]
		}
	}
	return nil
}

// takeNewLocked moves up to n addresses that status reports and the pool may
// take into the pool's interfaces, pairing each with a carved IPv6 when the
// stack has IPv6, and returns them. An address on an interface not usable yet,
// or without its IPv6 prefix on a dual node, waits for a later call.
func (f *crdIPFactory) takeNewLocked(n int) []*zecTypes.ENIIP {
	if !f.seeded {
		return nil
	}
	// Held anywhere, not just on the same interface: an address can show up
	// under another interface after a move.
	held := map[string]bool{}
	for _, e := range f.enis {
		e.Lock()
		for _, r := range e.ips {
			if r.IPSet.IPv4 != nil {
				held[r.IPSet.IPv4.String()] = true
			}
		}
		e.Unlock()
	}
	var out []*zecTypes.ENIIP
	for _, es := range f.status.ENIs {
		if len(out) >= n {
			break
		}
		if es.State == v1alpha1.ENIStateExcluded {
			continue
		}
		e, ok := f.enis[es.ID]
		if !ok {
			continue
		}
		e.Lock()
		if f.stack.EnableIPv6() && e.IPv6Net == nil {
			e.Unlock()
			continue
		}
		for _, a := range es.IPv4 {
			if len(out) >= n {
				break
			}
			if a.Owner != "" || held[a.IP] || f.releasing[a.IP] || f.retired[a.IP] ||
				a.IP == f.metaPrimaryV4[es.ID] {
				continue
			}
			if _, unsettled := f.unsettled[a.IP]; unsettled {
				continue
			}
			v4 := net.ParseIP(a.IP)
			if v4 == nil || v4.Equal(e.PrimaryIP.IPv4) {
				continue
			}
			set := zecTypes.IPSet{IPv4: v4}
			if f.stack.EnableIPv6() {
				v6, err := e.AllocateIPv6AddressLocked(1)
				if err != nil {
					log.Warnf("Carve an ipv6 for %s on %s: %v", a.IP, es.ID, err)
					break
				}
				set.IPv6 = v6[0]
			}
			res := &ENIIPRes{ENIIP: &zecTypes.ENIIP{ENI: e.ENI, IPSet: set}}
			e.appendIPLocked(res)
			held[a.IP] = true
			out = append(out, res.ENIIP)
		}
		e.Unlock()
	}
	return out
}

// Create asks the controlplane for count more addresses and returns those
// that arrive within crdCreateWait -- possibly fewer, which the pool contract
// allows. Afterwards the spec asks for exactly what the pool holds again, so a
// shortfall is not chased forever: the pool calls Create again if it still
// wants more, and anything arriving late is adopted by GC.
func (f *crdIPFactory) Create(count int) ([]zecTypes.NetResource, error) {
	if count <= 0 {
		return nil, nil
	}
	f.createMu.Lock()
	defer f.createMu.Unlock()

	// Right after a Create that got nothing, while status still says why,
	// take what is here and ask nothing more: no spec write, no wait.
	f.mu.Lock()
	if time.Since(f.failedAt) < crdCreateWait && f.allocFailingLocked() {
		got := f.takeNewLocked(count)
		f.mu.Unlock()
		if len(got) == 0 {
			return nil, fmt.Errorf("the controlplane cannot provide an address: %s", f.statusReason())
		}
		res := make([]zecTypes.NetResource, 0, len(got))
		for _, r := range got {
			res = append(res, r)
		}
		return res, nil
	}
	f.creating, f.pending = true, count
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.creating, f.pending = false, 0
		f.mu.Unlock()
		// Ask for what the pool now holds. A failure leaves the spec dirty for
		// runSync; the addresses already taken are the pool's either way.
		wctx, cancel := context.WithTimeout(context.Background(), crdSpecWrite)
		defer cancel()
		if err := f.writeSpec(wctx); err != nil {
			log.Warnf("Reset ZecNode requested count: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), crdCreateWait)
	defer cancel()
	if err := f.writeSpec(ctx); err != nil {
		return nil, fmt.Errorf("ask the controlplane for %d address(es): %w", count, err)
	}

	var got []*zecTypes.ENIIP
	lastArrival := time.Now()
	tick := time.NewTicker(crdStatusPoll)
	defer tick.Stop()
wait:
	for {
		f.mu.Lock()
		before := len(got)
		got = append(got, f.takeNewLocked(count-len(got))...)
		f.pending = count - len(got)
		f.mu.Unlock()
		if len(got) >= count {
			break
		}
		if len(got) > before {
			lastArrival = time.Now()
		}
		// The rest may be a new interface still on its way; hand over what
		// is here so a pod waiting behind this call is not kept from it.
		if len(got) > 0 && time.Since(lastArrival) > crdPartialQuiet {
			break
		}
		select {
		case <-ctx.Done():
			break wait
		case <-f.waitCh():
		case <-tick.C:
			if zn, err := f.zn.Get(ctx); err == nil {
				f.onStatus(zn)
			}
		}
	}

	f.mu.Lock()
	if len(got) == 0 {
		f.failedAt = time.Now()
	} else {
		f.failedAt = time.Time{}
	}
	f.mu.Unlock()
	if len(got) == 0 {
		return nil, fmt.Errorf("the controlplane provided no address within %s: %s", crdCreateWait, f.statusReason())
	}
	res := make([]zecTypes.NetResource, 0, len(got))
	for _, r := range got {
		res = append(res, r)
	}
	return res, nil
}

// allocFailingLocked reports whether status says addresses cannot be had.
// The caller holds f.mu.
func (f *crdIPFactory) allocFailingLocked() bool {
	return meta.IsStatusConditionTrue(f.status.Conditions, v1alpha1.ConditionIPAllocationFailed)
}

// statusReason summarizes why status does not say Ready, for error messages.
func (f *crdIPFactory) statusReason() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, typ := range []string{v1alpha1.ConditionIPAllocationFailed, v1alpha1.ConditionInstanceVerified, v1alpha1.ConditionReady} {
		c := meta.FindStatusCondition(f.status.Conditions, typ)
		if c == nil {
			continue
		}
		bad := (typ == v1alpha1.ConditionIPAllocationFailed) == (c.Status == metav1.ConditionTrue)
		if bad {
			return fmt.Sprintf("%s=%s (%s: %s)", c.Type, c.Status, c.Reason, c.Message)
		}
	}
	return "no condition explains it; see kubectl get zn"
}

// findLocked returns the local interface and record of a resource.
func (f *crdIPFactory) findLocked(res *zecTypes.ENIIP) (*ENI, *ENIIPRes) {
	if res.ENI == nil {
		return nil, nil
	}
	e, ok := f.enis[res.ENI.ID]
	if !ok {
		return nil, nil
	}
	e.Lock()
	defer e.Unlock()
	for _, r := range e.ips {
		if r.IPSet.String() == res.IPSet.String() {
			return e, r
		}
	}
	return e, nil
}

// release takes a resource out of the pool and, for an address that is the
// node's to give back, adds its IPv4 to spec.releasing. Once out it never
// comes back: if the spec write fails here it is retried by runSync, because
// a failed write may still have landed and the controlplane may already be
// releasing the address.
func (f *crdIPFactory) release(res *zecTypes.ENIIP, giveBack bool) error {
	f.mu.Lock()
	e, rec := f.findLocked(res)
	if e == nil || rec == nil {
		f.mu.Unlock()
		return apiErr.ErrNotFound
	}
	e.Lock()
	e.deleteIPLocked(rec.ENIIP)
	e.Unlock()
	if giveBack && res.IPSet.IPv4 != nil {
		f.releasing[res.IPSet.IPv4.String()] = true
	}
	f.specDirty = true
	es := statusENI(&f.status, e.ID)
	uncounted := es != nil && !f.usableLocked(es)
	onCapacity := f.onCapacity
	f.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), crdSpecWrite)
	defer cancel()
	if err := f.writeSpec(ctx); err != nil {
		log.Warnf("Give %s back to the controlplane: %v; retrying in the background", res.IPSet.String(), err)
	}
	// Off an interface the controlplane does not count it leaves
	// GetResourceLimit too, which no status change will report. Told a moment
	// after this returns: the device plugin's figure also subtracts pods by
	// their stored record, which the caller deletes next. Late, it
	// under-reports by one until the next count.
	if uncounted && onCapacity != nil {
		time.AfterFunc(time.Second, onCapacity)
	}
	return nil
}

func (f *crdIPFactory) Release(resource zecTypes.NetResource) error {
	res, ok := resource.(*zecTypes.ENIIP)
	if !ok {
		return fmt.Errorf("type of resource is not %s", zecTypes.NetResourceTypeEniIp)
	}
	if res.ENI == nil {
		return fmt.Errorf("eni of resource is nil")
	}
	if namesInterfacePrimary(res.ENI, res.IPSet) {
		return apiErr.ErrInvalidDeletionPrimaryIP
	}
	return f.release(res, true)
}

// ReleaseInValid drops an invalid resource. A record naming an interface's
// own primary is only forgotten: that address is the interface's, not the
// pool's, and must never be asked back.
func (f *crdIPFactory) ReleaseInValid(resource zecTypes.NetResource) (zecTypes.NetResource, error) {
	res, ok := resource.(*zecTypes.ENIIP)
	if !ok {
		return nil, fmt.Errorf("type of resource is not %s", zecTypes.NetResourceTypeEniIp)
	}
	if res.ENI == nil {
		return nil, fmt.Errorf("eni of resource is nil")
	}
	err := f.release(res, !namesInterfacePrimary(res.ENI, res.IPSet))
	if errors.Is(err, apiErr.ErrNotFound) {
		return nil, nil
	}
	return nil, err
}

func (f *crdIPFactory) Valid(resource zecTypes.NetResource) error {
	res, ok := resource.(*zecTypes.ENIIP)
	if !ok {
		return fmt.Errorf("type of resource is not %s", zecTypes.NetResourceTypeEniIp)
	}
	if res.ENI == nil {
		return fmt.Errorf("eni of resource is nil")
	}
	if namesInterfacePrimary(res.ENI, res.IPSet) {
		return fmt.Errorf("address set %s on eni %s names the interface's own primary address; it is not allocatable",
			res.IPSet.String(), res.ENI.ID)
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	_, rec := f.findLocked(res)
	if rec == nil {
		return apiErr.ErrNotFound
	}
	return rec.err
}

var _ pool.UsabilityChecker = (*crdIPFactory)(nil)

// Usable is pool.UsabilityChecker: an address that status has stopped listing,
// or whose interface it has, is kept through crdMissingGrace but not handed
// out -- the plugin cannot find a missing interface on the host, and status is
// the only source of an address. Called under the pool lock; takes only f.mu.
func (f *crdIPFactory) Usable(resource zecTypes.NetResource) bool {
	res, ok := resource.(*zecTypes.ENIIP)
	if !ok || res.ENI == nil {
		return true
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if _, held := f.enis[res.ENI.ID]; !held {
		return false
	}
	if _, missing := f.eniMissingSince[res.ENI.ID]; missing {
		return false
	}
	if es := statusENI(&f.status, res.ENI.ID); es != nil && es.State == v1alpha1.ENIStateExcluded {
		return false
	}
	if res.IPSet.IPv4 != nil {
		if _, missing := f.missingSince[res.IPSet.IPv4.String()]; missing {
			return false
		}
	}
	return true
}

// List reports every held resource against the latest status. An address
// status does not list is Invalid only once it has been missing for
// crdMissingGrace (tracked by GC), or once it was marked gone; until then it
// is reported as it was. No platform call.
func (f *crdIPFactory) List() (map[zecTypes.ResStatus]map[string]zecTypes.NetResource, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	list := map[zecTypes.ResStatus]map[string]zecTypes.NetResource{
		zecTypes.ResStatusNormal:  {},
		zecTypes.ResStatusLegacy:  {},
		zecTypes.ResStatusInvalid: {},
	}
	now := time.Now()
	gone := func(since time.Time, seen bool) bool { return seen && now.Sub(since) > crdMissingGrace }
	for id, e := range f.enis {
		es := statusENI(&f.status, id)
		eniSince, eniMissing := f.eniMissingSince[id]
		v4 := map[string]bool{}
		var v6Net *net.IPNet
		if es != nil {
			for _, a := range es.IPv4 {
				v4[a.IP] = true
			}
			if es.IPv6 != nil {
				_, v6Net, _ = net.ParseCIDR(es.IPv6.Prefix)
			}
		}
		e.Lock()
		for _, r := range e.ips {
			valid := true
			if es == nil {
				valid = !gone(eniSince, eniMissing)
			} else {
				if r.IPSet.IPv4 != nil && !v4[r.IPSet.IPv4.String()] {
					since, seen := f.missingSince[r.IPSet.IPv4.String()]
					valid = !gone(since, seen)
				}
				if r.IPSet.IPv6 != nil && v6Net != nil && !v6Net.Contains(r.IPSet.IPv6) {
					valid = false
				}
			}
			item := &zecTypes.ENIIP{ENI: r.ENI, IPSet: r.IPSet}
			switch {
			case !valid:
				list[zecTypes.ResStatusInvalid][item.GetID()] = item
			case r.err == nil:
				list[zecTypes.ResStatusNormal][item.GetID()] = item
			case errors.Is(r.err, ErrLegacy):
				list[zecTypes.ResStatusLegacy][item.GetID()] = item
			default:
				list[zecTypes.ResStatusInvalid][item.GetID()] = item
			}
		}
		e.Unlock()
	}
	return list, nil
}

// GC settles the local view against the latest status, with no network call
// (the pool is paused throughout):
//
//   - an interface missing from status for crdMissingGrace is dropped, and
//     the addresses the pool held on it are retired for good;
//   - an address missing from status for crdMissingGrace is marked gone;
//   - an address status reports that the pool may take is adopted, unless a
//     Create is waiting for it.
//
// A spec change is left for runSync to write.
func (f *crdIPFactory) GC() error {
	// Retiring addresses and dropping interfaces both move GetResourceLimit;
	// told once f.mu is released (deferred first, so it runs last).
	var limitChanged bool
	var onCapacity func()
	defer func() {
		if limitChanged && onCapacity != nil {
			onCapacity()
		}
	}()
	f.mu.Lock()
	defer f.mu.Unlock()
	oldLimit := f.limitLocked()
	defer func() { limitChanged, onCapacity = f.limitLocked() != oldLimit, f.onCapacity }()
	f.excludedGCDue = false // the pool's GC, around this call, takes the idle ones
	now := time.Now()
	f.stampMissingLocked(now)

	for id, e := range f.enis {
		if since, missing := f.eniMissingSince[id]; missing {
			if now.Sub(since) <= crdMissingGrace {
				continue
			}
			log.Warnf("Interface %s has not been reported for this node for %s; dropping it",
				id, now.Sub(since).Round(time.Second))
			e.Lock()
			for _, r := range e.ips {
				if r.IPSet.IPv4 != nil {
					f.retired[r.IPSet.IPv4.String()] = true
				}
				if r.IPSet.IPv6 != nil {
					f.retiredV6[id] = append(f.retiredV6[id], r.IPSet.IPv6)
				}
			}
			e.Unlock()
			delete(f.enis, id)
			delete(f.metaPrimaryV4, id)
			delete(f.eniMissingSince, id)
			f.specDirty = true
			continue
		}
		e.Lock()
		for _, r := range e.ips {
			if r.IPSet.IPv4 == nil {
				continue
			}
			ip := r.IPSet.IPv4.String()
			if since, missing := f.missingSince[ip]; missing && now.Sub(since) > crdMissingGrace && r.err == nil {
				log.Warnf("Address %s on %s has not been reported for %s; treating it as gone",
					ip, id, now.Sub(since).Round(time.Second))
				r.err = apiErr.ErrNotFound
			}
		}
		e.Unlock()
	}

	f.markExcludedLocked()
	if !f.creating {
		if adopted := f.takeNewLocked(1 << 20); len(adopted) > 0 {
			log.Infof("Adopted %d address(es) the controlplane reports for this node", len(adopted))
			f.specDirty = true
		}
	}
	f.takeExcludedLocked()
	return nil
}

// takeExcludedLocked takes what status lists on an Excluded secondary and the
// pool does not hold, IPv4 alone and marked errENIExcluded, so the pool's GC
// gives it back: left alone it would be released by no one, and the
// controlplane frees the interface only once empty. Not on the instance's
// primary, where an unrecorded address may be an operator's (a VIP, one with
// an EIP bound). Caller holds f.mu.
func (f *crdIPFactory) takeExcludedLocked() {
	held := map[string]bool{}
	for _, e := range f.enis {
		e.Lock()
		for _, r := range e.ips {
			if r.IPSet.IPv4 != nil {
				held[r.IPSet.IPv4.String()] = true
			}
		}
		e.Unlock()
	}
	for _, es := range f.status.ENIs {
		e, ok := f.enis[es.ID]
		if es.State != v1alpha1.ENIStateExcluded || es.Primary || !ok {
			continue
		}
		e.Lock()
		for _, a := range es.IPv4 {
			if held[a.IP] || f.releasing[a.IP] || f.retired[a.IP] || a.IP == f.metaPrimaryV4[es.ID] {
				continue
			}
			if _, unsettled := f.unsettled[a.IP]; unsettled {
				continue
			}
			v4 := net.ParseIP(a.IP)
			if v4 == nil || v4.Equal(e.PrimaryIP.IPv4) {
				continue
			}
			log.Infof("Taking %s on Excluded interface %s to give it back", a.IP, es.ID)
			e.appendIPLocked(&ENIIPRes{ENIIP: &zecTypes.ENIIP{ENI: e.ENI, IPSet: zecTypes.IPSet{IPv4: v4}}, err: errENIExcluded})
			held[a.IP] = true
		}
		e.Unlock()
	}
}

// stampMissingLocked records when a held interface or address was first
// missing from status, and forgets the ones status lists again or the pool
// no longer holds. GC acts on the stamps once crdMissingGrace has passed.
func (f *crdIPFactory) stampMissingLocked(now time.Time) {
	held := map[string]bool{}
	for id, e := range f.enis {
		es := statusENI(&f.status, id)
		if es == nil {
			if _, seen := f.eniMissingSince[id]; !seen {
				f.eniMissingSince[id] = now
			}
			continue
		}
		delete(f.eniMissingSince, id)
		// The record's own interface, not any: an address that moved to
		// another interface is gone from this one, and handing it out with
		// this interface's MAC and route would black-hole the pod.
		on := ipv4Set(es)
		e.Lock()
		for _, r := range e.ips {
			if r.IPSet.IPv4 == nil {
				continue
			}
			ip := r.IPSet.IPv4.String()
			held[ip] = true
			if on[ip] {
				delete(f.missingSince, ip)
			} else if _, seen := f.missingSince[ip]; !seen {
				f.missingSince[ip] = now
			}
		}
		e.Unlock()
	}
	for ip := range f.missingSince {
		if !held[ip] {
			delete(f.missingSince, ip)
		}
	}
}

// gcDueLocked reports whether GC has something to act on: an interface or an
// address missing for longer than crdMissingGrace and not yet handled.
func (f *crdIPFactory) gcDueLocked(now time.Time) bool {
	if f.excludedGCDue {
		return true
	}
	for id, e := range f.enis {
		if since, missing := f.eniMissingSince[id]; missing {
			if now.Sub(since) > crdMissingGrace {
				return true
			}
			continue
		}
		e.Lock()
		for _, r := range e.ips {
			if r.IPSet.IPv4 == nil || r.err != nil {
				continue
			}
			if since, missing := f.missingSince[r.IPSet.IPv4.String()]; missing && now.Sub(since) > crdMissingGrace {
				e.Unlock()
				return true
			}
		}
		e.Unlock()
	}
	return false
}

// GetResourceLimit is status.capacity.allocatable, adjusted both ways:
//   - plus every address held on an interface the controlplane does not count
//     (Excluded, or refused an IPv6 prefix on a dual node): it leaves out the
//     interface's share while the pool and the device plugin count what is
//     held there as used, so the room for new pods would shrink twice. Idle
//     ones count until the pool's GC gives them back (see excludedGCDue);
//   - less the addresses this node will never take (refusedLocked): they fill
//     slots allocatable counts as free.
func (f *crdIPFactory) GetResourceLimit() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.limitLocked()
}

// limitLocked is GetResourceLimit. Caller holds f.mu.
func (f *crdIPFactory) limitLocked() int {
	held := 0
	for id, e := range f.enis {
		if es := statusENI(&f.status, id); es == nil || f.usableLocked(es) {
			continue
		}
		e.Lock()
		held += len(e.ips)
		e.Unlock()
	}
	return max(0, f.status.Capacity.Allocatable-f.refusedLocked()) + held
}

// ---- ZecNode access over the dynamic client ----

type dynamicZecNode struct {
	res  dynamic.NamespaceableResourceInterface
	name string
}

func (d *dynamicZecNode) Get(ctx context.Context) (*v1alpha1.ZecNode, error) {
	u, err := d.res.Get(ctx, d.name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return zecNodeFrom(u.Object)
}

func (d *dynamicZecNode) PatchSpec(ctx context.Context, spec v1alpha1.ZecNodeSpec) (int64, error) {
	// Releasing is always sent, empty included: a merge patch leaves out a
	// missing field, and an emptied list would otherwise never clear.
	body := map[string]any{"spec": map[string]any{
		// Every key sent, empty ones too: a merge patch keeps a field it is
		// not given, so a claim field that became empty would never clear.
		"identity": map[string]any{
			"instanceID": spec.Identity.InstanceID,
			"primaryMAC": spec.Identity.PrimaryMAC,
			"primaryIP":  spec.Identity.PrimaryIP,
		},
		"ipv4": map[string]any{
			"requested": spec.IPv4.Requested,
			"releasing": append([]string{}, spec.IPv4.Releasing...),
		},
	}}
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	u, err := d.res.Patch(ctx, d.name, k8stypes.MergePatchType, b, metav1.PatchOptions{})
	if err != nil {
		return 0, err
	}
	return u.GetGeneration(), nil
}

func zecNodeFrom(obj map[string]any) (*v1alpha1.ZecNode, error) {
	zn := &v1alpha1.ZecNode{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, zn); err != nil {
		return nil, fmt.Errorf("decode ZecNode: %w", err)
	}
	return zn, nil
}

// watchZecNode feeds every change of the node's ZecNode to onChange until ctx
// ends. Only that one object is watched.
func watchZecNode(ctx context.Context, dyn dynamic.Interface, name string, onChange func(*v1alpha1.ZecNode)) error {
	selector := fields.OneTermEqualSelector("metadata.name", name).String()
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, metav1.NamespaceAll,
		func(o *metav1.ListOptions) { o.FieldSelector = selector })
	inf := factory.ForResource(v1alpha1.GVR).Informer()
	handle := func(obj any) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return
		}
		zn, err := zecNodeFrom(u.Object)
		if err != nil {
			log.Warnf("%v", err)
			return
		}
		onChange(zn)
	}
	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    handle,
		UpdateFunc: func(_, obj any) { handle(obj) },
	}); err != nil {
		return err
	}
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
		return fmt.Errorf("ZecNode %s watch did not sync; is the CRD installed?", name)
	}
	return nil
}

// isNotFound reports a missing object.
func isNotFound(err error) bool { return apierrors.IsNotFound(err) }
