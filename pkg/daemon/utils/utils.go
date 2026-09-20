// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"fmt"
)

const (
	cniNicNamePrefix = "zenlayer-cni-create-nic"

	// Tags stamped on every ENI this CNI creates, and what identifies them
	// again afterwards (isOwnENI in pkg/daemon/factory).
	//
	// The name prefix alone is a weak claim of ownership -- a name is something
	// an operator can set to anything, including this exact string, and then
	// the daemon would adopt and eventually delete an interface that is not
	// its own. Both are written; identification should rest on the tag, which
	// has to be applied deliberately.
	TagKeyCreatedBy   = "zenlayer:cni:created-by"
	TagValueCreatedBy = "zenlayer-vpc-cni"
	// TagKeyInstanceID names the node that created the ENI. Ownership requires
	// both tags, so one node never takes another node's interface for its own
	// creation, and never deletes it when sweeping for leaked ones.
	TagKeyInstanceID = "zenlayer:cni:instance-id"

	// TagKeyNoManage opts an interface out of management by this daemon. Set it
	// to "true" on any interface attached for another purpose -- storage,
	// management, monitoring -- so the daemon neither serves pod addresses from
	// it nor detaches it when the pool shrinks.
	//
	// Checked before every other rule, including the created-by tags, so it
	// also exempts an interface this daemon created.
	TagKeyNoManage   = "zenlayer:cni:no-manage"
	TagValueNoManage = "true"
)

// CniENITags returns the tags identifying an ENI created by this daemon on this
// instance.
func CniENITags(instanceID string) map[string]string {
	return map[string]string{
		TagKeyCreatedBy:  TagValueCreatedBy,
		TagKeyInstanceID: instanceID,
	}
}

// GetCniNicName builds the name given to an ENI this daemon creates. Only the
// create call passes it; nothing matches on it afterwards, because
// identification rests on the tags above.
//
// The suffix is the instance id, not the Kubernetes node name: the caller reads
// it from metadata, and it is what CniENITags records alongside.
func GetCniNicName(instanceID string) string {
	return fmt.Sprintf("%s-%s", cniNicNamePrefix, instanceID)
}
