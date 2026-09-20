// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package pool

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// listFactory is an ObjectFactory whose List reports the resources the platform
// still has, which is what GC rebuilds the available queue from.
type listFactory struct {
	normal   map[string]types.NetResource
	released []string
}

func (f *listFactory) Name() string                            { return "list factory" }
func (f *listFactory) Create(int) ([]types.NetResource, error) { return nil, nil }
func (f *listFactory) Valid(types.NetResource) error           { return nil }
func (f *listFactory) GC() error                               { return nil }
func (f *listFactory) GetResourceLimit() int                   { return 10 }
func (f *listFactory) Release(r types.NetResource) error {
	f.released = append(f.released, r.GetID())
	return nil
}
func (f *listFactory) ReleaseInValid(r types.NetResource) (types.NetResource, error) {
	return r, nil
}
func (f *listFactory) List() (map[types.ResStatus]map[string]types.NetResource, error) {
	return map[types.ResStatus]map[string]types.NetResource{types.ResStatusNormal: f.normal}, nil
}

// gcPool builds a pool holding one in-use resource whose owner is podKey, old
// enough that the protect period has elapsed.
func gcPool(t *testing.T, id, podKey string, ownerAlive func(string) bool) (*poolImpl, *listFactory) {
	t.Helper()
	res := &fakeRes{id: id}
	f := &listFactory{normal: map[string]types.NetResource{id: res}}
	p := newTestPool()
	p.factory = f
	p.ownerAlive = ownerAlive
	p.gcProtectPeriod = time.Minute
	// newTestPool only wires what the Add* paths touch; GC also reads maxCap and
	// the ticket channel.
	p.maxCap = 10
	p.ticket = make(chan struct{}, 10)
	p.metricsMaxCap = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_max_cap"})
	p.inUse[id] = poolItem{owner: podKey, res: res, lastUse: time.Now().Add(-time.Hour)}
	return p, f
}

func noRecords() (map[string]types.NetResourceAllocated, error) {
	return map[string]types.NetResourceAllocated{}, nil
}

// GC decides what is in use from the stored records alone, so an address whose
// record was lost while its pod kept running is invisible to it. Measured
// before this check existed: the first GC after a lost record store moved three
// live pods' addresses to available, and the pool monitor unassigned all three
// from the ENI 74 seconds later -- one of them was then handed to a second pod,
// which took over the traffic of the pod already using it.
func TestGCKeepsResourceWhoseOwnerIsStillAlive(t *testing.T) {
	const id = "eni-1/10.0.0.11"
	p, f := gcPool(t, id, "zldp/dpsrv", func(string) bool { return true })

	if err := p.GC(noRecords); err != nil {
		t.Fatalf("GC() = %v", err)
	}

	if _, held := p.inUse[id]; !held {
		t.Fatal("address of a live pod was dropped from inUse; the pool monitor would unassign it next")
	}
	if p.available.Size() != 0 {
		t.Errorf("available = %d, want 0: the address is still carrying traffic", p.available.Size())
	}
	if len(f.released) != 0 {
		t.Errorf("factory released %v for a live pod", f.released)
	}
}

// The other half: once the pod really is gone, the address has to come back, or
// a lost record store would pin capacity for the life of the node.
func TestGCReclaimsResourceWhoseOwnerIsGone(t *testing.T) {
	const id = "eni-1/10.0.0.11"
	p, _ := gcPool(t, id, "gone/pod", func(string) bool { return false })

	if err := p.GC(noRecords); err != nil {
		t.Fatalf("GC() = %v", err)
	}

	if _, held := p.inUse[id]; held {
		t.Fatal("address of a pod that no longer exists stayed in use")
	}
	if p.available.Size() != 1 {
		t.Errorf("available = %d, want 1", p.available.Size())
	}
}

// A nil hook must leave the previous behavior untouched: the ENI-level pool
// does not set one, and every already-verified reclaim path depends on it.
func TestGCWithoutOwnerAliveHookIsUnchanged(t *testing.T) {
	const id = "eni-1/10.0.0.11"
	p, _ := gcPool(t, id, "any/pod", nil)

	if err := p.GC(noRecords); err != nil {
		t.Fatalf("GC() = %v", err)
	}

	if _, held := p.inUse[id]; held {
		t.Fatal("with no hook the record-based decision must still apply")
	}
}

// The protect period still governs a freshly handed-out resource, so a pod
// whose ADD is mid-flight -- record not yet written, pod not yet in the
// informer -- is not reclaimed underneath it.
func TestGCProtectPeriodStillApplies(t *testing.T) {
	const id = "eni-1/10.0.0.11"
	p, _ := gcPool(t, id, "fresh/pod", func(string) bool { return false })
	item := p.inUse[id]
	item.lastUse = time.Now()
	p.inUse[id] = item

	if err := p.GC(noRecords); err != nil {
		t.Fatalf("GC() = %v", err)
	}

	if _, held := p.inUse[id]; !held {
		t.Fatal("a resource handed out seconds ago was reclaimed inside the protect period")
	}
}
