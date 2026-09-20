// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package common

const (
	ZEC_PLATFORM string = "Zenlayer Elastic Compute"
	// Read straight off DMI rather than over the metadata service, so they work
	// before any network is up. Platform instance-metadata reference:
	//   https://docs.console.zenlayer.com/welcome/elastic-compute/get-started/manage-instances/get-instance-metadata
	ZEC_SYS_VENDOR_PATH     string = "/sys/class/dmi/id/sys_vendor"     // platform, read by GetPlatform
	ZEC_PRODUCT_FAMILY_PATH string = "/sys/class/dmi/id/product_family" // availability zone, read by GetZone
	ZEC_PRODUCT_SERIAL_PATH string = "/sys/class/dmi/id/product_serial" // instance ID, read by GetInstanceID
)
