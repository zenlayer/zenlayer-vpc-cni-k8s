// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package linux

import (
	"bytes"
	"os"
)

func EnsureConf(fPath string, cfg string) error {
	if content, err := os.ReadFile(fPath); err == nil {
		if bytes.Equal(bytes.TrimSpace(content), []byte(cfg)) {
			return nil
		}
	}
	return os.WriteFile(fPath, []byte(cfg), 0644)
}
