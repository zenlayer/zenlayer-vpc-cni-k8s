// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/apis/network/v1alpha1"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/factory"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/providerid"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/tracing"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
)

// ---- fake platform for one node ----

type fakeNIC struct {
	id, mac, subnet, primaryIP, v6cidr string
	primary, own, managed              bool
	secondary                          []string
}

type fakeNode struct {
	nics       []*fakeNIC
	perENI     int
	quota      int
	nextIP     int
	created    int
	freed      []string
	unassigned []string
	assignErr  error
	createErr  error
	v6Assigned []string
	// createUntagged: interfaces come back without this plugin's tags, as
	// when the credential may create but not tag.
	createUntagged bool
}

func (f *fakeNode) DescribeAttachedNICs() ([]*zecApi.NicInfo, error) {
	var out []*zecApi.NicInfo
	for _, n := range f.nics {
		typ := "secondary"
		if n.primary {
			typ = "primary"
		}
		info := &zecApi.NicInfo{
			NicId: zecCom.String(n.id), MacAddress: zecCom.String(n.mac), SubnetId: zecCom.String(n.subnet),
			PrimaryIpv4: zecCom.String(n.primaryIP), NicType: zecCom.String(typ), SecondaryIpv4s: append([]string(nil), n.secondary...),
		}
		if n.v6cidr != "" {
			info.Ipv6Cidr = zecCom.String(n.v6cidr)
		}
		out = append(out, info)
	}
	return out, nil
}

func (f *fakeNode) nic(id string) *fakeNIC {
	for _, n := range f.nics {
		if n.id == id {
			return n
		}
	}
	return nil
}

func (f *fakeNode) IsManagedENI(nic *zecApi.NicInfo) bool {
	n := f.nic(zecCom.ToString(nic.NicId))
	return n != nil && (n.managed || n.own)
}

func (f *fakeNode) IsOwnENI(nic *zecApi.NicInfo) bool {
	n := f.nic(zecCom.ToString(nic.NicId))
	return n != nil && n.own
}

func (f *fakeNode) GetInstanceLimit() (*factory.InstanceLimits, error) {
	l := &factory.InstanceLimits{}
	l.ENIQuota, l.IPv4MaxPerENI = f.quota, f.perENI
	return l, nil
}

func (f *fakeNode) newIP() string {
	f.nextIP++
	return fmt.Sprintf("10.0.1.%d", f.nextIP)
}

func (f *fakeNode) AssignIPv4(eniID string, n int) ([]net.IP, error) {
	if f.assignErr != nil {
		return nil, f.assignErr
	}
	nic := f.nic(eniID)
	var out []net.IP
	for i := 0; i < n; i++ {
		ip := f.newIP()
		nic.secondary = append(nic.secondary, ip)
		out = append(out, net.ParseIP(ip))
	}
	return out, nil
}

func (f *fakeNode) UnassignIPv4(eniID string, ips []net.IP) error {
	nic := f.nic(eniID)
	for _, ip := range ips {
		f.unassigned = append(f.unassigned, ip.String())
		for i, s := range nic.secondary {
			if s == ip.String() {
				nic.secondary = append(nic.secondary[:i], nic.secondary[i+1:]...)
				break
			}
		}
	}
	return nil
}

func (f *fakeNode) CreateAndAttachENI(subnet, _ string, ipCnt int) (*zecApi.NicInfo, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created++
	nic := &fakeNIC{id: fmt.Sprintf("eni-new-%d", f.created), mac: fmt.Sprintf("02:00:00:00:00:%02x", f.created),
		subnet: subnet, primaryIP: f.newIP(), own: !f.createUntagged}
	for i := 1; i < ipCnt; i++ {
		nic.secondary = append(nic.secondary, f.newIP())
	}
	f.nics = append(f.nics, nic)
	return &zecApi.NicInfo{NicId: zecCom.String(nic.id)}, nil
}

