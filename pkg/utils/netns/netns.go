// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package netns

import (
	"fmt"
	"os"

	"github.com/vishvananda/netns"
)

const nsRootPath = "/var/run/netns"

// CheckNetNsExist reports whether nsPath names an existing netns. A missing path
// is reported as (false, nil), not as an error.
func CheckNetNsExist(nsPath string) (bool, error) {
	h, err := netns.GetFromPath(nsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer func(h *netns.NsHandle) {
		_ = h.Close()
	}(&h)
	return true, nil
}

// ListAllNetNs returns the path of every non-directory entry under
// /var/run/netns, the directory named network namespaces are bind-mounted into.
func ListAllNetNs() ([]string, error) {
	var namespaces []string
	files, err := os.ReadDir(nsRootPath)
	if err != nil {
		return namespaces, fmt.Errorf("read dir %s failed: %v", nsRootPath, err)
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		namespaces = append(namespaces, fmt.Sprintf("%s/%s", nsRootPath, f.Name()))
	}
	return namespaces, nil
}
