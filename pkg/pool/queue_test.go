// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package pool

import (
	"testing"
	"time"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

type fakeRes struct{ id string }

func (f *fakeRes) GetID() string                       { return f.id }
func (f *fakeRes) GetType() string                     { return types.NetResourceTypeEniIp }
func (f *fakeRes) GetResourceItem() types.ResourceItem { return types.ResourceItem{ID: f.id} }

func newQueueWith(t *testing.T, ids ...string) (*priorityQueue, time.Time) {
	t.Helper()
	q := newPriorityQueue()
	base := time.Now()
	for i, id := range ids {
		q.Push(&poolItem{res: &fakeRes{id: id}, reserveBefore: base.Add(time.Duration(i) * time.Second)})
	}
	return q, base
}

// Popping the item that happens to sit last in the backing slice used to hand
// heap.Fix an index the truncation had just invalidated.
func TestPopPreferLastElementDoesNotPanic(t *testing.T) {
	q, _ := newQueueWith(t, "a", "b", "c")
	last := q.innerQueue[q.Size()-1].res.GetID()

	item := q.PopPrefer(last)
	if item == nil {
		t.Fatalf("PopPrefer(%q) returned nil", last)
	}
	if item.res.GetID() != last {
		t.Fatalf("PopPrefer(%q) returned %q", last, item.res.GetID())
	}
	if q.Size() != 2 {
		t.Fatalf("size after pop = %d, want 2", q.Size())
	}
	if q.Find(last) != nil {
		t.Fatalf("%q still in queue after pop", last)
	}
}

// Every position must be poppable, not just the ones that leave the heap intact.
func TestPopPreferEveryPosition(t *testing.T) {
	for _, target := range []string{"a", "b", "c", "d", "e"} {
		q, _ := newQueueWith(t, "a", "b", "c", "d", "e")
		item := q.PopPrefer(target)
		if item == nil || item.res.GetID() != target {
			t.Fatalf("PopPrefer(%q) failed", target)
		}
		if q.Size() != 4 || q.Find(target) != nil {
			t.Fatalf("queue inconsistent after popping %q: size=%d", target, q.Size())
		}
		// remaining items must still come out in reserveBefore order
		var prev time.Time
		for q.Size() > 0 {
			got := q.Pop()
			if !prev.IsZero() && got.reserveBefore.Before(prev) {
				t.Fatalf("heap order broken after popping %q", target)
			}
			prev = got.reserveBefore
		}
	}
}

// A stale prefer hint must not fail an allocation the pool can still serve.
func TestPopPreferFallsBackWhenMissing(t *testing.T) {
	q, _ := newQueueWith(t, "a", "b")
	item := q.PopPrefer("gone")
	if item == nil {
		t.Fatal("PopPrefer with unknown id returned nil, expected fallback to head")
	}
	if item.res.GetID() != "a" {
		t.Fatalf("fallback returned %q, want head %q", item.res.GetID(), "a")
	}
}

func TestPopPreferEmptyQueue(t *testing.T) {
	q := newPriorityQueue()
	if item := q.PopPrefer("anything"); item != nil {
		t.Fatalf("PopPrefer on empty queue returned %v", item)
	}
}

// assertHeapOrdered walks the whole heap and fails on the first parent that
// sorts after one of its children.
//
// Peek and Pop only ever look at the root, so a violation deeper in the tree is
// invisible until the element that should have surfaced is skipped over. A
// whole-tree check is what makes the defect legible instead of intermittent.
func assertHeapOrdered(t *testing.T, q *priorityQueue) {
	t.Helper()
	for i := 1; i < q.Size(); i++ {
		parent := (i - 1) / 2
		if q.innerQueue[i].reserveBefore.Before(q.innerQueue[parent].reserveBefore) {
			t.Fatalf("heap invariant broken: child %d (%s, reserveBefore %v) sorts before "+
				"parent %d (%s, reserveBefore %v)",
				i, q.innerQueue[i].res.GetID(), q.innerQueue[i].reserveBefore,
				parent, q.innerQueue[parent].res.GetID(), q.innerQueue[parent].reserveBefore)
		}
	}
}

// TestPushRefreshingADeadlineKeepsTheHeapOrdered pins the repush path.
//
// Pushing an id the queue already holds refreshes that entry's deadline in
// place rather than adding a second entry -- two entries for one id would let
// two pods pop the same address. But mutating reserveBefore is exactly the
// field the heap orders on, so the entry has to be re-sifted afterwards.
//
// heap.Push cannot do it: it calls Push and then sifts index Len()-1, which on
// the refresh path is some unrelated element that never moved. The refreshed
// entry keeps whatever position its old deadline had earned, and from then on
// Peek and Pop answer from a tree that is no longer a heap.
//
// Three entries, not two, and the refreshed one is the head rather than the
// tail: otherwise the stray up(Len()-1) happens to sift the very element that
// needed moving and the queue comes out ordered by accident. The defect only
// shows when the refreshed entry has to sink, which sifting another element
// upward cannot do for it.
func TestPushRefreshingADeadlineKeepsTheHeapOrdered(t *testing.T) {
	base := time.Now()
	q := newPriorityQueue()
	for i, id := range []string{"a", "b", "c"} {
		q.Push(&poolItem{res: &fakeRes{id: id}, reserveBefore: base.Add(time.Duration(i+1) * time.Minute)})
	}

	// "a" is reserved far longer than the rest. It must give up the head.
	q.Push(&poolItem{res: &fakeRes{id: "a"}, reserveBefore: base.Add(100 * time.Minute)})

	if q.Size() != 3 {
		t.Fatalf("size = %d, want 3: the repush added a second entry for one id", q.Size())
	}
	if got := q.Find("a"); got == nil || !got.reserveBefore.Equal(base.Add(100*time.Minute)) {
		t.Fatalf("the repush did not refresh a's deadline")
	}
	assertHeapOrdered(t, q)
	if head := q.Peek(); head.res.GetID() != "b" {
		t.Errorf("Peek() = %q, want \"b\": a is now reserved far longer than the rest "+
			"and must not stay at the head", head.res.GetID())
	}
	if first := q.Pop(); first.res.GetID() != "b" {
		t.Errorf("Pop() = %q, want \"b\"", first.res.GetID())
	}
}

// TestPushRefreshingToAnEarlierDeadlineAlsoResifts is the other direction.
//
// A refresh can shorten a reservation as well as extend it, and that entry has
// to be able to move up. Only testing the extend direction would leave a fix
// that sifts one way looking correct.
func TestPushRefreshingToAnEarlierDeadlineAlsoResifts(t *testing.T) {
	base := time.Now()
	q := newPriorityQueue()
	for i, id := range []string{"a", "b", "c", "d"} {
		q.Push(&poolItem{res: &fakeRes{id: id}, reserveBefore: base.Add(time.Duration(i+1) * time.Minute)})
	}

	// "d" was the last to free up; now it is the first.
	q.Push(&poolItem{res: &fakeRes{id: "d"}, reserveBefore: base})

	assertHeapOrdered(t, q)
	if head := q.Peek(); head.res.GetID() != "d" {
		t.Errorf("Peek() = %q, want \"d\": its deadline is now the earliest in the queue",
			head.res.GetID())
	}
}
