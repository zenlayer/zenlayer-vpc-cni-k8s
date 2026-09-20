// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package metrics defines the Prometheus series the daemon exports and the
// helpers that label them.
//
// Label cardinality is the thing to watch here. Prometheus keeps one time
// series per distinct label combination for the life of the process, so a label
// derived from an error's text grows without bound. ResourceManagerErr does
// exactly that today -- see the note on it -- which is why a new metric should
// take its labels from a fixed set such as an API name or an error code.
package metrics

import (
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
)

var (
	log                  = zlog.GetLogger().WithFields(zlog.Fields{"subsys": "metrics"})
	prometheusRegistered = false
)

const (
	envDisableMetrics = "ZENLAYER_DISABLE_METRICS"
)

func getEnvBoolWithDefault(envName string, def bool) bool {
	if strValue := os.Getenv(envName); strValue != "" {
		parsedValue, err := strconv.ParseBool(strValue)
		if err == nil {
			return parsedValue
		}
		log.Errorf("Failed to parse %s, using default `%t`: %v", envName, def, err.Error())
	}
	return def
}

func disableMetrics() bool {
	return getEnvBoolWithDefault(envDisableMetrics, false)
}

// ServeMetrics registers the /metrics handler on serveMux unless metrics are
// disabled by ZENLAYER_DISABLE_METRICS.
func ServeMetrics(serveMux *http.ServeMux) {
	if disableMetrics() {
		log.Info("Metrics endpoint disabled")
		return
	}
	log.Infof("Register metrics server")
	serveMux.Handle("/metrics", promhttp.Handler())
}

// PrometheusRegister registers this package's metrics with the default
// registry. Guarded so that a second call is a no-op: MustRegister panics when
// a collector is registered twice.
func PrometheusRegister() {
	if prometheusRegistered {
		return
	}

	// Every metric this package defines must be listed here. An unregistered
	// metric still costs its writers whatever it takes to compute, never
	// reaches /metrics, and reports no error.
	prometheus.MustRegister(RpcLatency)
	prometheus.MustRegister(Ec2APILatency)
	prometheus.MustRegister(Ec2ApiReq)
	prometheus.MustRegister(Ec2APIErr)
	prometheus.MustRegister(MetadataLatency)
	prometheus.MustRegister(MetadataErr)
	prometheus.MustRegister(ResourceManagerErr)

	prometheus.MustRegister(ResourcePoolMaxCap)
	prometheus.MustRegister(ResourcePoolTarget)
	prometheus.MustRegister(ResourcePoolTargetMin)
	prometheus.MustRegister(ResourcePoolTotal)
	prometheus.MustRegister(ResourcePoolAvailable)

	prometheusRegistered = true
}

// MsSince returns whole milliseconds since start. The division truncates, so
// anything under a millisecond is reported as 0.
func MsSince(start time.Time) float64 {
	return float64(time.Since(start) / time.Millisecond)
}
