// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package common

import (
	"fmt"
	"os"
	"strings"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
)

var log = zlog.GetLogger().WithFields(zlog.Fields{"subsys": "zapiCommon"})

// GetInstanceID reads the instance ID from DMI.
func GetInstanceID() (string, error) {
	data, err := os.ReadFile(ZEC_PRODUCT_SERIAL_PATH)
	if err != nil {
		log.Errorf("failed to get instance ID from %s: %v", ZEC_PRODUCT_SERIAL_PATH, err)
		return "", fmt.Errorf("cannot read instance ID from %s: %w", ZEC_PRODUCT_SERIAL_PATH, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// GetPlatform reads the platform from DMI (the system vendor).
func GetPlatform() (string, error) {
	data, err := os.ReadFile(ZEC_SYS_VENDOR_PATH)
	if err != nil {
		log.Errorf("failed to get platform from %s: %v", ZEC_SYS_VENDOR_PATH, err)
		return "", fmt.Errorf("cannot read platform from %s: %w", ZEC_SYS_VENDOR_PATH, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// GetZone reads the availability zone from DMI, e.g. "SH1A".
//
// This is the zone, not the region. The OpenAPI keys subnets and ENIs on the
// region ("asia-east-1"), which nothing local reports; InitInstanceMetadata
// resolves that through the OpenAPI instead. The two are not interchangeable,
// and passing one where the other is expected is not rejected -- a subnet
// filtered by a zone simply matches nothing.
func GetZone() (string, error) {
	data, err := os.ReadFile(ZEC_PRODUCT_FAMILY_PATH)
	if err != nil {
		log.Errorf("failed to get zone from %s: %v", ZEC_PRODUCT_FAMILY_PATH, err)
		return "", fmt.Errorf("cannot read zone from %s: %w", ZEC_PRODUCT_FAMILY_PATH, err)
	}
	return strings.TrimSpace(string(data)), nil
}
