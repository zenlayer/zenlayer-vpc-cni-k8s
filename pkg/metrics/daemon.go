// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	// RpcLatency times the daemon's gRPC handlers. Its "error" label is the
	// string "true" or "false", not a message, so the series count is bounded.
	RpcLatency = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name: "rpc_latency_ms",
			Help: "zenlayer cni rpc call latency in ms",
		},
		[]string{"rpc_api", "error"},
	)

	// ResourceManagerErr counts resource manager failures, labeled by function
	// name and by a bounded rendering of the error -- see ResourceManagerErrInc.
	ResourceManagerErr = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "resource_manager_error_count",
			Help: "The number of errors encountered in eni manager",
		},
		[]string{"fn", "error"},
	)
)

// ResourceManagerErrInc increments ResourceManagerErr for fn.
//
// The error reaches the label through errLabel, not as its own text. Prometheus
// keeps one child metric per distinct label combination in a map with no
// expiry -- only explicit Delete or Reset removes one, and nothing here calls
// either -- so an unbounded label value grows the daemon's memory for the life
// of the process. These errors carry variable detail: an address, an interface
// id, a host and port from a timeout. Measured at 50k distinct errors: 50k time
// series and ~23 MiB against one series and ~1 KiB. The /metrics body grows with
// them, and so does the scraping Prometheus.
//
// The fn label is what the metric is actually for -- how often does this
// function fail -- and the error half only needs to separate kinds. Both call
// sites in pkg/daemon/eni.go log the full text, so nothing is lost.
func ResourceManagerErrInc(fn string, err error) {
	ResourceManagerErr.WithLabelValues(fn, errLabel(err)).Inc()
}
