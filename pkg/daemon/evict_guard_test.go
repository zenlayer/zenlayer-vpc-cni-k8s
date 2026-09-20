// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"testing"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// evictCtx is the minimum netContext judgmentEvictPod touches.
func evictCtx() *netContext {
	return &netContext{
		Context: context.Background(),
		pod:     &zecTypes.Pod{Name: "victim", Namespace: "default"},
		log:     zlog.GetLogger(),
	}
}

// TestJudgmentEvictPodDoesNotEvictWhenCapacityIsUnknown covers a limit of zero
// or less, which means the node could not work out its capacity, not that its
// capacity is zero. Without the guard the comparison is "len(pods) >= 0", true
// even for the first pod on an empty node, so a node that cannot report capacity
// evicts every pod scheduled to it. The only caller passes the pool's max cap,
// which is clamped at zero, so zero is what reaches this in practice; the
// negative cases are defensive.
//
// The nil podPersistenceManager and nil k8s are the tripwire, not an oversight:
// anything past the guard dereferences one of them, so a regression fails loudly
// instead of passing quietly. TestJudgmentEvictPodTripwireIsArmed proves the
// tripwire still bites.
func TestJudgmentEvictPodDoesNotEvictWhenCapacityIsUnknown(t *testing.T) {
	for _, limit := range []int{0, -1, -116} {
		d := &ipamDaemon{}
		if err := d.judgmentEvictPod(evictCtx(), limit); err != nil {
			t.Errorf("judgmentEvictPod(limit=%d) = %v, want nil: an unknown capacity is "+
				"nothing to compare against, so there is nothing to evict for", limit, err)
		}
	}
}

// TestJudgmentEvictPodTripwireIsArmed guards the guard. The test above proves a
// negative -- "it did not evict" -- which a function that no longer does
// anything would also satisfy. With a limit the guard admits, the same daemon
// must reach the persistence manager and panic on it. If this stops panicking,
// the test above has become vacuous and both need rewriting against a real fake.
func TestJudgmentEvictPodTripwireIsArmed(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("judgmentEvictPod(limit=1) returned without touching the nil " +
				"podPersistenceManager; the nil-dependency tripwire no longer proves " +
				"anything, so TestJudgmentEvictPodDoesNotEvictWhenCapacityIsUnknown is vacuous")
		}
	}()
	d := &ipamDaemon{}
	//nolint:errcheck // the panic is the assertion
	_ = d.judgmentEvictPod(evictCtx(), 1)
}
