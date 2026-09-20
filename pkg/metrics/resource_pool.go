// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	ResourcePoolMaxCap = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "resource_pool_max_cap",
			Help: "The max capacity of resource pool"},
		[]string{"name", "type"},
	)

	ResourcePoolTarget = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "resource_pool_target",
			Help: "The cache target of resource pool"},
		[]string{"name", "type"},
	)

	ResourcePoolTargetMin = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "resource_pool_target_min",
			Help: "The min cache target of resource pool"},
		[]string{"name", "type"},
	)

	ResourcePoolTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "resource_pool_total",
			Help: "The total number of resource in pool"},
		[]string{"name", "type"},
	)

	ResourcePoolAvailable = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "resource_pool_available",
			Help: "The available number of resource in pool"},
		[]string{"name", "type"},
	)
)
