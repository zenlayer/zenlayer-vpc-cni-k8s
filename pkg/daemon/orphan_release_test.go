// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/k8s"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/pool"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// ownerMgr is a ResourceManager whose pool holds addresses for known owners and
// records what was released.
type ownerMgr struct {
	held     map[string][]zecTypes.ResourceItem
	released []string
	// Keyed by resource id so one manager can fail on one address and succeed
	// on the next, which is the only way to reach the continue in
	// releaseOrphanedResources.
	relErr map[string]error
}

func (m *ownerMgr) Name() string { return "owner manager" }

func (m *ownerMgr) Allocate(*netContext, string) (zecTypes.NetResource, error) { return nil, nil }

func (m *ownerMgr) Release(_ *netContext, resource *zecTypes.ResourceItem) error {
	if err := m.relErr[resource.ID]; err != nil {
		return err
	}
	m.released = append(m.released, resource.ID)
	return nil
}

func (m *ownerMgr) InuseByOwner(owner string) []zecTypes.ResourceItem { return m.held[owner] }

func (m *ownerMgr) GetSnapshot() (pool.ResourcePoolSnapshot, error) { return nil, nil }
func (m *ownerMgr) GetPoolStatus() pool.Status                      { return pool.Status{} }
func (m *ownerMgr) GetResourceLimit() int                           { return 0 }

// A DEL for a pod with no stored record used to return success and do nothing:
// the address stayed in use until reconcile picked it up ~20 minutes later, and
// the 512/2048 rules naming it were never removed, because reclaimStaleRules
// only runs on records that exist. What the pool attributes to the pod is the
// last remaining evidence, so it has to be honored.
func TestReleaseOrphanedResources(t *testing.T) {
	// releaseOrphanedResources goes on to reclaimStaleRules, which without this
	// would query and delete real ip rules on the machine running the suite. On
	// a node where this CNI is running, a rule for a live pod holding one of the
	// addresses below would be deleted for real.
	stubRuleOps(t, &ruleOps{})

	t.Run("releases what the pool attributes to the pod", func(t *testing.T) {
		mgr := &ownerMgr{held: map[string][]zecTypes.ResourceItem{
			"default/orphan": {
				{Type: zecTypes.NetResourceTypeEniIp, ID: "eni-1/10.0.0.42", ENIId: "eni-1", IPv4: "10.0.0.42"},
			},
		}}
		d := &ipamDaemon{k8s: goneK8s(), managers: map[string]ResourceManager{zecTypes.NetResourceTypeEniIp: mgr}}

		d.releaseOrphanedResources(context.Background(), zlog.GetLogger(), "default", "orphan")

		if len(mgr.released) != 1 || mgr.released[0] != "eni-1/10.0.0.42" {
			t.Errorf("released = %v, want [eni-1/10.0.0.42]", mgr.released)
		}
	})

	t.Run("a pod the pool knows nothing about releases nothing", func(t *testing.T) {
		mgr := &ownerMgr{held: map[string][]zecTypes.ResourceItem{
			"default/somebody-else": {{ID: "eni-1/10.0.0.9"}},
		}}
		d := &ipamDaemon{k8s: goneK8s(), managers: map[string]ResourceManager{zecTypes.NetResourceTypeEniIp: mgr}}

		// This is the common case -- a DEL replayed after a successful one --
		// and it must stay a no-op. Releasing another pod's address here would
		// hand a live container's address to the next pod scheduled.
		d.releaseOrphanedResources(context.Background(), zlog.GetLogger(), "default", "orphan")

		if len(mgr.released) != 0 {
			t.Errorf("released %v for a pod the pool does not know", mgr.released)
		}
	})

	// The failure these two subtests inject has to be one the implementation
	// does not tolerate. pool.ErrResourceInvalid is not: it means the pool no
	// longer holds the resource, which is the outcome the release wanted, so a
	// manager returning it takes the success path and the continue is never
	// reached. Injecting it -- which is what this test used to do -- leaves the
	// continue-on-error path with no coverage at all.
	t.Run("a failed release does not stop the rest of the same manager", func(t *testing.T) {
		r := &ruleOps{}
		stubRuleOps(t, r)
		mgr := &ownerMgr{
			held: map[string][]zecTypes.ResourceItem{"default/orphan": {
				{Type: zecTypes.NetResourceTypeEniIp, ID: "eni-1/10.0.0.42", ENIId: "eni-1", IPv4: "10.0.0.42"},
				{Type: zecTypes.NetResourceTypeEniIp, ID: "eni-1/10.0.0.43", ENIId: "eni-1", IPv4: "10.0.0.43"},
			}},
			relErr: map[string]error{"eni-1/10.0.0.42": errors.New("the platform refused the unassign")},
		}
		d := &ipamDaemon{k8s: goneK8s(), managers: map[string]ResourceManager{zecTypes.NetResourceTypeEniIp: mgr}}

		d.releaseOrphanedResources(context.Background(), zlog.GetLogger(), "default", "orphan")

		// Slice order is fixed, so the failing address really is the one
		// reached first and the second one really is behind the continue.
		if len(mgr.released) != 1 || mgr.released[0] != "eni-1/10.0.0.43" {
			t.Errorf("released = %v, want [eni-1/10.0.0.43]", mgr.released)
		}

		// The address that could not be released keeps its rules. It is still
		// assigned to this node, so deleting them would break a path that is
		// still live -- and reconcile will come back to it.
		var reclaimed []string
		for _, rule := range r.lookedUp {
			n := rule.Src
			if n == nil {
				n = rule.Dst
			}
			reclaimed = append(reclaimed, n.IP.String())
		}
		if len(reclaimed) == 0 {
			t.Error("no rules were reclaimed at all, so the successful release was skipped too")
		}
		for _, addr := range reclaimed {
			if addr == "10.0.0.42" {
				t.Errorf("reclaimed rules for %s, whose release failed", addr)
			}
		}
	})

	t.Run("a failed release does not stop the other managers", func(t *testing.T) {
		// d.managers is a map, so which manager is reached first is random and
		// a single round passes half the time even against an implementation
		// that gives up on the first failure. Repeating makes the bad-first
		// order effectively certain to occur.
		for i := 0; i < 32; i++ {
			bad := &ownerMgr{
				held:   map[string][]zecTypes.ResourceItem{"default/orphan": {{ID: "eni-1/10.0.0.42", IPv4: "10.0.0.42"}}},
				relErr: map[string]error{"eni-1/10.0.0.42": errors.New("the platform refused the unassign")},
			}
			good := &ownerMgr{held: map[string][]zecTypes.ResourceItem{"default/orphan": {{ID: "eni-2/10.0.0.43", IPv4: "10.0.0.43"}}}}
			d := &ipamDaemon{k8s: goneK8s(), managers: map[string]ResourceManager{
				zecTypes.NetResourceTypeEniIp: bad,
				zecTypes.NetResourceTypeEni:   good,
			}}

			d.releaseOrphanedResources(context.Background(), zlog.GetLogger(), "default", "orphan")

			if len(good.released) != 1 {
				t.Fatalf("round %d: the other manager released %v, want one resource", i, good.released)
			}
			if len(bad.released) != 0 {
				t.Fatalf("round %d: the failing manager recorded a release: %v", i, bad.released)
			}
		}
	})
}

