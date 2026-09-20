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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/k8s"
)

// ownerK8s answers the two lookups ownerStillExists makes. The direct read
// honors the context it is given, which is the whole point: a client that
// ignored it would never come back.
type ownerK8s struct {
	k8s.K8sService

	cached   bool
	pod      *corev1.Pod
	liveErr  error
	blockGet bool
	gets     int
}

func (f *ownerK8s) podOrEmpty() *corev1.Pod {
	if f.pod != nil {
		return f.pod
	}
	return &corev1.Pod{}
}

func (f *ownerK8s) GetLocalCachedPod(string, string) (*corev1.Pod, error) {
	if f.cached {
		return f.podOrEmpty(), nil
	}
	return nil, errors.New("not in cache")
}

func (f *ownerK8s) GetPod(ctx context.Context, _, name string) (*corev1.Pod, error) {
	f.gets++
	if f.blockGet {
		// An API server that accepted the connection and then went quiet.
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.liveErr != nil {
		return nil, f.liveErr
	}
	if f.pod != nil {
		return f.pod, nil
	}
	return nil, k8sErrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
}

// finishedPod is a pod that reached a terminal phase ago seconds in the past.
func finishedPod(ago time.Duration) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodSucceeded,
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				FinishedAt: metav1.NewTime(time.Now().Add(-ago)),
			}},
		}},
	}}
}

// TestOwnerAliveReleasesAPodThatFinishedLongAgo is the other half of the
// terminal-pod fix, and the half that actually closes it.
//
// PreStart declines to attribute an address to a pod that finished long ago, but
// only for pods already past the window when it runs. A daemon restart landing
// inside that window marks the address anyway -- and before this, nothing ever
// took it back, because existence of the Pod object was the whole test. GC now
// gets a no here and falls through to its own gcProtectPeriod check.
func TestOwnerAliveReleasesAPodThatFinishedLongAgo(t *testing.T) {
	tests := []struct {
		name   string
		pod    *corev1.Pod
		cached bool
		want   bool
		why    string
	}{
		{
			name: "finished long ago, from the api server",
			pod:  finishedPod(time.Hour),
			why:  "its sandbox is gone and its DEL has run; the Pod object outliving it is not a claim",
		},
		{
			name:   "finished long ago, from the cache",
			pod:    finishedPod(time.Hour),
			cached: true,
			why:    "the cache is asked first, so the judgement has to be made there too",
		},
		{
			name: "finished just now",
			pod:  finishedPod(time.Second),
			want: true,
			why: "the phase turns before kubelet tears the sandbox down, so the address may still " +
				"be configured and still be its own",
		},
		{
			name:   "running",
			pod:    &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
			cached: true,
			want:   true,
			why:    "the ordinary case: a live pod keeps its address",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &ownerK8s{pod: tt.pod, cached: tt.cached}
			if got := ownerStillExists(f, "default/web"); got != tt.want {
				t.Errorf("ownerStillExists() = %v, want %v -- %s", got, tt.want, tt.why)
			}
		})
	}
}

// TestOwnerAliveReadIsBounded is the regression this file exists for.
//
// The direct read used context.Background() and the client has no timeout of
// its own. It runs inside pool.GC, which holds pause() throughout, so every
// Allocate on the node queues behind it: an API server that stopped answering
// turned into every pod on the node failing its CNI ADD at daemonRPCTimeout,
// with nothing in the error to say why.
func TestOwnerAliveReadIsBounded(t *testing.T) {
	f := &ownerK8s{blockGet: true}

	start := time.Now()
	alive := ownerStillExists(f, "default/web")
	elapsed := time.Since(start)

	if elapsed > 4*ownerAliveAPITimeout {
		t.Fatalf("the read took %v; it must give up near %v, because pool.GC holds pause() for "+
			"all of it and every Allocate on the node waits behind that", elapsed, ownerAliveAPITimeout)
	}
	// Giving up is not a NotFound, so the resource stays with its owner. The
	// opposite answer would hand a running pod's address to another pod every
	// time the API server was slow.
	if !alive {
		t.Error("a read that timed out reported the owner as gone; only a definite NotFound may do that")
	}
}

// TestOwnerAliveSkipsTheApiServerWhenTheCacheAnswers pins why the timeout above
// is affordable: on an API server outage the cache still answers from memory
// for every pod it holds, so the bounded read is reached only on a cache miss.
func TestOwnerAliveSkipsTheApiServerWhenTheCacheAnswers(t *testing.T) {
	f := &ownerK8s{cached: true, blockGet: true}

	if !ownerStillExists(f, "default/web") {
		t.Error("a pod the informer cache holds was reported as gone")
	}
	if f.gets != 0 {
		t.Errorf("the api server was read %d times for a pod the cache answered for, want 0", f.gets)
	}
}

// TestOwnerAliveOnlyNotFoundReleases covers the three answers apart from the
// timeout. Everything that is not a definite NotFound has to keep the resource:
// a broken connection must not read as every pod on this node having gone at
// once.
func TestOwnerAliveOnlyNotFoundReleases(t *testing.T) {
	tests := []struct {
		name  string
		owner string
		fake  *ownerK8s
		want  bool
	}{
		{
			name:  "definitely gone",
			owner: "default/web",
			fake:  &ownerK8s{},
			want:  false,
		},
		{
			name:  "api server refused",
			owner: "default/web",
			fake:  &ownerK8s{liveErr: k8sErrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "web", errors.New("rbac"))},
			want:  true,
		},
		{
			name:  "connection broken",
			owner: "default/web",
			fake:  &ownerK8s{liveErr: errors.New("connection refused")},
			want:  true,
		},
		{
			// available and invalid entries carry an empty owner, and nothing
			// else legitimately owns a resource.
			name:  "not a pod key",
			owner: "",
			fake:  &ownerK8s{},
			want:  false,
		},
		{
			name:  "half a pod key",
			owner: "default/",
			fake:  &ownerK8s{},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ownerStillExists(tt.fake, tt.owner); got != tt.want {
				t.Errorf("ownerStillExists(%q) = %v, want %v", tt.owner, got, tt.want)
			}
		})
	}
}
