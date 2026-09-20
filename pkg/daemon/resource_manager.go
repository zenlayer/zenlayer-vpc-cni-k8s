// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/pool"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// ResourceManager is the interface to manage network resources.
type ResourceManager interface {
	Name() string
	// Allocate hands out a resource, reusing the one named by prefer when it is
	// still available.
	Allocate(ctx *netContext, prefer string) (zecTypes.NetResource, error)
	Release(ctx *netContext, resource *zecTypes.ResourceItem) error
	// InuseByOwner returns the resources held by owner according to the pool,
	// for the case where no stored record names them.
	InuseByOwner(owner string) []zecTypes.ResourceItem
	GetSnapshot() (pool.ResourcePoolSnapshot, error)
	GetPoolStatus() pool.Status
	// GetResourceLimit returns how many resources this node can serve, never
	// negative: the device plugin advertises this figure less the pods already
	// on the node, and judgmentEvictPod compares the pod count against it.
	//
	// This is the pool's own max cap (pool.ResourcePool.GetResourceLimit) --
	// both managers here just forward to it. Not to be confused with the
	// same-named pool.ObjectFactory method, which is only an upper bound on
	// what the platform could supply.
	GetResourceLimit() int
}
