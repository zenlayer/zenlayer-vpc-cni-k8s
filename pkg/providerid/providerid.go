// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package providerid formats and parses Node.spec.providerID.
//
// The apiserver lets that field go from empty to set exactly once, so the
// format chosen here is permanent for every node it is written to -- and it
// must be the one every other component writing the field uses, or whichever
// writes second is locked out. It is zenlayer-cloud-controller-manager's:
// zenlayer:///<regionId>/<instanceId>, the shape of the in-tree cloud
// providers (an empty URL host, then the location and the instance).
package providerid

import (
	"fmt"
	"strings"
)

const (
	// Scheme is the provider prefix; the cloud provider's name is "zenlayer".
	Scheme = "zenlayer://"
	prefix = Scheme + "/"
)

// Format returns the providerID of an instance in a region.
func Format(regionID, instanceID string) string {
	return prefix + regionID + "/" + instanceID
}

// Parse returns the region and instance IDs in providerID. It rejects
// anything but the exact form Format writes, so a value set by hand in another
// shape is reported rather than guessed at -- with one exception:
// zenlayer://<instanceId>, the older form written before the format was aligned with
// the cloud controller manager. Such a node cannot be rewritten, so it is still
// read; regionID is empty then.
func Parse(providerID string) (regionID, instanceID string, err error) {
	rest, ok := strings.CutPrefix(providerID, prefix)
	if !ok {
		legacy, ok := strings.CutPrefix(providerID, Scheme)
		if !ok || !valid(legacy) {
			return "", "", invalid(providerID)
		}
		return "", legacy, nil
	}
	regionID, instanceID, ok = strings.Cut(rest, "/")
	if !ok || !valid(regionID) || !valid(instanceID) {
		return "", "", invalid(providerID)
	}
	return regionID, instanceID, nil
}

func valid(part string) bool {
	return part != "" && !strings.ContainsAny(part, "/: \t")
}

func invalid(providerID string) error {
	return fmt.Errorf("providerID %q is not of the form %s<regionId>/<instanceId>", providerID, prefix)
}
