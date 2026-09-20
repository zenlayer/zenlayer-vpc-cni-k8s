// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package pool keeps resources ready ahead of demand.
//
// Allocation on this platform is a round trip to a remote API, far too slow to
// sit inside a pod's startup path, so the pool holds idle resources and refills
// itself in the background. It tracks three states -- available, in use, invalid
// -- and sizes itself between a target for the idle part and floor and ceiling
// bounds on the total. Everything platform-specific lives behind ObjectFactory.
package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/metrics"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

var log = zlog.GetLogger().WithFields(zlog.Fields{"subsys": "pool"})

var (
	// ErrContextDone is what Allocate returns when the caller's deadline expires
	// while it is waiting for a resource to be created. Whether the creation
	// eventually succeeded is not knowable from here.
	ErrContextDone = errors.New("context done")
	// ErrNoResourceAvailableInPool never leaves this package. allocateFromPool
	// uses it to tell Allocate "nothing idle, but the pool may still grow", and
	// Allocate answers by waiting for a ticket and calling the factory.
	ErrNoResourceAvailableInPool = errors.New("no resource available in pool, applying")
	// ErrNoResourceAvailable means the pool is empty and may not grow, because
	// the total already reaches maxCap. Unlike the one above it is returned to
	// the caller: waiting cannot help until something is released.
	ErrNoResourceAvailable = errors.New("no resource available, check quota")
	// ErrResourceInvalid is what Release returns when the pool holds no record
	// of the resource as in use. Callers outside this package match it with
	// errors.Is and treat it as "already released", so its identity is part of
	// the API -- but Release itself reports it rather than swallowing it.
	ErrResourceInvalid = errors.New("resource state invalid")

	defaultPoolBackoffPeriod = 5 * time.Second
	maxPoolBackoffPeriod     = 1 * time.Minute
)

// ObjectFactory is everything platform-specific about a resource: how to make
// one, how to check it is still real, and how to destroy it. The pool holds no
// platform knowledge of its own, so a factory is what makes a pool concrete.
//
// Implementations talk to a remote API and are therefore slow and fallible.
// The pool calls them off the allocation path wherever it can, and the one
// place it cannot -- GC, which holds the pool paused for the duration -- is why
// no new network call should be added to that path.
type ObjectFactory interface {
	Name() string

	// Create requests count resources. It may return fewer than requested
	// without an error, so callers must go by the length of the returned slice.
	Create(count int) ([]types.NetResource, error)

	// Release destroys a resource the pool no longer wants. A resource that is
	// already gone is not an error: the platform is the authority, and the pool
	// asking twice must converge on the same state.
	Release(resource types.NetResource) error

	// ReleaseInValid destroys an invalid resource. It may return a replacement
	// that takes the released resource's place in the pool.
	ReleaseInValid(resource types.NetResource) (types.NetResource, error)

	// Valid reports whether the resource is still usable. It answers from what
	// the factory already knows rather than by querying the platform, so it is
	// cheap enough for the allocation path. A resource that is merely incomplete
	// -- missing an address family the node serves -- is reported through a
	// distinct error so the pool can retire it without treating it as a failure.
	Valid(resource types.NetResource) error

	// List returns every resource the factory knows of, keyed by status.
	List() (map[types.ResStatus]map[string]types.NetResource, error)

	// GC settles the factory's own view against the platform's: it releases what
	// the platform no longer has, and adopts what the platform reports but the
	// factory never recorded.
	//
	// The pool is paused for the whole call, which blocks every allocation and
	// release on the node, and this is the one factory method that makes remote
	// calls on that path. A slow GC is therefore indistinguishable from an
	// outage for any pod starting at the time -- do not add network calls here.
	GC() error

	// GetResourceLimit is the upper bound on what the platform could supply.
	// The pool applies its own cap on top; see poolImpl.GetResourceLimit.
	GetResourceLimit() int
}