func (f *fakeNode) FreeENI(id string) error {
	f.freed = append(f.freed, id)
	for i, n := range f.nics {
		if n.id == id {
			f.nics = append(f.nics[:i], f.nics[i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakeNode) GetOrAssignIPv6Block(id, _ string) (*net.IPNet, net.IP, error) {
	f.v6Assigned = append(f.v6Assigned, id)
	f.nic(id).v6cidr = "2001:db8::/96"
	return nil, nil, nil
}

type fakeSubnets struct{ none bool }

func (fakeSubnets) UpdateSubnetsStatus(...factory.UpdateSubnetsStatusOption) error { return nil }

func (fakeSubnets) GetPodSubnet(id string) *factory.PodSubnet {
	return &factory.PodSubnet{SubnetId: id, AvailableIPv4AddressCount: 100}
}

func (s fakeSubnets) SelectSubnet(types.IPStack, ...factory.UpdateSubnetsStatusOption) *factory.PodSubnet {
	if s.none {
		return nil
	}
	return &factory.PodSubnet{SubnetId: "subnet-1", AvailableIPv4AddressCount: 100}
}

func testReconciler(stack string) *Reconciler {
	manage := false
	return &Reconciler{
		Cfg: &Config{IPStack: stack, SecurityGroup: "sg-1", VpcID: "vpc-1", ManageUntaggedENI: &manage,
			Subnets: []string{"subnet-1"}, Scaler: ScalerConfig{TargetMin: 1, Target: 2}},
		Subnets: fakeSubnets{},
	}
}

func primaryOnly() *fakeNode {
	return &fakeNode{perENI: 4, quota: 3, nics: []*fakeNIC{
		{id: "eni-p", mac: "02:00:00:00:00:aa", subnet: "subnet-1", primaryIP: "10.0.0.2", primary: true},
	}}
}

func zecNode(requested int, releasing ...string) *v1alpha1.ZecNode {
	return &v1alpha1.ZecNode{Spec: v1alpha1.ZecNodeSpec{IPv4: v1alpha1.IPDemand{Requested: requested, Releasing: releasing}}}
}

func run(t *testing.T, r *Reconciler, fn nodePlatform, zn *v1alpha1.ZecNode) (*v1alpha1.ZecNodeStatus, addrResult) {
	t.Helper()
	st := &v1alpha1.ZecNodeStatus{}
	res := r.reconcileAddresses(&nodeState{api: fn}, zn, st)
	return st, res
}

func countIPs(st *v1alpha1.ZecNodeStatus) int {
	n := 0
	for _, e := range st.ENIs {
		n += len(e.IPv4)
	}
	return n
}

// ---- address reconciliation ----

func TestFillsPrimaryBeforeCreating(t *testing.T) {
	fn := primaryOnly()
	st, res := run(t, testReconciler("ipv4"), fn, zecNode(3))
	if res.err != nil {
		t.Fatal(res.err)
	}
	if fn.created != 0 || countIPs(st) != 3 {
		t.Fatalf("created %d interfaces, %d addresses; want 0 and 3", fn.created, countIPs(st))
	}
	if st.Capacity.Allocatable != 9 || st.Capacity.IPv4PerENI != 4 {
		t.Fatalf("capacity %+v", st.Capacity)
	}
}

func TestCreatesInterfaceWhenFull(t *testing.T) {
	fn := primaryOnly()
	st, res := run(t, testReconciler("ipv4"), fn, zecNode(5))
	if res.err != nil {
		t.Fatal(res.err)
	}
	if fn.created != 1 || countIPs(st) != 5 || !res.short {
		t.Fatalf("created %d, addresses %d, short %v", fn.created, countIPs(st), res.short)
	}
}

// Requested is clamped to what the node can hold, and asking for more is
// not an error by itself when the quota is what stops it.
func TestRequestedClampedToAllocatable(t *testing.T) {
	fn := primaryOnly()
	st, res := run(t, testReconciler("ipv4"), fn, zecNode(100))
	if countIPs(st) != 9 {
		t.Fatalf("got %d addresses, want allocatable 9", countIPs(st))
	}
	if res.err != nil {
		t.Fatalf("clamped request reported an error: %v", res.err)
	}
}

// Only what releasing names is given back.
func TestReleasesOnlyReleasing(t *testing.T) {
	fn := primaryOnly()
	fn.nics[0].secondary = []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"}
	st, res := run(t, testReconciler("ipv4"), fn, zecNode(2, "10.0.0.11"))
	if res.err != nil {
		t.Fatal(res.err)
	}
	if strings.Join(fn.unassigned, ",") != "10.0.0.11" || countIPs(st) != 2 {
		t.Fatalf("unassigned %v, held %d", fn.unassigned, countIPs(st))
	}
}

// Held excludes the primary: an interface's own address is never a pod's.
func TestPrimaryAddressNeverReported(t *testing.T) {
	fn := primaryOnly()
	fn.nics[0].secondary = []string{"10.0.0.2", "10.0.0.10"}
	st, _ := run(t, testReconciler("ipv4"), fn, zecNode(1))
	for _, e := range st.ENIs {
		for _, a := range e.IPv4 {
			if a.IP == e.PrimaryIPv4 {
				t.Fatalf("primary %s reported as a pod address", a.IP)
			}
		}
	}
}

// An own interface left empty is freed; an adopted or the primary is not.
func TestFreesOnlyOwnEmptyInterfaces(t *testing.T) {
	fn := primaryOnly()
	fn.nics = append(fn.nics,
		&fakeNIC{id: "eni-own", mac: "02:00:00:00:00:01", subnet: "subnet-1", primaryIP: "10.0.0.20", own: true, secondary: []string{"10.0.0.21"}},
		&fakeNIC{id: "eni-adopted", mac: "02:00:00:00:00:02", subnet: "subnet-1", primaryIP: "10.0.0.30", managed: true},
	)
	fn.nics[0].secondary = []string{"10.0.0.10"}
	_, res := run(t, testReconciler("ipv4"), fn, zecNode(1, "10.0.0.21"))
	if res.err != nil {
		t.Fatal(res.err)
	}
	if strings.Join(fn.freed, ",") != "eni-own" {
		t.Fatalf("freed %v, want only eni-own", fn.freed)
	}
}

// A created interface without this plugin's tags would never be counted or
// freed: it is freed at once and creating pauses for untaggedPause, so a
// missing tag grant does not churn an interface every pass.
func TestUntaggedInterfaceFreedAndCreatingPaused(t *testing.T) {
	fn := primaryOnly()
	fn.createUntagged = true
	r := testReconciler("ipv4")
	ns := &nodeState{api: fn}

	res := r.reconcileAddresses(ns, zecNode(5), &v1alpha1.ZecNodeStatus{})
	if fn.created != 1 || strings.Join(fn.freed, ",") != "eni-new-1" {
		t.Fatalf("created %d, freed %v; want 1 and the untagged one", fn.created, fn.freed)
	}
	if !errors.Is(res.err, errUntagged) {
		t.Fatalf("err = %v, want errUntagged", res.err)
	}

	res = r.reconcileAddresses(ns, zecNode(5), &v1alpha1.ZecNodeStatus{})
	if fn.created != 1 {
		t.Fatalf("created again during the pause: %d", fn.created)
	}
	if res.err == nil || !strings.Contains(res.err.Error(), "creating paused until") {
		t.Fatalf("err = %v, want the pause named", res.err)
	}

	ns.untaggedAt = time.Now().Add(-untaggedPause - time.Minute)
	fn.createUntagged = false
	if res = r.reconcileAddresses(ns, zecNode(5), &v1alpha1.ZecNodeStatus{}); res.err != nil || fn.created != 2 {
		t.Fatalf("after the pause: created %d, err %v; want a second create", fn.created, res.err)
	}
}

// Pod addresses come only from the configured pod subnets. A primary outside
// them is not filled, not counted, given no IPv6 prefix, and reported
// Excluded; its share leaves allocatable, and the node's demand goes to an
// interface created in a pod subnet. What pods still hold on it stays listed.
func TestPrimaryOutsidePodSubnetsExcluded(t *testing.T) {
	fn := primaryOnly()
	fn.nics[0].subnet = "subnet-node"
	fn.nics[0].secondary = []string{"10.0.0.10"} // a pod still holds it
	st, res := run(t, testReconciler("dual"), fn, zecNode(3))
	if res.err != nil {
		t.Fatal(res.err)
	}
	if st.Capacity.Allocatable != 6 { // (2 more interfaces) * (4-1), the primary's 3 gone
		t.Fatalf("allocatable %d, want 6", st.Capacity.Allocatable)
	}
	if fn.created != 1 || slices.Contains(fn.v6Assigned, "eni-p") {
		t.Fatalf("created %d, v6 assigned %v; want one interface and no prefix on the primary", fn.created, fn.v6Assigned)
	}
	var primary *v1alpha1.ENIStatus
	for i := range st.ENIs {
		if st.ENIs[i].Primary {
			primary = &st.ENIs[i]
		} else if len(st.ENIs[i].IPv4) != 3 {
			t.Fatalf("pod interface holds %d, want all 3 requested", len(st.ENIs[i].IPv4))
		}
	}
	if primary == nil || primary.State != v1alpha1.ENIStateExcluded || len(primary.IPv4) != 1 {
		t.Fatalf("primary status %+v, want Excluded still listing 10.0.0.10", primary)
	}
}

// With the primary excluded and no room for another interface the node can
// serve no pod; the pass says so instead of leaving pods to wait unexplained.
func TestPrimaryExcludedNoOtherInterface(t *testing.T) {
	fn := primaryOnly()
	fn.nics[0].subnet = "subnet-node"
	fn.quota = 1
	st, res := run(t, testReconciler("ipv4"), fn, zecNode(2))
	if st.Capacity.Allocatable != 0 || res.reason != reasonNoPodENI {
		t.Fatalf("allocatable %d, reason %q; want 0 and %q", st.Capacity.Allocatable, res.reason, reasonNoPodENI)
	}
}

// An interface about to be filled again is not freed in the same pass.
func TestEmptyInterfaceRefilledNotFreed(t *testing.T) {
	fn := primaryOnly()
	fn.nics[0].secondary = []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"}
	fn.nics = append(fn.nics, &fakeNIC{id: "eni-own", mac: "02:00:00:00:00:01", subnet: "subnet-1", primaryIP: "10.0.0.20", own: true})
	_, _ = run(t, testReconciler("ipv4"), fn, zecNode(4))
	if len(fn.freed) != 0 {
		t.Fatalf("freed %v while it was needed", fn.freed)
	}
}

func TestDualAssignsMissingPrefix(t *testing.T) {
	fn := primaryOnly()
	st, _ := run(t, testReconciler("dual"), fn, zecNode(1))
	if len(fn.v6Assigned) != 1 || st.ENIs[0].IPv6 == nil {
		t.Fatalf("v6 assigned %v, status %+v", fn.v6Assigned, st.ENIs)
	}
}

func TestReportsReasons(t *testing.T) {
	fn := primaryOnly()
	fn.nics[0].secondary = []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"}
	r := testReconciler("ipv4")
	r.Subnets = fakeSubnets{none: true}
	_, res := run(t, r, fn, zecNode(4))
	if res.reason != reasonNoSubnet {
		t.Fatalf("reason %q, want %q", res.reason, reasonNoSubnet)
	}

	fn = primaryOnly()
	fn.quota = 1
	fn.nics[0].secondary = []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"}
	_, res = run(t, testReconciler("ipv4"), fn, zecNode(4))
	if res.err != nil {
		t.Fatalf("a request above allocatable is clamped, not an error: %v", res.err)
	}

	fn = primaryOnly()
	fn.assignErr = errors.New("boom")
	_, res = run(t, testReconciler("ipv4"), fn, zecNode(4))
	if res.err == nil || res.reason != reasonAllocError {
		t.Fatalf("err %v reason %q", res.err, res.reason)
	}
}

// fullThenAttachFails refuses creates in subnet-1 as full and fails the
// attach of any other.
type fullThenAttachFails struct{ *fakeNode }

func (n fullThenAttachFails) CreateAndAttachENI(subnet, sg string, c int) (*zecApi.NicInfo, error) {
	if subnet == "subnet-1" {
		return nil, sdkErr("INVALID_SUBNET_IPV4_INSUFFICIENT")
	}
	return nil, fmt.Errorf("alloc ENI: %w (the ENI was freed): %w", factory.ErrAttachENI, sdkErr("X"))
}

// The reason names the failure that stopped the pass, not a refusal it
// worked around on the way.
func TestReasonIsTheBlockingFailure(t *testing.T) {
	fn := fullThenAttachFails{primaryOnly()}
	fn.nics[0].secondary = []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"}
	r := testReconciler("ipv4")
	r.Cfg.Subnets = append(r.Cfg.Subnets, "subnet-2")
	r.Subnets = twoSubnets{second: 50}
	_, res := run(t, r, fn, zecNode(5))
	if res.reason != tracing.EventAttachENIFailed {
		t.Fatalf("reason %q, want %s (err %v)", res.reason, tracing.EventAttachENIFailed, res.err)
	}
}

// A Node recheck that fails for any reason but NotFound keeps the ZecNode.
func TestGCZecNodesKeepsOnRecheckError(t *testing.T) {
	cached := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(verifiedZN("back", "i-x")).Build()
	live := interceptor.NewClient(fake.NewClientBuilder().WithScheme(testScheme()).Build(), interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("apiserver timeout")
		}})
	r := &Reconciler{Client: cached, APIReader: live, Lookup: lookupFixture()}
	r.gcZecNodes(context.Background())
	if err := cached.Get(context.Background(), client.ObjectKey{Name: "back"}, &v1alpha1.ZecNode{}); err != nil {
		t.Fatalf("ZecNode deleted on a failed recheck: %v", err)
	}
}

// ---- identity ----

type fakeLookup struct {
	byIP      map[string][]*zecApi.InstanceInfo
	instances map[string]*zecApi.InstanceInfo
}

func (f *fakeLookup) DescribeInstances(ids []string) (*zecApi.DescribeInstancesResponse, error) {
	r := &zecApi.DescribeInstancesResponse{Response: &zecApi.DescribeInstancesResponseParams{}}
	if i, ok := f.instances[ids[0]]; ok {
		r.Response.DataSet = []*zecApi.InstanceInfo{i}
		r.Response.TotalCount = zecCom.Integer(1)
	}
	return r, nil
}

func (f *fakeLookup) DescribeInstancesByIPv4(ip string, _ int) (*zecApi.DescribeInstancesResponse, error) {
	set := f.byIP[ip]
	return &zecApi.DescribeInstancesResponse{Response: &zecApi.DescribeInstancesResponseParams{
		DataSet: set, TotalCount: zecCom.Integer(len(set))}}, nil
}

func inst(id, vpc, ip, mac string) *zecApi.InstanceInfo {
	return &zecApi.InstanceInfo{InstanceId: zecCom.String(id), ZoneId: zecCom.String("SH1A"),
		Nics: []*zecApi.NicInfo{{NicType: zecCom.String("primary"), VpcId: zecCom.String(vpc),
			PrimaryIpv4: zecCom.String(ip), MacAddress: zecCom.String(mac), RegionId: zecCom.String("asia-east-1")}}}
}

func node(providerID, ip string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: providerID},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}}}
}

