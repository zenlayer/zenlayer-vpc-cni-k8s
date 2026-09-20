// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package pool

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func newTestPool() *poolImpl {
	return &poolImpl{
		name:            "test",
		inUse:           map[string]poolItem{},
		available:       newPriorityQueue(),
		invalid:         map[string]poolItem{},
		metricAvailable: prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_available"}),
		metricTotal:     prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_total"}),
		Logger:          log,
	}
}

// countAvailable counts the entries the queue physically holds for id. Each one
// is popped separately, so two entries hand the same address to two pods.
func countAvailable(p *poolImpl, id string) int {
	n := 0
	for i := 0; i < p.available.Size(); i++ {
		if p.available.innerQueue[i].res.GetID() == id {
			n++
		}
	}
	return n
}

// TestPoolHoldsOneEntryPerResource guards the worst failure this pool can
// produce: handing one address to two pods. Several callers can arrive with a
// resource the pool already holds: PreStart adoption, pool scale-up,
// create-on-demand, and the paths that hand a resource back after a failed
// release. priorityQueue.Push only dedupes within the queue, so the checks
// across the three holders live in AddAvailable and AddInuse; this pins that a
// resource ends up in exactly one of available, inUse and invalid.
func TestPoolHoldsOneEntryPerResource(t *testing.T) {
	t.Run("available twice", func(t *testing.T) {
		p := newTestPool()
		res := &fakeRes{id: "eni-1/10.0.0.5"}

		p.AddAvailable(res)
		p.AddAvailable(res)

		if got := countAvailable(p, res.id); got != 1 {
			t.Errorf("queue holds %d entries for %s, want 1", got, res.id)
		}
		if got := p.available.Size(); got != 1 {
			t.Errorf("available size = %d, want 1", got)
		}
	})

	t.Run("available while already in use", func(t *testing.T) {
		p := newTestPool()
		res := &fakeRes{id: "eni-1/10.0.0.5"}

		p.AddInuse(res, "default/pod-a")
		p.AddAvailable(res)

		if got := p.available.Size(); got != 0 {
			t.Errorf("available size = %d, want 0: the address belongs to pod-a", got)
		}
		if _, held := p.inUse[res.id]; !held {
			t.Error("resource left inUse")
		}
	})

	t.Run("available while held as invalid", func(t *testing.T) {
		p := newTestPool()
		res := &fakeRes{id: "eni-1/10.0.0.5"}
		p.invalid[res.id] = poolItem{res: res}

		p.AddAvailable(res)

		if got := p.available.Size(); got != 0 {
			t.Errorf("available size = %d, want 0: the address is pending release", got)
		}
	})

	// The create-on-demand path hands a freshly created resource straight to a
	// pod without going through the queue. If reconcile had already adopted
	// that address, the stale queue entry would be popped by the next pod.
	t.Run("in use evicts a stale available entry", func(t *testing.T) {
		p := newTestPool()
		res := &fakeRes{id: "eni-1/10.0.0.5"}

		p.AddAvailable(res)
		p.AddInuse(res, "default/pod-a")

		if got := p.available.Size(); got != 0 {
			t.Fatalf("available size = %d, want 0; a second pod could still pop %s", got, res.id)
		}
		item, held := p.inUse[res.id]
		if !held {
			t.Fatal("resource is not inUse")
		}
		if item.owner != "default/pod-a" {
			t.Errorf("owner = %q, want %q", item.owner, "default/pod-a")
		}
	})

	t.Run("re-adding an in-use resource keeps one owner", func(t *testing.T) {
		p := newTestPool()
		res := &fakeRes{id: "eni-1/10.0.0.5"}

		p.AddInuse(res, "default/pod-a")
		p.AddInuse(res, "default/pod-b")

		if got := len(p.inUse); got != 1 {
			t.Errorf("inUse holds %d entries, want 1", got)
		}
		if got := p.inUse[res.id].owner; got != "default/pod-b" {
			t.Errorf("owner = %q, want the later claim %q", got, "default/pod-b")
		}
	})

	// Distinct resources must still coexist -- the guard is about identity, not
	// about capping the pool.
	t.Run("distinct resources all queue", func(t *testing.T) {
		p := newTestPool()
		for _, id := range []string{"eni-1/10.0.0.5", "eni-1/10.0.0.6", "eni-2/10.0.0.7"} {
			p.AddAvailable(&fakeRes{id: id})
		}
		if got := p.available.Size(); got != 3 {
			t.Errorf("available size = %d, want 3", got)
		}
	})
}

func TestQueueRemove(t *testing.T) {
	q, _ := newQueueWith(t, "a", "b", "c")

	if item := q.Remove("b"); item == nil || item.res.GetID() != "b" {
		t.Fatalf("Remove(\"b\") = %v, want the item with id b", item)
	}
	if got := q.Size(); got != 2 {
		t.Errorf("size after Remove = %d, want 2", got)
	}
	if q.Find("b") != nil {
		t.Error("b is still in the queue")
	}
	for _, id := range []string{"a", "c"} {
		if q.Find(id) == nil {
			t.Errorf("Remove dropped %q as well", id)
		}
	}
	if item := q.Remove("missing"); item != nil {
		t.Errorf("Remove of an absent id returned %v, want nil", item)
	}

	// The last slot in the backing slice is the index heap.Fix cannot be handed
	// after a truncation; heap.Remove handles it, a hand-rolled version does not.
	q2, _ := newQueueWith(t, "x", "y", "z")
	last := q2.innerQueue[q2.Size()-1].res.GetID()
	if item := q2.Remove(last); item == nil || item.res.GetID() != last {
		t.Fatalf("Remove(%q) = %v, want that item", last, item)
	}
	if got := q2.Size(); got != 2 {
		t.Errorf("size after removing the last slot = %d, want 2", got)
	}
}