// ResourcePool is the allocation surface the daemon uses. Implementations keep
// idle resources ready so that a pod's startup path does not have to wait for
// the platform, and reconcile what they hold against what the factory reports.
type ResourcePool interface {
	Name() string

	// Allocate hands out prefer when owner already holds it or it is still
	// queued, otherwise any pooled resource, creating one when the pool has
	// none.
	Allocate(ctx context.Context, prefer, owner string) (types.NetResource, error)

	Release(resID string) error

	// InuseByOwner returns the resources the pool records as held by owner.
	//
	// The pool is the only place that knows this when the stored pod record is
	// gone: PreStart adopts an address the API server says a live pod holds and
	// marks it in use, but there is no record naming it, so a later release has
	// nothing to release from. Looking the owner up here closes that gap.
	InuseByOwner(owner string) []types.NetResource

	Status() Status

	GetSnapshot() (ResourcePoolSnapshot, error)

	// GC sync resource in ResourcePool with resource in ObjectFactory
	GC(getAllocatedResMap func() (map[string]types.NetResourceAllocated, error)) error // resourceId -- vpcResource

	// GetResourceLimit is how many resources the pool will hand out: its max
	// cap. See poolImpl.GetResourceLimit.
	GetResourceLimit() int

	// ReCfgCache reconfigure pooling parameters
	ReCfgCache(target, targetMin int)
}

type ResourcePoolSnapshot interface {
	// PoolSnapshot is what the pool believes it holds.
	PoolSnapshot() map[string]types.NetResourceStatus

	// MetaSnapshot is what the ObjectFactory reports, which may differ.
	MetaSnapshot() map[string]types.NetResourceStatus
}

type ResourcePoolSnapshotCache struct {
	Pool map[string]types.NetResourceStatus
	Meta map[string]types.NetResourceStatus
}

func (r *ResourcePoolSnapshotCache) PoolSnapshot() map[string]types.NetResourceStatus {
	return r.Pool
}

func (r *ResourcePoolSnapshotCache) MetaSnapshot() map[string]types.NetResourceStatus {
	return r.Meta
}

// ResourcePoolOp is the write surface handed to a ResourcePoolHook, which runs
// before the pool starts serving.
type ResourcePoolOp interface {
	AddInuse(res types.NetResource, owner string)

	AddInvalid(res types.NetResource)

	AddAvailable(res types.NetResource)
}

type ResourcePoolHook func(pool ResourcePoolOp) error

type poolItem struct {
	owner         string
	res           types.NetResource
	lastUse       time.Time
	reserveBefore time.Time
}

type poolImpl struct {
	name      string
	resType   string
	inUse     map[string]poolItem // id <--> item
	available *priorityQueue
	invalid   map[string]poolItem // id <--> item, unusable and waiting to be released
	factory   ObjectFactory

	mutex     sync.Mutex
	pauseLock sync.RWMutex

	// concurrency control
	ticket  chan struct{}
	backoff time.Duration

	scale chan struct{}

	// ownerAlive is Config.OwnerAlive; see the comment there.
	ownerAlive func(owner string) bool

	// config
	poolConfigInner

	// metrics
	metricAvailable prometheus.Gauge
	metricTotal     prometheus.Gauge

	zlog.Logger
}

// backoffCallFactory lengthens the pause taken before the next factory call.
//
// It floors at defaultPoolBackoffPeriod rather than doubling whatever is
// already there, because doubling the zero value gives zero: backoffReset was
// the only thing that ever wrote a non-zero period and it runs on success, so a
// pool whose factory had not yet succeeded once -- unreachable platform,
// rejected credentials, exhausted quota -- slept 0s between attempts and
// retried the platform API as fast as the worker could go round.
//
// Clamped rather than merely gated: doubling 40s passes the gate and lands on
// 80s, longer than the maximum this is supposed to reach.
func (p *poolImpl) backoffCallFactory() {
	if p.backoff < defaultPoolBackoffPeriod {
		p.backoff = defaultPoolBackoffPeriod
		return
	}
	p.backoff = min(p.backoff*2, maxPoolBackoffPeriod)
}

func (p *poolImpl) backoffReset() {
	p.backoff = defaultPoolBackoffPeriod
}

func (p *poolImpl) lock() {
	p.mutex.Lock()
}

func (p *poolImpl) unlock() {
	p.mutex.Unlock()
}

func (p *poolImpl) blockPause() {
	p.pauseLock.RLock()
}

func (p *poolImpl) unblockPause() {
	p.pauseLock.RUnlock()
}

func (p *poolImpl) pause() {
	// pauseLock before mutex, which is the order every other path takes them in
	// (blockPause, then lock). Reversing it here deadlocks against a caller that
	// already holds pauseLock and is waiting on mutex.
	p.pauseLock.Lock()
	p.mutex.Lock()
}

func (p *poolImpl) unpause() {
	p.mutex.Unlock()
	p.pauseLock.Unlock()
}

func (p *poolImpl) Name() string {
	return p.name
}

