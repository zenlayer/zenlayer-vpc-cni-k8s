// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package linux

import (
	"net"
)

func IsIPv6(ip net.IP) bool {
	return ip.To4() == nil
}