func lookupFixture() *fakeLookup {
	self := inst("i-1", "vpc-1", "10.0.0.2", "02:00:00:00:00:aa")
	// The platform's filter is a prefix match across VPCs: 10.0.0.2 also hits
	// 10.0.0.24 here and an instance in another VPC with the same address.
	return &fakeLookup{
		byIP: map[string][]*zecApi.InstanceInfo{"10.0.0.2": {
			self, inst("i-2", "vpc-1", "10.0.0.24", "02:00:00:00:00:bb"), inst("i-3", "vpc-other", "10.0.0.2", "02:00:00:00:00:cc"),
		}},
		instances: map[string]*zecApi.InstanceInfo{"i-1": self},
	}
}

func goodClaim() v1alpha1.IdentityClaim {
	return v1alpha1.IdentityClaim{InstanceID: "i-1", PrimaryMAC: "02:00:00:00:00:AA", PrimaryIP: "10.0.0.2"}
}

func TestVerifyByAddressFiltersExactly(t *testing.T) {
	cfg := &Config{VpcID: "vpc-1", Identity: IdentityConfig{ProviderID: IdentityAuto}}
	id, verr := verifyIdentity(lookupFixture(), cfg, node("", "10.0.0.2"), goodClaim())
	if verr != nil {
		t.Fatal(verr)
	}
	if id.instanceID != "i-1" || !id.fromLookup || id.regionID != "asia-east-1" {
		t.Fatalf("identity %+v", id)
	}
}

// A node verifies by the providerID the cloud controller manager writes, and
// by the 2.0.0 form that names no region, which cannot be rewritten.
func TestVerifyByProviderID(t *testing.T) {
	cfg := &Config{VpcID: "vpc-1", Identity: IdentityConfig{ProviderID: IdentityAuto}}
	for _, pid := range []string{"zenlayer:///asia-east-1/i-1", "zenlayer://i-1"} {
		id, verr := verifyIdentity(lookupFixture(), cfg, node(pid, "10.0.0.2"), goodClaim())
		if verr != nil {
			t.Fatalf("%s: %v", pid, verr)
		}
		if id.instanceID != "i-1" || id.fromLookup || id.regionID != "asia-east-1" {
			t.Fatalf("%s: identity %+v", pid, id)
		}
	}
}

func TestVerifyRefusals(t *testing.T) {
	auto := &Config{VpcID: "vpc-1", Identity: IdentityConfig{ProviderID: IdentityAuto}}
	manual := &Config{VpcID: "vpc-1", Identity: IdentityConfig{ProviderID: IdentityManual}}
	elsewhere := &Config{VpcID: "vpc-1", RegionID: "asia-south-1", Identity: IdentityConfig{ProviderID: IdentityAuto}}
	cases := map[string]struct {
		cfg    *Config
		node   *corev1.Node
		claim  v1alpha1.IdentityClaim
		reason string
	}{
		"no claim":         {auto, node("", "10.0.0.2"), v1alpha1.IdentityClaim{}, v1alpha1.ReasonIdentityClaimMissing},
		"manual no pid":    {manual, node("", "10.0.0.2"), goodClaim(), v1alpha1.ReasonProviderIDMissing},
		"bad providerID":   {auto, node("other:///x/i-1", "10.0.0.2"), goodClaim(), v1alpha1.ReasonProviderIDUnparseable},
		"claim other id":   {auto, node("", "10.0.0.2"), v1alpha1.IdentityClaim{InstanceID: "i-9", PrimaryMAC: "02:00:00:00:00:aa", PrimaryIP: "10.0.0.2"}, v1alpha1.ReasonIdentityMismatch},
		"claim other mac":  {auto, node("", "10.0.0.2"), v1alpha1.IdentityClaim{InstanceID: "i-1", PrimaryMAC: "02:00:00:00:00:ff", PrimaryIP: "10.0.0.2"}, v1alpha1.ReasonIdentityMismatch},
		"no instance":      {auto, node("", "10.0.0.99"), goodClaim(), v1alpha1.ReasonInstanceNotFound},
		"pid other node":   {auto, node(providerid.Format("asia-east-1", "i-1"), "10.0.0.7"), goodClaim(), v1alpha1.ReasonIdentityMismatch},
		"pid other region": {auto, node(providerid.Format("asia-south-1", "i-1"), "10.0.0.2"), goodClaim(), v1alpha1.ReasonIdentityMismatch},
		"pid no region":    {auto, node("zenlayer:///i-1", "10.0.0.2"), goodClaim(), v1alpha1.ReasonProviderIDUnparseable},
		// One VPC spans regions; the controlplane serves only its own.
		"other region": {elsewhere, node("", "10.0.0.2"), goodClaim(), v1alpha1.ReasonIdentityMismatch},
	}
	for name, c := range cases {
		_, verr := verifyIdentity(lookupFixture(), c.cfg, c.node, c.claim)
		if verr == nil || verr.reason != c.reason {
			t.Errorf("%s: got %v, want reason %s", name, verr, c.reason)
		}
	}
}

func TestVerifyAmbiguous(t *testing.T) {
	l := lookupFixture()
	l.byIP["10.0.0.2"] = append(l.byIP["10.0.0.2"], inst("i-4", "vpc-1", "10.0.0.2", "02:00:00:00:00:dd"))
	cfg := &Config{VpcID: "vpc-1", Identity: IdentityConfig{ProviderID: IdentityAuto}}
	if _, verr := verifyIdentity(l, cfg, node("", "10.0.0.2"), goodClaim()); verr == nil || verr.reason != v1alpha1.ReasonInstanceAmbiguous {
		t.Fatalf("got %v", verr)
	}
}

// ---- config ----

