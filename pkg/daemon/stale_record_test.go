// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/database"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/k8s"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// memDB is database.Database over a map, so the reclaim logic can be exercised
// without a bolt file.
type memDB struct{ m map[string]interface{} }

func newMemDB() *memDB { return &memDB{m: map[string]interface{}{}} }

func (d *memDB) Put(key string, value interface{}) error { d.m[key] = value; return nil }
func (d *memDB) Get(key string) (interface{}, error) {
	v, ok := d.m[key]
	if !ok {
		return nil, database.ErrNotFound
	}
	return v, nil
}
func (d *memDB) List() []interface{} {
	out := make([]interface{}, 0, len(d.m))
	for _, v := range d.m {
		out = append(out, v)
	}
	return out
}
func (d *memDB) Count() int              { return len(d.m) }
func (d *memDB) Delete(key string) error { delete(d.m, key); return nil }
func (d *memDB) Close() error            { return nil }

// syncK8s answers the two calls syncPodPersistence makes. cached is what the
// informer reports; live is what a direct read returns. They are kept separate
// because the informer lags, so the decision rests on the direct read.
type syncK8s struct {
	k8s.K8sService
	cached  []*corev1.Pod
	live    map[string]bool
	liveErr error
	gets    int
}

func (f *syncK8s) ListLocalCachedPods() ([]*corev1.Pod, error) { return f.cached, nil }

func (f *syncK8s) GetPod(_ context.Context, namespace, name string) (*corev1.Pod, error) {
	f.gets++
	if f.liveErr != nil {
		return nil, f.liveErr
	}
	if f.live[namespace+"/"+name] {
		return &corev1.Pod{}, nil
	}
	return nil, k8sErrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
}

func daemonWithRecord(t *testing.T, k *syncK8s, rec *zecTypes.Pod) (*ipamDaemon, *memDB) {
	t.Helper()
	db := newMemDB()
	ppm := &podPersistenceManager{storage: db}
	if err := ppm.Put(rec); err != nil {
		t.Fatalf("seed record: %v", err)
	}
	return &ipamDaemon{k8s: k, podPersistenceManager: ppm}, db
}

func record(ns, name string) *zecTypes.Pod {
	return &zecTypes.Pod{
		Namespace: ns,
		Name:      name,
		// A netns path that no longer resolves is exactly the state that used
		// to pin a record forever.
		NetNs:     "/var/run/netns/gone-" + name,
		Resources: []zecTypes.ResourceItem{{IPv4: "10.0.0.42"}},
	}
}

// TestStaleRecordIsReclaimedEventually guards termination. The delete
// condition used to be "absent from k8s AND netns absent", and a leaked netns
// never becomes absent -- so the record, and the address it holds, stayed for
// the life of the node. Time has to bound it.
func TestStaleRecordIsReclaimedEventually(t *testing.T) {
	k := &syncK8s{live: map[string]bool{}}
	d, db := daemonWithRecord(t, k, record("default", "gone"))

	// First pass only marks: the pod may simply be mid-teardown.
	if err := d.syncPodPersistence(); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if db.Count() != 1 {
		t.Fatal("record was reclaimed on the first sighting, before any grace period")
	}
	if _, marked := d.staleSince["default/gone"]; !marked {
		t.Fatal("record was not marked stale")
	}

	// Still inside the grace period.
	if err := d.syncPodPersistence(); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if db.Count() != 1 {
		t.Fatal("record was reclaimed before the grace period elapsed")
	}

	// Past it.
	d.staleSince["default/gone"] = time.Now().Add(-staleRecordGrace - time.Minute)
	if err := d.syncPodPersistence(); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if db.Count() != 0 {
		t.Fatal("record outlived the grace period; a leaked netns still pins it")
	}
	if _, marked := d.staleSince["default/gone"]; marked {
		t.Error("mark left behind after the record was reclaimed")
	}
}

