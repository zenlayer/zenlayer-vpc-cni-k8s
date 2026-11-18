// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/deviceplugin"
)

// A nudge never blocks, and nudges that arrive before the watcher reads are
// one recount: the channel holds a single pending request.
func TestNudgeCapacityCoalesces(t *testing.T) {
	d := &ipamDaemon{}
	d.nudgeCapacity() // device plugin disabled: a no-op, not a panic

	d.capacityNudge = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		for range 5 {
			d.nudgeCapacity()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("nudgeCapacity blocked with nobody reading")
	}
	if n := len(d.capacityNudge); n != 1 {
		t.Fatalf("%d pending recounts; want 1", n)
	}
}

// Only pods GetStockPodCount counts change the advertised figure when they
// come or go; a pod that requests the resource is accounted by the scheduler.
func TestCountsAsStock(t *testing.T) {
	requesting := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceName(deviceplugin.ZenlayerNameSpace + deviceplugin.ENIIPResourceName): resource.MustParse("1"),
		}},
	}}}}
	hostNet := &corev1.Pod{Spec: corev1.PodSpec{HostNetwork: true}}
	plain := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{}}}}

	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"requests eni-ip", requesting, false},
		{"host network", hostNet, false},
		{"no request", plain, true},
		{"gone from the cache", nil, true},
	} {
		if got := countsAsStock(tc.pod); got != tc.want {
			t.Errorf("%s: countsAsStock = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// GC dropping a stale record changes the stock count too, so it asks for a
// recount rather than leaving the figure to the 5-minute tick.
func TestStaleRecordReclaimNudgesCapacity(t *testing.T) {
	k := &syncK8s{live: map[string]bool{}}
	d, _ := daemonWithRecord(t, k, record("default", "gone"))
	d.capacityNudge = make(chan struct{}, 1)
	if err := d.syncPodPersistence(); err != nil {
		t.Fatal(err)
	}
	if len(d.capacityNudge) != 0 {
		t.Fatal("nudged on the first sighting, before anything was reclaimed")
	}
	d.staleSince["default/gone"] = time.Now().Add(-staleRecordGrace - time.Minute)
	if err := d.syncPodPersistence(); err != nil {
		t.Fatal(err)
	}
	if len(d.capacityNudge) != 1 {
		t.Fatal("reclaiming the record did not ask for a recount")
	}
}
