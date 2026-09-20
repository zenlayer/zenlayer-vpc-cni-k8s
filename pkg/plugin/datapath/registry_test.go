// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package datapath

import (
	"testing"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/plugin/types"
)

// types.dataPathNames decides what a conflist may ask for; the switch in
// newDataPathDriver decides what can actually be built. A value in the first
// with no case in the second compiles fine and only fails when a pod lands on
// that node, so assert the two agree here instead.
func TestEveryDataPathHasADriver(t *testing.T) {
	for _, dp := range types.AllDataPaths() {
		driver, err := newDataPathDriver(dp)
		if err != nil {
			t.Errorf("datapath %s is accepted by ParseDataPath but has no driver: %v", dp, err)
			continue
		}
		if driver == nil {
			t.Errorf("datapath %s resolved to a nil driver", dp)
			continue
		}
		if driver.Name() == "" {
			t.Errorf("driver for datapath %s has an empty Name()", dp)
		}
	}
}

// The zero value must not be buildable, so that skipping the error from
// ParseDataPath cannot produce a working datapath.
func TestNewDataPathDriverRejectsUnknown(t *testing.T) {
	if driver, err := newDataPathDriver(types.DataPathUnknown); err == nil {
		t.Fatalf("newDataPathDriver(DataPathUnknown) returned %v, want an error", driver)
	}
}

// A known datapath resolves to a usable driver of the expected concrete type.
//
// Only the type is asserted, not the identity: the only driver today is a
// zero-sized struct, and Go is free to hand out the same address for every
// zero-sized allocation, so comparing two pointers would say nothing either way.
func TestNewDataPathDriverBuildsEachTime(t *testing.T) {
	first, err := newDataPathDriver(types.PolicyRoute)
	if err != nil {
		t.Fatalf("newDataPathDriver: %v", err)
	}
	if first == nil {
		t.Fatal("newDataPathDriver returned a nil driver")
	}
	if _, ok := first.(*PolicyRouteDriver); !ok {
		t.Errorf("newDataPathDriver(PolicyRoute) = %T, want *PolicyRouteDriver", first)
	}
}