func TestConfigValidate(t *testing.T) {
	manage := true
	good := func() *Config {
		return &Config{RegionID: "r", VpcID: "v", Subnets: []string{"s"}, SecurityGroup: "sg", IPStack: "dual",
			IPv6InternetChargeType: "ByBandwidth", IPv6Bandwidth: 10,
			ManageUntaggedENI: &manage, Scaler: ScalerConfig{TargetMin: 1, Target: 2}}
	}
	if err := good().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"no region":    func(c *Config) { c.RegionID = "" },
		"no vpc":       func(c *Config) { c.VpcID = "" },
		"no subnets":   func(c *Config) { c.Subnets = nil },
		"no sg":        func(c *Config) { c.SecurityGroup = "" },
		"no stack":     func(c *Config) { c.IPStack = "" },
		"ipv6":         func(c *Config) { c.IPStack = "ipv6" },
		"no manage":    func(c *Config) { c.ManageUntaggedENI = nil },
		"bad identity": func(c *Config) { c.Identity.ProviderID = "always" },
		"bad scaler":   func(c *Config) { c.Scaler = ScalerConfig{TargetMin: 5, Target: 3} },
		"empty group":  func(c *Config) { c.NodeGroups = []NodeGroupConfig{{Scaler: c.Scaler}} },
		"v6 billing":   func(c *Config) { c.IPv6InternetChargeType = "" },
		"v6 package":   func(c *Config) { c.IPv6InternetChargeType = "ByTrafficPackage" },
	} {
		c := good()
		mut(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := good()
	_ = c.Validate()
	if c.Identity.ProviderID != IdentityAuto || c.Workers == 0 || c.RateLimit.WriteQPS == 0 {
		t.Fatalf("defaults not filled: %+v", c)
	}
}

func TestScalerForAndServes(t *testing.T) {
	c := &Config{Scaler: ScalerConfig{Target: 2},
		NodeGroups: []NodeGroupConfig{{Selector: map[string]string{"pool": "big"}, Scaler: ScalerConfig{Target: 20}}}}
	if c.ScalerFor(map[string]string{"pool": "big"}).Target != 20 || c.ScalerFor(nil).Target != 2 {
		t.Fatal("node group not applied")
	}
	if c.Serves(map[string]string{"type": "virtual-kubelet"}) || !c.Serves(nil) {
		t.Fatal("virtual node served or real node skipped")
	}
}

// Dual: an interface that cannot get its /96 is neither filled nor counted.
func TestDualSkipsInterfaceWithoutPrefix(t *testing.T) {
	fn := &noPrefixNode{fakeNode: primaryOnly()}
	fn.nics[0].secondary = []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"}
	st, res := run(t, testReconciler("dual"), fn, zecNode(2))
	if fn.assigned != 0 {
		t.Fatalf("filled an interface with no ipv6 prefix")
	}
	if fn.created != 1 || res.err == nil {
		t.Fatalf("created %d, err %v; want a new interface and the prefix error reported", fn.created, res.err)
	}
	// perENI 4, quota 3: 9 addresses, less the primary's share it cannot serve.
	if st.Capacity.Allocatable != 6 {
		t.Fatalf("allocatable %d, want 6 without the prefix-less interface", st.Capacity.Allocatable)
	}
}

type noPrefixNode struct {
	*fakeNode
	assigned int
}

func (n *noPrefixNode) GetOrAssignIPv6Block(id, _ string) (*net.IPNet, net.IP, error) {
	if id == "eni-p" {
		return nil, nil, errors.New("INVALID_SUBNET_IPSTACK_NOT_SUPPORT")
	}
	return n.fakeNode.GetOrAssignIPv6Block(id, "")
}

func (n *noPrefixNode) AssignIPv4(id string, c int) ([]net.IP, error) {
	if id == "eni-p" {
		n.assigned++
	}
	return n.fakeNode.AssignIPv4(id, c)
}

// A pass that released something does not fill: the platform could hand the
// address straight back while it is still in releasing.
func TestNoFillInTheSamePassAsARelease(t *testing.T) {
	fn := primaryOnly()
	fn.nics[0].secondary = []string{"10.0.0.10"}
	_, res := run(t, testReconciler("ipv4"), fn, zecNode(3, "10.0.0.10"))
	if fn.nextIP != 0 || !res.short {
		t.Fatalf("assigned %d in a releasing pass, short %v", fn.nextIP, res.short)
	}
}

// An assign with no answer may have landed: no interface is created for it.
func TestAmbiguousAssignDoesNotCreate(t *testing.T) {
	fn := primaryOnly()
	fn.assignErr = errors.New("timeout")
	_, res := run(t, testReconciler("ipv4"), fn, zecNode(6))
	if fn.created != 0 || res.err == nil {
		t.Fatalf("created %d after an ambiguous assign, err %v", fn.created, res.err)
	}
}

// An empty own interface the fill never reached, because an assign before it
// went unanswered, is kept: the next pass may fill it.
func TestAmbiguousAssignKeepsEmptyInterface(t *testing.T) {
	fn := primaryOnly()
	fn.nics = append(fn.nics, &fakeNIC{id: "eni-own", mac: "02:00:00:00:00:bb", subnet: "subnet-1",
		primaryIP: "10.0.0.3", own: true, managed: true})
	fn.assignErr = errors.New("timeout")
	_, res := run(t, testReconciler("ipv4"), fn, zecNode(6))
	if len(fn.freed) != 0 || fn.created != 0 || res.err == nil {
		t.Fatalf("freed %v, created %d, err %v; want the empty interface kept", fn.freed, fn.created, res.err)
	}
}

// An instance another Node is verified for is refused; another Node merely
// carrying the providerID is not enough to block.
func TestBindIsExclusive(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	verified := &v1alpha1.ZecNode{ObjectMeta: metav1.ObjectMeta{Name: "victim"}, Status: v1alpha1.ZecNodeStatus{
		Node: v1alpha1.NodeIdentity{InstanceID: "i-1"},
		Conditions: []metav1.Condition{{Type: v1alpha1.ConditionInstanceVerified, Status: metav1.ConditionTrue,
			Reason: "Verified", LastTransitionTime: metav1.Now()}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(verified, liveNode("victim")).Build()
	r := &Reconciler{Client: c, NewNodePlatform: func(*identity) nodePlatform { return nil }}
	me := node("", "10.0.0.2")
	if verr := r.bind(context.Background(), r.state(me.Name), me, &identity{instanceID: "i-1"}, "k"); verr == nil ||
		verr.reason != v1alpha1.ReasonInstanceAmbiguous {
		t.Fatalf("got %v", verr)
	}
	if verr := r.bind(context.Background(), r.state(me.Name), me, &identity{instanceID: "i-2"}, "k"); verr != nil {
		t.Fatalf("unrelated instance refused: %v", verr)
	}
	// Now bound in memory: a second node claiming i-2 is refused.
	other := node("", "10.0.0.3")
	other.Name = "n2"
	if verr := r.bind(context.Background(), r.state(other.Name), other, &identity{instanceID: "i-2"}, "k"); verr == nil {
		t.Fatal("second binding of i-2 accepted")
	}
}

// AttachENIFailed is decided by the step that failed, not by the code: a full
// interface's code contains ATTACH too and is not an attach failure.
func TestReasonForAttachRefusal(t *testing.T) {
	attach := fmt.Errorf("create interface: alloc ENI: %w: %w", factory.ErrAttachENI,
		&zecCom.ZenlayerCloudSdkError{Code: "OPERATION_DENIED_INSTANCE_ATTACH_NIC_EXCEED_LIMIT"})
	if got := reasonFor(attach); got != "AttachENIFailed" {
		t.Fatalf("attach failure: reason %q", got)
	}
	full := fmt.Errorf("assign: %w", &zecCom.ZenlayerCloudSdkError{Code: "OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT"})
	if got := reasonFor(full); got == "AttachENIFailed" {
		t.Fatalf("a full interface reported as %q", got)
	}
}

func sdkErr(code string) error { return &zecCom.ZenlayerCloudSdkError{Code: code} }

// While the owner of an instance is mid-verification after a restart (in
// memory, not bound yet), its ZecNode status still reserves the instance.
func TestBindHonoursStatusWhileOwnerVerifies(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	owner := &v1alpha1.ZecNode{ObjectMeta: metav1.ObjectMeta{Name: "owner"}, Status: v1alpha1.ZecNodeStatus{
		Node: v1alpha1.NodeIdentity{InstanceID: "i-1"},
		Conditions: []metav1.Condition{{Type: v1alpha1.ConditionInstanceVerified, Status: metav1.ConditionTrue,
			Reason: "Verified", LastTransitionTime: metav1.Now()}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, liveNode("owner")).Build()
	r := &Reconciler{Client: c, NewNodePlatform: func(*identity) nodePlatform { return nil }}
	r.state("owner") // its pass has started, its binding not made yet
	me := node("", "10.0.0.2")
	if verr := r.bind(context.Background(), r.state(me.Name), me, &identity{instanceID: "i-1"}, "k"); verr == nil {
		t.Fatal("bound an instance whose owner is still verifying")
	}
}

type failingLookup struct{}

func (failingLookup) DescribeInstances([]string) (*zecApi.DescribeInstancesResponse, error) {
	return nil, sdkErr("NETWORK_ERROR")
}
func (failingLookup) DescribeInstancesByIPv4(string, int) (*zecApi.DescribeInstancesResponse, error) {
	return nil, sdkErr("NETWORK_ERROR")
}

// A re-verification the platform cannot answer keeps the binding.
func TestInconclusiveReverifyKeepsBinding(t *testing.T) {
	r := &Reconciler{Lookup: failingLookup{}, Cfg: &Config{VpcID: "vpc-1", Identity: IdentityConfig{ProviderID: IdentityAuto}}}
	n := node(providerid.Format("asia-east-1", "i-1"), "10.0.0.2")
	zn := &v1alpha1.ZecNode{Spec: v1alpha1.ZecNodeSpec{Identity: goodClaim()}}
	c := zn.Spec.Identity
	key := strings.Join([]string{n.Spec.ProviderID, c.InstanceID, c.PrimaryMAC, c.PrimaryIP, "10.0.0.2"}, "|")
	ns := r.state(n.Name)
	ns.id, ns.verifiedFor = &identity{instanceID: "i-1"}, key // verifiedAt zero: re-verification due
	id, verr := r.verify(context.Background(), ns, n, zn)
	if verr != nil || id == nil || ns.id == nil {
		t.Fatalf("binding dropped on a platform failure: %v", verr)
	}
	// A fresh node gets the failure, marked inconclusive.
	_, verr = r.verify(context.Background(), &nodeState{}, n, zn)
	if verr == nil || !verr.inconclusive {
		t.Fatalf("got %v, want an inconclusive failure", verr)
	}
}

// An exhausted subnet refuses that interface only; one in another subnet is
// still filled.
func TestSubnetFullTriesOtherSubnet(t *testing.T) {
	fn := &subnetFullNode{fakeNode: primaryOnly()}
	fn.nics = append(fn.nics, &fakeNIC{id: "eni-2", mac: "02:00:00:00:00:02", subnet: "subnet-2", primaryIP: "10.0.2.2", own: true})
	r := testReconciler("ipv4")
	r.Cfg.Subnets = append(r.Cfg.Subnets, "subnet-2")
	st, res := run(t, r, fn, zecNode(2))
	if res.err != nil || countIPs(st) != 2 {
		t.Fatalf("err %v, %d addresses; want 2 from the other subnet", res.err, countIPs(st))
	}
}

type subnetFullNode struct{ *fakeNode }

func (n *subnetFullNode) AssignIPv4(id string, c int) ([]net.IP, error) {
	if n.nic(id).subnet == "subnet-1" {
		return nil, sdkErr("OPERATION_DENIED_SUBNET_IP_INSUFFICIENT")
	}
	return n.fakeNode.AssignIPv4(id, c)
}

// One address the platform refuses to release does not hold back the rest.
func TestRefusedReleaseDoesNotBlockOthers(t *testing.T) {
	fn := &stickyNode{fakeNode: primaryOnly(), sticky: "10.0.0.10"}
	fn.nics[0].secondary = []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"}
	st, res := run(t, testReconciler("ipv4"), fn, zecNode(0, "10.0.0.10", "10.0.0.11", "10.0.0.12"))
	if res.err == nil || countIPs(st) != 1 {
		t.Fatalf("err %v, %d left; want only the refused address left", res.err, countIPs(st))
	}
}

type stickyNode struct {
	*fakeNode
	sticky string
}

func (n *stickyNode) UnassignIPv4(id string, ips []net.IP) error {
	for _, ip := range ips {
		if ip.String() == n.sticky {
			return sdkErr("INVALID_NIC_HAS_PUBLIC_IP_CANT_DETACH")
		}
	}
	return n.fakeNode.UnassignIPv4(id, ips)
}

// An address released earlier that the daemon has not settled yet (still in
// spec, gone from the platform) holds the fill back too.
func TestNoFillWhileReleaseUnsettled(t *testing.T) {
	fn := primaryOnly()
	_, res := run(t, testReconciler("ipv4"), fn, zecNode(2, "10.0.0.10"))
	if fn.nextIP != 0 || !res.deferred {
		t.Fatalf("assigned %d while a release is unsettled, deferred %v", fn.nextIP, res.deferred)
	}
}

func liveNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func testScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	return scheme
}

func verifiedZN(name, inst string) *v1alpha1.ZecNode {
	return &v1alpha1.ZecNode{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: v1alpha1.ZecNodeStatus{
		Node: v1alpha1.NodeIdentity{InstanceID: inst},
		Conditions: []metav1.Condition{{Type: v1alpha1.ConditionInstanceVerified, Status: metav1.ConditionTrue,
			Reason: "Verified", LastTransitionTime: metav1.Now()}}}}
}

// A node name keeps the instance it was verified as while that instance
// exists; once it is gone the name may move.
func TestNameStaysWithItsInstance(t *testing.T) {
	r := &Reconciler{Lookup: lookupFixture()} // knows i-1 only
	if verr := r.keepsName(verifiedZN("n1", "i-1"), &identity{instanceID: "i-9"}); verr == nil ||
		verr.reason != v1alpha1.ReasonBoundToOtherInstance {
		t.Fatalf("name moved off a live instance: %v", verr)
	}
	if verr := r.keepsName(verifiedZN("n1", "i-gone"), &identity{instanceID: "i-9"}); verr != nil {
		t.Fatalf("name held by a gone instance: %v", verr)
	}
	if verr := r.keepsName(verifiedZN("n1", "i-1"), &identity{instanceID: "i-1"}); verr != nil {
		t.Fatalf("same instance refused: %v", verr)
	}
}

// A ZecNode whose Node is gone reserves nothing for others, so a renamed
// machine can bind its instance under the new name.
func TestOrphanZecNodeReservesNothing(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(verifiedZN("old-name", "i-1")).Build()
	r := &Reconciler{Client: c, NewNodePlatform: func(*identity) nodePlatform { return nil }}
	me := node("", "10.0.0.2")
	if verr := r.bind(context.Background(), r.state(me.Name), me, &identity{instanceID: "i-1"}, "k"); verr != nil {
		t.Fatalf("renamed machine refused: %v", verr)
	}
}

// Cleanup keeps a ZecNode left by kubectl delete node (instance alive) and
// removes those whose binding protects nothing.
func TestGCZecNodes(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		verifiedZN("deleted-node", "i-1"),  // Node gone, instance alive: kept
		verifiedZN("dead-instance", "i-x"), // Node gone, instance gone: removed
		verifiedZN("renamed-old", "i-2"),   // Node gone, i-2 now serves a live node: removed
		verifiedZN("renamed-new", "i-2"), liveNode("renamed-new"),
		&v1alpha1.ZecNode{ObjectMeta: metav1.ObjectMeta{Name: "never-verified"}},
	).Build()
	r := &Reconciler{Client: c, Lookup: lookupFixture()}
	r.gcZecNodes(context.Background())
	left := &v1alpha1.ZecNodeList{}
	_ = c.List(context.Background(), left)
	var names []string
	for _, o := range left.Items {
		names = append(names, o.Name)
	}
	if strings.Join(names, ",") != "deleted-node,renamed-new" {
		t.Fatalf("left %v", names)
	}
}

// providerID is written once and for good, so never without its region.
func TestProviderIDWriteNeedsRegion(t *testing.T) {
	n := node("", "10.0.0.2")
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(n).Build()
	r := &Reconciler{Client: c}
	if err := r.writeProviderID(context.Background(), n, "", "i-1"); err == nil {
		t.Fatal("wrote a providerID without a region")
	}
	got := &corev1.Node{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(n), got)
	if got.Spec.ProviderID != "" {
		t.Fatalf("providerID %q", got.Spec.ProviderID)
	}
}

// A Node still being updated by its kubelet refuses the first providerID
// write with a conflict; it is written on a fresh read, not left for a resync.
func TestProviderIDWriteRetriesConflict(t *testing.T) {
	n := node("", "10.0.0.2")
	conflicts := 1
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(n).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			if conflicts > 0 {
				conflicts--
				return apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, obj.GetName(), errors.New("modified"))
			}
			return cl.Patch(ctx, obj, p, opts...)
		},
	}).Build()
	r := &Reconciler{Client: c}
	if err := r.writeProviderID(context.Background(), n, "asia-east-1", "i-1"); err != nil {
		t.Fatal(err)
	}
	got := &corev1.Node{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(n), got)
	if got.Spec.ProviderID != "zenlayer:///asia-east-1/i-1" {
		t.Fatalf("providerID %q", got.Spec.ProviderID)
	}
}

