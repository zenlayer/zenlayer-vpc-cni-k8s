// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"testing"
	"time"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/utils"
)

// stubMeta supplies the one metadata field isOwnENI reads. The embedded
// interface satisfies the rest; nothing else is called.
type stubMeta struct {
	InstanceMetadataGetter
	id string
}

func (s stubMeta) GetInstanceId() string { return s.id }

// stubSubnets answers only the "is this configured" question isManagedENI asks;
// the embedded interface satisfies the rest.
type stubSubnets struct {
	SubnetManager
	configured map[string]bool
}

func (s stubSubnets) IsPodSubnet(id string) bool { return s.configured[id] }

func nicIn(subnetID string, tags map[string]string) *zecApi.NicInfo {
	nic := nicWith("eni", tags)
	nic.SubnetId = zecCom.String(subnetID)
	return nic
}

func nicWith(name string, tags map[string]string) *zecApi.NicInfo {
	nic := &zecApi.NicInfo{Name: zecCom.String(name)}
	if tags == nil {
		return nic
	}
	list := make([]*zecApi.Tag, 0, len(tags))
	for k, v := range tags {
		list = append(list, &zecApi.Tag{Key: zecCom.String(k), Value: zecCom.String(v)})
	}
	nic.Tags = &zecApi.Tags{Tags: list}
	return nic
}

