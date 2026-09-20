// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/signal"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/tracing"
)

// InstanceLimitsAttr holds the per-instance quota, before anything the node has
// done with it. IPv4MaxPerENI counts the interface's own primary address, so an
// interface serves at most IPv4MaxPerENI-1 pods. IPv6CidrMaxPerENI is not in the
// published spec table: one /96 per interface is what the datapath assumes.
type InstanceLimitsAttr struct {
	// ENIQuota is the per-instance cap on network interfaces, resolved from a
	// spec table keyed on vCPU (see resolveFromSpecTable). It is the only cap
	// this daemon models: nothing here tracks a limit above the instance.
	ENIQuota          int
	IPv4MaxPerENI     int
	IPv6CidrMaxPerENI int
}

// InstanceLimits is InstanceLimitsAttr plus what this node has done with the
// quota.
type InstanceLimits struct {
	InstanceLimitsAttr
	// ENIUnmanaged counts the interfaces attached to this instance that this
	// daemon does not manage, excluding the instance's own. They hold interface
	// slots without serving pod addresses, so they come off the budget.
	ENIUnmanaged int

	// ENIManaged counts the non-primary interfaces this daemon manages: the ones
	// it created and the ones it adopted. Adopted interfaces are why this is not
	// named for creation.
	ENIManaged int
	Cordon     bool
}

// String renders the quota fields alone. InstanceLimits.String adds what only
// the running node knows.
func (a InstanceLimitsAttr) String() string {
	return fmt.Sprintf("{ENIQuota: %d, IPv4MaxPerENI: %d, IPv6CidrMaxPerENI: %d}",
		a.ENIQuota, a.IPv4MaxPerENI, a.IPv6CidrMaxPerENI)
}

// String renders the quota fields, what the node has done with them, and the
// allocatable count they produce -- without allocatable the line holds only the
// inputs and a reader has to redo the arithmetic.
//
// allocatable is this daemon's own figure, not the number on the node: the
// device plugin advertises zenlayer.com/eni-ip as this minus the pods that do
// not request the resource, so the two differ by however many such pods are
// running. Compare them for a trend, never expect them to be equal.
func (l *InstanceLimits) String() string {
	return fmt.Sprintf("{ENIQuota: %d, IPv4MaxPerENI: %d, IPv6CidrMaxPerENI: %d, ENIManaged: %d, ENIUnmanaged: %d, Cordon: %t, allocatable: %d}",
		l.ENIQuota, l.IPv4MaxPerENI, l.IPv6CidrMaxPerENI, l.ENIManaged, l.ENIUnmanaged, l.Cordon,
		l.AllocatableIPs())
}

// ENIAvailable is how many interfaces the CNI may still have attached: the
// quota, less the instance's own, less any attached by someone else. Under a
// cordon it is instead the number already managed, which is the figure
// createEniAsync compares its own count against.
//
// Never negative. Expanded, this is
//
//	ENIQuota - totalAttached + cniCreated
//
// which goes below zero whenever more interfaces than the quota allows are
// attached that the CNI did not create. No misuse is needed: ENIQuota comes
// from a hardcoded spec table keyed on vCPU, rounding DOWN to the nearest tier
// (see resolveFromSpecTable), and the table has no row between 8 and 16 vCPU --
// a 9-15 vCPU instance is told 2. If the platform in fact allows more there and
// four interfaces end up attached, this reaches -2.
//
// The clamp is here rather than at each call site because it used to return a
// negative, and all three consumers of the capacity derived from it read that
// wrong: the pool sized its ticket channel from GetResourceLimit and panicked
// in make(), judgmentEvictPod found "len(pods) >= resourceLimit" true for the
// very first pod and evicted every pod on the node, and AllocatableIPs clamped
// the product instead of the term, canceling out the instance's own interface
// along with the negative.
//
// createEniAsync, the one caller that compares against it directly, is
// unaffected: "created < available" with created >= 0 refuses at 0 exactly as
// it did at -2. The over-budget magnitude is not exposed because nothing needs
// it; it shows up in the InstanceLimits log line.
func (l *InstanceLimits) ENIAvailable() int {
	cnt := l.ENIQuota - 1 - l.ENIUnmanaged
	if l.Cordon {
		cnt = l.ENIManaged
	}
	return max(0, cnt)
}

