// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package pool

import (
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

var _ types.Grouped = (*types.ENIIP)(nil)

func eniIP(eni string, primary bool, ip string) *types.ENIIP {
	return &types.ENIIP{
		ENI:   &types.ENI{ID: eni, IsPrimary: primary},
		IPSet: types.IPSet{IPv4: net.ParseIP(ip)},
	}
}

func shrinkPool(target int) *poolImpl {
	p := newTestPool()
	p.poolConfigInner = (&Config{Name: "shrink", Type: "test", Target: target, MaxCap: 20}).newPoolConfigInner()
	return p
}

// queueAt queues res as available, aged so that it is past its reservation.
func queueAt(p *poolImpl, res types.NetResource, age time.Duration) {
	p.available.Push(&poolItem{res: res, reserveBefore: time.Now().Add(-age)})
}

func inUse(p *poolImpl, res types.NetResource) {
	p.inUse[res.GetID()] = poolItem{res: res}
}

// mature marks groups as seen long enough ago to be drained.
func mature(p *poolImpl, groups ...string) {
	p.groupSeen = map[string]time.Time{}
	for _, g := range groups {
		p.groupSeen[g] = time.Now().Add(-2 * minDrainAge)
	}
}

func popIDs(t *testing.T, p *poolImpl, n int) []string {
	t.Helper()
	var got []string
	for range n {
		it := p.popOverflow(nil)
		if it == nil {
			t.Fatalf("popOverflow returned nil after %v", got)
		}
		got = append(got, it.res.GetID())
	}
	return got
}

func wantIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// ENI-05: a secondary with nothing in use whose idle addresses fit within the
// overflow is drained whole, even though the oldest idle address is on the
// primary, so the interface can be released.
func TestShrinkDrainsIdleSecondary(t *testing.T) {
	p := shrinkPool(1)
	inUse(p, eniIP("eni-p", true, "10.0.0.2"))
	queueAt(p, eniIP("eni-p", true, "10.0.0.3"), 3*time.Minute)
	queueAt(p, eniIP("eni-a", false, "10.0.0.11"), time.Minute)
	queueAt(p, eniIP("eni-a", false, "10.0.0.10"), 2*time.Minute)
	mature(p, "eni-a")

	wantIDs(t, popIDs(t, p, 2), "eni-a/10.0.0.10", "eni-a/10.0.0.11")
	if it := p.popOverflow(nil); it != nil {
		t.Fatalf("popped %s at target", it.res.GetID())
	}
}

// Review finding: at the target boundary a pod start grows the pool onto a new
// interface; draining that young interface on the next delete would free it
// and the next start would create it again. It is left to oldest-first.
func TestShrinkSparesYoungGroup(t *testing.T) {
	p := shrinkPool(1)
	inUse(p, eniIP("eni-b", false, "10.0.0.20"))
	queueAt(p, eniIP("eni-b", false, "10.0.0.21"), 3*time.Minute)
	queueAt(p, eniIP("eni-c", false, "10.0.0.30"), time.Second)
	p.groupSeen = map[string]time.Time{"eni-b": time.Now().Add(-2 * minDrainAge)}

	wantIDs(t, popIDs(t, p, 1), "eni-b/10.0.0.21")
}

// A group still serving a pod, or one that cannot be emptied within the
// overflow, is not drained whole; the oldest secondary address goes, and the
// primary's -- older still -- stays.
func TestShrinkSecondaryBeforePrimaryWhenNoGroupEmpties(t *testing.T) {
	p := shrinkPool(3) // 5 idle, over = 2: eni-b's 3 do not fit
	inUse(p, eniIP("eni-a", false, "10.0.0.10"))
	queueAt(p, eniIP("eni-a", false, "10.0.0.11"), 2*time.Minute)
	queueAt(p, eniIP("eni-b", false, "10.0.0.20"), time.Minute)
	queueAt(p, eniIP("eni-b", false, "10.0.0.21"), time.Minute)
	queueAt(p, eniIP("eni-b", false, "10.0.0.22"), time.Minute)
	queueAt(p, eniIP("eni-p", true, "10.0.0.3"), 5*time.Minute)
	mature(p, "eni-a", "eni-b")

	wantIDs(t, popIDs(t, p, 1), "eni-a/10.0.0.11")
}

// Measured on a crd node: after a scale-down within minDrainAge of a secondary
// interface appearing, oldest-first gave back every idle address on the
// primary and left the warm ones on the young secondary, which then never
// emptied. The primary's addresses must go last, even when they are older.
func TestShrinkKeepsPrimaryOverYoungSecondary(t *testing.T) {
	p := shrinkPool(2)
	for i, ip := range []string{"10.0.0.3", "10.0.0.4", "10.0.0.5"} {
		queueAt(p, eniIP("eni-p", true, ip), time.Duration(10-i)*time.Minute)
	}
	queueAt(p, eniIP("eni-y", false, "10.0.0.33"), 2*time.Minute)
	queueAt(p, eniIP("eni-y", false, "10.0.0.34"), time.Minute)
	p.groupSeen = map[string]time.Time{"eni-y": time.Now()}

	// 5 idle, target 2: the young secondary's two go, then the oldest primary.
	wantIDs(t, popIDs(t, p, 3), "eni-y/10.0.0.33", "eni-y/10.0.0.34", "eni-p/10.0.0.3")
	if it := p.popOverflow(nil); it != nil {
		t.Fatalf("popped %s at target", it.res.GetID())
	}
}

// Among secondaries that cannot be drained whole, an interface older than
// minDrainAge gives back before a young one.
func TestShrinkMatureSecondaryBeforeYoung(t *testing.T) {
	p := shrinkPool(1)
	inUse(p, eniIP("eni-m", false, "10.0.0.20"))
	queueAt(p, eniIP("eni-m", false, "10.0.0.21"), time.Minute)
	queueAt(p, eniIP("eni-y", false, "10.0.0.30"), 5*time.Minute)
	inUse(p, eniIP("eni-y", false, "10.0.0.31"))
	p.groupSeen = map[string]time.Time{"eni-m": time.Now().Add(-2 * minDrainAge), "eni-y": time.Now()}

	wantIDs(t, popIDs(t, p, 1), "eni-m/10.0.0.21")
}

// Between drainable groups the one with fewer idle addresses goes first.
func TestShrinkSmallestGroupFirst(t *testing.T) {
	p := shrinkPool(0)
	queueAt(p, eniIP("eni-a", false, "10.0.0.10"), 5*time.Minute)
	queueAt(p, eniIP("eni-a", false, "10.0.0.11"), 5*time.Minute)
	queueAt(p, eniIP("eni-b", false, "10.0.0.20"), time.Minute)
	mature(p, "eni-a", "eni-b")

	wantIDs(t, popIDs(t, p, 1), "eni-b/10.0.0.20")
}

// Resources with no group keep the oldest-first order.
func TestShrinkUngroupedOldestFirst(t *testing.T) {
	p := shrinkPool(0)
	queueAt(p, eniIP("eni-p", true, "10.0.0.4"), time.Minute)
	queueAt(p, eniIP("eni-p", true, "10.0.0.3"), 2*time.Minute)
	wantIDs(t, popIDs(t, p, 1), "eni-p/10.0.0.3")
}

// Review finding: the drain order is deterministic, so a resource whose release
// failed is skipped for the rest of the run instead of taking every attempt.
func TestShrinkSkipsFailedInRun(t *testing.T) {
	p := shrinkPool(0)
	queueAt(p, eniIP("eni-a", false, "10.0.0.10"), time.Minute)
	queueAt(p, eniIP("eni-p", true, "10.0.0.3"), 2*time.Minute)
	mature(p, "eni-a")
	it := p.popOverflow(map[string]bool{"eni-a/10.0.0.10": true})
	if it == nil || it.res.GetID() != "eni-p/10.0.0.3" {
		t.Fatalf("popOverflow = %v, want the primary's address", it)
	}
}

// A resource the factory reports unusable is neither handed out, nor shrunk
// away, nor counted as warm.
func TestUnusableResourceSkipped(t *testing.T) {
	p := shrinkPool(1)
	p.usable = func(res types.NetResource) bool {
		return res.(*types.ENIIP).ENI.ID != "eni-gone"
	}
	queueAt(p, eniIP("eni-gone", false, "10.0.0.76"), 2*time.Minute)
	queueAt(p, eniIP("eni-gone", false, "10.0.0.77"), 2*time.Minute)
	queueAt(p, eniIP("eni-a", false, "10.0.0.10"), time.Minute)

	// 3 idle but 1 usable: at target, nothing to shrink, nothing to grow.
	if it := p.popOverflow(nil); it != nil {
		t.Fatalf("shrank %s although only 1 idle entry is usable", it.res.GetID())
	}
	if n := p.shouldIncrease(); n != 0 {
		t.Fatalf("shouldIncrease = %d with 1 usable at target 1", n)
	}
	res, err := p.allocateFromPool("eni-gone/10.0.0.76", "pod-a")
	if err != nil || res.GetID() != "eni-a/10.0.0.10" {
		t.Fatalf("allocate = %v, %v; want eni-a/10.0.0.10", res, err)
	}
	if n := p.shouldIncrease(); n != 1 {
		t.Fatalf("shouldIncrease = %d, want 1: the unusable entries are not warm", n)
	}
	if _, err := p.allocateFromPool("", "pod-b"); !errors.Is(err, ErrNoResourceAvailableInPool) {
		t.Fatalf("allocate with only unusable entries: err = %v, want ErrNoResourceAvailableInPool", err)
	}
	if p.available.Size() != 2 {
		t.Fatalf("unusable entries were dropped: available = %d", p.available.Size())
	}
}

// PopPreferWhere honors a usable prefer and falls back to the oldest usable.
func TestPopPreferWhere(t *testing.T) {
	q := newPriorityQueue()
	for _, it := range []struct {
		res types.NetResource
		age time.Duration
	}{
		{eniIP("eni-a", false, "10.0.0.10"), 2 * time.Minute},
		{eniIP("eni-a", false, "10.0.0.11"), time.Minute},
		{eniIP("eni-gone", false, "10.0.0.76"), 3 * time.Minute},
	} {
		q.Push(&poolItem{res: it.res, reserveBefore: time.Now().Add(-it.age)})
	}
	ok := func(it *poolItem) bool { return it.res.(*types.ENIIP).ENI.ID != "eni-gone" }
	if it := q.PopPreferWhere("eni-a/10.0.0.11", ok, nil); it.res.GetID() != "eni-a/10.0.0.11" {
		t.Fatalf("prefer usable: got %s", it.res.GetID())
	}
	if it := q.PopPreferWhere("eni-gone/10.0.0.76", ok, nil); it.res.GetID() != "eni-a/10.0.0.10" {
		t.Fatalf("prefer unusable: got %s, want the oldest usable", it.res.GetID())
	}
	if it := q.PopPreferWhere("", ok, nil); it != nil {
		t.Fatalf("only unusable left: got %s", it.res.GetID())
	}
	if q.Size() != 1 {
		t.Fatalf("unusable entry dropped: size %d", q.Size())
	}
}

// Allocation takes the primary first, then the secondary with the most
// addresses in use, the oldest within a group; a usable prefer still wins.
func TestAllocPacksOntoBusiestInterface(t *testing.T) {
	p := shrinkPool(1)
	inUse(p, eniIP("eni-b", false, "10.0.0.20"))
	inUse(p, eniIP("eni-b", false, "10.0.0.21"))
	inUse(p, eniIP("eni-c", false, "10.0.0.30"))
	queueAt(p, eniIP("eni-c", false, "10.0.0.31"), 5*time.Minute)
	queueAt(p, eniIP("eni-d", false, "10.0.0.40"), 4*time.Minute)
	queueAt(p, eniIP("eni-b", false, "10.0.0.22"), time.Minute)
	queueAt(p, eniIP("eni-b", false, "10.0.0.23"), 2*time.Minute)
	queueAt(p, eniIP("eni-p", true, "10.0.0.2"), time.Second)

	alloc := func(prefer string) string {
		t.Helper()
		res, err := p.allocateFromPool(prefer, "pod")
		if err != nil {
			t.Fatalf("allocate: %v", err)
		}
		return res.GetID()
	}
	if got := alloc("eni-d/10.0.0.40"); got != "eni-d/10.0.0.40" {
		t.Fatalf("prefer: got %s", got)
	}
	// eni-b holds 2 in use, eni-c and eni-d 1 each.
	var got []string
	for range 4 {
		got = append(got, alloc(""))
	}
	wantIDs(t, got, "eni-p/10.0.0.2", "eni-b/10.0.0.23", "eni-b/10.0.0.22", "eni-c/10.0.0.31")
}

// Unusable idle entries are not warm, but they still hold capacity: with the
// cap reached counting them, the pool asks the factory for nothing. Measured
// before: an interface turned Excluded left two idle addresses on it, the pool
// asked for two more on a full node, and the Create waited out its minute.
func TestUnusableEntriesHoldCapacity(t *testing.T) {
	p := shrinkPool(2)
	p.maxCap = 3
	p.usable = func(res types.NetResource) bool {
		return res.(*types.ENIIP).ENI.ID != "eni-excluded"
	}
	queueAt(p, eniIP("eni-excluded", false, "10.0.16.8"), time.Minute)
	queueAt(p, eniIP("eni-excluded", false, "10.0.16.9"), time.Minute)
	inUse(p, eniIP("eni-a", false, "10.0.0.10"))
	if n := p.shouldIncrease(); n != 0 {
		t.Fatalf("shouldIncrease = %d at max cap 3 with 1 in use and 2 unusable idle, want 0", n)
	}
}

// Resources created for blocked Allocates are not surplus: the increase asked
// for target+waiting, and shrinking straight after would give them back before
// the waiters take them.
func TestShrinkLeavesWhatWaitersAreOwed(t *testing.T) {
	p := shrinkPool(1)
	p.waiting = 2
	for i, ip := range []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"} {
		queueAt(p, eniIP("eni-a", false, ip), time.Duration(i+1)*time.Minute)
	}
	if it := p.popOverflow(nil); it != nil {
		t.Fatalf("shrank %s with 3 idle at target 1 and 2 Allocates waiting", it.res.GetID())
	}
	p.waiting = 0
	if it := p.popOverflow(nil); it == nil {
		t.Fatal("nothing shrunk with 3 idle at target 1 and no one waiting")
	}
}