// A create the platform keeps refusing is not retried on every pass: the
// next try waits, longer after each failure, and a success resets it.
func TestCreateFailureBacksOff(t *testing.T) {
	fn := &attachFailNode{fakeNode: primaryOnly()}
	fn.createErr = fmt.Errorf("alloc ENI: %w: %w", factory.ErrAttachENI, sdkErr("OPERATION_DENIED_INSTANCE_ATTACH_NIC_FAIL"))
	r, ns, zn := testReconciler("ipv4"), &nodeState{api: fn}, zecNode(5)
	for i := 0; i < 3; i++ {
		res := r.reconcileAddresses(ns, zn, &v1alpha1.ZecNodeStatus{})
		if res.reason != tracing.EventAttachENIFailed {
			t.Fatalf("pass %d: reason %q, want %s", i, res.reason, tracing.EventAttachENIFailed)
		}
	}
	if fn.calls != 1 || ns.createFails != 1 {
		t.Fatalf("%d creates over 3 passes (fails %d); want 1 until the backoff passes", fn.calls, ns.createFails)
	}
	first := ns.createAt
	ns.createAt = time.Now().Add(-time.Second)
	r.reconcileAddresses(ns, zn, &v1alpha1.ZecNodeStatus{})
	if fn.calls != 2 || time.Until(ns.createAt) <= time.Until(first)+30*time.Second {
		t.Fatalf("calls %d; the second wait should be about double the first", fn.calls)
	}
	fn.createErr = nil
	ns.createAt = time.Now().Add(-time.Second)
	if res := r.reconcileAddresses(ns, zn, &v1alpha1.ZecNodeStatus{}); res.err != nil || ns.createFails != 0 {
		t.Fatalf("after a success: err %v, fails %d; want reset", res.err, ns.createFails)
	}
}

// A pass held back only by the create backoff asks to be looked at when the
// backoff ends, and a pass that is satisfied without creating forgets the
// streak, so a later unrelated failure starts from the shortest wait.
func TestCreateBackoffRequeueAndDecay(t *testing.T) {
	fn := &attachFailNode{fakeNode: primaryOnly()}
	fn.createErr = fmt.Errorf("alloc ENI: %w: %w", factory.ErrAttachENI, sdkErr("OPERATION_DENIED_INSTANCE_ATTACH_NIC_FAIL"))
	r, ns, zn := testReconciler("ipv4"), &nodeState{api: fn}, zecNode(5)
	r.reconcileAddresses(ns, zn, &v1alpha1.ZecNodeStatus{})
	res := r.reconcileAddresses(ns, zn, &v1alpha1.ZecNodeStatus{})
	if res.err == nil || !res.retryAt.Equal(ns.createAt) {
		t.Fatalf("blocked pass: err %v, retryAt %v; want the backoff end %v", res.err, res.retryAt, ns.createAt)
	}
	if res := r.reconcileAddresses(ns, zecNode(0), &v1alpha1.ZecNodeStatus{}); res.err != nil || ns.createFails != 0 {
		t.Fatalf("a satisfied pass: err %v, fails %d; want the streak forgotten", res.err, ns.createFails)
	}
}

type attachFailNode struct {
	*fakeNode
	calls int
}

func (n *attachFailNode) CreateAndAttachENI(subnet, sg string, c int) (*zecApi.NicInfo, error) {
	n.calls++
	return n.fakeNode.CreateAndAttachENI(subnet, sg, c)
}