func (p *poolImpl) AddInuse(res types.NetResource, owner string) {
	p.lock()
	defer p.unlock()
	id := res.GetID()
	p.Debugf("Add resource %s to pool inuse", id)

	// One entry per resource, everywhere. A resource handed straight to a pod
	// (the create-on-demand path) is not supposed to be sitting in the
	// available queue as well, but it can be: reconcile adopts addresses the
	// platform reports, and so does the factory when it recovers one from an
	// assign whose outcome was never confirmed. Leaving the stale entry behind
	// would let a second pod pop the address this one is already using.
	prev, alreadyHeld := p.inUse[id]
	if alreadyHeld && prev.owner != owner {
		// No caller legitimately re-owns a resource: PreStart restates owners
		// it read from the stored records or the API server, and the
		// create-on-demand path deals in resources nothing else has seen. A
		// change here means two pods believe they hold one address.
		p.Warnf("Resource %s changes owner from %s to %s", id, prev.owner, owner)
	}

	if evicted := p.available.Remove(id); evicted != nil {
		p.Warnf("Resource %s was also queued as available; taking it for %s", id, owner)
		p.metricAvailable.Dec()
		alreadyHeld = true
	}
	// Likewise for the invalid set: checkInvalid releases what it finds there,
	// and releasing an address a pod is using is worse than leaking one.
	if _, held := p.invalid[id]; held {
		p.Warnf("Resource %s was queued for release; taking it for %s instead", id, owner)
		delete(p.invalid, id)
		alreadyHeld = true
	}

	// metricTotal counts resources, not calls: a resource moving between the
	// three holders was already counted when it first entered the pool.
	if !alreadyHeld {
		p.metricTotal.Inc()
	}
	p.inUse[id] = poolItem{
		owner:   owner,
		res:     res,
		lastUse: time.Now(),
	}
}

func (p *poolImpl) AddInvalid(res types.NetResource) {
	p.lock()
	defer p.unlock()
	p.Debugf("Add resource %s to pool invalid", res.GetID())
	p.invalid[res.GetID()] = poolItem{
		res: res,
	}
	p.metricTotal.Inc()
}

func (p *poolImpl) AddAvailable(res types.NetResource) {
	p.lock()
	defer p.unlock()
	if p.addAvailableLocked(res) {
		p.metricTotal.Inc()
	}
}

// addAvailableLocked queues res for handing out, unless the pool already holds
// it somewhere. It reports whether the resource was new to the pool.
//
// Several paths can legitimately arrive here with a resource the pool already
// knows: PreStart adoption, and a create whose result the factory recovered
// from the platform after an unconfirmed assign. Neither can see the other's
// timing, so the check belongs here rather than in each caller.
//
// The queue only guards its own contents -- a repeated id collapses into the
// entry already queued (priorityQueue.Push). It knows nothing about inUse and
// invalid, which is where a duplicate actually hurts: queuing an address a pod
// is already using hands it to a second pod, and queuing one that is pending
// release gets it handed out and unassigned underneath its holder.
func (p *poolImpl) addAvailableLocked(res types.NetResource) bool {
	id := res.GetID()
	if _, held := p.inUse[id]; held {
		p.Warnf("Resource %s is already in use, refusing to also queue it as available", id)
		return false
	}
	if _, held := p.invalid[id]; held {
		p.Warnf("Resource %s is held as invalid, refusing to queue it as available", id)
		return false
	}
	if p.available.Find(id) != nil {
		p.Debugf("Resource %s is already available, not queuing it twice", id)
		return false
	}
	p.Debugf("Add resource %s to pool available", id)
	p.available.Push(&poolItem{
		res:           res,
		reserveBefore: time.Now(),
	})
	p.metricAvailable.Inc()
	return true
}

// capWithLocked returns inUse + available + invalid. Caller must hold p.mutex.
func (p *poolImpl) capWithLocked() int {
	return len(p.inUse) + p.available.Size() + len(p.invalid)
}

func (p *poolImpl) notifyScale() {
	select {
	case p.scale <- struct{}{}:
	default:
	}
}

func (p *poolImpl) productTicket() {
	select {
	case p.ticket <- struct{}{}:
	default:
	}
}

