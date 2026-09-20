// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package types

const (
	// AnnotationK8sPrefix is the DNS-subdomain prefix for every label and
	// annotation this CNI owns.
	AnnotationK8sPrefix = "k8s.zenlayer.com/"

	AnnotationPodNetworksDefinition = AnnotationK8sPrefix + "pod-networks-definition"

	// LabelNodeDynamicConfigKey is an optional node label pointing at a
	// per-node config ConfigMap, in "<namespace>.<name>" form. The ConfigMap it
	// names replaces the cluster-wide one for this node, so a single node can
	// be given different settings without touching the rest of the cluster.
	//
	// The namespace must be the daemon's own; naming another one is refused.
	// The label is on the Node, and a kubelet may set labels on its own Node
	// with any prefix outside kubernetes.io, so without that restriction a
	// compromised node could point its daemon at a config it controls -- and
	// eni_conf names the OpenAPI host the credentials are sent to.
	LabelNodeDynamicConfigKey = AnnotationK8sPrefix + "vpc-cni-config"

	EvictionPolicyAnnotation = AnnotationK8sPrefix + "pod-evict-policy"

	AnnotationTrue = "true"
	AllowEviction  = "allow"

	// EventReasonAllocIPFailed is the reason on every event about a pod that
	// could not get an address, whether the plugin reports the failure or the
	// daemon evicts the pod over it. Operators filter on it, so the two sites
	// have to agree; it lives here rather than as a literal in each.
	EventReasonAllocIPFailed = "AllocIPFailed"
)
