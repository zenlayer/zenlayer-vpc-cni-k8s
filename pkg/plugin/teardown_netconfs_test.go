// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"strings"
	"testing"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/rpc"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// unbuildableNetConf is a record entry generateTearDownConf refuses: it carries
// neither a pod address nor interface information, which is what a truncated or
// half-written record looks like. Nothing in this file reaches netlink, because
// the refusal happens before the teardown is attempted.
func unbuildableNetConf(ifName string) *rpc.NetConf {
	return &rpc.NetConf{IfName: ifName}
}

func teardownArgs(t *testing.T) (*zecTypes.CNIConf, *zecTypes.K8SArgs) {
	t.Helper()
	return &zecTypes.CNIConf{ENIIPVirtualType: "policyRoute"},
		&zecTypes.K8SArgs{K8S_POD_NAME: "web", K8S_POD_NAMESPACE: "default"}
}

// TestTearDownNetConfsReportsAConfigItCannotBuild is the regression this file
// exists for.
//
// The refusal used to be logged and swallowed as nil. DoCmdDel read that nil as
// success and released the address, while the host veth and the two ip rules
// naming it stayed on the node -- and nothing else on this path removes them,
// because reclaimStaleRules only runs for a pod whose record is already gone.
// The next pod given that address inherits rules pointing at a dead veth.
func TestTearDownNetConfsReportsAConfigItCannotBuild(t *testing.T) {
	conf, k8sConfig := teardownArgs(t)

	err := tearDownNetConfs(zlog.GetLogger(), []*rpc.NetConf{unbuildableNetConf("eth0")},
		rpc.IPType_TypeENIMultiIP, conf, k8sConfig, nil)

	if err == nil {
		t.Fatal("tearDownNetConfs returned nil for a config it could not build; DoCmdDel reads that " +
			"as success and releases the address while its host rules are still installed")
	}
}

// TestTearDownNetConfsAttemptsEveryEntry covers the other half of the same
// return: it abandoned the remaining networks as well, so one unbuildable entry
// took the teardown of the pod's other networks with it.
func TestTearDownNetConfsAttemptsEveryEntry(t *testing.T) {
	conf, k8sConfig := teardownArgs(t)

	err := tearDownNetConfs(zlog.GetLogger(),
		[]*rpc.NetConf{unbuildableNetConf("eth0"), unbuildableNetConf("eth1")},
		rpc.IPType_TypeENIMultiIP, conf, k8sConfig, nil)

	if err == nil {
		t.Fatal("tearDownNetConfs returned nil for two configs it could not build")
	}
	// Both refusals name their own entry, so the joined error mentions each.
	// Stopping at the first would carry only one.
	if got := strings.Count(err.Error(), "build teardown config"); got != 2 {
		t.Errorf("the error reports %d failures, want 2 -- the second entry was never attempted: %v",
			got, err)
	}
}

// TestTearDownNetConfsIsANoOpForAnEmptyRecord pins the ordinary case: a record
// with nothing in it must not manufacture a failure, or every DEL for a pod that
// never got an address would fail and the pod would never terminate.
func TestTearDownNetConfsIsANoOpForAnEmptyRecord(t *testing.T) {
	conf, k8sConfig := teardownArgs(t)

	if err := tearDownNetConfs(zlog.GetLogger(), nil, rpc.IPType_TypeENIMultiIP, conf, k8sConfig, nil); err != nil {
		t.Errorf("tearDownNetConfs() = %v for an empty record, want nil", err)
	}
}