func (p *poolImpl) allocateFromPool(prefer, owner string) (types.NetResource, error) {
	p.lock()
	defer p.unlock()

	lg := p.WithFields(zlog.Fields{"prefer": prefer, "owner": owner})
	if item, exist := p.inUse[prefer]; exist && item.owner == owner {
		return item.res, nil
	}

	if p.available.Size() > 0 {
		item := p.available.PopPrefer(prefer)
		if item == nil {
			return nil, apiErr.ErrNotFound
		}
		if prefer != "" && item.res.GetID() != prefer {
			lg.Infof("Preferred resource %s no longer in pool, allocated %s instead", prefer, item.res.GetID())
		}
		res := item.res
		p.inUse[res.GetID()] = poolItem{
			owner:   owner,
			lastUse: time.Now(),
			res:     res,
		}
		p.metricAvailable.Dec()
		lg.Infof("Allocate resource from pool success: %s", res.GetID())
		p.notifyScale()
		return res, nil
	}

	if curCap := p.capWithLocked(); curCap >= p.getMaxCap() {
		lg.Warnf("Allocate resource from pool failed: %s (current cap: %d, max cap: %d)",
			ErrNoResourceAvailable, curCap, p.getMaxCap())
		return nil, ErrNoResourceAvailable
	}

	return nil, ErrNoResourceAvailableInPool
}

func (p *poolImpl) Allocate(ctx context.Context, prefer, owner string) (types.NetResource, error) {
	lg := p.WithFields(zlog.Fields{"prefer": prefer, "owner": owner})

	for {
		res, err := p.allocateFromPool(prefer, owner)
		if err == nil {
			return res, nil
		}
		if !errors.Is(err, ErrNoResourceAvailableInPool) {
			return nil, err
		}

		select {
		case <-ctx.Done():
			lg.Infof("Allocate resource return because %s", ErrContextDone)
			return nil, ErrContextDone
		case <-p.ticket:
			exec := func() (types.NetResource, error) {
				p.blockPause()
				defer p.unblockPause()
				resources, err := p.factory.Create(1)
				if err != nil || len(resources) == 0 {
					p.productTicket()
					lg.Errorf("Factory create resource err: %v", err)
					return nil, fmt.Errorf("factory create resource err: %v", err)
				}
				p.AddInuse(resources[0], owner)
				lg.Infof("Allocate resource from pool success after create: %s", resources[0].GetID())
				return resources[0], nil
			}
			return exec()
		default:
			time.Sleep(defaultPoolBackoffPeriod)
		}
	}
}

func (p *poolImpl) Release(resID string) error {
	// pauseLock first, then mutex -- the order pause() documents, and reversing
	// it here would deadlock against a GC waiting on the mutex.
	//
	// Held for the whole call, not just the factory round trip: GC replaces
	// p.available and p.invalid wholesale from factory.List(), and the window
	// below has the resource out of inUse while the factory still reports it.
	// A GC landing there would push it into available -- allocatable to the next
	// pod -- while ReleaseInValid is unassigning that very address from the
	// interface. checkInvalid is safe from this only because worker() wraps it
	// in blockPause; this path has to do the same for itself.
	//
	// What this trades is deliberate: the blockage moves off the mutex, which
	// every Allocate needs, and onto pauseLock, which only GC needs.
	p.blockPause()
	defer p.unblockPause()

	p.lock()
	item, exist := p.inUse[resID]
	if !exist {
		p.unlock()
		p.Errorf("Find resource %s not exist in pool", resID)
		return ErrResourceInvalid
	}
	delete(p.inUse, resID)

	// Valid stays under the lock: it answers from the factory's own in-memory
	// view of the interfaces and their addresses, with no remote call.
	validErr := p.factory.Valid(item.res)
	if validErr == nil {
		p.available.Push(&poolItem{
			res:           item.res,
			reserveBefore: time.Now(),
		})
		p.metricAvailable.Inc()
		p.unlock()
		p.Infof("Release resource to pool %s success", resID)
		return nil
	}
	p.unlock()

	// ReleaseInValid does make a remote call -- an unassign with exponential
	// backoff, then a metadata poll -- and it must not run under p.mutex. That
	// mutex is what every Allocate takes, so holding it across a platform
	// hiccup here stalled every pod starting on the node for tens of seconds,
	// well into the CNI ADD budget, with nothing in the log but one ordinary
	// dealloc retry. checkInvalid and tryReducePool already split this way; this
	// path was the one that did not.
	//
	// While the lock is down the resource belongs to nobody: it has left inUse
	// and has reached none of the other holders. Every branch below has to put
	// it somewhere, or the pool silently loses a resource it is still counting.
	// Nothing else can place it in the meantime: GC is held off by pauseLock
	// above, and the paths that only take the mutex all act on ids they are
	// already holding, never on this one.
	temp, relErr := p.factory.ReleaseInValid(item.res)

	p.lock()
	defer p.unlock()

	switch {
	case relErr == nil && temp == nil:
		p.productTicket()
		p.metricTotal.Dec()
	case temp != nil:
		p.Warnf("Convert invalid resource %s to valid %s", resID, temp.GetID())
		if !p.addAvailableLocked(temp) {
			// Another holder already has it, so the pool is no longer counting
			// this one separately.
			p.metricTotal.Dec()
		}
	default:
		p.Warnf("Destroy resource %v failed: %v", item.res, relErr)
		p.invalid[resID] = item
	}
	return nil
}

