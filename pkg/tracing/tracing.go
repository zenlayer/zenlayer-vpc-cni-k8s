// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package tracing surfaces daemon events as Kubernetes events on the node and on
// individual pods.
//
// Recorders are registered rather than constructed here so that packages deep in
// the allocation path can report what happened without taking a dependency on
// the API server client.
package tracing

import (
	"errors"
	"sync"
)

// Reasons for the Kubernetes events this daemon records on the node or on a
// pod. They go in the reason argument of the Record* functions; a few are
// declared here without any caller today.
const (
	EventConfigMapUpdateFailed     = "ConfigMapUpdateFailed"
	EventUpdateSubnetFailed        = "UpdateSubnetFailed"
	EventUpdateSecurityGroupFailed = "UpdateSecurityGroupFailed"

	EventNoAvailableSubnet               = "NoAvailableSubnet"
	EventSubnetCidrBlockInvalid          = "SubnetCidrBlockInvalid"
	EventSubnetAvailableIPBelowThreshold = "SubnetAvailableIPBelowThreshold"

	EventVpcResourceQuotaExceeded = "VpcResourceQuotaExceeded"
	EventOpenApiFlowLimit         = "OpenApiFlowLimit"
	EventInsufficientIpInSubnet   = "InsufficientIpInSubnet"
	EventGetInstanceQuotaFailed   = "GetInstanceQuotaFailed"
	EventInstanceQuotaUpdated     = "InstanceQuotaUpdated"

	EventMetadataServiceAbnormal   = "MetadataServiceAbnormal"
	EventCredentialServiceAbnormal = "CredentialServiceAbnormal"

	EventAllocateResourceFailed = "AllocateResourceFailed"
	EventReleaseResourceFailed  = "ReleaseResourceFailed"
)

var (
	defaultTracer Tracer
)

// PodEventRecorder records an event on a pod.
type PodEventRecorder func(podName, podNamespace, eventType, reason, message string) error

// NodeEventRecorder records an event on a node.
type NodeEventRecorder func(eventType, reason, message string)

// Tracer holds the recorders that turn daemon-side conditions into Kubernetes
// events on the node or on a pod.
//
// mtx is never locked anywhere in this package, so a Tracer offers no
// synchronization: registration has to happen before the Record* methods are
// used from other goroutines. The daemon does that once at startup.
type Tracer struct {
	mtx sync.Mutex

	podEvent  PodEventRecorder
	nodeEvent NodeEventRecorder
}

// RegisterEventRecorder sets the node and pod event recorders on t, replacing
// whatever was there. Either argument may be nil, which turns the matching
// Record* method back into an error.
func (t *Tracer) RegisterEventRecorder(node NodeEventRecorder, pod PodEventRecorder) {
	t.nodeEvent = node
	t.podEvent = pod
}

// RecordPodEvent forwards to the registered PodEventRecorder and returns its
// error. It returns an error if no recorder is registered.
func (t *Tracer) RecordPodEvent(podName, podNamespace, eventType, reason, message string) error {
	if t.podEvent == nil {
		return errors.New("no pod event recorder registered")
	}

	return t.podEvent(podName, podNamespace, eventType, reason, message)
}

// RecordNodeEvent forwards to the registered NodeEventRecorder. The only error
// it can return is "no recorder registered": NodeEventRecorder reports none.
func (t *Tracer) RecordNodeEvent(eventType, reason, message string) error {
	if t.nodeEvent == nil {
		return errors.New("no node event recorder registered")
	}

	t.nodeEvent(eventType, reason, message)
	return nil
}

// RegisterEventRecorder registers the recorders on the default tracer.
func RegisterEventRecorder(node NodeEventRecorder, pod PodEventRecorder) {
	defaultTracer.RegisterEventRecorder(node, pod)
}

// RecordPodEvent records a pod event on the default tracer.
func RecordPodEvent(podName, podNamespace, eventType, reason, message string) error {
	return defaultTracer.RecordPodEvent(podName, podNamespace, eventType, reason, message)
}

// RecordNodeEvent records a node event on the default tracer.
func RecordNodeEvent(eventType, reason, message string) error {
	return defaultTracer.RecordNodeEvent(eventType, reason, message)
}

// NewTracer returns a Tracer with no recorders registered, so both Record*
// methods fail until RegisterEventRecorder is called on it.
func NewTracer() *Tracer {
	return &Tracer{
		mtx: sync.Mutex{},
	}
}

// DefaultGlobalTracer returns the tracer behind the package-level
// RegisterEventRecorder, RecordPodEvent and RecordNodeEvent functions.
func DefaultGlobalTracer() *Tracer {
	return &defaultTracer
}