// AllocatableIPs is how many pod addresses this instance can hold:
//
//	(ENIAvailable() + 1) * (IPv4MaxPerENI - 1)
//
// Every interface is counted, including the instance's own, and every interface
// reserves one address for itself -- the datapath configures each interface's
// own primary on it (see eniAddrs), and a configured address cannot also be a
// pod's.
//
// The alternative convention, (adapters-1)*ipsPerAdapter, gives up one whole
// interface instead. Either is self-consistent; combining them subtracts the
// same quota twice.
func (l *InstanceLimits) AllocatableIPs() int {
	// IPv4MaxPerENI-1 is what an interface can give pods, since it keeps its
	// own primary. At 0 the terms below would go negative, so this floor is
	// load-bearing; at 1 the formula already yields 0 and this only says so
	// outright.
	if l.IPv4MaxPerENI <= 1 {
		return 0
	}
	// The instance's own interface is added, never multiplied in: folding it
	// into the multiplication would let a zero interface count cancel it, and a
	// node whose eth0 can still serve perENI-1 pods would report nothing -- the
	// device plugin advertises zero, the pool's maxCap drains every warm
	// address, and every ADD is refused.
	//
	// ENIAvailable() clamps itself, so there is no clamp here.
	return l.ENIAvailable()*(l.IPv4MaxPerENI-1) + (l.IPv4MaxPerENI - 1)
}

type InstanceLimitManager interface {
	// GetLimit returns a snapshot of the current limits.
	GetLimit() InstanceLimits
	// Update refreshes the limits from the platform, at most once a minute.
	// Failures are logged, not returned.
	Update()
	// WatchUpdate registers a channel that is signaled after every successful
	// refresh and on uncordon -- not only when a field actually changed. The
	// send is non-blocking, so a watcher that is not draining loses it.
	WatchUpdate(name string, watcher chan<- struct{})
	// CordonCreate stops ENI creation until UnCordonCreate: ENIAvailable()
	// falls back to the count already managed. It refreshes the limits first,
	// and does not apply the cordon if that fails.
	CordonCreate(name string)
	// UnCordonCreate lifts the cordon and notifies the watchers.
	UnCordonCreate(name string)
	// NotifyWatcher signals every registered watcher.
	NotifyWatcher()
}

type defaultInstanceLimit struct {
	lock          sync.RWMutex
	api           ZecAPI
	limit         InstanceLimits
	lastUpdate    time.Time
	eventWatchers []chan<- struct{}
	// wakeGCPending records that GC should run because an interface left the
	// instance, and that no notification has reached it yet. Guarded by lock.
	wakeGCPending bool
}

var instanceLimitManager *defaultInstanceLimit

func (m *defaultInstanceLimit) GetLimit() InstanceLimits {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return m.limit
}

func (m *defaultInstanceLimit) Update() {
	if err := m.update(); err != nil {
		log.Errorf("Update InstanceLimit failed, %v", err)
	}
}

func (m *defaultInstanceLimit) WatchUpdate(name string, watcher chan<- struct{}) {
	m.lock.Lock()
	defer m.lock.Unlock()
	log.Infof("Component %s watch update", name)
	m.eventWatchers = append(m.eventWatchers, watcher)
}

func (m *defaultInstanceLimit) NotifyWatcher() {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.notifyWatcherLocked()
}

func (m *defaultInstanceLimit) notifyWatcherLocked() {
	log.Infof("Notify watcher due to limit update")
	for _, watcher := range m.eventWatchers {
		select {
		case watcher <- struct{}{}:
		default:
		}
	}
}

// fetchLimit does the three platform calls a refresh needs, with no lock held.
// Each retries with backoff, and GetLimit takes the read side of m.lock from
// inside f.Lock -- so holding it across them freezes allocation on the node.
func (m *defaultInstanceLimit) fetchLimit() (attr InstanceLimitsAttr, managed, total int, err error) {
	log.Infof("InstanceLimit Updating")
	newLimit, err := m.api.GetInstanceLimit()
	if err != nil {
		return attr, 0, 0, err
	}
	created, err := m.api.GetAttachedENIs()
	if err != nil {
		return attr, 0, 0, err
	}
	total, err = m.api.GetTotalAttachedEniCnt()
	if err != nil {
		return attr, 0, 0, err
	}

	// GetAttachedENIs returns the instance's own interface plus every secondary
	// this daemon manages -- the ones it created and the ones it adopted. Count
	// only the secondaries: both figures below are about interfaces this daemon
	// may attach and detach.
	for _, eni := range created {
		if !eni.IsPrimary {
			managed++
		}
	}
	return newLimit.InstanceLimitsAttr, managed, total, nil
}

