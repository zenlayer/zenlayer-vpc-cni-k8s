// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"fmt"
	"runtime"
)

var Version = "v1.6.8"
var ComponentName = "zenlayer-ipam"
var GitCommit = "unset"

func UserAgent() string {
	return fmt.Sprintf("%s/%s (%s/%s)/%s", ComponentName, Version, runtime.GOOS, runtime.GOARCH, GitCommit)
}
