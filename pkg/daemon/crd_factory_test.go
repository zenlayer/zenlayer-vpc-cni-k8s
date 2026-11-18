// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/apis/network/v1alpha1"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

type fakeMeta struct{ metaV4 map[string]string }

func (m *fakeMeta) GetENI(id, subnet, mac string) (*zecTypes.ENI, error) {
	hw, _ := net.ParseMAC(mac)
	_, cidr, _ := net.ParseCIDR("10.0.0.0/20")
	return &zecTypes.ENI{ID: id, Mac: hw, Subnet: zecTypes.Subnet{ID: subnet,
		Gateway: &zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.1")}, CIDR: &zecTypes.IPNetSet{IPv4: cidr}}}, nil
}

func (m *fakeMeta) MetadataPrimaryIPv4(mac string) (net.IP, error) {
	if ip, ok := m.metaV4[mac]; ok {
		return net.ParseIP(ip), nil
	}
	return nil, errors.New("not found")
}

func (m *fakeMeta) MetadataPrimaryIPv6(string) (net.IP, error) { return nil, errors.New("not found") }

// fakeZN is the API server side of one ZecNode: PatchSpec stores the spec, and
// a test plays the controlplane by changing status.
type fakeZN struct {
	mu      sync.Mutex
	obj     v1alpha1.ZecNode
	patches int
	fail    bool
}

func (z *fakeZN) Get(context.Context) (*v1alpha1.ZecNode, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.obj.DeepCopy(), nil
}

func (z *fakeZN) PatchSpec(_ context.Context, spec v1alpha1.ZecNodeSpec) (int64, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.fail {
		return 0, errors.New("apiserver down")
	}
	z.patches++
	z.obj.Spec = *spec.DeepCopy()
	z.obj.Generation++
	return z.obj.Generation, nil
}

func (z *fakeZN) spec() v1alpha1.ZecNodeSpec {
	z.mu.Lock()
	defer z.mu.Unlock()
	return *z.obj.Spec.DeepCopy()
}

func readyStatus(enis ...v1alpha1.ENIStatus) v1alpha1.ZecNodeStatus {
	return v1alpha1.ZecNodeStatus{
		Capacity: v1alpha1.Capacity{Allocatable: 9},
		Scaler:   v1alpha1.Scaler{Target: 2},
		ENIs:     enis,
		Conditions: []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Ready", LastTransitionTime: metav1.Now()}},
	}
}

func eniStatus(id, mac, primary string, ips ...string) v1alpha1.ENIStatus {
	es := v1alpha1.ENIStatus{ID: id, MAC: mac, PrimaryIPv4: primary, SubnetID: "subnet-1", State: v1alpha1.ENIStateReady}
	for _, ip := range ips {
		es.IPv4 = append(es.IPv4, v1alpha1.IPAddr{IP: ip, State: v1alpha1.IPStateReady})
	}
	return es
}

func newTestFactory(t *testing.T, stack types.IPStack, st v1alpha1.ZecNodeStatus) (*crdIPFactory, *fakeZN) {
	t.Helper()
	zn := &fakeZN{}
	zn.obj.Status = st
	f := newCRDIPFactory(stack, v1alpha1.IdentityClaim{InstanceID: "i-1"}, &fakeMeta{metaV4: map[string]string{}}, zn)
	f.onStatus(zn.obj.DeepCopy())
	f.syncENIs()
	f.seeded = true
	return f, zn
}

// setStatus plays the controlplane, which computed st from the current spec.
func setStatus(f *crdIPFactory, zn *fakeZN, st v1alpha1.ZecNodeStatus) {
	zn.mu.Lock()
	st.ObservedGeneration = zn.obj.Generation
	zn.obj.Status = st
	obj := zn.obj.DeepCopy()
	zn.mu.Unlock()
	f.onStatus(obj)
	f.syncENIs()
}

func TestCRDCreateTakesNewAddresses(t *testing.T) {
	f, zn := newTestFactory(t, types.IPStackIPv4, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2")))
	go func() {
		// The controlplane fills the request a moment later.
		time.Sleep(50 * time.Millisecond)
		if zn.spec().IPv4.Requested != 2 {
			t.Errorf("requested %d, want 2", zn.spec().IPv4.Requested)
		}
		setStatus(f, zn, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10", "10.0.0.11")))
	}()
	res, err := f.Create(2)
	if err != nil || len(res) != 2 {
		t.Fatalf("got %d resources, err %v", len(res), err)
	}
	if got := zn.spec().IPv4.Requested; got != 2 {
		t.Fatalf("after Create requested %d, want what the pool holds (2)", got)
	}
}

// The interface's own primary, by either source, never enters the pool.
func TestCRDNeverPoolsPrimaries(t *testing.T) {
	st := readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.2", "10.0.0.9", "10.0.0.10"))
	zn := &fakeZN{}
	zn.obj.Status = st
	f := newCRDIPFactory(types.IPStackIPv4, v1alpha1.IdentityClaim{InstanceID: "i-1"},
		&fakeMeta{metaV4: map[string]string{"02:00:00:00:00:aa": "10.0.0.9"}}, zn)
	f.onStatus(zn.obj.DeepCopy())
	f.syncENIs()
	f.seeded = true
	f.mu.Lock()
	got := f.takeNewLocked(10)
	f.mu.Unlock()
	if len(got) != 1 || got[0].IPSet.IPv4.String() != "10.0.0.10" {
		t.Fatalf("pooled %v, want only 10.0.0.10", got)
	}
}

// Release puts the address in releasing; it is never handed out again
// while there, and leaves the list once status drops it.
func TestCRDReleaseAndGC(t *testing.T) {
	st := readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10", "10.0.0.11"))
	f, zn := newTestFactory(t, types.IPStackIPv4, st)
	f.mu.Lock()
	got := f.takeNewLocked(2)
	f.mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("took %d", len(got))
	}
	if err := f.Release(got[0]); err != nil {
		t.Fatal(err)
	}
	spec := zn.spec()
	if len(spec.IPv4.Releasing) != 1 || spec.IPv4.Releasing[0] != got[0].IPSet.IPv4.String() || spec.IPv4.Requested != 1 {
		t.Fatalf("spec after release %+v", spec.IPv4)
	}
	// Still on the platform: GC must not adopt it back.
	if err := f.GC(); err != nil {
		t.Fatal(err)
	}
	if err := f.Valid(got[0]); !errors.Is(err, apiErr.ErrNotFound) {
		t.Fatalf("released address readopted: %v", err)
	}
	// The controlplane unassigns it.
	setStatus(f, zn, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", got[1].IPSet.IPv4.String())))
	_ = f.GC()
	f.mu.RLock()
	left := len(f.releasing)
	f.mu.RUnlock()
	if left != 0 {
		t.Fatalf("releasing not cleared after the address left status")
	}
}

// A failed spec write does not put the address back: the write may have
// landed, so the address stays out and the spec is retried in the background.
func TestCRDReleaseNeverRepoolsOnWriteFailure(t *testing.T) {
	f, zn := newTestFactory(t, types.IPStackIPv4, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10")))
	f.mu.Lock()
	got := f.takeNewLocked(1)
	f.mu.Unlock()
	zn.fail = true
	if err := f.Release(got[0]); err != nil {
		t.Fatalf("release failed: %v", err)
	}
	if err := f.Valid(got[0]); !errors.Is(err, apiErr.ErrNotFound) {
		t.Fatalf("released address still pooled: %v", err)
	}
	f.mu.RLock()
	dirty, rel := f.specDirty, f.releasing["10.0.0.10"]
	f.mu.RUnlock()
	if !dirty || !rel {
		t.Fatalf("dirty %v releasing %v", dirty, rel)
	}
	_ = f.GC()
	f.mu.Lock()
	again := f.takeNewLocked(5)
	f.mu.Unlock()
	if len(again) != 0 {
		t.Fatalf("released address re-adopted: %v", again)
	}
}

// Create's request must survive status updates that arrive while it waits.
func TestCRDCreateRequestNotWithdrawn(t *testing.T) {
	f, zn := newTestFactory(t, types.IPStackIPv4, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2")))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.Create(3)
	}()
	time.Sleep(30 * time.Millisecond)
	// A status update (e.g. a condition change) arrives; runSync writes.
	setStatus(f, zn, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2")))
	_ = f.writeSpec(context.Background())
	if got := zn.spec().IPv4.Requested; got != 3 {
		t.Fatalf("requested %d while Create waits, want 3", got)
	}
	setStatus(f, zn, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10", "10.0.0.11", "10.0.0.12")))
	<-done
}

// An interface absent from status is kept through the grace; an empty status
// (a recreated object) changes nothing; once dropped, its addresses are never
// adopted again.
func TestCRDInterfaceDropIsGracefulAndRetires(t *testing.T) {
	st := readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2"), eniStatus("eni-2", "02:00:00:00:00:bb", "10.0.0.20", "10.0.0.21"))
	f, zn := newTestFactory(t, types.IPStackIPv4, st)
	_ = f.GC()
	// Empty status: ignored.
	setStatus(f, zn, v1alpha1.ZecNodeStatus{})
	_ = f.GC()
	f.mu.RLock()
	n := len(f.enis)
	f.mu.RUnlock()
	if n != 2 {
		t.Fatalf("empty status dropped interfaces: %d left", n)
	}
	// eni-2 gone for real: kept within the grace, dropped after.
	setStatus(f, zn, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2")))
	_ = f.GC()
	if l, _ := f.List(); len(l[zecTypes.ResStatusNormal]) != 1 {
		t.Fatalf("address invalidated within the grace: %v", l)
	}
	f.mu.Lock()
	f.eniMissingSince["eni-2"] = time.Now().Add(-2 * crdMissingGrace)
	f.mu.Unlock()
	_ = f.GC()
	// It comes back: its address must not be adopted as free.
	setStatus(f, zn, st)
	_ = f.GC()
	if l, _ := f.List(); len(l[zecTypes.ResStatusNormal]) != 0 {
		t.Fatalf("retired address re-adopted: %v", l)
	}
}

// An address on an interface status stopped listing is not handed out during
// the grace (the plugin cannot find the interface), and is again once it is
// listed again.
func TestCRDUsableWhileInterfaceMissing(t *testing.T) {
	st := readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2"), eniStatus("eni-2", "02:00:00:00:00:bb", "10.0.0.20", "10.0.0.21"))
	f, zn := newTestFactory(t, types.IPStackIPv4, st)
	_ = f.GC()
	var res zecTypes.NetResource
	l, _ := f.List()
	for _, r := range l[zecTypes.ResStatusNormal] {
		if r.(*zecTypes.ENIIP).ENI.ID == "eni-2" {
			res = r
		}
	}
	if res == nil {
		t.Fatalf("no address adopted on eni-2: %v", l)
	}
	if !f.Usable(res) {
		t.Fatal("listed interface reported unusable")
	}
	setStatus(f, zn, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2")))
	if f.Usable(res) {
		t.Fatal("address on a missing interface reported usable")
	}
	setStatus(f, zn, st)
	if !f.Usable(res) {
		t.Fatal("interface listed again but its address still unusable")
	}
	// The address alone missing from status: not handed out either.
	setStatus(f, zn, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2"),
		eniStatus("eni-2", "02:00:00:00:00:bb", "10.0.0.20")))
	_ = f.GC()
	if ip := res.(*zecTypes.ENIIP).IPSet.IPv4.String(); ip == "10.0.0.21" && f.Usable(res) {
		t.Fatal("address missing from status reported usable")
	}
	// Dropped after the grace: unusable for good.
	setStatus(f, zn, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2")))
	f.mu.Lock()
	f.eniMissingSince["eni-2"] = time.Now().Add(-2 * crdMissingGrace)
	f.mu.Unlock()
	_ = f.GC()
	if f.Usable(res) {
		t.Fatal("address on a dropped interface reported usable")
	}
}

// GC adopts what status reports and the pool does not hold.
func TestCRDGCAdopts(t *testing.T) {
	f, _ := newTestFactory(t, types.IPStackIPv4, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10")))
	_ = f.GC()
	list, _ := f.List()
	if len(list[zecTypes.ResStatusNormal]) != 1 {
		t.Fatalf("adopted %d, want 1", len(list[zecTypes.ResStatusNormal]))
	}
}

// An address missing from status is not invalid at once, only after the
// grace.
func TestCRDMissingAddressGrace(t *testing.T) {
	f, zn := newTestFactory(t, types.IPStackIPv4, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10")))
	_ = f.GC()
	setStatus(f, zn, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2")))
	_ = f.GC()
	list, _ := f.List()
	if len(list[zecTypes.ResStatusNormal]) != 1 || len(list[zecTypes.ResStatusInvalid]) != 0 {
		t.Fatalf("missing address not kept through the grace: %v", list)
	}
	f.mu.Lock()
	f.missingSince["10.0.0.10"] = time.Now().Add(-2 * crdMissingGrace)
	f.mu.Unlock()
	_ = f.GC()
	list, _ = f.List()
	if len(list[zecTypes.ResStatusInvalid]) != 1 {
		t.Fatalf("missing address not invalid after the grace: %v", list)
	}
	for _, r := range list[zecTypes.ResStatusInvalid] {
		if err := f.Valid(r); !errors.Is(err, apiErr.ErrNotFound) {
			t.Fatalf("not marked gone after the grace: %v", err)
		}
	}
}

// A spec that differs from local state -- the ZecNode recreated empty --
// is marked for rewriting.
func TestCRDRewritesLostSpec(t *testing.T) {
	f, zn := newTestFactory(t, types.IPStackIPv4, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10")))
	_ = f.GC()
	_ = f.writeSpec(context.Background())
	zn.mu.Lock()
	zn.obj.Spec = v1alpha1.ZecNodeSpec{}
	obj := zn.obj.DeepCopy()
	zn.mu.Unlock()
	f.onStatus(obj)
	f.mu.RLock()
	dirty := f.specDirty
	f.mu.RUnlock()
	if !dirty {
		t.Fatal("lost spec not noticed")
	}
}

// Dual: every pooled address carries an IPv6 from its interface's prefix, and
// an interface whose prefix has not arrived contributes nothing yet.
func TestCRDDualPairsFromPrefix(t *testing.T) {
	es := eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10")
	f, zn := newTestFactory(t, types.IPStackDual, readyStatus(es))
	f.mu.Lock()
	if got := f.takeNewLocked(5); len(got) != 0 {
		f.mu.Unlock()
		t.Fatalf("paired without a prefix: %v", got)
	}
	f.mu.Unlock()
	es.IPv6 = &v1alpha1.IPv6Prefix{Prefix: "2001:db8::/96", Primary: "2001:db8::1"}
	setStatus(f, zn, readyStatus(es))
	f.mu.Lock()
	got := f.takeNewLocked(5)
	f.mu.Unlock()
	if len(got) != 1 || got[0].IPSet.IPv6 == nil || got[0].IPSet.IPv6.String() == "2001:db8::1" {
		t.Fatalf("pairing %v", got)
	}
}

func TestCRDResourceLimitFromStatus(t *testing.T) {
	f, _ := newTestFactory(t, types.IPStackIPv4, readyStatus())
	if f.GetResourceLimit() != 9 {
		t.Fatalf("limit %d", f.GetResourceLimit())
	}
}

// A settled release stays out of the pool until a status computed from the
// spec that dropped it arrives: a pass that read the older spec may still
// unassign it after handing it back.
func TestCRDUnsettledWaitsForObservedGeneration(t *testing.T) {
	f, zn := newTestFactory(t, types.IPStackIPv4, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10")))
	f.mu.Lock()
	got := f.takeNewLocked(1)
	f.mu.Unlock()
	if err := f.Release(got[0]); err != nil {
		t.Fatal(err)
	}
	// Released by the controlplane, then the daemon writes the spec without it.
	setStatus(f, zn, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2")))
	if err := f.writeSpec(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A pass that read the older spec hands the address straight back.
	stale := readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10"))
	zn.mu.Lock()
	stale.ObservedGeneration = zn.obj.Generation - 1
	zn.obj.Status = stale
	obj := zn.obj.DeepCopy()
	zn.mu.Unlock()
	f.onStatus(obj)
	_ = f.GC()
	if l, _ := f.List(); len(l[zecTypes.ResStatusNormal]) != 0 {
		t.Fatalf("adopted before the controlplane saw the settled spec: %v", l)
	}
	// Status computed from the current spec: now it may be taken.
	setStatus(f, zn, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10")))
	_ = f.GC()
	if l, _ := f.List(); len(l[zecTypes.ResStatusNormal]) != 1 {
		t.Fatalf("not adopted once settled: %v", l)
	}
}

// A held record whose address status dropped is not asked for again:
// requested stays at what the controlplane can count.
func TestCRDRequestedIgnoresDroppedRecords(t *testing.T) {
	f, zn := newTestFactory(t, types.IPStackIPv4, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10", "10.0.0.11")))
	_ = f.GC()
	setStatus(f, zn, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.11")))
	f.mu.RLock()
	req := f.desiredSpecLocked().IPv4.Requested
	f.mu.RUnlock()
	if req != 1 {
		t.Fatalf("requested %d, want 1", req)
	}
}

// Something missing past the grace makes GC due, so it is woken instead of
// waiting for its long period.
func TestCRDGCDueAfterGrace(t *testing.T) {
	f, zn := newTestFactory(t, types.IPStackIPv4, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10")))
	_ = f.GC()
	setStatus(f, zn, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2")))
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gcDueLocked(time.Now()) {
		t.Fatal("due within the grace")
	}
	if !f.gcDueLocked(time.Now().Add(2 * crdMissingGrace)) {
		t.Fatal("not due after the grace, though onStatus saw the address go")
	}
}

// Right after a Create that got nothing, a queued one fails at once with the
// condition's reason and writes no spec; an address that arrived is still taken.
func TestCRDCreateFailsFastAfterRefusal(t *testing.T) {
	st := readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2"))
	st.Conditions = append(st.Conditions, metav1.Condition{Type: v1alpha1.ConditionIPAllocationFailed,
		Status: metav1.ConditionTrue, Reason: "AttachENIFailed", Message: "refused", LastTransitionTime: metav1.Now()})
	f, zn := newTestFactory(t, types.IPStackIPv4, st)
	f.failedAt = time.Now()
	patches := zn.patches
	start := time.Now()
	_, err := f.Create(1)
	if err == nil || !strings.Contains(err.Error(), "AttachENIFailed") || time.Since(start) > time.Second {
		t.Fatalf("err %v after %s; want an immediate error naming the condition", err, time.Since(start))
	}
	if zn.patches != patches {
		t.Fatalf("%d spec writes; a fast failure must not ask again", zn.patches-patches)
	}
	st.ENIs = []v1alpha1.ENIStatus{eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10")}
	setStatus(f, zn, st)
	if res, err := f.Create(1); err != nil || len(res) != 1 {
		t.Fatalf("got %d, err %v; an address already here is still handed out", len(res), err)
	}
}

// An Excluded interface (outside the pod subnets) serves no new pod: nothing on
// it is handed out or counted as held by the controlplane, and every address
// on it is Legacy -- an unrecorded one is taken as such -- so the pool gives
// an idle one back and an in-use one when its pod releases it. Excluded
// lifted, it serves again.
func TestCRDExcludedInterfaceDrains(t *testing.T) {
	excluded := func(ips ...string) v1alpha1.ENIStatus {
		es := eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", ips...)
		es.State = v1alpha1.ENIStateExcluded
		return es
	}
	// Held before the primary was excluded.
	f, zn := newTestFactory(t, types.IPStackIPv4, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10")))
	_ = f.GC()
	held, _ := f.List()
	if len(held[zecTypes.ResStatusNormal]) != 1 {
		t.Fatalf("held %d before exclusion, want 1", len(held[zecTypes.ResStatusNormal]))
	}
	var res zecTypes.NetResource
	for _, r := range held[zecTypes.ResStatusNormal] {
		res = r
	}

	// Excluded, with one more address on it the pool has never held.
	setStatus(f, zn, readyStatus(excluded("10.0.0.10", "10.0.0.11")))
	// Until a GC has run, runSync keeps asking for one: a single wake can be
	// dropped or debounced, and the idle addresses on it would wait for the
	// periodic GC while still counted in the limit below.
	f.mu.RLock()
	due := f.gcDueLocked(time.Now())
	f.mu.RUnlock()
	if !due {
		t.Fatal("GC not due after an interface became Excluded")
	}
	_ = f.GC()
	f.mu.RLock()
	due = f.gcDueLocked(time.Now())
	f.mu.RUnlock()
	if due {
		t.Fatal("GC still due after it ran")
	}
	list, _ := f.List()
	if len(list[zecTypes.ResStatusNormal]) != 0 || len(list[zecTypes.ResStatusLegacy]) != 2 {
		t.Fatalf("normal %d, legacy %d; want 0 and 2 (10.0.0.11 taken as Legacy to be given back)",
			len(list[zecTypes.ResStatusNormal]), len(list[zecTypes.ResStatusLegacy]))
	}
	if f.Usable(res) {
		t.Fatal("an address on an Excluded interface is still handed out")
	}
	if !errors.Is(f.Valid(res), ErrLegacy) {
		t.Fatalf("Valid = %v, want ErrLegacy so a release gives it back", f.Valid(res))
	}
	f.mu.RLock()
	n := f.heldLocked()
	f.mu.RUnlock()
	if n != 0 {
		t.Fatalf("held %d on an Excluded interface, want 0", n)
	}
	// allocatable leaves the interface out, while what is held on it still
	// counts as used: it is added back, or the room for new pods would shrink
	// twice. The idle one counts until the pool gives it back.
	if got := f.GetResourceLimit(); got != 9+2 {
		t.Fatalf("resource limit %d, want %d", got, 9+2)
	}
	f.mu.Lock()
	again := f.markExcludedLocked()
	f.mu.Unlock()
	if again {
		t.Fatal("markExcludedLocked reported an already marked address as new, waking GC on every status")
	}

	// The subnet becomes a pod subnet again.
	setStatus(f, zn, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10", "10.0.0.11")))
	if err := f.Valid(res); err != nil || !f.Usable(res) {
		t.Fatalf("after Excluded is lifted: Valid %v, Usable %v", err, f.Usable(res))
	}

	// Excluded again, its pod goes. Giving the address back lowers the limit,
	// which no status change reports: the device plugin is told directly.
	setStatus(f, zn, readyStatus(excluded("10.0.0.10", "10.0.0.11")))
	// Giving it back lowers the limit, which no status change reports: the
	// device plugin is told directly.
	told := make(chan struct{}, 1)
	f.setCallbacks(nil, func() { told <- struct{}{} })
	if _, err := f.ReleaseInValid(res); err != nil {
		t.Fatal(err)
	}
	if got := f.GetResourceLimit(); got != 9+1 {
		t.Fatalf("after release: limit %d, want 10", got)
	}
	select {
	case <-told:
	case <-time.After(5 * time.Second):
		t.Fatal("capacity watchers not told after the release")
	}
}

// An unrecorded address on an Excluded primary is never taken: it may be an
// operator's (a VIP, or one with an EIP bound), and giving it back would take
// it off the instance's own interface. On a secondary it is taken to be given
// back.
func TestCRDExcludedPrimaryUnrecordedLeftAlone(t *testing.T) {
	excluded := func(id, mac, primaryIP string, primary bool, ips ...string) v1alpha1.ENIStatus {
		es := eniStatus(id, mac, primaryIP, ips...)
		es.State, es.Primary = v1alpha1.ENIStateExcluded, primary
		return es
	}
	f, _ := newTestFactory(t, types.IPStackIPv4, readyStatus(
		excluded("eni-p", "02:00:00:00:00:aa", "10.0.0.2", true, "10.0.0.50"),
		excluded("eni-s", "02:00:00:00:00:bb", "10.0.0.3", false, "10.0.0.60"),
	))
	_ = f.GC()
	f.mu.RLock()
	defer f.mu.RUnlock()
	if n := len(f.enis["eni-p"].ips); n != 0 {
		t.Fatalf("took %d unrecorded address(es) on the Excluded primary", n)
	}
	if n := len(f.enis["eni-s"].ips); n != 1 {
		t.Fatalf("took %d on the Excluded secondary, want its 1 to give back", n)
	}
}

// On a dual node an address taken IPv4 alone while its interface was Excluded
// stays Legacy when the interface is a pod interface again: a half set must
// never reach a pod.
func TestCRDDualUnexcludedHalfSetStaysLegacy(t *testing.T) {
	es := eniStatus("eni-s", "02:00:00:00:00:bb", "10.0.0.3", "10.0.0.60")
	es.IPv6 = &v1alpha1.IPv6Prefix{Prefix: "2001:db8::/96", Primary: "2001:db8::1"}
	es.State = v1alpha1.ENIStateExcluded
	f, zn := newTestFactory(t, types.IPStackDual, readyStatus(es))
	_ = f.GC()
	f.mu.RLock()
	if len(f.enis["eni-s"].ips) != 1 {
		f.mu.RUnlock()
		t.Fatal("the unrecorded address on the Excluded secondary was not taken")
	}
	res := f.enis["eni-s"].ips[0].ENIIP
	f.mu.RUnlock()

	es.State = v1alpha1.ENIStateReady
	setStatus(f, zn, readyStatus(es))
	if err := f.Valid(res); !errors.Is(err, ErrLegacy) {
		t.Fatalf("Valid = %v for an IPv4-only set on a dual node, want ErrLegacy", err)
	}
}

// An interface the controlplane does not count -- on a dual node, one refused
// an IPv6 prefix -- has its share left out of allocatable, while what is held
// there still counts as used: it is added back, as on an Excluded one.
func TestCRDLimitAddsBackHeldOnPrefixlessInterface(t *testing.T) {
	es := eniStatus("eni-s", "02:00:00:00:00:bb", "10.0.0.3", "10.0.0.60")
	f, _ := newTestFactory(t, types.IPStackDual, readyStatus(es))
	f.mu.Lock()
	e := f.enis["eni-s"]
	e.Lock()
	e.ips = append(e.ips, &ENIIPRes{ENIIP: &zecTypes.ENIIP{ENI: e.ENI, IPSet: zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.60")}}})
	e.Unlock()
	f.mu.Unlock()
	if got := f.GetResourceLimit(); got != 9+1 {
		t.Fatalf("limit %d, want %d: the address held on the prefixless interface counts as used", got, 9+1)
	}
}

// An address this node will never take (retired, or an interface's own by the
// metadata service's account) fills a slot allocatable counts as free.
func TestCRDLimitLessRefused(t *testing.T) {
	f, _ := newTestFactory(t, types.IPStackIPv4, readyStatus(
		eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10", "10.0.0.11")))
	f.mu.Lock()
	f.retired["10.0.0.11"] = true
	f.mu.Unlock()
	if got := f.GetResourceLimit(); got != 9-1 {
		t.Fatalf("limit %d, want %d", got, 9-1)
	}
}

// A recreated ZecNode whose status is not filled yet keeps the last interface
// view, but its generation and conditions are the new object's: a verdict and
// a generation from the deleted object must not be read as current.
func TestCRDRecreatedObjectTakesItsOwnConditions(t *testing.T) {
	st := readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10"))
	st.ObservedGeneration = 40
	st.Conditions = append(st.Conditions, metav1.Condition{Type: v1alpha1.ConditionInstanceVerified,
		Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonVerified, LastTransitionTime: metav1.Now()})
	f, zn := newTestFactory(t, types.IPStackIPv4, st)

	zn.mu.Lock()
	zn.obj.UID = "new-uid"
	zn.obj.ResourceVersion = "999"
	zn.obj.Generation = 2
	zn.obj.Status = v1alpha1.ZecNodeStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{{
		Type: v1alpha1.ConditionInstanceVerified, Status: metav1.ConditionFalse,
		Reason: v1alpha1.ReasonIdentityMismatch, LastTransitionTime: metav1.Now()}}}
	obj := zn.obj.DeepCopy()
	zn.mu.Unlock()
	f.onStatus(obj)

	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration %d; want the new object's 1", f.status.ObservedGeneration)
	}
	if c := meta.FindStatusCondition(f.status.Conditions, v1alpha1.ConditionInstanceVerified); c == nil ||
		c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonIdentityMismatch {
		t.Errorf("InstanceVerified %+v; want the new object's IdentityMismatch", c)
	}
	if len(f.status.ENIs) != 1 || f.status.Capacity.Allocatable != 9 || f.status.Scaler.Target != 2 {
		t.Errorf("lost the last view: %d ENIs, capacity %+v, scaler %+v", len(f.status.ENIs), f.status.Capacity, f.status.Scaler)
	}
}

// An interface whose metadata names a primary other than the platform's takes
// that address out of the limit and into the request when it is built: the
// device plugin and the spec hear of it then, not at some later event.
func TestCRDMetadataPrimaryMovesLimitAndSpec(t *testing.T) {
	f, zn := newTestFactory(t, types.IPStackIPv4, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10")))
	told := 0
	f.setCallbacks(nil, func() { told++ })
	f.meta.(*fakeMeta).metaV4["02:00:00:00:00:bb"] = "10.0.0.21"
	before := f.GetResourceLimit()
	f.mu.Lock()
	f.specDirty = false
	f.mu.Unlock()
	setStatus(f, zn, readyStatus(
		eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10"),
		eniStatus("eni-s", "02:00:00:00:00:bb", "10.0.0.20", "10.0.0.21")))
	if got := f.GetResourceLimit(); got != before-1 {
		t.Fatalf("limit %d, want %d: the metadata primary is not the pool's", got, before-1)
	}
	f.mu.RLock()
	dirty := f.specDirty
	f.mu.RUnlock()
	if told == 0 || !dirty {
		t.Fatalf("onCapacity calls %d, specDirty %v; want both", told, dirty)
	}
}

// An address status now lists under another interface is gone from the one
// its record names: it is stamped missing there and not counted as held, so
// it stops being handed out with the old interface's MAC and route.
func TestCRDAddressMovedToAnotherInterfaceIsMissing(t *testing.T) {
	p := eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2", "10.0.0.10")
	f, zn := newTestFactory(t, types.IPStackIPv4, readyStatus(p, eniStatus("eni-s", "02:00:00:00:00:bb", "10.0.0.20")))
	_ = f.GC()
	setStatus(f, zn, readyStatus(eniStatus("eni-p", "02:00:00:00:00:aa", "10.0.0.2"),
		eniStatus("eni-s", "02:00:00:00:00:bb", "10.0.0.20", "10.0.0.10")))
	f.mu.RLock()
	_, stamped := f.missingSince["10.0.0.10"]
	held := f.heldLocked()
	f.mu.RUnlock()
	if !stamped {
		t.Fatal("an address moved to another interface was not stamped missing on its record's")
	}
	if held != 0 {
		t.Fatalf("held %d; the moved address is not held on the interface its record names", held)
	}
}
