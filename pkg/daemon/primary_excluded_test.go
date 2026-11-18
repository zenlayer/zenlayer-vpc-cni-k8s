// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"net"
	"slices"
	"testing"

	helper "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/factory"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// podSubnets configures exactly the listed subnets as pod subnets.
type podSubnets struct {
	orderSubnets
	pod map[string]bool
}

func (s podSubnets) IsPodSubnet(id string) bool { return s.pod[id] }

func excludedFactory(primarySubnet string) (*eniIPFactory, *ENI) {
	f := &eniIPFactory{
		eniFactory: &eniFactory{
			ipStack:    types.IPStackIPv4,
			subnets:    podSubnets{pod: map[string]bool{"subnet-pods": true}},
			secManager: initSec{},
			limit: initLimit{lim: helper.InstanceLimits{
				InstanceLimitsAttr: helper.InstanceLimitsAttr{ENIQuota: 2, IPv4MaxPerENI: 15},
			}},
		},
		ipStack: types.IPStackIPv4,
	}
	primary := newTestENI("eni-p")
	primary.IsPrimary = true
	primary.Subnet = zecTypes.Subnet{ID: primarySubnet}
	f.Lock()
	f.appendEniLocked(primary)
	f.Unlock()
	return f, primary
}

// Pod addresses come only from the configured pod subnets. A primary outside
// them takes no order, gives up its share of the node's capacity, and what it
// already holds is Legacy -- not handed out, given back as it is freed --
// from startup, before any GC has looked at it.
func TestDirectPrimaryOutsidePodSubnetsExcluded(t *testing.T) {
	f, primary := excludedFactory("subnet-node")
	held := &ENIIPRes{ENIIP: &zecTypes.ENIIP{ENI: primary.ENI, IPSet: zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.10")}}}
	primary.ips = append(primary.ips, held)

	if primary.forbidAssign&forbidNotPodSubnet == 0 {
		t.Fatalf("forbidAssign %s, want the primary marked outside the pod subnets", primary.forbidAssign)
	}
	if err := f.submitOrder(); err == nil {
		t.Fatal("an order was admitted onto the excluded primary")
	}
	if !errors.Is(f.Valid(held.ENIIP), ErrLegacy) {
		t.Fatalf("Valid = %v, want ErrLegacy so its release gives it back", f.Valid(held.ENIIP))
	}
	if f.Usable(held.ENIIP) {
		t.Fatal("an address on the excluded primary is handed out")
	}
	// (ENIQuota-1+1) * (15-1) = 28, less the primary's 14 not yet held: 1 held
	// still counts until its pod lets it go.
	if got := f.GetResourceLimit(); got != 28-13 {
		t.Fatalf("resource limit %d, want %d", got, 28-13)
	}
}

// The control: a primary in a pod subnet serves as before.
func TestDirectPrimaryInPodSubnetServes(t *testing.T) {
	f, primary := excludedFactory("subnet-pods")
	held := &ENIIPRes{ENIIP: &zecTypes.ENIIP{ENI: primary.ENI, IPSet: zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.10")}}}
	primary.ips = append(primary.ips, held)

	if primary.forbidAssign != forbidNone {
		t.Fatalf("forbidAssign %s, want none", primary.forbidAssign)
	}
	if err := f.Valid(held.ENIIP); err != nil || !f.Usable(held.ENIIP) {
		t.Fatalf("Valid %v, Usable %v; want nil and true", err, f.Usable(held.ENIIP))
	}
	if got := f.GetResourceLimit(); got != 28 {
		t.Fatalf("resource limit %d, want 28", got)
	}
}

// excludedENI is the one test both the startup IPv6 repair and
// appendEniLocked use: any interface outside the pod subnets, primary or not,
// is excluded, and the startup path asks it before any interface is appended,
// so an excluded interface is never given a (billed) prefix.
func TestExcludedENIDecision(t *testing.T) {
	f, _ := excludedFactory("subnet-pods")
	for _, tc := range []struct {
		name string
		eni  *zecTypes.ENI
		want bool
	}{
		{"primary outside the pod subnets", &zecTypes.ENI{ID: "p", IsPrimary: true, Subnet: zecTypes.Subnet{ID: "subnet-node"}}, true},
		{"primary in a pod subnet", &zecTypes.ENI{ID: "p", IsPrimary: true, Subnet: zecTypes.Subnet{ID: "subnet-pods"}}, false},
		{"secondary outside the pod subnets", &zecTypes.ENI{ID: "s", Subnet: zecTypes.Subnet{ID: "subnet-node"}}, true},
		{"secondary in a pod subnet", &zecTypes.ENI{ID: "s", Subnet: zecTypes.Subnet{ID: "subnet-pods"}}, false},
		{"nil", nil, false},
	} {
		if got := f.excludedENI(tc.eni); got != tc.want {
			t.Errorf("%s: excludedENI = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// drainedFactory's API records FreeENI; nothing else of it is reached.
func drainedFactory() (*eniIPFactory, *initAPI) {
	api := &initAPI{}
	f, _ := excludedFactory("subnet-pods")
	f.eniFactory.zecApi = api
	f.eniPending = make(chan struct{}, defaultEniPending)
	return f, api
}

// An interface outside the pod subnets that no pod holds an address on is
// freed by GC, in the background so GC makes no API call; the primary never
// is, and neither is one a pod still uses (Release frees that one with its
// last address).
func TestFreeDrainedExcludedENI(t *testing.T) {
	f, api := drainedFactory()
	old := newTestENI("eni-old")
	old.Subnet = zecTypes.Subnet{ID: "subnet-gone"}
	held := newTestENI("eni-held")
	held.Subnet = zecTypes.Subnet{ID: "subnet-gone"}
	held.ips = append(held.ips, &ENIIPRes{ENIIP: &zecTypes.ENIIP{ENI: held.ENI, IPSet: zecTypes.IPSet{IPv4: net.ParseIP("10.0.2.10")}}})
	f.Lock()
	f.appendEniLocked(old)
	f.appendEniLocked(held)
	var freed []bool
	for _, e := range slices.Clone(f.enis) { // freeing removes it from f.enis
		e.Lock()
		freed = append(freed, f.freeDrainedLocked(e))
		e.Unlock()
	}
	f.Unlock()
	if want := []bool{false, true, false}; !slices.Equal(freed, want) { // primary, eni-old, eni-held
		t.Fatalf("freed %v, want %v", freed, want)
	}
	for range cap(f.eniPending) { // fills only once the background free gave its slot back
		f.eniPending <- struct{}{}
	}
	if !slices.Equal(api.freed, []string{"eni-old"}) || !workerStopped(old) {
		t.Fatalf("FreeENI %v, worker stopped %v; want eni-old freed and stopped", api.freed, workerStopped(old))
	}
	f.RLock()
	defer f.RUnlock()
	for _, e := range f.enis {
		if e == old {
			t.Fatal("eni-old still in the factory")
		}
	}
}

type countingSeedPool struct{ inuse, avail []zecTypes.NetResource }

func (p *countingSeedPool) AddInuse(r zecTypes.NetResource, _ string) { p.inuse = append(p.inuse, r) }
func (p *countingSeedPool) AddInvalid(zecTypes.NetResource)           {}
func (p *countingSeedPool) AddAvailable(r zecTypes.NetResource)       { p.avail = append(p.avail, r) }

// On an excluded interface of a dual node nothing is carved -- it may have no
// prefix at all. A live pod's address is recovered for it. Every other
// unrecorded address is taken IPv4 alone as Legacy on a secondary (this
// plugin's own), for the pool to give back; on the instance's primary it is
// left alone, since it may be an operator's.
func TestSeedExcludedUnrecorded(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stack     types.IPStack
		primary   bool
		wantAvail int
	}{
		{"dual primary", types.IPStackDual, true, 0},
		{"dual secondary", types.IPStackDual, false, 1},
		{"ipv4 primary", types.IPStackIPv4, true, 0},
		{"ipv4 secondary", types.IPStackIPv4, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := excludedFactory("subnet-pods")
			f.ipStack = tc.stack
			e := newTestENI("eni-x")
			e.IsPrimary = tc.primary
			e.Subnet = zecTypes.Subnet{ID: "subnet-gone"}
			f.Lock()
			f.appendEniLocked(e)
			f.Unlock()
			p := &countingSeedPool{}
			v4s := []net.IP{net.ParseIP("10.0.0.10"), net.ParseIP("10.0.0.11")}
			live := map[string]string{"10.0.0.10": "ns/pod"}
			if err := seedENIAddresses(p, e, v4s, tc.stack, nil, live, nil, 28); err != nil {
				t.Fatalf("seed on an excluded interface without a prefix: %v", err)
			}
			if len(p.inuse) != 1 || len(p.avail) != tc.wantAvail {
				t.Fatalf("in use %d, available %d; want 1 and %d", len(p.inuse), len(p.avail), tc.wantAvail)
			}
			for _, r := range e.ips {
				if r.IPSet.IPv6 != nil {
					t.Fatalf("carved %s on an excluded interface", r.IPSet.IPv6)
				}
				if !errors.Is(f.Valid(r.ENIIP), ErrLegacy) {
					t.Fatalf("%s: Valid = %v, want ErrLegacy", r.IPSet.IPv4, f.Valid(r.ENIIP))
				}
			}
		})
	}
}

// gcAPI reports a fixed set of attached interfaces and their addresses.
type gcAPI struct {
	initAPI
	attached []*zecTypes.ENI
}

func (a *gcAPI) GetAttachedENIs() ([]*zecTypes.ENI, error) { return a.attached, nil }

// GC on interfaces outside the pod subnets: on the instance's primary an
// unrecorded address is left alone (it may be an operator's); on this
// plugin's own secondary it is taken as Legacy for the pool to give back, and
// an own secondary holding nothing is freed.
func TestGCExcludedInterfaces(t *testing.T) {
	f, api := drainedFactory()
	gapi := &gcAPI{initAPI: *api}
	f.zecAPI, f.eniFactory.zecApi = gapi, gapi

	primary := f.enis[0]
	primary.Subnet = zecTypes.Subnet{ID: "subnet-node"}
	primary.forbidAssign |= forbidNotPodSubnet
	old := newTestENI("eni-old")
	old.Subnet = zecTypes.Subnet{ID: "subnet-gone"}
	empty := newTestENI("eni-empty")
	empty.Subnet = zecTypes.Subnet{ID: "subnet-gone"}
	f.Lock()
	f.appendEniLocked(old)
	f.appendEniLocked(empty)
	f.Unlock()
	gapi.attached = []*zecTypes.ENI{primary.ENI, old.ENI, empty.ENI}
	gapi.allocatable = func(e *zecTypes.ENI) ([]net.IP, *net.IPNet, error) {
		switch e.ID {
		case "eni-p":
			return []net.IP{net.ParseIP("10.0.0.50")}, nil, nil
		case "eni-old":
			return []net.IP{net.ParseIP("10.0.2.10")}, nil, nil
		}
		return nil, nil, nil
	}

	if err := f.GC(); err != nil {
		t.Fatal(err)
	}
	for range cap(f.eniPending) { // the background free has given its slot back
		f.eniPending <- struct{}{}
	}
	if len(primary.ips) != 0 {
		t.Fatalf("took %v on the excluded primary", primary.ips[0].IPSet)
	}
	if len(old.ips) != 1 || !errors.Is(f.Valid(old.ips[0].ENIIP), ErrLegacy) {
		t.Fatalf("eni-old holds %d; want the unrecorded address taken as Legacy", len(old.ips))
	}
	if !slices.Equal(gapi.freed, []string{"eni-empty"}) {
		t.Fatalf("freed %v, want only eni-empty", gapi.freed)
	}
}

// An excluded secondary that shows up new in a GC pass -- one that dropped out
// of a describe and came back, its records gone with it -- has nothing adopted
// on that pass: what its pods hold would be taken under IDs their records do
// not match, and given back.
func TestGCNoAdoptionOnRebuiltExcludedENI(t *testing.T) {
	f, api := drainedFactory()
	gapi := &gcAPI{initAPI: *api}
	f.zecAPI, f.eniFactory.zecApi = gapi, gapi
	f.eniIpReceiver = make(chan *ENIIPRes, defaultOrderCnt)
	back := &zecTypes.ENI{ID: "eni-back", Subnet: zecTypes.Subnet{ID: "subnet-gone"}}
	gapi.attached = []*zecTypes.ENI{f.enis[0].ENI, back}
	gapi.allocatable = func(e *zecTypes.ENI) ([]net.IP, *net.IPNet, error) {
		if e.ID == "eni-back" {
			return []net.IP{net.ParseIP("10.0.2.10")}, nil, nil
		}
		return []net.IP{net.ParseIP("10.0.0.10")}, nil, nil
	}
	if err := f.GC(); err != nil {
		t.Fatal(err)
	}
	f.RLock()
	defer f.RUnlock()
	for _, e := range f.enis {
		if e.ID == "eni-back" {
			if len(e.ips) != 0 {
				t.Fatalf("adopted %d on the pass that rebuilt the excluded interface", len(e.ips))
			}
			e.stopWorkerOnce()
			return
		}
	}
	t.Fatal("eni-back not in the factory")
}

// With no interface reporting any address, GC reconciles nothing -- but an
// excluded secondary that the platform and the records both show empty is
// still freed; nothing else would ever free it.
func TestGCFreesDrainedExcludedWithNoAddresses(t *testing.T) {
	f, api := drainedFactory()
	gapi := &gcAPI{initAPI: *api}
	f.zecAPI, f.eniFactory.zecApi = gapi, gapi
	empty := newTestENI("eni-empty")
	empty.Subnet = zecTypes.Subnet{ID: "subnet-gone"}
	f.Lock()
	f.appendEniLocked(empty)
	f.Unlock()
	gapi.attached = []*zecTypes.ENI{f.enis[0].ENI, empty.ENI}
	gapi.allocatable = func(*zecTypes.ENI) ([]net.IP, *net.IPNet, error) { return nil, nil, nil }
	if err := f.GC(); err == nil {
		t.Fatal("GC reconciled against an empty answer")
	}
	for range cap(f.eniPending) {
		f.eniPending <- struct{}{}
	}
	if !slices.Equal(gapi.freed, []string{"eni-empty"}) {
		t.Fatalf("freed %v, want eni-empty", gapi.freed)
	}
}