// TestLiveRecordSurvives covers the other half. Reclaiming early hands a
// running container's address to a second pod, which is worse than holding an
// address too long, so every signal that the pod is alive has to win.
func TestLiveRecordSurvives(t *testing.T) {
	t.Run("present in the informer cache", func(t *testing.T) {
		k := &syncK8s{
			cached: []*corev1.Pod{podNamed("default", "alive")},
			live:   map[string]bool{"default/alive": true},
		}
		d, db := daemonWithRecord(t, k, record("default", "alive"))
		if err := d.syncPodPersistence(); err != nil {
			t.Fatal(err)
		}
		if db.Count() != 1 {
			t.Fatal("a pod the informer still lists was reclaimed")
		}
		if k.gets != 0 {
			t.Errorf("asked the API server %d times about a pod already in the cache", k.gets)
		}
	})

	t.Run("absent from the cache but live on the API server", func(t *testing.T) {
		k := &syncK8s{live: map[string]bool{"default/alive": true}}
		d, db := daemonWithRecord(t, k, record("default", "alive"))
		if err := d.syncPodPersistence(); err != nil {
			t.Fatal(err)
		}
		if db.Count() != 1 {
			t.Fatal("a stale informer cache was enough to reclaim a live pod")
		}
		if k.gets != 1 {
			t.Errorf("direct reads = %d, want 1", k.gets)
		}
	})

	// A pod can come back between passes -- same name, new sandbox. The mark
	// has to be cleared or it would carry the earlier absence forward.
	t.Run("returning pod clears the mark", func(t *testing.T) {
		k := &syncK8s{live: map[string]bool{}}
		d, _ := daemonWithRecord(t, k, record("default", "flap"))
		if err := d.syncPodPersistence(); err != nil {
			t.Fatal(err)
		}
		if _, marked := d.staleSince["default/flap"]; !marked {
			t.Fatal("not marked on the first pass")
		}

		k.live["default/flap"] = true
		if err := d.syncPodPersistence(); err != nil {
			t.Fatal(err)
		}
		if _, marked := d.staleSince["default/flap"]; marked {
			t.Error("mark survived the pod coming back; it would age towards a wrongful reclaim")
		}
	})
}

// TestUnreachableAPIServerKeepsRecords guards against an inconclusive answer
// being read as absence. If a dropped connection read as "not found", one bad
// interval would mark every record on the node stale and start them all aging
// out together.
func TestUnreachableAPIServerKeepsRecords(t *testing.T) {
	k := &syncK8s{liveErr: errors.New("connection refused")}
	d, db := daemonWithRecord(t, k, record("default", "unknown"))

	if err := d.syncPodPersistence(); err != nil {
		t.Fatalf("sync returned an error instead of holding the record: %v", err)
	}
	if db.Count() != 1 {
		t.Fatal("record reclaimed while the API server was unreachable")
	}
	if _, marked := d.staleSince["default/unknown"]; marked {
		t.Error("record marked stale on an inconclusive answer; it would age out during an outage")
	}
}

// TestStaleMarksDoNotAccumulate keeps the in-memory map bounded: a record that
// disappears by some other route must not leave its mark behind forever.
func TestStaleMarksDoNotAccumulate(t *testing.T) {
	k := &syncK8s{live: map[string]bool{}}
	d, db := daemonWithRecord(t, k, record("default", "gone"))

	if err := d.syncPodPersistence(); err != nil {
		t.Fatal(err)
	}
	if len(d.staleSince) != 1 {
		t.Fatalf("marks = %d, want 1", len(d.staleSince))
	}

	// Removed by another path -- a CNI DEL that finally arrived, say.
	if err := db.Delete("default/gone"); err != nil {
		t.Fatal(err)
	}
	if err := d.syncPodPersistence(); err != nil {
		t.Fatal(err)
	}
	if len(d.staleSince) != 0 {
		t.Errorf("marks = %d, want 0: the map grows for the life of the process", len(d.staleSince))
	}
}

func podNamed(ns, name string) *corev1.Pod {
	p := &corev1.Pod{}
	p.Namespace, p.Name = ns, name
	return p
}