// A failure to allocate leaves Ready true once the node's interfaces are known:
// a cluster-wide outage must not mark every node not Ready.
func TestAllocFailureKeepsReady(t *testing.T) {
	r, zn := testReconciler("ipv4"), zecNode(5)
	fail := addrResult{err: errors.New("describe interfaces: credential invalid"), reason: "CredentialServiceAbnormal"}
	cond := func(st *v1alpha1.ZecNodeStatus, typ string) metav1.ConditionStatus {
		return meta.FindStatusCondition(st.Conditions, typ).Status
	}

	known := &v1alpha1.ZecNodeStatus{ENIs: []v1alpha1.ENIStatus{{ID: "eni-p", Primary: true}}}
	r.allocConditions(known, zn, fail)
	if cond(known, v1alpha1.ConditionReady) != metav1.ConditionTrue ||
		cond(known, v1alpha1.ConditionIPAllocationFailed) != metav1.ConditionTrue {
		t.Fatalf("with interfaces known: %+v; want Ready True, IPAllocationFailed True", known.Conditions)
	}

	unknown := &v1alpha1.ZecNodeStatus{}
	r.allocConditions(unknown, zn, fail)
	if c := meta.FindStatusCondition(unknown.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse ||
		c.Reason != "CredentialServiceAbnormal" {
		t.Fatalf("no interfaces known yet: %+v; want Ready False with the reason", c)
	}

	gone := &v1alpha1.ZecNodeStatus{ENIs: []v1alpha1.ENIStatus{{ID: "eni-p", Primary: true}}}
	r.allocConditions(gone, zn, addrResult{err: fmt.Errorf("describe: %w", errInstanceGone), reason: "InstanceNotFound"})
	if cond(gone, v1alpha1.ConditionReady) != metav1.ConditionFalse {
		t.Fatalf("instance gone with a stale interface list: %+v; want Ready False", gone.Conditions)
	}

	r.allocConditions(known, zn, addrResult{})
	if cond(known, v1alpha1.ConditionIPAllocationFailed) != metav1.ConditionFalse {
		t.Fatalf("after a success: %+v; want IPAllocationFailed False", known.Conditions)
	}
}

// An interface the plugin created in a subnet since removed from the pod
// subnets is Excluded like a primary outside them: not filled, its share out
// of allocatable, the demand served by a new interface in a pod subnet. Once
// its pods are gone it is freed.
func TestRemovedSubnetInterfaceDrainsThenFreed(t *testing.T) {
	fn := primaryOnly()
	fn.nics = append(fn.nics, &fakeNIC{id: "eni-old", mac: "02:00:00:00:00:bb", subnet: "subnet-gone",
		primaryIP: "10.0.2.2", own: true, secondary: []string{"10.0.2.10"}})
	st, res := run(t, testReconciler("ipv4"), fn, zecNode(4))
	if res.err != nil {
		t.Fatal(res.err)
	}
	if st.Capacity.Allocatable != 6 { // primary + one new interface, (4-1) each
		t.Fatalf("allocatable %d, want 6", st.Capacity.Allocatable)
	}
	if fn.created != 1 || len(fn.nic("eni-old").secondary) != 1 {
		t.Fatalf("created %d, eni-old holds %v; want one new interface and eni-old not filled",
			fn.created, fn.nic("eni-old").secondary)
	}
	var old *v1alpha1.ENIStatus
	for i := range st.ENIs {
		if st.ENIs[i].ID == "eni-old" {
			old = &st.ENIs[i]
		}
	}
	if old == nil || old.State != v1alpha1.ENIStateExcluded {
		t.Fatalf("eni-old status %+v, want Excluded", old)
	}

	// Its pod is gone.
	fn.nic("eni-old").secondary = nil
	if _, res = run(t, testReconciler("ipv4"), fn, zecNode(4)); res.err != nil {
		t.Fatal(res.err)
	}
	if !slices.Contains(fn.freed, "eni-old") {
		t.Fatalf("freed %v, want eni-old once empty", fn.freed)
	}
}

// fullSubnetCreateNode refuses an interface in subnet-1 as out of addresses.
type fullSubnetCreateNode struct {
	*fakeNode
	code string
}

func (n *fullSubnetCreateNode) CreateAndAttachENI(subnet, sg string, ipCnt int) (*zecApi.NicInfo, error) {
	if subnet == "subnet-1" {
		return nil, sdkErr(n.code)
	}
	return n.fakeNode.CreateAndAttachENI(subnet, sg, ipCnt)
}

// The subnet picked for a new interface can be full although its cached count
// says otherwise: the same pass creates it in another configured subnet, with
// no backoff. The platform words exhaustion two ways: the assign that fills a
// new interface, and the create itself once not even its primary fits.
func TestCreateTriesAnotherSubnetWhenFull(t *testing.T) {
	for _, code := range []string{"OPERATION_DENIED_SUBNET_IP_INSUFFICIENT", "INVALID_SUBNET_IPV4_INSUFFICIENT"} {
		t.Run(code, func(t *testing.T) {
			fn := &fullSubnetCreateNode{fakeNode: primaryOnly(), code: code}
			fn.nics[0].secondary = []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"} // primary full
			r := testReconciler("ipv4")
			r.Cfg.Subnets = append(r.Cfg.Subnets, "subnet-2")
			ns := &nodeState{api: fn}
			st := &v1alpha1.ZecNodeStatus{}
			res := r.reconcileAddresses(ns, zecNode(5), st)
			if res.err != nil || fn.created != 1 || fn.nics[len(fn.nics)-1].subnet != "subnet-2" {
				t.Fatalf("err %v, created %d; want one interface in subnet-2", res.err, fn.created)
			}
			if ns.createFails != 0 {
				t.Fatalf("createFails %d, want no backoff for a full subnet", ns.createFails)
			}
		})
	}
}

// A daemon whose wait timed out drops its request until the pool retries.
// That lull must not read Satisfied, nor reset the event dedup so the same
// failure is reported again when the request returns (CP-13). An allocation
// that actually succeeds clears it at once, and the hold expires on its own.
func TestAllocFailureHoldsAcrossAnIdlePass(t *testing.T) {
	rec := record.NewFakeRecorder(10)
	r, zn := testReconciler("ipv4"), zecNode(5)
	r.Recorder = rec
	ns, node := &nodeState{}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	st := &v1alpha1.ZecNodeStatus{ENIs: []v1alpha1.ENIStatus{{ID: "eni-p", Primary: true}}}
	failed := func() bool {
		return meta.IsStatusConditionTrue(st.Conditions, v1alpha1.ConditionIPAllocationFailed)
	}
	fail := addrResult{err: errors.New("create interface: refused"), reason: "AllocationError"}

	// The daemon's wait ran out and it dropped the request: idle, not met.
	dropped := zn.DeepCopy()
	dropped.Spec.IPv4.Requested--
	r.reportAlloc(ns, node, st, zn, fail)
	r.reportAlloc(ns, node, st, dropped, addrResult{idle: true})
	if !failed() {
		t.Fatalf("an idle pass right after a failure cleared it: %+v", st.Conditions)
	}
	if ns.lastWarn != "AllocationError" {
		t.Fatalf("lastWarn %q after the idle pass; the hold must keep it so the failure is not reported again", ns.lastWarn)
	}
	r.reportAlloc(ns, node, st, zn, fail)
	if n := len(rec.Events); n != 1 {
		t.Fatalf("%d events; want the repeated failure reported once", n)
	}

	// The same request met (the address landed after all) is a recovery.
	r.reportAlloc(ns, node, st, zn, addrResult{idle: true})
	if failed() {
		t.Fatalf("an idle pass with the request unchanged kept the failure: %+v", st.Conditions)
	}

	r.reportAlloc(ns, node, st, zn, fail)
	ns.allocFailedAt = time.Now().Add(-allocFailHold - time.Second)
	r.reportAlloc(ns, node, st, dropped, addrResult{idle: true})
	if failed() {
		t.Fatalf("still failed after the hold expired: %+v", st.Conditions)
	}

	r.reportAlloc(ns, node, st, zn, fail)
	r.reportAlloc(ns, node, st, zn, addrResult{})
	if failed() || ns.lastWarn != "" {
		t.Fatalf("a pass that allocated did not clear the failure: %+v, lastWarn %q", st.Conditions, ns.lastWarn)
	}
}

// countingFullNode refuses every create in subnet-1 as full and counts them.
type countingFullNode struct {
	*fakeNode
	refused int
}

func (n *countingFullNode) CreateAndAttachENI(subnet, sg string, ipCnt int) (*zecApi.NicInfo, error) {
	if subnet == "subnet-1" {
		n.refused++
		return nil, sdkErr("INVALID_SUBNET_IPV4_INSUFFICIENT")
	}
	return n.fakeNode.CreateAndAttachENI(subnet, sg, ipCnt)
}

// A subnet the platform called full is skipped by every node until
// subnetFullHold passes, and a pass whose only trouble is that is looked at
// again after requeueNoSubnet, not requeueShort: with every subnet full, each
// node used to retry the create every 15 s.
func TestFullSubnetHeldForAllNodes(t *testing.T) {
	r := testReconciler("ipv4")
	pass := func() (*countingFullNode, addrResult) {
		fn := &countingFullNode{fakeNode: primaryOnly()}
		fn.nics[0].secondary = []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"} // primary full
		res := r.reconcileAddresses(&nodeState{api: fn}, zecNode(5), &v1alpha1.ZecNodeStatus{})
		return fn, res
	}

	first, res := pass()
	// The batch, then once more with the smallest create before calling it full.
	if first.refused != 2 || !errors.Is(res.err, errNoSubnet) || !res.waitSubnet {
		t.Fatalf("first node: %d refused, err %v, waitSubnet %v; want 2, errNoSubnet, true", first.refused, res.err, res.waitSubnet)
	}
	second, res := pass()
	if second.refused != 0 || !errors.Is(res.err, errNoSubnet) {
		t.Fatalf("second node: %d refused, err %v; want the held subnet skipped", second.refused, res.err)
	}

	r.mu.Lock()
	r.fullUntil["subnet-1"] = time.Now().Add(-time.Second)
	r.mu.Unlock()
	if third, _ := pass(); third.refused != 2 {
		t.Fatalf("after the hold: %d refused; want the subnet tried again", third.refused)
	}
}

// fewSubnets reports subnet-1 as the subnet with the most free addresses, at
// the given count.
type fewSubnets struct {
	count     int
	refreshed time.Time
}

func (fewSubnets) UpdateSubnetsStatus(...factory.UpdateSubnetsStatusOption) error { return nil }

// GetPodSubnet: only subnet-1 is known.
func (s fewSubnets) GetPodSubnet(id string) *factory.PodSubnet {
	if id != "subnet-1" {
		return nil
	}
	return s.SelectSubnet("")
}

func (s fewSubnets) SelectSubnet(types.IPStack, ...factory.UpdateSubnetsStatusOption) *factory.PodSubnet {
	return &factory.PodSubnet{SubnetId: "subnet-1", AvailableIPv4AddressCount: s.count, LastUpdate: s.refreshed}
}

// With one free address a new interface takes it as its own primary and the
// fill fails, so the interface is created only to be deleted.
func TestPickSubnetNeedsRoomBeyondThePrimary(t *testing.T) {
	r := testReconciler("ipv4")
	r.Cfg.Subnets = append(r.Cfg.Subnets, "subnet-2")
	r.Subnets = fewSubnets{count: 1}
	if got, _, _, _ := r.pickSubnet(map[string]bool{}); got != "" {
		t.Fatalf("picked %q with one free address at most; want none", got)
	}
	r.Subnets = fewSubnets{count: 2}
	if got, _, _, _ := r.pickSubnet(map[string]bool{}); got != "subnet-1" {
		t.Fatalf("picked %q; want subnet-1", got)
	}
}

// A subnet called full before is tried by one create at a time: the claim
// is held until releaseProbe, whatever the outcome, and does not extend the
// hold itself -- only a refusal does. A subnet never called full is not limited.
func TestClaimSubnetOneProbeAtATime(t *testing.T) {
	r := testReconciler("ipv4")
	claim := func() (bool, bool) { return r.claimSubnet("subnet-1") }
	for range 2 {
		if ok, claimed := claim(); !ok || claimed {
			t.Fatalf("a subnet never called full: ok=%v claimed=%v; want true, false", ok, claimed)
		}
	}
	r.markFull("subnet-1")
	r.mu.Lock()
	r.fullUntil["subnet-1"] = time.Now().Add(-time.Second) // hold over
	r.mu.Unlock()
	if ok, claimed := claim(); !ok || !claimed {
		t.Fatalf("the first create after the hold: ok=%v claimed=%v; want true, true", ok, claimed)
	}
	if ok, _ := claim(); ok {
		t.Fatal("a second create got to probe beside the first")
	}
	if r.heldFull("subnet-1") {
		t.Fatal("the claim extended the hold; assigns in the subnet would be skipped")
	}
	r.releaseProbe("subnet-1")
	if ok, claimed := claim(); !ok || !claimed {
		t.Fatal("still claimed after the probe ended")
	}
	r.releaseProbe("subnet-1")
	r.clearFull("subnet-1")
	for range 2 {
		if ok, claimed := claim(); !ok || claimed {
			t.Fatal("still limited after a create succeeded")
		}
	}
}

// A new interface asks for no more than the cached count leaves beyond its
// own primary; asking for more fails the whole fill and deletes it again.
func TestCreateSizedToTheCachedCount(t *testing.T) {
	fn := primaryOnly()
	fn.nics[0].secondary = []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"} // primary full
	r := testReconciler("ipv4")
	r.Subnets = fewSubnets{count: 2}
	ns := &nodeState{api: fn}
	res := r.reconcileAddresses(ns, zecNode(5), &v1alpha1.ZecNodeStatus{})
	if fn.created != 1 {
		t.Fatalf("created %d, err %v; want one interface", fn.created, res.err)
	}
	if got := len(fn.nics[len(fn.nics)-1].secondary); got != 1 {
		t.Fatalf("new interface filled with %d; want 1, what two free addresses leave beyond its primary", got)
	}
	// The rest of the demand waits: by the cached count the subnet has no room
	// left, and asking it again in this pass would only make another
	// interface to delete.
	if !errors.Is(res.err, errNoSubnet) {
		t.Fatalf("err %v; want the remaining demand reported as no subnet with room", res.err)
	}
}

// The two ways subnet exhaustion is reported alternate while it lasts; they
// are one Warning, not one per change.
func TestNoSubnetAndSubnetFullWarnOnce(t *testing.T) {
	rec := record.NewFakeRecorder(10)
	r := testReconciler("ipv4")
	r.Recorder = rec
	ns, node := &nodeState{}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	for _, reason := range []string{tracing.EventInsufficientIpInSubnet, reasonNoSubnet, tracing.EventInsufficientIpInSubnet, reasonNoSubnet} {
		r.warnOnce(ns, node, reason, "full")
	}
	if n := len(rec.Events); n != 1 {
		t.Fatalf("%d events; want 1", n)
	}
}

// Every node's pass draws on the same cached count. With two free addresses,
// the first node's new interface takes them; the next node, in its own pass,
// does not try the subnet again until a refresh shows a new count.
func TestCachedCountSharedAcrossNodes(t *testing.T) {
	r := testReconciler("ipv4")
	r.Subnets = fewSubnets{count: 2}
	pass := func() (*fakeNode, addrResult) {
		fn := primaryOnly()
		fn.nics[0].secondary = []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"} // primary full
		return fn, r.reconcileAddresses(&nodeState{api: fn}, zecNode(5), &v1alpha1.ZecNodeStatus{})
	}
	if fn, _ := pass(); fn.created != 1 {
		t.Fatalf("first node created %d; want 1", fn.created)
	}
	if fn, res := pass(); fn.created != 0 || !errors.Is(res.err, errNoSubnet) {
		t.Fatalf("second node created %d, err %v; want none, the count being spent", fn.created, res.err)
	}
	r.Subnets = fewSubnets{count: 5} // refreshed
	if fn, _ := pass(); fn.created != 1 {
		t.Fatalf("after a refresh created %d; want 1", fn.created)
	}
}

// A create that leaves no interface gives its addresses back to the count.
func TestFailedCreateReturnsReservation(t *testing.T) {
	r := testReconciler("ipv4")
	t0 := time.Now()
	if n := r.reserveAddrs("subnet-1", 3, t0, 5); n != 2 {
		t.Fatalf("reserved %d of 3 free; want 2, leaving one for the primary", n)
	}
	if n := r.reserveAddrs("subnet-1", 3, t0, 5); n != 0 {
		t.Fatalf("reserved %d more from a spent count", n)
	}
	r.unreserveAddrs("subnet-1", t0, 2)
	if n := r.reserveAddrs("subnet-1", 3, t0, 1); n != 1 {
		t.Fatalf("after giving back reserved %d; want 1", n)
	}
	// A refresh that reads the same number starts the spending over.
	if n := r.reserveAddrs("subnet-1", 3, t0.Add(time.Minute), 5); n != 2 {
		t.Fatalf("after a refresh with the same count reserved %d; want 2", n)
	}
}

// Two failures taking turns are each reported once within warnRepeatQuiet,
// and again after it.
func TestAlternatingWarningsQuiet(t *testing.T) {
	rec := record.NewFakeRecorder(10)
	r := testReconciler("ipv4")
	r.Recorder = rec
	ns, node := &nodeState{}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	for range 3 {
		r.warnOnce(ns, node, tracing.EventInsufficientIpInSubnet, "full")
		r.warnOnce(ns, node, reasonQuotaFull, "quota")
	}
	if n := len(rec.Events); n != 2 {
		t.Fatalf("%d events; want one per reason", n)
	}
	for k := range ns.warnedAt {
		ns.warnedAt[k] = time.Now().Add(-warnRepeatQuiet - time.Second)
	}
	r.warnOnce(ns, node, tracing.EventInsufficientIpInSubnet, "full")
	if n := len(rec.Events); n != 3 {
		t.Fatalf("%d events; want the reason reported again after the quiet window", n)
	}
}

// A create that leaves no interface -- refused, or undone because it came back
// untagged -- gives its addresses back to the shared count: the next node can
// still create with the full size.
func TestReservationReturnedWhenCreateLeavesNoInterface(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeNode)
	}{
		{"refused", func(f *fakeNode) { f.createErr = sdkErr("INVALID_SECURITY_GROUP_NOT_FOUND") }},
		{"untagged", func(f *fakeNode) { f.createUntagged = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testReconciler("ipv4")
			r.Subnets = fewSubnets{count: 3, refreshed: time.Unix(1, 0)}
			pass := func(setup func(*fakeNode)) *fakeNode {
				fn := primaryOnly()
				fn.nics[0].secondary = []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"} // primary full
				if setup != nil {
					setup(fn)
				}
				r.reconcileAddresses(&nodeState{api: fn}, zecNode(5), &v1alpha1.ZecNodeStatus{})
				return fn
			}
			pass(tc.setup)
			fn := pass(nil)
			if fn.created != 1 || len(fn.nics[len(fn.nics)-1].secondary) != 2 {
				t.Fatalf("next node created %d; want one interface with 2 addresses, the count given back", fn.created)
			}
		})
	}
}