// ruleOps records what reclaimStaleRules looked for and what it deleted, and
// stands in for netlink so the suite never touches the host's rule table.
type ruleOps struct {
	lookedUp []*netlink.Rule
	deleted  []netlink.Rule
	present  []netlink.Rule
	findErr  error
}

// stubRuleOps swaps the package's netlink indirection for r and restores it when
// the test ends.
func stubRuleOps(t *testing.T, r *ruleOps) {
	t.Helper()
	prevFind, prevDel := findIPRule, ruleDel
	findIPRule = func(rule *netlink.Rule) ([]netlink.Rule, error) {
		r.lookedUp = append(r.lookedUp, rule)
		if r.findErr != nil {
			return nil, r.findErr
		}
		return r.present, nil
	}
	ruleDel = func(rule *netlink.Rule) error {
		r.deleted = append(r.deleted, *rule)
		return nil
	}
	t.Cleanup(func() { findIPRule, ruleDel = prevFind, prevDel })
}

// TestReclaimStaleRulesTargetsOnlyThisDatapathsRules pins what the reclaim
// actually asks netlink for. Deleting by priority alone would take rules this
// CNI never installed, and matching the wrong direction would leave a live rule
// pointing at a released address.
func TestReclaimStaleRulesTargetsOnlyThisDatapathsRules(t *testing.T) {
	r := &ruleOps{}
	stubRuleOps(t, r)

	d := &ipamDaemon{}
	d.reclaimStaleRules(&zecTypes.Pod{
		Namespace: "default",
		Name:      "gone",
		Resources: []zecTypes.ResourceItem{{IPv4: "10.0.0.42", IPv6: "fd00::2a"}},
	})

	// One from-rule and one to-rule per address, and nothing else: two families
	// times the two priorities this datapath installs.
	if len(r.lookedUp) != 4 {
		t.Fatalf("looked up %d rules, want 4 (from and to, per family)", len(r.lookedUp))
	}
	for _, rule := range r.lookedUp {
		switch rule.Priority {
		case staleFromPodRulePriority:
			if rule.Src == nil || rule.Dst != nil {
				t.Errorf("from-rule at priority %d selects src=%v dst=%v, want src only",
					rule.Priority, rule.Src, rule.Dst)
			}
		case staleToPodRulePriority:
			if rule.Dst == nil || rule.Src != nil {
				t.Errorf("to-rule at priority %d selects src=%v dst=%v, want dst only",
					rule.Priority, rule.Src, rule.Dst)
			}
		default:
			t.Errorf("looked up a rule at priority %d, which this datapath never installs",
				rule.Priority)
		}
	}

	// A single address, not the whole subnet: a wider mask would match rules
	// belonging to other pods on the same node.
	for _, rule := range r.lookedUp {
		n := rule.Src
		if n == nil {
			n = rule.Dst
		}
		ones, bits := n.Mask.Size()
		if ones != bits {
			t.Errorf("rule selects %v, want a single host address", n)
		}
	}
}