func (m *defaultInstanceLimit) updateLocked(newAttr InstanceLimitsAttr, managed, total int) {
	oldLimit := m.limit.InstanceLimitsAttr
	emptyLimit := InstanceLimitsAttr{}
	if oldLimit != emptyLimit && oldLimit != newAttr {
		// Both sides are InstanceLimitsAttr, matching what the condition above
		// compared. Printing the whole InstanceLimits would report a change in
		// fields this branch never looked at -- and it would not even render:
		// InstanceLimits.String has a pointer receiver, so a value of that type
		// falls back to Go's default struct formatting.
		_ = tracing.RecordNodeEvent(v1.EventTypeWarning, tracing.EventInstanceQuotaUpdated,
			fmt.Sprintf("Instance quota updated from %v to %v", oldLimit, newAttr))
	}

	// A drop means an interface this daemon managed is no longer on the
	// instance -- detached out of band, or released by the pool. GC is what
	// stops its worker and drops it from the factory; until then submitOrder
	// keeps routing orders to an interface that looks like it has room, and
	// each one costs an API round trip to find out it is gone.
	//
	// Waking GC here shortens that from a full reconcile interval to the limit
	// monitor's own period (instanceLimitPeriod), at the cost of at most one
	// extra pool pause per poll -- GC pauses the pool while it runs, and
	// gcDebounce keeps it to once a minute in any case.
	//
	// It goes through wakeGCPending rather than a bare NotifySignal because GC
	// mutes this signal while it runs: a notification sent in that window is
	// discarded, and by the next poll the counts match again and the drop is no
	// longer visible, so the wake would be lost and the interface would sit in
	// the factory until the next reconcile tick.
	if managed < m.limit.ENIManaged {
		log.Infof("Managed eni count dropped from %d to %d, waking gc", m.limit.ENIManaged, managed)
		m.wakeGCPending = true
	}
	m.limit.ENIManaged = managed
	if m.wakeGCPending && signal.NotifySignal(signal.WakeGC, signal.SigWakeGC) {
		m.wakeGCPending = false
	}

	// Whatever is attached but not managed. GetAttachedENIs skips an interface
	// for any of three reasons: it carries the no-manage tag, it sits outside
	// the configured pod subnets, or it is foreign and manageUntaggedENI is
	// off. Each still holds quota without contributing addresses, so it comes
	// off the budget. The -1 is the instance's own interface.
	m.limit.ENIUnmanaged = max(0, total-managed-1)
	m.limit.InstanceLimitsAttr = newAttr
	log.Infof("InstanceLimit Updated %s", m.limit.String())
	m.lastUpdate = time.Now()
	m.notifyWatcherLocked()
}

func (m *defaultInstanceLimit) update() error {
	m.lock.RLock()
	seen := m.lastUpdate
	fresh := time.Since(m.lastUpdate) < time.Minute
	m.lock.RUnlock()
	if fresh {
		return nil
	}

	newAttr, managed, total, err := m.fetchLimit()
	if err != nil {
		return err
	}

	// Someone else refreshed while we were fetching. Writing our count over
	// theirs would revive an interface the pool has already released, and under
	// cordon ENIAvailable is ENIManaged, so the node would advertise a whole
	// interface of addresses it cannot serve. Either way the next poll re-reads
	// both counts, so a refresh dropped here costs at most one period.
	m.lock.Lock()
	defer m.lock.Unlock()
	if m.lastUpdate.After(seen) {
		return nil
	}
	m.updateLocked(newAttr, managed, total)
	return nil
}

func (m *defaultInstanceLimit) CordonCreate(name string) {
	m.lock.RLock()
	already := m.limit.Cordon
	seen := m.lastUpdate
	m.lock.RUnlock()
	if already {
		return
	}

	log.Infof("Cordon eni create by %s", name)
	newAttr, managed, total, err := m.fetchLimit()
	if err != nil {
		log.Errorf("Update InstanceLimit failed, %v", err)
		return
	}

	// The fetch above is not under the lock, so an UnCordonCreate can land
	// between the check and this set and be overwritten. The subnet monitor
	// calls UnCordonCreate every SubnetStatUpdateIntervalSec, so the worst case
	// is one such period of refusing to create interfaces -- the conservative
	// direction, and nothing that is already serving pods is affected.
	m.lock.Lock()
	defer m.lock.Unlock()
	// Someone else refreshed while we were fetching, so our counts are dropped
	// for the same reason as in update. The cordon still goes on, and so does
	// the notify updateLocked would have sent with it.
	if m.lastUpdate.After(seen) {
		m.limit.Cordon = true
		m.notifyWatcherLocked()
		return
	}
	m.updateLocked(newAttr, managed, total)
	m.limit.Cordon = true
}

func (m *defaultInstanceLimit) UnCordonCreate(name string) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if !m.limit.Cordon {
		return
	}
	log.Infof("UnCordon eni create by %s", name)
	m.limit.Cordon = false
	m.notifyWatcherLocked()
}

// NewInstanceLimitManager returns the process-wide manager, building it and
// doing a first refresh on the initial call. Later calls return the existing
// manager and ignore api.
func NewInstanceLimitManager(api ZecAPI) (InstanceLimitManager, error) {
	if instanceLimitManager != nil {
		return instanceLimitManager, nil
	}

	instanceLimitManager = &defaultInstanceLimit{
		lock:          sync.RWMutex{},
		api:           api,
		eventWatchers: []chan<- struct{}{},
	}
	if err := instanceLimitManager.update(); err != nil {
		return nil, err
	}
	return instanceLimitManager, nil
}
