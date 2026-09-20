// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// countSeries reports how many child metrics a collector currently holds. That
// number is the thing that grows without bound when an unbounded value reaches a
// label, and it never shrinks on its own: prometheus keeps the child in a map
// with no expiry, and nothing in this repo calls Delete or Reset on it.
func countSeries(t *testing.T, c prometheus.Collector) int {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	return testutil.CollectAndCount(reg)
}

// TestResourceManagerErrIncLabelIsBounded is the regression this file exists
// for. The label used to be err.Error(), so an error naming an address, an
// interface id, or a host and port from a timeout started a new time series on
// every call -- held for the life of the daemon, and mirrored into whatever
// Prometheus scrapes it.
func TestResourceManagerErrIncLabelIsBounded(t *testing.T) {
	ResourceManagerErr.Reset()
	t.Cleanup(ResourceManagerErr.Reset)

	// The shape the daemon actually produces: same failure, different detail.
	for i := 0; i < 100; i++ {
		ResourceManagerErrInc("Release", fmt.Errorf(
			"cannot unassign 10.0.0.%d from eni-%d: dial tcp 10.1.2.3:443: i/o timeout", i, i))
	}
	if got := countSeries(t, ResourceManagerErr); got != 1 {
		t.Errorf("100 errors differing only in detail produced %d time series, want 1", got)
	}

	// A second function name is a legitimate new series: fn is the bounded half
	// the metric is keyed on, and telling the two functions apart is its job.
	ResourceManagerErrInc("CreateWithIPCount", fmt.Errorf("no available subnet"))
	if got := countSeries(t, ResourceManagerErr); got != 2 {
		t.Errorf("a second fn produced %d time series, want 2", got)
	}
}

// TestErrLabelKeepsTheLabelledEscapeHatch pins the one way a package can still
// contribute its own label value: implement LabelledError with a bounded one.
// pkg/zapi/metadata uses it to report HTTP status codes, which are bounded.
func TestErrLabelKeepsTheLabelledEscapeHatch(t *testing.T) {
	if got := errLabel(labeledErr{"404"}); got != "404" {
		t.Errorf("errLabel(LabelledError) = %q, want %q", got, "404")
	}
	if got := errLabel(fmt.Errorf("wrapped: %w", labeledErr{"503"})); got != "503" {
		t.Errorf("errLabel through a wrap = %q, want %q", got, "503")
	}
	if got := errLabel(fmt.Errorf("plain")); got != "error" {
		t.Errorf("errLabel(plain) = %q, want %q", got, "error")
	}
}

type labeledErr struct{ code string }

func (l labeledErr) Error() string       { return "labeled error " + l.code }
func (l labeledErr) MetricLabel() string { return l.code }