// failingSubnets cannot refresh its counts.
type failingSubnets struct{ fakeSubnets }

func (failingSubnets) UpdateSubnetsStatus(...factory.UpdateSubnetsStatusOption) error {
	return sdkErr("SERVICE_TEMPORARY_UNAVAILABLE")
}

// A refresh that fails is reported as itself, not as "no subnet has room",
// and does not slow the next look.
func TestSubnetRefreshFailureIsNotNoSubnet(t *testing.T) {
	fn := primaryOnly()
	fn.nics[0].secondary = []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"} // primary full
	r := testReconciler("ipv4")
	r.Subnets = failingSubnets{}
	res := r.reconcileAddresses(&nodeState{api: fn}, zecNode(5), &v1alpha1.ZecNodeStatus{})
	if res.err == nil || errors.Is(res.err, errNoSubnet) || res.waitSubnet || res.reason == reasonNoSubnet {
		t.Fatalf("err %v, reason %q, waitSubnet %v; want the refresh error", res.err, res.reason, res.waitSubnet)
	}
}

// Every spelling of exhaustion counts as full, and quota refusals get the
// quota reason rather than a generic one.
func TestSubnetFullSpellingsAndQuotaReason(t *testing.T) {
	for _, c := range []string{"OPERATION_DENIED_SUBNET_IP_INSUFFICIENT", "INVALID_SUBNET_IPV4_INSUFFICIENT", "InsufficientIpInSubnet"} {
		if !subnetFull(sdkErr(c)) {
			t.Errorf("%s not recognized as a full subnet", c)
		}
	}
	for _, c := range []string{"QuotaExceeded.Eni", "LimitExceeded.EnisPerInstance"} {
		if got := reasonFor(fmt.Errorf("create: %w", sdkErr(c))); got != tracing.EventVpcResourceQuotaExceeded {
			t.Errorf("%s: reason %q, want %s", c, got, tracing.EventVpcResourceQuotaExceeded)
		}
	}
}