func (p *poolImpl) InuseByOwner(owner string) []types.NetResource {
	p.lock()
	defer p.unlock()

	var found []types.NetResource
	for _, item := range p.inUse {
		if item.owner == owner {
			found = append(found, item.res)
		}
	}
	return found
}

func (p *poolImpl) Status() Status {
	p.lock()
	defer p.unlock()

	cur := p.available.Size()
	cnt := max(p.getTarget()-cur, 0)
	cur += len(p.inUse)
	cnt = max(cnt, p.getTargetMin()-cur)
	cur += len(p.invalid)
	cnt = min(cnt, p.getMaxCap()-cur)
	short := max(0, cnt)

	over := max(p.available.Size()-p.getTarget(), 0)
	over = max(min(over, cur-p.getTargetMin()), 0)

	return Status{
		TargetMin:       p.getTargetMin(),
		Target:          p.getTarget(),
		MaxCap:          p.getMaxCap(),
		MaxCapProbe:     p.getMaxCapProbe(),
		MonitorInterval: p.getMonitorInterval(),
		Total:           cur,
		Available:       p.available.Size(),
		Short:           short,
		Over:            over,
	}
}

// GetResourceLimit is how many resources this pool will actually hand out.
//
// It deliberately reports the pool's own cap and not the factory's, which is
// only an upper bound on what the platform could supply: advertising room the
// pool would refuse to give leaves a pod scheduled on that claim without an
// address.
//
// Which way this errs matters, because two things downstream act on it -- the
// capacity the node advertises, and the point at which the daemon starts
// evicting pods. With capacity probing on, getMaxCap takes the factory's figure
// and the two agree. With probing off the pool stops at the configured maxCap,
// so an instance that could hold far more is described as holding only that
// many, and eviction begins at that lower number. Neither symptom looks like a
// misconfiguration from the node's side: the pool is working exactly as
// configured. That is why the cap has to be raised to match the instance
// whenever probing is turned off.
func (p *poolImpl) GetResourceLimit() int {
	return p.getMaxCap()
}

func (p *poolImpl) resetTicketWithLocked() {
out:
	for {
		select {
		case <-p.ticket:
		default:
			break out
		}
	}
	cnt := p.getMaxCap() - p.capWithLocked()
	for i := 0; i < cnt; i++ {
		p.productTicket()
	}
}

func (p *poolImpl) shouldIncrease() int {
	p.lock()
	defer p.unlock()
	cur := p.available.Size()
	cnt := max(p.getTarget()-cur, 0)
	cur += len(p.inUse)
	cnt = max(cnt, p.getTargetMin()-cur)
	cur += len(p.invalid)
	cnt = min(cnt, p.getMaxCap()-cur)
	return max(0, cnt)
}

func (p *poolImpl) tryIncreasePool() {
	p.Debugf("Try Increase pool")
	toIncrease := p.shouldIncrease()
	if toIncrease <= 0 {
		return
	}
	toCreate := 0
	for i := 0; i < toIncrease; i++ {
		select {
		case <-p.ticket:
			toCreate++
		default:
			continue
		}
	}
	p.Infof("Try create %d resource", toCreate)
	if toCreate > 0 {
		res, err := p.factory.Create(toCreate)
		if err != nil {
			p.Errorf("Create resource failed: %v, backoff: %v", err, p.backoff)
			defer func() {
				p.backoffCallFactory()
				time.Sleep(p.backoff)
			}()
		}
		if len(res) == toCreate {
			p.backoffReset()
		} else {
			p.Warnf("Resource created: %d, expected: %d", len(res), toCreate)
		}
		releaseTickets := max(0, toCreate-len(res))
		for i := 0; i < releaseTickets; i++ {
			p.productTicket()
		}
		if releaseTickets != 0 { // part failed, try again
			p.notifyScale()
		}
		for _, item := range res {
			p.AddAvailable(item)
		}
		p.Infof("%d resource increased", len(res))
	}
}

