// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"time"

	"github.com/gdexlab/go-render/render"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/rpc"
)

// PodNetworkModeENIShare is the only pod network mode this CNI sets, and the
// daemon sets it only while its own network mode is eni_multi_ip. The AllocIP,
// ReleaseIP and GetIPInfo handlers all check for it (see verifyPodNetwork), so
// a daemon configured for anything else refuses the request outright instead of
// serving it with a datapath that does not match.
const (
	PodNetworkModeENIShare = "eni_shared"
)

// Pod is the daemon's own record of a pod and the resources handed to it. It is
// persisted so allocations survive a daemon restart; see PodPersistenceManager.
type Pod struct {
	Namespace          string    `json:"nameSpace"`
	Name               string    `json:"name"`
	SandboxContainerId string    `json:"sandboxContainerId"`
	CreateTime         time.Time `json:"createTime,omitempty"`

	NetNs string `json:"netNs,omitempty"`

	AllowEviction bool `json:"allowEviction,omitempty"`
	VpcENI        bool `json:"vpcENI,omitempty"`

	MainInterface             *rpc.NetConf `json:"mainInterface,omitempty"`             // Deprecated
	IsMainInterfaceSharedMode bool         `json:"isMainInterfaceSharedMode,omitempty"` // Deprecated
	PodNetworkMode            string       `json:"podNetworkMode,omitempty"`

	Resources []ResourceItem `json:"resources,omitempty"`
}

// PodKey is the "namespace/name" identifier a pod is tracked under: the owner
// string recorded on a pool resource, and the key of the daemon's pod maps.
func PodKey(podNameSpace, podName string) string {
	return fmt.Sprintf("%s/%s", podNameSpace, podName)
}

// GetNetResourceByType returns the pod's resources of one ResourceItem.Type,
// or nil if it has none. A nil receiver returns nil rather than panicking.
func (p *Pod) GetNetResourceByType(rType string) []ResourceItem {
	var ret []ResourceItem
	if p == nil {
		return ret
	}
	for _, r := range p.Resources {
		if rType == r.Type {
			ret = append(ret, r)
		}
	}
	return ret
}

func (p *Pod) String() string {
	if p == nil {
		return ""
	}
	return render.AsCode(p)
}

type Route struct {
	Dst string `json:"dst,omitempty"`
}

type PodNetwork struct {
	Name      string     `json:"name,omitempty"`
	Namespace string     `json:"namespace,omitempty"`
	IfName    string     `json:"ifName,omitempty"`
	PodIP     rpc.IPSet  `json:"podIP"`
	ExtraIPs  []IPSet    `json:"extraIPs,omitempty"`
	Mac       string     `json:"mac,omitempty"`
	Gateway   rpc.IPSet  `json:"gateway,omitempty"`
	Cidr      *rpc.IPSet `json:"cidr,omitempty"`
	Route     *Route     `json:"route,omitempty"`
	ENIId     string     `json:"eniId,omitempty"`
}