// The Node list is a cache read taken before the platform calls; a Node that
// came back under the same name since then keeps its ZecNode.
func TestGCZecNodesRechecksNodeBeforeDelete(t *testing.T) {
	cached := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(verifiedZN("back", "i-x")).Build()
	live := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(liveNode("back")).Build()
	r := &Reconciler{Client: cached, APIReader: live, Lookup: lookupFixture()}
	r.gcZecNodes(context.Background())
	if err := cached.Get(context.Background(), client.ObjectKey{Name: "back"}, &v1alpha1.ZecNode{}); err != nil {
		t.Fatalf("ZecNode of a Node that came back was deleted: %v", err)
	}
}

// batchRefusingNode refuses any assign or create in subnet-1 asking for more
// than one address as subnet-full, and serves single ones.
type batchRefusingNode struct {
	*fakeNode
	assigns []int
}

func (n *batchRefusingNode) AssignIPv4(id string, c int) ([]net.IP, error) {
	n.assigns = append(n.assigns, c)
	if c > 1 {
		return nil, sdkErr("OPERATION_DENIED_SUBNET_IP_INSUFFICIENT")
	}
	return n.fakeNode.AssignIPv4(id, c)
}

func (n *batchRefusingNode) CreateAndAttachENI(subnet, sg string, c int) (*zecApi.NicInfo, error) {
	if c > 2 {
		return nil, sdkErr("OPERATION_DENIED_SUBNET_IP_INSUFFICIENT")
	}
	return n.fakeNode.CreateAndAttachENI(subnet, sg, c)
}

// A batch the subnet cannot cover is refused whole, but a smaller request may
// fit: the subnet is held full only when a single address is refused too.
func TestBatchRefusalRetriesSmallerBeforeHoldingFull(t *testing.T) {
	r := testReconciler("ipv4")
	fn := &batchRefusingNode{fakeNode: primaryOnly()}
	res := r.reconcileAddresses(&nodeState{api: fn}, zecNode(3), &v1alpha1.ZecNodeStatus{})
	if fmt.Sprint(fn.assigns) != "[3 1 1 1]" || fn.created != 0 {
		t.Fatalf("assigns %v, created %d; want the batch of 3, then single addresses on the room the primary has, and no new interface", fn.assigns, fn.created)
	}
	if r.heldFull("subnet-1") {
		t.Fatalf("held full though a single address fitted (err %v)", res.err)
	}

	fn2 := &batchRefusingNode{fakeNode: primaryOnly()}
	fn2.nics[0].secondary = []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"} // primary full
	r.reconcileAddresses(&nodeState{api: fn2}, zecNode(5), &v1alpha1.ZecNodeStatus{})
	if fn2.created != 1 || r.heldFull("subnet-1") {
		t.Fatalf("created %d, held %v; want one interface with a single address, filled by later passes", fn2.created, r.heldFull("subnet-1"))
	}
	for _, nic := range fn2.nics[1:] {
		if len(nic.secondary) != 1 {
			t.Fatalf("interface %s filled with %d; want 1 once a batch was refused", nic.id, len(nic.secondary))
		}
	}
}

// A create that loses the probe to another node's is not exhaustion: no
// NoAvailableSubnet, no Warning, and the node is looked at again soon.
func TestLostProbeIsNotNoSubnet(t *testing.T) {
	r := testReconciler("ipv4")
	r.markFull("subnet-1")
	r.mu.Lock()
	r.fullUntil["subnet-1"] = time.Now().Add(-time.Second)
	r.probing = map[string]bool{"subnet-1": true}
	r.mu.Unlock()
	fn := primaryOnly()
	fn.nics[0].secondary = []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"} // primary full
	res := r.reconcileAddresses(&nodeState{api: fn}, zecNode(5), &v1alpha1.ZecNodeStatus{})
	if res.err != nil || res.waitSubnet || !res.short || fn.created != 0 {
		t.Fatalf("err %v, waitSubnet %v, short %v, created %d; want a quiet short pass", res.err, res.waitSubnet, res.short, fn.created)
	}
}

// A create whose cleanup failed leaves its interface and addresses behind,
// so its reservation stays spent; a definite refusal gives it back.
func TestLeftNothing(t *testing.T) {
	refused := sdkErr("INVALID_SECURITY_GROUP_NOT_FOUND")
	behind := fmt.Errorf("alloc ENI: %w, %w (freeing it also failed): %w", factory.ErrAttachENI, factory.ErrENILeftBehind, sdkErr("X"))
	freed := fmt.Errorf("alloc ENI: %w (the ENI was freed): %w", factory.ErrAttachENI, sdkErr("X"))
	lost := sdkErr("NETWORK_ERROR")
	garbled := sdkErr("JSON_PARSE_FAILED")
	timedOut, serverErr := sdkErr(apiErr.RequestTimedOut), sdkErr(apiErr.InternalServerError)
	freedUncoded := fmt.Errorf("alloc ENI: %w (the ENI was freed): %w", factory.ErrAttachENI, errors.New("attach timed out"))
	for _, tc := range []struct {
		err  error
		want bool
	}{{refused, true}, {behind, false}, {freed, true}, {lost, false}, {garbled, false}, {timedOut, false}, {serverErr, false},
		{freedUncoded, true}, {errors.New("no code"), false}} {
		if got := leftNothing(tc.err); got != tc.want {
			t.Errorf("leftNothing(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// twoSubnets: subnet-1 leads by count, subnet-2 has the given count.
type twoSubnets struct{ second int }

func (twoSubnets) UpdateSubnetsStatus(...factory.UpdateSubnetsStatusOption) error { return nil }

func (twoSubnets) SelectSubnet(types.IPStack, ...factory.UpdateSubnetsStatusOption) *factory.PodSubnet {
	return &factory.PodSubnet{SubnetId: "subnet-1", AvailableIPv4AddressCount: 50}
}

func (s twoSubnets) GetPodSubnet(id string) *factory.PodSubnet {
	if id == "subnet-2" {
		return &factory.PodSubnet{SubnetId: id, AvailableIPv4AddressCount: s.second}
	}
	return nil
}

// With the leader found full, the fallback uses the other subnets' own counts:
// one without room beyond a primary is not chosen.
func TestPickSubnetFallbackUsesCounts(t *testing.T) {
	r := testReconciler("ipv4")
	r.Cfg.Subnets = append(r.Cfg.Subnets, "subnet-2")
	full := map[string]bool{"subnet-1": true}
	r.Subnets = twoSubnets{second: 1}
	if got, _, _, _ := r.pickSubnet(full); got != "" {
		t.Fatalf("picked %q with one free address there", got)
	}
	r.Subnets = twoSubnets{second: 5}
	if got, free, _, _ := r.pickSubnet(full); got != "subnet-2" || free != 5 {
		t.Fatalf("picked %q (free %d); want subnet-2 with its count", got, free)
	}
}
