// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func terminated(at time.Time) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.NewTime(at)},
		},
	}
}

func running() corev1.ContainerStatus {
	return corev1.ContainerStatus{
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}
}

// TestFinishedLongerThan pins the judgement PreStart makes about whether a pod
// still holds the address its status names.
//
// Both directions cost something, and they are not symmetric. Saying "finished"
// too eagerly hands a live address to the next pod, and a duplicated address
// does not recover on its own -- the index this feeds is consulted only for
// addresses no stored record claims, so nothing else would catch it. Saying it
// too late holds the address for as long as the Pod object exists, which for a
// Job with no ttlSecondsAfterFinished is for ever. Hence a window, and a
// generous one.
func TestFinishedLongerThan(t *testing.T) {
	const window = 10 * time.Minute
	long := time.Now().Add(-time.Hour)
	recent := time.Now().Add(-time.Second)

	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
		why  string
	}{
		{
			name: "running",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase:             corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{running()},
			}},
			why: "a running pod holds its address, whatever the timestamps say",
		},
		{
			name: "pending",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}},
			why:  "its ADD may be in flight right now",
		},
		{
			name: "succeeded but only just",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase:             corev1.PodSucceeded,
				ContainerStatuses: []corev1.ContainerStatus{terminated(recent)},
			}},
			why: "the phase turns when the containers exit, which is before kubelet tears the " +
				"sandbox down and runs the DEL -- the address is still configured and still its own",
		},
		{
			name: "succeeded long ago",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase:             corev1.PodSucceeded,
				ContainerStatuses: []corev1.ContainerStatus{terminated(long)},
			}},
			want: true,
			why:  "the teardown takes seconds; an hour later there is nothing left to hold it",
		},
		{
			name: "failed long ago",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase:             corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{terminated(long)},
			}},
			want: true,
			why:  "a Failed pod kept for debugging is as finished as a Succeeded one",
		},
		{
			name: "one container still has no terminated state",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase:             corev1.PodSucceeded,
				ContainerStatuses: []corev1.ContainerStatus{terminated(long), running()},
			}},
			why: "a container the status cannot account for is not evidence that the sandbox is gone",
		},
		{
			name: "the newest container decides",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase:             corev1.PodSucceeded,
				ContainerStatuses: []corev1.ContainerStatus{terminated(long), terminated(recent)},
			}},
			why: "one sidecar that only just exited means the teardown has not happened yet",
		},
		{
			name: "init containers count too",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase:                 corev1.PodFailed,
				InitContainerStatuses: []corev1.ContainerStatus{terminated(recent)},
				ContainerStatuses:     []corev1.ContainerStatus{terminated(long)},
			}},
			why: "a pod that failed in its init container reports the finish there",
		},
		{
			name: "terminal with no container statuses at all",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}},
			why:  "there is no finish time to measure a window from, so the pod is kept",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := finishedLongerThan(tt.pod, window); got != tt.want {
				t.Errorf("finishedLongerThan() = %v, want %v -- %s", got, tt.want, tt.why)
			}
		})
	}
}

// TestPodTerminalGraceOutlastsOneCNIOperation guards the constant against being
// trimmed to something that cannot do its job.
//
// daemonRPCTimeout is what a single CNI operation is allowed to take, and DELs
// queue behind other operations on the node-wide lock. A window shorter than
// that can expire while one DEL is still running -- and the failure it then
// causes, a second pod handed an address the first is still using, does not
// recover on its own.
func TestPodTerminalGraceOutlastsOneCNIOperation(t *testing.T) {
	if podTerminalGrace <= daemonRPCTimeout {
		t.Errorf("podTerminalGrace = %v, which is not longer than the %v budget one CNI operation "+
			"gets; a DEL still in flight would fall outside the window", podTerminalGrace, daemonRPCTimeout)
	}
}