func (p *poolImpl) popOverflow() *poolItem {
	p.lock()
	defer p.unlock()

	var item *poolItem
	if (p.available.Size() > p.getTarget() &&
		p.available.Size()+len(p.inUse) > p.getTargetMin()) ||
		p.capWithLocked() > p.getMaxCap() {
		item = p.available.Peek()
	}
	if item != nil && item.reserveBefore.Before(time.Now()) {
		return p.available.Pop()
	}
	return nil
}

func (p *poolImpl) tryReducePool() {
	p.Debugf("Try Reduce pool")
	var reAvailable []types.NetResource
	// Bound the loop: a resource that fails to release goes back into
	// available, where popOverflow picks it up again. Without a cap that is an
	// endless (backoff-paced) loop which starves the rest of the worker.
	maxAttempts := p.getMaxCap() + 1
	for attempt := 0; ; attempt++ {
		if attempt >= maxAttempts {
			p.Warnf("Reduce pool stopped after %d attempts, will retry next cycle", attempt)
			break
		}
		item := p.popOverflow()
		if item == nil {
			break
		}
		p.metricTotal.Dec()
		p.metricAvailable.Dec()
		err := p.factory.Release(item.res)
		if err == nil {
			p.Infof("Destroy resource %v succeed", item.res)
			p.productTicket()
			p.backoffReset()
		} else if errors.Is(err, apiErr.ErrInvalidDeletionPrimaryIP) {
			// An interface's own primary address cannot be given back on its
			// own, so it goes straight back into the pool. Logged because
			// otherwise the pool silently stops shrinking: these addresses
			// still occupy warm slots, and once they fill the target the
			// reduction halts for good.
			p.Infof("Resource %v is an interface's primary address and cannot be released on its own, "+
				"returning it to available", item.res.GetID())
			reAvailable = append(reAvailable, item.res)
		} else {
			p.Warnf("Destroy resource %v failed: %v, backoff: %v", item.res, err, p.backoff)
			p.backoffCallFactory()
			p.AddAvailable(item.res)
			time.Sleep(p.backoff)
		}
	}

	for _, res := range reAvailable {
		p.AddAvailable(res)
	}
}

func (p *poolImpl) checkInvalid() {
	// Snapshot under the lock, then call the factory (a cloud API round trip)
	// without holding it -- the same split tryReducePool uses. Holding p.mutex
	// across those calls stalls every Allocate/Release on the node.
	p.lock()
	snapshot := make(map[string]types.NetResource, len(p.invalid))
	for id, invalid := range p.invalid {
		snapshot[id] = invalid.res
	}
	p.unlock()

	for id, res := range snapshot {
		lg := p.WithFields(map[string]interface{}{
			"id":     res.GetID(),
			"reason": "invalid",
		})
		ret, err := p.factory.ReleaseInValid(res)
		if err != nil {
			lg.Warnf("Release invalid resource %v failed, %v", res, err)
			continue
		}

		p.lock()
		if _, stillInvalid := p.invalid[id]; !stillInvalid {
			// The lock was dropped for the factory call, so something else
			// took this id out of invalid in the meantime.
			p.unlock()
			continue
		}
		delete(p.invalid, id)
		if ret != nil {
			// Already removed from invalid above, so this only re-checks the
			// other two holders. If one of them already has it, the resource
			// just stopped being counted twice.
			if !p.addAvailableLocked(ret) {
				p.metricTotal.Dec()
			}
			p.unlock()
			lg.Infof("Release invalid resource succeed and get new one: %s", ret.GetID())
		} else {
			p.metricTotal.Dec()
			p.productTicket()
			p.unlock()
			lg.Infof("Release invalid resource succeed")
		}
	}
}

