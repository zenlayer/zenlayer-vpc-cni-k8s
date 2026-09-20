// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"fmt"
	"runtime"
)

// Build and version information, filled in at link time from the Makefile.
var (
	BuildTime   = ""
	GitCommitID = ""
	GitBranch   = ""
	// CNIVersion is the output of `git describe --tags --always --dirty` and
	// nothing else: a tag, a tag plus commit count and hash, or a bare SHA on
	// an untagged tree, plus -dirty on a modified one, falling back to the
	// literal "unknown" when git cannot answer. A tag's leading v is kept, so
	// it matches the image tag and Chart.yaml's appVersion. The branch was once
	// appended as "<version>@<branch>", which added nothing: describe already
	// identifies the source, and the same commit built from two branches
	// produces the same binary. GitBranch carries the branch on its own.
	CNIVersion = ""
)

func Show() {
	fmt.Println("CNI Version: \t" + CNIVersion)
	fmt.Println("Go Version: \t" + runtime.Version())
	fmt.Printf("Go OS/Arch: \t%s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Println("Build Time: \t" + BuildTime)
	fmt.Println("Git Commit ID: \t" + GitCommitID)
	fmt.Println("Git Branch: \t" + GitBranch)
}
