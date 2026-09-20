// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package k8s

import "fmt"

func PodInfoKey(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}
