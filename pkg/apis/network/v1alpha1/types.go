// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 holds ZecNode, the per-node object through which the
// credential-less daemon (ipamType crd) and the credential-holding
// controlplane agree on a node's addresses.
//
// The split is declarative: the daemon states how many IPv4 addresses it
// wants and which ones it no longer needs (spec); the controlplane decides how
// to get there on the platform and reports what the node holds (status).
// Neither side tells the other which platform call to make.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types.
const (
	ConditionReady              = "Ready"
	ConditionIPAllocationFailed = "IPAllocationFailed"
	ConditionInstanceVerified   = "InstanceVerified"
)

// Reasons of the InstanceVerified condition.
const (
	ReasonVerified              = "Verified"
	ReasonIdentityClaimMissing  = "IdentityClaimMissing"
	ReasonProviderIDMissing     = "ProviderIDMissing"
	ReasonProviderIDUnparseable = "ProviderIDUnparseable"
	ReasonIdentityMismatch      = "IdentityMismatch"
	ReasonInstanceNotFound      = "InstanceNotFound"
	ReasonInstanceAmbiguous     = "InstanceAmbiguous"
	// ReasonBoundToOtherInstance: the node name was verified as another
	// instance, which still exists. A name stays with its instance; see
	// the ZecNode lifecycle in the controlplane.
	ReasonBoundToOtherInstance = "BoundToOtherInstance"
)

// IPState values. v1alpha1 only ever reports Ready: an address being released
// is recorded once, in spec.ipv4.releasing, not twice.
const (
	IPStateReady = "Ready"
)

// ENI states.
const (
	ENIStateReady = "Ready"
	// ENIStateExcluded: an interface in a subnet outside the configured pod
	// subnets. It serves no new pod; the addresses listed are what pods still
	// hold, released as they go. One the plugin created is then freed.
	ENIStateExcluded = "Excluded"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=zn
// +kubebuilder:subresource:status

// ZecNode is one node's address state. Its name is the core Node's name.
type ZecNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZecNodeSpec   `json:"spec,omitempty"`
	Status ZecNodeStatus `json:"status,omitempty"`
}

// ZecNodeSpec is written by the node's daemon only. An admission policy keeps
// a daemon to the ZecNode named after its own node.
type ZecNodeSpec struct {
	// Identity is what the daemon read about its own instance. The
	// controlplane never acts on it alone; see the controlplane's identity check.
	Identity IdentityClaim `json:"identity,omitempty"`
	IPv4     IPDemand      `json:"ipv4,omitempty"`
}

// IdentityClaim is the node's own account of which instance it is.
type IdentityClaim struct {
	// InstanceID is read from DMI (product_serial).
	InstanceID string `json:"instanceID,omitempty"`
	// PrimaryMAC and PrimaryIP are the primary interface's, read from the
	// instance metadata service.
	PrimaryMAC string `json:"primaryMAC,omitempty"`
	PrimaryIP  string `json:"primaryIP,omitempty"`
}

// IPDemand is the daemon's request.
type IPDemand struct {
	// Requested is the total number of pod IPv4 addresses the node wants to
	// hold -- an absolute figure, so either side can restart and converge.
	Requested int `json:"requested,omitempty"`
	// Releasing lists addresses the daemon has taken out of its pool and wants
	// returned. The controlplane releases only these, never one of its own
	// choosing.
	Releasing []string `json:"releasing,omitempty"`
}

// ZecNodeStatus is written by the controlplane only.
type ZecNodeStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// IPStack is the controlplane's; a daemon configured otherwise refuses to
	// start rather than wait for prefixes that will never come.
	IPStack    string             `json:"ipStack,omitempty"`
	Node       NodeIdentity       `json:"node,omitempty"`
	Capacity   Capacity           `json:"capacity,omitempty"`
	Scaler     Scaler             `json:"scaler,omitempty"`
	ENIs       []ENIStatus        `json:"enis,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NodeIdentity is filled once the controlplane has verified the node.
type NodeIdentity struct {
	InstanceID string `json:"instanceID,omitempty"`
	ZoneID     string `json:"zoneID,omitempty"`
}

// Capacity is the instance's interface and address budget.
type Capacity struct {
	ENIQuota     int `json:"eniQuota,omitempty"`
	IPv4PerENI   int `json:"ipv4PerENI,omitempty"`
	ENIUnmanaged int `json:"eniUnmanaged,omitempty"`
	// Allocatable is the most pod addresses the node can hold, never negative.
	Allocatable int `json:"allocatable,omitempty"`
}

// Scaler is the warm-pool sizing the daemon applies. The pool's upper bound is
// Capacity.Allocatable, not a setting: a lower one would cap pods per node.
type Scaler struct {
	TargetMin int `json:"targetMin,omitempty"`
	Target    int `json:"target,omitempty"`
}

// ENIStatus is one interface attached to the node and managed for pods.
type ENIStatus struct {
	ID          string      `json:"id"`
	MAC         string      `json:"mac"`
	Primary     bool        `json:"primary,omitempty"`
	SubnetID    string      `json:"subnetID,omitempty"`
	State       string      `json:"state,omitempty"`
	PrimaryIPv4 string      `json:"primaryIPv4,omitempty"`
	IPv4        []IPAddr    `json:"ipv4,omitempty"`
	IPv6        *IPv6Prefix `json:"ipv6,omitempty"`
}

// IPAddr is one secondary IPv4 address on an interface.
type IPAddr struct {
	IP    string `json:"ip"`
	State string `json:"state,omitempty"`
	// Owner is reserved for fixed addresses; always empty in v1alpha1. The
	// daemon never pools nor releases an owned address.
	Owner string `json:"owner,omitempty"`
}

// IPv6Prefix is the /96 the platform assigned to an interface.
type IPv6Prefix struct {
	Prefix string `json:"prefix"`
	// Primary is the interface's own IPv6 as the platform describes it; the
	// daemon excludes it, and the metadata service's answer, from its carve.
	Primary string `json:"primary,omitempty"`
}

// +kubebuilder:object:root=true

// ZecNodeList is a list of ZecNodes.
type ZecNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZecNode `json:"items"`
}
