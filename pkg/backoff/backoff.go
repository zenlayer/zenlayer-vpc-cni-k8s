// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package backoff

import (
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	DefaultKey     = ""
	APIWriteOps    = "api_write_ops"
	APIStatusWait  = "api_status_wait"
	APIFastRetry   = "api_fast_retry"
	WaitCRDStatus  = "wait_crd_status"
	MetaStatusWait = "meta_status_wait"
)

var backoffMap = map[string]wait.Backoff{
	DefaultKey: {
		Duration: time.Second * 2,
		Factor:   1.5,
		Jitter:   0.3,
		Steps:    6,
	},
	APIWriteOps: {
		Duration: time.Second * 4,
		Factor:   1.5,
		Jitter:   0.5,
		Steps:    6,
	},
	APIStatusWait: {
		Duration: time.Second * 5,
		Factor:   1.5,
		Jitter:   0.5,
		Steps:    8,
	},
	APIFastRetry: {
		Duration: time.Millisecond * 500,
		Factor:   1.7,
		Jitter:   0.3,
		Steps:    6,
	},
	WaitCRDStatus: {
		Duration: time.Second * 5,
		Factor:   2,
		Jitter:   0.3,
		Steps:    3,
	},
	MetaStatusWait: {
		Duration: time.Millisecond * 1100,
		Factor:   1,
		Jitter:   0.2,
		Steps:    10,
	},
}

// BackOff returns the wait.Backoff registered for key, or the default one when
// key is unknown.
//
// The result is a copy. wait.Backoff is consumed by mutation -- each Step
// advances its own Duration and Steps -- so handing out a shared value would
// leak one caller's progress into the next retry loop that used the same key.
func BackOff(key string) wait.Backoff {
	if b, exist := backoffMap[key]; exist {
		return b
	}

	return backoffMap[DefaultKey]
}