func (p *poolImpl) GetSnapshot() (ResourcePoolSnapshot, error) {
	p.lock()
	defer p.unlock()

	pool := map[string]types.NetResourceStatus{}
	meta := map[string]types.NetResourceStatus{}
	for id, item := range p.invalid {
		pool[id] = &types.NetResourceSnapshot{
			ResourceItem: item.res.GetResourceItem(),
			Status:       types.ResStatusInvalid,
		}
	}
	for id, item := range p.inUse {
		pool[id] = &types.NetResourceSnapshot{
			Owner:        item.owner,
			ResourceItem: item.res.GetResourceItem(),
			Status:       types.ResStatusInUse,
		}
	}

	for _, item := range p.available.innerQueue {
		res := item.res
		pool[res.GetID()] = &types.NetResourceSnapshot{
			ResourceItem: res.GetResourceItem(),
			Status:       types.ResStatusAvailable,
		}
	}

	list, err := p.factory.List()
	if err != nil {
		return nil, err
	}
	for _, item := range list[types.ResStatusNormal] {
		status := types.ResStatusNotAdded
		owner := ""
		if poolRes, exist := pool[item.GetID()]; exist {
			status = poolRes.GetStatus()
			owner = poolRes.GetOwner()
		}
		meta[item.GetID()] = &types.NetResourceSnapshot{
			Owner:        owner,
			ResourceItem: item.GetResourceItem(),
			Status:       status,
		}
	}

	for _, item := range list[types.ResStatusInvalid] {
		owner := ""
		if poolRes, exist := pool[item.GetID()]; exist {
			owner = poolRes.GetOwner()
		}
		meta[item.GetID()] = &types.NetResourceSnapshot{
			ResourceItem: item.GetResourceItem(),
			Status:       types.ResStatusInvalid,
			Owner:        owner,
		}
	}

	for _, item := range list[types.ResStatusLegacy] {
		owner := ""
		if poolRes, exist := pool[item.GetID()]; exist {
			owner = poolRes.GetOwner()
		}
		meta[item.GetID()] = &types.NetResourceSnapshot{
			ResourceItem: item.GetResourceItem(),
			Status:       types.ResStatusLegacy,
			Owner:        owner,
		}
	}

	return &ResourcePoolSnapshotCache{
		Pool: pool,
		Meta: meta,
	}, nil
}

func (p *poolImpl) worker() {
	cycle := make(chan struct{})
	go wait.JitterUntil(func() {
		cycle <- struct{}{}
	}, p.getMonitorInterval(), 0.2, true, wait.NeverStop)

	for {
		// HandleCrash logs and then re-panics (ReallyCrash defaults to true), so
		// this does not keep the worker alive -- it makes sure a panic in here
		// is attributable before the DaemonSet restarts the pod.
		func() {
			defer runtime.HandleCrash(func(r interface{}) {
				p.Errorf("Pool worker panicked: %v", r)
			})
			select {
			case <-p.scale:
				p.blockPause()
				defer p.unblockPause()
				p.tryIncreasePool()
			case <-cycle:
				p.blockPause()
				defer p.unblockPause()
				p.checkInvalid()
				p.tryIncreasePool()
				p.tryReducePool()
			}
		}()
	}
}

func (p *poolImpl) init() error {
	p.lock()
	defer p.unlock()

	p.resetTicketWithLocked()
	return nil
}

