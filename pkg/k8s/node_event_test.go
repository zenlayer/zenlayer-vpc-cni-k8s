// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func managerWithNode() (*k8sManager, *fake.Clientset) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", UID: "uid-1"}}
	client := fake.NewClientset(node)
	return &k8sManager{nodeName: node.Name, node: node, kubeClient: client}, client
}

// The platform reads these events by involvedObject and reason, so both have
// to land where `kubectl describe node` looks: the default namespace, pointing
// at the node by name and UID.
func TestRecordNodeEventSyncStoresEvent(t *testing.T) {
	k, client := managerWithNode()

	if err := k.RecordNodeEventSync(context.Background(), corev1.EventTypeWarning,
		"CredentialServiceAbnormal", "AUTHFAILURE_SIGNATURE_FAILURE"); err != nil {
		t.Fatalf("RecordNodeEventSync() error = %v", err)
	}

	evs, err := client.CoreV1().Events(metav1.NamespaceDefault).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evs.Items) != 1 {
		t.Fatalf("got %d events in %q, want 1", len(evs.Items), metav1.NamespaceDefault)
	}
	ev := evs.Items[0]
	want := corev1.ObjectReference{Kind: "Node", Name: "node-1", UID: "uid-1"}
	if ev.InvolvedObject != want {
		t.Errorf("involvedObject = %+v, want %+v", ev.InvolvedObject, want)
	}
	if ev.Type != corev1.EventTypeWarning || ev.Reason != "CredentialServiceAbnormal" ||
		ev.Message != "AUTHFAILURE_SIGNATURE_FAILURE" {
		t.Errorf("type/reason/message = %q/%q/%q", ev.Type, ev.Reason, ev.Message)
	}
	if ev.Source.Component != eventSourceComponent || ev.Source.Host != "node-1" {
		t.Errorf("source = %+v, want component %q on host node-1", ev.Source, eventSourceComponent)
	}
	if ev.Count != 1 || ev.FirstTimestamp.IsZero() || ev.LastTimestamp.IsZero() {
		t.Errorf("count/first/last = %d/%v/%v, want 1 and both timestamps set",
			ev.Count, ev.FirstTimestamp, ev.LastTimestamp)
	}
}

// A crash-looping daemon reports once per start. Each report must be its own
// object: a name collision would turn the second create into AlreadyExists and
// the later cause would be lost.
func TestRecordNodeEventSyncRepeatedCallsDoNotCollide(t *testing.T) {
	k, client := managerWithNode()

	for i := range 3 {
		if err := k.RecordNodeEventSync(context.Background(), corev1.EventTypeWarning,
			"DaemonStartFailed", "boom"); err != nil {
			t.Fatalf("call %d: RecordNodeEventSync() error = %v", i, err)
		}
	}

	evs, err := client.CoreV1().Events(metav1.NamespaceDefault).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evs.Items) != 3 {
		t.Errorf("got %d events, want 3", len(evs.Items))
	}
}

// The caller logs the failure instead of exiting silently, so the API error
// has to come back rather than being swallowed.
func TestRecordNodeEventSyncReturnsAPIError(t *testing.T) {
	k, client := managerWithNode()
	denied := errors.New("events is forbidden")
	client.PrependReactor("create", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, denied
	})

	err := k.RecordNodeEventSync(context.Background(), corev1.EventTypeWarning, "DaemonStartFailed", "boom")
	if !errors.Is(err, denied) {
		t.Errorf("RecordNodeEventSync() error = %v, want %v", err, denied)
	}
}