// TestIsOwnENIRestsOnTagsNotName pins ownership on the tags rather than the
// name. An ENI judged to be this daemon's is eventually detached and deleted
// (freeENI), and a name is forgeable: the check this replaced matched the CNI's
// name prefix alone, so an operator's own interface carrying that prefix was
// adopted and destroyed. Both tags are required -- created-by alone would let
// one node reap another node's interfaces.
func TestIsOwnENIRestsOnTagsNotName(t *testing.T) {
	const self = "1755670178034880550"
	const other = "1755516676029419624"

	ownTags := utils.CniENITags(self, "")
	foreignTags := utils.CniENITags(other, "")

	for _, tc := range []struct {
		name string
		nic  *zecApi.NicInfo
		want bool
	}{
		{"nil nic", nil, false},
		{"both tags for this instance", nicWith("anything", ownTags), true},
		{
			"correct prefix but tags name another instance",
			nicWith(utils.GetCniNicName(other), foreignTags),
			false,
		},
		{
			"operator interface wearing this daemon's name prefix",
			nicWith(utils.GetCniNicName("some-node"), nil),
			false,
		},
		{
			"created-by without instance-id",
			nicWith("x", map[string]string{utils.TagKeyCreatedBy: utils.TagValueCreatedBy}),
			false,
		},
		{
			"instance-id without created-by",
			nicWith("x", map[string]string{utils.TagKeyInstanceID: self}),
			false,
		},
		{
			"created-by set to someone else's value",
			nicWith("x", map[string]string{
				utils.TagKeyCreatedBy:  "some-other-cni",
				utils.TagKeyInstanceID: self,
			}),
			false,
		},
		{"no tags at all", nicWith("x", nil), false},
		{"empty tag list", nicWith("x", map[string]string{}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &ZecAPIImpl{InstanceMetadataGetter: stubMeta{id: self}}
			if got := e.isOwnENI(tc.nic); got != tc.want {
				t.Errorf("isOwnENI() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsManagedENIPrecedence pins the three rules that decide whether a
// secondary interface is this daemon's to serve addresses from and to detach.
//
// The order is what matters. The no-manage tag is checked before ownership, so
// it can exempt an interface this daemon itself created, and it holds whichever
// way manageUntaggedENI is set -- an operator who tags a storage or management
// interface gets one answer they can rely on without also knowing the config.
// Clusters may share a VPC, and the controlplane sweeps the whole VPC: it may
// delete only what this cluster created, plus interfaces with no cluster id
// (made before the tag, or by a cluster on an older version, which sweeps them
// the same way). Another cluster's interface is that cluster's to sweep.
func TestSweepableKeepsToThisCluster(t *testing.T) {
	const self, other = "uid-self", "uid-other"
	withCluster := func(id string) map[string]string { return utils.CniENITags("i-1", id) }
	for _, tc := range []struct {
		name      string
		clusterID string
		tags      map[string]string
		want      bool
	}{
		{"this cluster's", self, withCluster(self), true},
		{"no cluster tag", self, withCluster(""), true},
		{"another cluster's", self, withCluster(other), false},
		{"not created by the plugin", self, map[string]string{utils.TagKeyClusterID: self}, false},
		{"own id unknown: untagged still swept", "", withCluster(""), true},
		{"own id unknown: any tagged one left alone", "", withCluster(other), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &ZecAPIImpl{clusterID: tc.clusterID}
			if got := e.sweepable(nicWith("eni", tc.tags)); got != tc.want {
				t.Fatalf("sweepable = %v, want %v", got, tc.want)
			}
		})
	}
}

// The sweep threshold must outlast a create still waiting for its interface
// to attach (attachENI's retry budget, about 5.5 minutes), or a create in
// progress -- this cluster's or another's -- is deleted under it.
func TestLeakThresholdOutlastsAttach(t *testing.T) {
	if eniLeakedTime < 6*time.Minute {
		t.Fatalf("eniLeakedTime %s is shorter than the attach budget", eniLeakedTime)
	}
}

func TestIsManagedENIPrecedence(t *testing.T) {
	const self = "1755670178034880550"
	const other = "1755516676029419624"

	own := utils.CniENITags(self, "")
	foreign := utils.CniENITags(other, "")
	noManage := map[string]string{utils.TagKeyNoManage: "true"}

	ownAndNoManage := map[string]string{}
	for k, v := range own {
		ownAndNoManage[k] = v
	}
	ownAndNoManage[utils.TagKeyNoManage] = "true"

	for _, tc := range []struct {
		name           string
		tags           map[string]string
		manageUntagged bool
		want           bool
	}{
		{"created by this daemon", own, true, true},
		{"created by this daemon, adoption off", own, false, true},

		{"untagged, adoption on", nil, true, true},
		{"untagged, adoption off", nil, false, false},

		{"another node's interface, adoption on", foreign, true, true},
		{"another node's interface, adoption off", foreign, false, false},

		// The tag wins in every combination, including over ownership.
		{"no-manage tag, adoption on", noManage, true, false},
		{"no-manage tag, adoption off", noManage, false, false},
		{"no-manage tag on our own interface", ownAndNoManage, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &ZecAPIImpl{
				InstanceMetadataGetter: stubMeta{id: self},
				manageUntaggedENI:      tc.manageUntagged,
				subnetMgr:              stubSubnets{configured: map[string]bool{"subnet-ok": true}},
			}
			if got := e.isManagedENI(nicIn("subnet-ok", tc.tags)); got != tc.want {
				t.Errorf("isManagedENI = %v, want %v (manageUntaggedENI=%v)",
					got, tc.want, tc.manageUntagged)
			}
		})
	}
}

// TestIsManagedENITagValueIsExact guards the opt-out against being read as a
// mere presence check. Anything other than "true" leaves the interface managed,
// so an operator who writes the tag with a value they think means "no" -- or
// leaves a stale "false" behind -- does not get a silent exemption they cannot
// see in the config.
func TestIsManagedENITagValueIsExact(t *testing.T) {
	const self = "1755670178034880550"
	for _, tc := range []struct {
		value string
		want  bool // want managed
	}{
		{"true", false},
		{"True", false}, // compared with strings.EqualFold, so case does not matter
		{"TRUE", false},
		{"false", true},
		{"", true},
		{"yes", true},
		{"1", true},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			e := &ZecAPIImpl{
				InstanceMetadataGetter: stubMeta{id: self},
				manageUntaggedENI:      true,
				subnetMgr:              stubSubnets{configured: map[string]bool{"subnet-ok": true}},
			}
			nic := nicIn("subnet-ok", map[string]string{utils.TagKeyNoManage: tc.value})
			if got := e.isManagedENI(nic); got != tc.want {
				t.Errorf("isManagedENI with no-manage=%q = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// TestIsManagedENIRefusesNil is the guard GetAttachedENIs, the one caller,
// relies on: it hands every element of the describe result straight through,
// and a nil there must not be read as "adopt it".
func TestIsManagedENIRefusesNil(t *testing.T) {
	e := &ZecAPIImpl{InstanceMetadataGetter: stubMeta{id: "x"}, manageUntaggedENI: true}
	if e.isManagedENI(nil) {
		t.Error("isManagedENI(nil) = true, want false")
	}
}

// TestIsManagedENIAdoptsOnlyConfiguredSubnets pins the gate that keeps adoption
// inside the subnets chosen for pod traffic.
//
// Without it the node advertises capacity for addresses on a subnet nobody
// picked for pods, and eniIPFactory.submitOrder goes on to order addresses
// there: it resolves the subnet with GetPodSubnet, which falls back to
// describing the subnet on the platform, so its own nil-subnet check does not
// stop it.
//
// The gate applies to adoption only. An interface this daemon created stays
// managed even once its subnet leaves the configuration -- dropping it there
// would strand the interface together with the addresses already on it.
func TestIsManagedENIAdoptsOnlyConfiguredSubnets(t *testing.T) {
	const self = "1755670178034880550"
	subnets := stubSubnets{configured: map[string]bool{"subnet-pods": true}}

	for _, tc := range []struct {
		name     string
		subnetID string
		tags     map[string]string
		want     bool
	}{
		{"untagged, configured subnet", "subnet-pods", nil, true},
		{"untagged, some other subnet", "subnet-storage", nil, false},
		{"untagged, no subnet reported", "", nil, false},

		// Ownership outranks the subnet: a subnet removed from the config later
		// must not strand an interface this daemon created.
		{"our own, configured subnet", "subnet-pods", utils.CniENITags(self, ""), true},
		{"our own, subnet since removed from config", "subnet-gone", utils.CniENITags(self, ""), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &ZecAPIImpl{
				InstanceMetadataGetter: stubMeta{id: self},
				manageUntaggedENI:      true,
				subnetMgr:              subnets,
			}
			if got := e.isManagedENI(nicIn(tc.subnetID, tc.tags)); got != tc.want {
				t.Errorf("isManagedENI(subnet=%q) = %v, want %v", tc.subnetID, got, tc.want)
			}
		})
	}
}

// TestIsManagedENIWithoutSubnetManagerAdoptsNothing is the fail-closed case.
// An interface left alone costs capacity; one adopted by mistake is detached
// and deleted when the pool releases it (freeENI), which cannot be undone.
func TestIsManagedENIWithoutSubnetManagerAdoptsNothing(t *testing.T) {
	e := &ZecAPIImpl{
		InstanceMetadataGetter: stubMeta{id: "self"},
		manageUntaggedENI:      true,
	}
	if e.isManagedENI(nicIn("subnet-pods", nil)) {
		t.Error("an untagged interface was adopted with no subnet manager to confirm the subnet")
	}
}
