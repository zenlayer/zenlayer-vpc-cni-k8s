// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package pool

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/metrics"
)

// Status is a point-in-time view of a pool's configuration and its counts.
type Status struct {
	TargetMin       int
	Target          int
	MaxCap          int
	MaxCapProbe     bool
	MonitorInterval time.Duration
	Total           int
	Available       int
	Short           int
	Over            int
}

// Config of pool.
type Config struct {
	// Name of pool, also a metrics label
	Name string

	// Type of resource in pool, also a metrics label
	Type string

	// TargetMin minimum number of all resources(contains the used) in pool
	TargetMin int

	// Target number of idle resources to keep in pool
	Target int

	// MaxCap max capacity of pool
	MaxCap int

	// MaxCapProbe takes the cap from the factory's live limit on every check
	// instead of the fixed MaxCap above
	MaxCapProbe bool

	// MonitorInterval period of pool monitor
	MonitorInterval time.Duration

	// GCProtectPeriod protect period of resource while gc action
	GCProtectPeriod time.Duration

	// Factory to create resource in pool
	Factory ObjectFactory

	// PreStart run before the pool starts working
	PreStart ResourcePoolHook

	// OwnerAlive reports whether the owner of an in-use resource still exists.
	//
	// GC matches its in-use set against the stored records, so a record that is
	// missing while its pod is still running reads as "no longer in use". That
	// is what a lost record store leaves behind: PreStart adopts such an address
	// from the API server without writing a record for it. This hook is the
	// second opinion for that case -- the API server still knows whether the
	// owning pod exists, so GC asks before releasing.
	//
	// Optional: with a nil hook GC releases on the records alone.
	OwnerAlive func(owner string) bool
}

func (c *Config) newPoolConfigInner() poolConfigInner {
	return poolConfigInner{
		configLock:      sync.RWMutex{},
		targetMin:       c.TargetMin,
		target:          c.Target,
		maxCap:          c.MaxCap,
		maxCapProbe:     c.MaxCapProbe,
		monitorInterval: c.MonitorInterval,
		gcProtectPeriod: c.GCProtectPeriod,

		metricsMaxCap:   metrics.ResourcePoolMaxCap.WithLabelValues(c.Name, c.Type),
		metricTarget:    metrics.ResourcePoolTarget.WithLabelValues(c.Name, c.Type),
		metricTargetMin: metrics.ResourcePoolTargetMin.WithLabelValues(c.Name, c.Type),
	}
}

type poolConfigInner struct {
	configLock      sync.RWMutex
	targetMin       int
	target          int
	maxCap          int
	maxCapProbe     bool
	monitorInterval time.Duration
	gcProtectPeriod time.Duration

	metricsMaxCap   prometheus.Gauge
	metricTargetMin prometheus.Gauge
	metricTarget    prometheus.Gauge
}

func (p *poolImpl) getMaxCap() int {
	p.configLock.RLock()
	defer p.configLock.RUnlock()
	var maxCap int

	if p.maxCapProbe {
		maxCap = p.factory.GetResourceLimit()
	} else {
		maxCap = p.maxCap
	}
	p.metricsMaxCap.Set(float64(maxCap))
	return maxCap
}

func (p *poolImpl) getMonitorInterval() time.Duration {
	p.configLock.RLock()
	defer p.configLock.RUnlock()
	return p.monitorInterval
}

func (p *poolImpl) getTarget() int {
	p.configLock.RLock()
	defer p.configLock.RUnlock()
	return p.target
}

func (p *poolImpl) setTarget(t int) {
	p.configLock.Lock()
	defer p.configLock.Unlock()
	p.target = t
}

func (p *poolImpl) getTargetMin() int {
	p.configLock.RLock()
	defer p.configLock.RUnlock()
	return p.targetMin
}

func (p *poolImpl) setTargetMin(t int) {
	p.configLock.Lock()
	defer p.configLock.Unlock()
	p.targetMin = t
}

func (p *poolImpl) getMaxCapProbe() bool {
	p.configLock.RLock()
	defer p.configLock.RUnlock()
	return p.maxCapProbe
}

func (p *poolImpl) getGcProtectPeriod() time.Duration {
	p.configLock.RLock()
	defer p.configLock.RUnlock()
	return p.gcProtectPeriod
}
