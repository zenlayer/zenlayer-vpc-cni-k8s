// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/database"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// TestPodPersistenceGetMissingIsDomainNotFound pins the boundary between this
// codebase's two not-found sentinels, pkg/database's and pkg/zapi's.
//
// AllocIP, ReleaseIP and GetIPInfo all treat "no record for this pod" as the
// ordinary first-time case by matching errors.Is(err, apiErr.ErrNotFound). When
// the storage sentinel leaked through untranslated that match silently failed,
// every allocation was reported as a read error, and no pod on the node could
// obtain an address.
func TestPodPersistenceGetMissingIsDomainNotFound(t *testing.T) {
	ppm, err := newPodPersistenceManager(filepath.Join(t.TempDir(), "pod.db"), "pod")
	if err != nil {
		t.Fatalf("create persistence manager: %v", err)
	}
	defer ppm.Close()

	_, err = ppm.Get("kube-system", "never-stored")
	if err == nil {
		t.Fatal("expected an error for a pod that was never stored")
	}
	if !errors.Is(err, apiErr.ErrNotFound) {
		t.Errorf("errors.Is(err, apiErr.ErrNotFound) = false; err = %v", err)
	}
	// The storage sentinel must not be what callers see.
	if errors.Is(err, database.ErrNotFound) {
		t.Errorf("storage sentinel leaked to the caller: %v", err)
	}
}

// TestPodPersistenceRoundTrip covers the other half: a stored pod comes back,
// and comes back as a *Pod rather than tripping the unchecked type assertion
// in Get.
func TestPodPersistenceRoundTrip(t *testing.T) {
	ppm, err := newPodPersistenceManager(filepath.Join(t.TempDir(), "pod.db"), "pod")
	if err != nil {
		t.Fatalf("create persistence manager: %v", err)
	}
	defer ppm.Close()

	pod := &zecTypes.Pod{Namespace: "default", Name: "nginx"}
	if err := ppm.Put(pod); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := ppm.Get("default", "nginx")
	if err != nil {
		t.Fatalf("get after put: %v", err)
	}
	if got.Namespace != "default" || got.Name != "nginx" {
		t.Errorf("round trip returned %s/%s, want default/nginx", got.Namespace, got.Name)
	}

	if err := ppm.Delete("default", "nginx"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := ppm.Get("default", "nginx"); !errors.Is(err, apiErr.ErrNotFound) {
		t.Errorf("after delete, errors.Is(err, apiErr.ErrNotFound) = false; err = %v", err)
	}
}
