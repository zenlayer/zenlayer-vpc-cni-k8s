// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/database"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// PodPersistenceManager stores the pod-to-resource records that let the daemon
// recover its allocations across a restart.
type PodPersistenceManager interface {
	// Get returns the stored record, or apiErr.ErrNotFound if the pod has none
	// -- the normal state before its first allocation.
	Get(podNamespace, podName string) (*zecTypes.Pod, error)
	List() ([]*zecTypes.Pod, error)
	Put(pod *zecTypes.Pod) error
	Delete(podNamespace, podName string) error
	Close()
}

type podPersistenceManager struct {
	storage database.Database
}

func (ppm *podPersistenceManager) key(podNamespace, podName string) string {
	return podNamespace + "/" + podName
}

func (ppm *podPersistenceManager) Get(podNamespace, podName string) (*zecTypes.Pod, error) {
	pod, err := ppm.storage.Get(ppm.key(podNamespace, podName))
	if err != nil {
		// The storage layer carries its own not-found sentinel, while callers
		// match on apiErr.ErrNotFound. Passing the storage one through unchanged
		// makes errors.Is fail, so "this pod has no record yet" -- the normal
		// state on every first allocation -- is reported as a read failure and
		// no pod can ever be given an address. Translate at the boundary; the
		// daemon should not have to know what the store is.
		if errors.Is(err, database.ErrNotFound) {
			return nil, apiErr.ErrNotFound
		}
		return nil, err
	}
	typed, ok := pod.(*zecTypes.Pod)
	if !ok {
		// The store's deserializer only ever produces *zecTypes.Pod, so this is
		// an error rather than a panic on principle: a persistence layer that
		// takes the daemon down on one unreadable record leaves the node with no
		// IPAM at all, while an error lets the caller treat it as no record.
		return nil, fmt.Errorf("record for %s holds %T, want *zecTypes.Pod", ppm.key(podNamespace, podName), pod)
	}
	return typed, nil
}

func (ppm *podPersistenceManager) List() ([]*zecTypes.Pod, error) {
	rawPods := ppm.storage.List()
	var pods []*zecTypes.Pod
	for _, pod := range rawPods {
		typed, ok := pod.(*zecTypes.Pod)
		if !ok {
			// Skip rather than fail the whole listing. This runs at startup, so
			// returning an error for one bad record would keep the daemon from
			// reconciling any of the good ones.
			log.Warnf("Skipping a stored record that holds %T, want *zecTypes.Pod", pod)
			continue
		}
		pods = append(pods, typed)
	}

	return pods, nil
}

func (ppm *podPersistenceManager) Put(pod *zecTypes.Pod) error {
	return ppm.storage.Put(ppm.key(pod.Namespace, pod.Name), pod)
}

func (ppm *podPersistenceManager) Delete(podNamespace, podName string) error {
	return ppm.storage.Delete(ppm.key(podNamespace, podName))
}

func (ppm *podPersistenceManager) Close() {
	ppm.storage.Close()
}

func newPodPersistenceManager(dbPath, dbName string) (*podPersistenceManager, error) {
	storage, err := database.NewBoltHandler(dbName, dbPath,
		json.Marshal,
		func(bytes []byte) (interface{}, error) {
			pod := &zecTypes.Pod{}
			return pod, json.Unmarshal(bytes, &pod)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create resource db failed: %w", err)
	}
	ppm := &podPersistenceManager{storage: storage}
	return ppm, nil
}