// A cap that shrinks and grows back must not leave more tickets than room:
// each is a create Allocate may start.
func TestTicketsFollowCapBothWays(t *testing.T) {
	p := shrinkPool(1)
	p.ticket = make(chan struct{}, 64)
	p.growTicketsLocked(28)
	p.growTicketsLocked(14)
	p.growTicketsLocked(28)
	if n := len(p.ticket); n != 28 {
		t.Fatalf("%d tickets after 28 -> 14 -> 28, want 28", n)
	}
}

// A shrink that finds tickets already taken (the cap above the new max, or
// creates in flight) records only what it took back, so the regrow does not
// issue more tickets than there is room.
func TestTicketsShrinkCountsOnlyWhatCameBack(t *testing.T) {
	p := shrinkPool(1)
	p.ticket = make(chan struct{}, 64)
	p.growTicketsLocked(28)
	for range 20 { // 20 tickets in use: only 8 spare
		<-p.ticket
	}
	p.growTicketsLocked(14) // takes back the 8 spare, 6 still owed
	if n := len(p.ticket); n != 0 {
		t.Fatalf("%d tickets left after the shrink, want 0", n)
	}
	p.growTicketsLocked(28)
	if n := len(p.ticket); n != 8 {
		t.Fatalf("%d tickets after regrow, want 8: the room that is really free", n)
	}
}

// A GC reset while the pool holds more than the cap counts what it holds:
// the tickets its shrink's destroys hand back are taken back, not left to
// stack on top of a later regrow.
func TestTicketResetOverCapCountsWhatIsHeld(t *testing.T) {
	p := shrinkPool(1)
	p.ticket = make(chan struct{}, 64)
	p.maxCap = 14
	var ids []string
	for i := range 20 {
		res := eniIP("eni-a", false, fmt.Sprintf("10.0.1.%d", i+1))
		inUse(p, res)
		ids = append(ids, res.GetID())
	}
	p.resetTicketWithLocked()
	for _, id := range ids[:6] { // the shrink destroys the 6 over the cap
		delete(p.inUse, id)
		p.productTicket()
	}
	p.growTicketsLocked(14)
	p.maxCap = 28
	p.growTicketsLocked(28)
	if n := len(p.ticket); n != 28-14 {
		t.Fatalf("%d tickets with 14 held under a cap of 28, want %d", n, 28-14)
	}
}
