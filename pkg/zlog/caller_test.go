// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package zlog_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
)

// End to end: a record emitted from this file must be attributed to this file,
// not to zlog.go or to logrus internals.
func TestCallerIsTheRealCallSite(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "cni.log")
	if err := os.WriteFile(logFile, nil, 0600); err != nil {
		t.Fatal(err)
	}

	log := zlog.New(logFile)
	if err := log.SetLogLevel("debug"); err != nil {
		t.Fatal(err)
	}

	log.Infof("infof %s", "x")
	log.Info("info")
	log.Errorf("errorf %v", "y")
	log.Debugf("debugf")
	log.WithFields(zlog.Fields{"k": "v"}).Infof("with fields")
	log.WithField("k", "v").Errorf("with field")

	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 6 {
		t.Fatalf("expected 6 records, got %d:\n%s", len(lines), raw)
	}
	for _, line := range lines {
		if !strings.Contains(line, "source=") {
			t.Errorf("no source field: %s", line)
			continue
		}
		if !strings.Contains(line, "zlog/caller_test.go") {
			t.Errorf("call site should be this test file, got: %s", line)
		}
		if strings.Contains(line, "zlog/zlog.go") || strings.Contains(line, "logrus") {
			t.Errorf("a wrapper frame leaked into the call site: %s", line)
		}
	}
}