// GC rebuilds the pool from the factory's view of the node.
//
// pause() is held for the whole call, so every Allocate and Release blocks
// until it returns, and factory.GC below talks to the platform. A slow GC
// therefore looks like an outage to pods that are starting up. Do not add
// network calls to this path.
func (p *poolImpl) GC(getAllocatedResMap func() (map[string]types.NetResourceAllocated, error)) error {
	p.pause()
	defer p.unpause()
	usedResource, err := getAllocatedResMap()
	if err != nil {
		return err
	}
	p.Debugf("GC start, usedResource: %v", usedResource)
	err = p.factory.GC()
	if err != nil {
		return fmt.Errorf("factory gc failed, %v", err)
	}

	list, err := p.factory.List()
	if err != nil {
		return fmt.Errorf("factory list failed, %v", err)
	}

	p.WithFields(zlog.Fields{"phase": "before gc"}).Debugf("Factory items: %v", list)
	p.WithFields(zlog.Fields{"phase": "before gc"}).Debugf("Inuse items in pool: %v", p.inUse)
	p.WithFields(zlog.Fields{"phase": "before gc"}).Debugf("Available items in pool: %v", p.available.Dump())
	p.WithFields(zlog.Fields{"phase": "before gc"}).Debugf("Invalid items in pool: %v", p.invalid)
	for id, item := range p.inUse {
		if local, exist := usedResource[id]; !exist {
			// usedResource comes from the stored records only, so a pod whose
			// record was lost while it kept running does not appear in it. Ask
			// whether the owner is still there before taking its address away:
			// PreStart already adopts such an address from the API server, and
			// GC handing it straight back is the two paths disagreeing about
			// what counts as in use. Measured before this check: the first GC
			// after a lost record store moved three live pods' addresses to
			// available, and the pool monitor unassigned all three from the ENI
			// 74 seconds later.
			if p.ownerAlive != nil && p.ownerAlive(item.owner) {
				p.Warnf("Resource %v has no stored record but its owner %s is still running; keeping it in use",
					item.res.GetResourceItem(), item.owner)
				continue
			}
			if time.Since(item.lastUse) > p.getGcProtectPeriod() {
				p.Warnf("Release %v which used by %s once", item.res.GetResourceItem(), item.owner)
				delete(p.inUse, id)
			}
		} else {
			if local.Owner == item.owner {
				delete(usedResource, id)
			}
		}
	}
	for id, res := range usedResource {
		for _, resMap := range list {
			if item, exist := resMap[id]; exist {
				p.inUse[id] = poolItem{
					owner: res.Owner,
					res:   item,
					// Without a lastUse the zero time makes time.Since huge,
					// so the very next GC would skip the protect period.
					lastUse: time.Now(),
				}
			}
		}
	}

	p.invalid = map[string]poolItem{}
	for id, item := range list[types.ResStatusInvalid] {
		if _, exist := p.inUse[id]; !exist {
			p.invalid[id] = poolItem{
				res: item,
			}
		}
	}
	for id, item := range list[types.ResStatusLegacy] {
		if _, exist := p.inUse[id]; !exist {
			p.invalid[id] = poolItem{
				res: item,
			}
		}
	}

	oldAvailable := p.available.Dump()
	p.available = newPriorityQueue()
	for id, item := range list[types.ResStatusNormal] {
		if _, exist := p.inUse[id]; !exist {
			poolRes := poolItem{
				res: item,
			}
			if oldItem, ok := oldAvailable[id]; ok {
				poolRes.reserveBefore = oldItem.reserveBefore
			}
			p.available.Push(&poolRes)
		}
	}

	p.metricAvailable.Set(float64(p.available.Size()))
	p.metricTotal.Set(float64(p.capWithLocked()))
	p.resetTicketWithLocked()

	p.WithFields(zlog.Fields{"phase": "after gc"}).Debugf("Inuse items in pool: %v", p.inUse)
	p.WithFields(zlog.Fields{"phase": "after gc"}).Debugf("Available items in pool: %v", p.available.Dump())
	p.WithFields(zlog.Fields{"phase": "after gc"}).Debugf("Invalid items in pool: %v", p.invalid)
	return nil
}

func (p *poolImpl) ReCfgCache(target, targetMin int) {
	p.Infof("ReConfig pool target and targetMin to %d, %d", target, targetMin)
	p.setTarget(target)
	p.setTargetMin(targetMin)
	p.notifyScale()
}

// NewResourcePool builds a ResourcePool from config, runs Config.PreStart if
// set, and starts the background worker that keeps the pool sized.
func NewResourcePool(config Config) (ResourcePool, error) {
	impl := &poolImpl{
		name:            config.Name,
		resType:         config.Type,
		inUse:           make(map[string]poolItem),
		available:       newPriorityQueue(),
		invalid:         make(map[string]poolItem),
		factory:         config.Factory,
		mutex:           sync.Mutex{},
		pauseLock:       sync.RWMutex{},
		backoff:         defaultPoolBackoffPeriod,
		scale:           make(chan struct{}, 1),
		ownerAlive:      config.OwnerAlive,
		poolConfigInner: config.newPoolConfigInner(),
		metricAvailable: metrics.ResourcePoolAvailable.WithLabelValues(config.Name, config.Type),
		metricTotal:     metrics.ResourcePoolTotal.WithLabelValues(config.Name, config.Type),
		Logger:          log.WithFields(zlog.Fields{"pool": config.Name, "factory": config.Factory.Name()}),
	}
	maxCp := impl.getMaxCap()
	impl.ticket = make(chan struct{}, maxCp)
	impl.metricTarget.Set(float64(impl.target))
	impl.metricTargetMin.Set(float64(impl.targetMin))

	if config.PreStart != nil {
		err := config.PreStart(impl)
		if err != nil {
			return nil, err
		}
	}

	err := impl.init()
	if err != nil {
		return nil, fmt.Errorf("resource pool %s init failed: %v", config.Name, err)
	}
	go impl.worker()
	log.WithFields(zlog.Fields{
		"Type":            config.Type,
		"MaxCap":          config.MaxCap,
		"Target":          config.Target,
		"TargetMin":       config.TargetMin,
		"MonitorInterval": config.MonitorInterval,
	}).Infof("Resource pool %s start", config.Name)
	return impl, nil
}
