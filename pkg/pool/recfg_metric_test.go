// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package pool

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A hot scaler change must move the target gauges too; they were set only at
// construction, so the metric kept the start-up value forever.
func TestReCfgCacheUpdatesTargetGauges(t *testing.T) {
	c := Config{Name: "recfg-metric", Type: "test", Target: 2, TargetMin: 1, MaxCap: 10}
	p := &poolImpl{poolConfigInner: c.newPoolConfigInner(), scale: make(chan struct{}, 1), Logger: log}

	p.ReCfgCache(5, 3)

	if got := testutil.ToFloat64(p.metricTarget); got != 5 {
		t.Fatalf("target gauge = %v, want 5", got)
	}
	if got := testutil.ToFloat64(p.metricTargetMin); got != 3 {
		t.Fatalf("targetMin gauge = %v, want 3", got)
	}
}