// TestReclaimStaleRulesSkipsUnparseableAddresses covers the single-stack record,
// where the IPv6 half is empty and must not turn into a lookup.
func TestReclaimStaleRulesSkipsUnparseableAddresses(t *testing.T) {
	r := &ruleOps{}
	stubRuleOps(t, r)

	d := &ipamDaemon{}
	d.reclaimStaleRules(&zecTypes.Pod{
		Resources: []zecTypes.ResourceItem{{IPv4: "10.0.0.42", IPv6: ""}},
	})

	if len(r.lookedUp) != 2 {
		t.Fatalf("looked up %d rules for an ipv4-only record, want 2", len(r.lookedUp))
	}
}

// TestReclaimStaleRulesDeletesWhatItFinds checks the delete half: a lookup that
// matches must lead to a delete, or the rule outlives the address.
func TestReclaimStaleRulesDeletesWhatItFinds(t *testing.T) {
	match := netlink.NewRule()
	match.Priority = staleFromPodRulePriority
	r := &ruleOps{present: []netlink.Rule{*match}}
	stubRuleOps(t, r)

	d := &ipamDaemon{}
	d.reclaimStaleRules(&zecTypes.Pod{
		Resources: []zecTypes.ResourceItem{{IPv4: "10.0.0.42"}},
	})

	if len(r.deleted) != 2 {
		t.Errorf("deleted %d rules, want 2 -- one per lookup that matched", len(r.deleted))
	}
}

// goneK8s answers "this pod no longer exists", which is the state the three
// subtests above are about: a DEL for a pod that really has finished.
func goneK8s() *syncK8s { return &syncK8s{live: map[string]bool{}} }

// TestOrphanIsReleasable pins the asymmetry that keeps a replayed DEL from
// taking a running pod's address. ReleaseIP and GetIPInfo reject a DEL whose
// sandbox id is not the recorded one; this path has no record to compare, so the
// API server is the only thing left to ask.
func TestOrphanIsReleasable(t *testing.T) {
	deleting := metav1.NewTime(time.Now())

	tests := []struct {
		name string
		pod  *corev1.Pod
		err  error
		want bool
	}{
		{
			name: "gone from the api server",
			want: true,
		},
		{
			name: "deletion in flight -- the ordinary shape of a DEL",
			pod:  &corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &deleting}},
			want: true,
		},
		{
			name: "terminal phase",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
			want: true,
		},
		{
			name: "failed phase",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}},
			want: true,
		},
		{
			// The bug this guards: the pod is alive and using the address, and
			// the DEL names a sandbox it has already retired.
			name: "running with no deletion in flight",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
			want: false,
		},
		{
			// A broken connection must not be read as the pod having gone away
			// -- the same rule the stale-record path follows.
			name: "api server did not answer",
			err:  errors.New("connection refused"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &ipamDaemon{k8s: &podK8s{pod: tt.pod, err: tt.err}}
			if got := d.orphanIsReleasable(context.Background(), zlog.GetLogger(), "default", "orphan"); got != tt.want {
				t.Errorf("orphanIsReleasable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReleaseOrphanedResourcesRefusesForALivePod is the end-to-end half: the
// pool must keep the address and no host rule may be touched.
func TestReleaseOrphanedResourcesRefusesForALivePod(t *testing.T) {
	r := &ruleOps{}
	stubRuleOps(t, r)

	mgr := &ownerMgr{held: map[string][]zecTypes.ResourceItem{
		"default/orphan": {
			{Type: zecTypes.NetResourceTypeEniIp, ID: "eni-1/10.0.0.42", ENIId: "eni-1", IPv4: "10.0.0.42"},
		},
	}}
	d := &ipamDaemon{
		k8s:      &podK8s{pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}},
		managers: map[string]ResourceManager{zecTypes.NetResourceTypeEniIp: mgr},
	}

	d.releaseOrphanedResources(context.Background(), zlog.GetLogger(), "default", "orphan")

	if len(mgr.released) != 0 {
		t.Errorf("released %v for a pod that is still running", mgr.released)
	}
	if len(r.lookedUp) != 0 {
		t.Errorf("looked up %d host rules for a pod that is still running", len(r.lookedUp))
	}
}

// podK8s returns one pod, or one error, for any lookup.
type podK8s struct {
	k8s.K8sService
	pod *corev1.Pod
	err error
}

func (f *podK8s) GetPod(context.Context, string, string) (*corev1.Pod, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.pod == nil {
		return nil, k8sErrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "orphan")
	}
	return f.pod, nil
}
