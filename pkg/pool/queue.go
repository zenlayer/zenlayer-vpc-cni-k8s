// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package pool

import "container/heap"

type queue []*poolItem

func (q *queue) Len() int {
	return len(*q)
}

func (q *queue) Less(i, j int) bool {
	origin := *q
	return origin[i].reserveBefore.Before(origin[j].reserveBefore)
}

func (q *queue) Swap(i, j int) {
	origin := *q
	origin[i], origin[j] = origin[j], origin[i]
}

// Push is heap.Interface's, and heap.Push sifts Len()-1 right after calling it,
// so it has to append: dropping the value would leave heap.Push sifting an
// element it did not push. The type assertion is the one path that does not
// append, and priorityQueue.Push -- the only caller -- always passes a
// *poolItem. Deduplication lives there rather than here, because there it can
// re-sift the entry it touched.
func (q *queue) Push(x any) {
	item, ok := x.(*poolItem)
	if !ok {
		return
	}
	*q = append(*q, item)
}

func (q *queue) Pop() any {
	old := *q
	l := len(old)
	if l == 0 {
		return nil
	}
	item := old[l-1]
	*q = old[0 : l-1]
	return item
}

type priorityQueue struct {
	innerQueue queue
}

// PopPrefer pops the item with the given id, falling back to the head of the
// queue when the id is empty or no longer in the pool. prefer is only a hint
// (reuse the resource this pod had last time), so a stale hint must not fail an
// allocation the pool could otherwise serve.
func (q *priorityQueue) PopPrefer(id string) *poolItem {
	if id == "" {
		return q.Pop()
	}
	for i := 0; i < q.Size(); i++ {
		if q.innerQueue[i].res.GetID() == id {
			// heap.Remove handles i == Len()-1, which hand-rolled
			// swap-truncate-Fix does not: Fix would be handed an index that
			// the truncation just made out of range.
			// The heap holds *poolItem and nothing else, so this cannot fail
			// unless queue's element type changes -- which the compiler would
			// catch on the Push side. Deliberately not the two-value form: a
			// nil return here would silently drop a pooled resource instead of
			// reporting the programming error.
			//nolint:errcheck // see above
			return heap.Remove(&q.innerQueue, i).(*poolItem)
		}
	}
	return q.Pop()
}

// PopPreferWhere is PopPrefer restricted to entries ok accepts: it skips the
// rest, leaving them queued, and returns nil when none is acceptable. Without
// a usable prefer it takes the entry before puts first, the oldest on a tie.
// A nil ok accepts every entry; a nil before orders by age alone.
func (q *priorityQueue) PopPreferWhere(id string, ok func(*poolItem) bool, before func(a, b *poolItem) bool) *poolItem {
	if ok == nil && before == nil {
		return q.PopPrefer(id)
	}
	best := -1
	for i := 0; i < q.Size(); i++ {
		it := q.innerQueue[i]
		if ok != nil && !ok(it) {
			continue
		}
		if id != "" && it.res.GetID() == id {
			best = i
			break
		}
		if best < 0 {
			best = i
			continue
		}
		b := q.innerQueue[best]
		switch {
		case before != nil && before(it, b):
			best = i
		case before != nil && before(b, it):
		case it.reserveBefore.Before(b.reserveBefore):
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	//nolint:errcheck // the heap holds *poolItem; see Remove
	return heap.Remove(&q.innerQueue, best).(*poolItem)
}

// Remove drops the entry with the given id and returns it, or nil when the
// queue does not hold it.
func (q *priorityQueue) Remove(id string) *poolItem {
	for i := 0; i < q.Size(); i++ {
		if q.innerQueue[i].res.GetID() == id {
			// The heap holds *poolItem and nothing else, so this cannot fail
			// unless queue's element type changes -- which the compiler would
			// catch on the Push side. Deliberately not the two-value form: a
			// nil return here would silently drop a pooled resource instead of
			// reporting the programming error.
			//nolint:errcheck // see above
			return heap.Remove(&q.innerQueue, i).(*poolItem)
		}
	}
	return nil
}

func (q *priorityQueue) Peek() *poolItem {
	if q.Size() == 0 {
		return nil
	}
	item := q.innerQueue[0]
	return item
}

func (q *priorityQueue) Pop() *poolItem {
	if q.Size() == 0 {
		return nil
	}
	item := heap.Pop(&q.innerQueue)
	//nolint:errcheck // the heap holds *poolItem; see Remove
	return item.(*poolItem)
}

// Push queues item, or refreshes the deadline of the entry already holding its
// id. One entry per id: two would let two pods pop the same address.
//
// heap.Fix, because reserveBefore is the field Less orders on: writing it in
// place leaves the entry at the position its old deadline earned, which breaks
// the heap invariant, so Peek and Pop stop answering with the earliest deadline.
func (q *priorityQueue) Push(item *poolItem) {
	for i := range q.innerQueue {
		if q.innerQueue[i].res.GetID() == item.res.GetID() {
			q.innerQueue[i].reserveBefore = item.reserveBefore
			heap.Fix(&q.innerQueue, i)
			return
		}
	}
	heap.Push(&q.innerQueue, item)
}

func (q *priorityQueue) Size() int {
	if q == nil {
		return 0
	}
	return len(q.innerQueue)
}

func (q *priorityQueue) Find(id string) *poolItem {
	for i := 0; i < q.Size(); i++ {
		if q.innerQueue[i].res.GetID() == id {
			return q.innerQueue[i]
		}
	}
	return nil
}

func (q *priorityQueue) Dump() map[string]*poolItem {
	ret := map[string]*poolItem{}
	for i := 0; i < q.Size(); i++ {
		item := q.innerQueue[i]
		ret[item.res.GetID()] = item
	}
	return ret
}

func newPriorityQueue() *priorityQueue {
	q := &priorityQueue{
		innerQueue: queue{},
	}
	heap.Init(&q.innerQueue)
	return q
}
