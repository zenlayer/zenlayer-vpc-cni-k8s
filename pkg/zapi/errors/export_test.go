// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package errors

import "testing"

// DisableEventLimits lifts the event rate limits for one test. They are
// process-global, so a repeated run (-count>1) would otherwise exhaust them
// and the later runs would see no events.
func DisableEventLimits(t *testing.T) {
	cantRetry, flow, conn := cantRetryErrEventLimiter, flowLimitEventLimiter, connectivityEventLimiter
	cantRetryErrEventLimiter, flowLimitEventLimiter, connectivityEventLimiter = nil, nil, nil
	t.Cleanup(func() {
		cantRetryErrEventLimiter, flowLimitEventLimiter, connectivityEventLimiter = cantRetry, flow, conn
	})
}
