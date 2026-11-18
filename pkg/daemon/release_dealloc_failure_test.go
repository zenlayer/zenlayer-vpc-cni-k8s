// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"net"
	"testing"

	k8sErr "k8s.io/apimachinery/pkg/util/errors"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// halfwayAPI fails the deallocate with whatever the test supplies.
type halfwayAPI struct {
	*initAPI
	deallocErr error
	deallocs   int
}

func (a *halfwayAPI) DeallocIPv4Addresses(string, string, []net.IP) error {
	a.deallocs++
	return a.deallocErr
}

// releaseFixture wires a factory holding one interface with two recorded
// addresses, and returns the resource naming the first of them.
//
// Two addresses, not one: releasing the last address of a CNI-created interface
// takes a different branch entirely -- it gives up the whole interface -- and
// that is not the branch under test.
func releaseFixture(t *testing.T, api *halfwayAPI) (*eniIPFactory, *ENI, *zecTypes.ENIIP) {
	t.Helper()
	mac, err := net.ParseMAC("aa:bb:cc:dd:ee:03")
	if err != nil {
		t.Fatalf("parse mac: %v", err)
	}
	vpcENI := &zecTypes.ENI{
		ID:        "eni-1",
		Mac:       mac,
		PrimaryIP: zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.5")},
	}
	held := &zecTypes.ENIIP{ENI: vpcENI, IPSet: zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.7")}}
	other := &zecTypes.ENIIP{ENI: vpcENI, IPSet: zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.8")}}

	eni := &ENI{
		ENI: vpcENI,
		ips: []*ENIIPRes{{ENIIP: held}, {ENIIP: other}},
	}
	f := &eniIPFactory{
		eniFactory:    &eniFactory{ipStack: types.IPStackIPv4, zecApi: api, limit: initLimit{}},
		zecAPI:        api,
		ipStack:       types.IPStackIPv4,
		eniIpReceiver: make(chan *ENIIPRes, 8),
		eniPending:    make(chan struct{}, 1),
	}
	f.enis = append(f.enis, eni)
	return f, eni, &zecTypes.ENIIP{ENI: vpcENI, IPSet: zecTypes.IPSet{IPv4: net.ParseIP("10.0.0.7")}}
}

// TestReleaseKeepsRecordOnDeallocFailure pins that a failed deallocate always
// leaves the local record in place.
//
// The platform releases every address of one interface in a single call, so the
// operation is all-or-nothing: an error means nothing was released. Dropping the
// record then leaks the address -- still assigned on the platform, with nothing
// on this node referring to it, and no later pass able to find it.
//
// There is deliberately no error the caller treats as a partial success. An
// earlier version carried a sentinel for that, matched with errors.Is; with a
// single-part deallocate the sentinel could never be produced, so the branch
// only invited a future substring match on the message -- which would read any
// unrelated error whose wording happened to contain the phrase as a success.
// The second case below stands guard over exactly that.
//
// The success path is covered separately: only a nil error may delete a record.
func TestReleaseKeepsRecordOnDeallocFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantErr  bool
		wantKept int
		why      string
	}{
		{
			name:     "an aggregate of platform errors keeps the record",
			err:      k8sErr.NewAggregate([]error{errors.New("unassign refused on one address")}),
			wantErr:  true,
			wantKept: 2,
			why:      "nothing was released, so the record is the only thing still naming the address",
		},
		{
			name:     "an error whose wording suggests partial progress keeps it too",
			err:      errors.New("dealloc aborted: process halfway failed to reach the metadata service"),
			wantErr:  true,
			wantKept: 2,
			why: "reading a message as a partial success deletes the local record while the platform " +
				"still holds the address -- a leak nothing on this node can find again",
		},
		{
			name:     "an ordinary failure keeps the record",
			err:      errors.New("unassign refused"),
			wantErr:  true,
			wantKept: 2,
			why:      "the address is still assigned; dropping the record leaks it",
		},
		{
			name:     "success drops the record",
			err:      nil,
			wantErr:  false,
			wantKept: 1,
			why:      "the address is gone from the platform, so keeping the record strands it in the pool",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &halfwayAPI{initAPI: &initAPI{}, deallocErr: tc.err}
			f, eni, res := releaseFixture(t, api)

			err := f.Release(res)
			if (err != nil) != tc.wantErr {
				t.Errorf("Release() error = %v, want error: %v -- %s", err, tc.wantErr, tc.why)
			}
			eni.Lock()
			kept := len(eni.ips)
			eni.Unlock()
			if kept != tc.wantKept {
				t.Errorf("interface holds %d recorded addresses after Release, want %d -- %s",
					kept, tc.wantKept, tc.why)
			}
		})
	}
}

// An address the pool gives back from an interface taking no new addresses
// lowers the limit, so the daemon is told to recount; one from a usable
// interface does not move it.
func TestReleaseInValidOnForbiddenInterfaceTellsCapacity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		forbid forbidReason
		want   int
	}{{"excluded", forbidNotPodSubnet, 1}, {"usable", forbidNone, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			api := &halfwayAPI{initAPI: &initAPI{}}
			f, eni, res := releaseFixture(t, api)
			eni.forbidAssign = tc.forbid
			told := 0
			fn := func() { told++ }
			f.onCapacity.Store(&fn)
			if _, err := f.ReleaseInValid(res); err != nil {
				t.Fatal(err)
			}
			if told != tc.want {
				t.Fatalf("told %d times, want %d", told, tc.want)
			}
		})
	}
}

// The pool's shrink gives addresses back through Release: on an interface
// taking no new addresses that lowers the limit too.
func TestReleaseOnForbiddenInterfaceTellsCapacity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		forbid forbidReason
		want   int
	}{{"excluded", forbidNotPodSubnet, 1}, {"usable", forbidNone, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			api := &halfwayAPI{initAPI: &initAPI{}}
			f, eni, res := releaseFixture(t, api)
			eni.forbidAssign = tc.forbid
			told := 0
			fn := func() { told++ }
			f.onCapacity.Store(&fn)
			if err := f.Release(res); err != nil {
				t.Fatal(err)
			}
			if told != tc.want {
				t.Fatalf("told %d times, want %d", told, tc.want)
			}
		})
	}
}
