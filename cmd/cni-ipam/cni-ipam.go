// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Command zec-ipam is the per-node IPAM daemon.
//
// It owns every address decision on the node: it attaches and releases network
// interfaces, keeps a warm pool of addresses ahead of demand, persists what each
// pod was given, and reconciles that record against the platform. The CNI plugin
// binary holds none of this state and reaches the daemon over a Unix socket, so
// address bookkeeping survives the short-lived plugin process.
package main

import (
	"flag"
	"os"
	"strings"

	"k8s.io/klog/v2/textlogger"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon"
	vs "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/version"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
)

var (
	logLevel string

	log = zlog.GetLogger().WithFields(zlog.Fields{"subsys": "ipam"})
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		vs.Show()
		os.Exit(0)
	}

	vs.Show()

	fs := flag.NewFlagSet("zenlayer ipam", flag.ExitOnError)
	// Empty is the sentinel for "not given", so ZENLAYER_CNI_LOGLEVEL stays in
	// effect. A real default would overwrite the environment on every start: it
	// used to default to "debug", which pinned every deployment to debug
	// logging.
	fs.StringVar(&logLevel, "log-level", "",
		"log level (trace|debug|info|warn|error|fatal|panic); overrides "+
			zlog.EnvLogLevel+" (default info)")
	ctrl.RegisterFlags(fs)

	// ExitOnError: Parse prints the usage and exits(2) itself on a bad flag, so
	// it never returns an error here. Checking one would be dead code.
	_ = fs.Parse(os.Args[1:])
	// The flag wins over the environment; when it is absent, the level zlog
	// already picked up from the environment at init stands.
	effectiveLevel := logLevel
	if effectiveLevel == "" {
		effectiveLevel = os.Getenv(zlog.EnvLogLevel)
	}
	if logLevel != "" {
		// Applies at every level. The old code only called SetLogLevel inside
		// the debug branch, so -log-level=info was silently ignored.
		if err := zlog.GetLogger().SetLogLevel(logLevel); err != nil {
			log.Warnf("Ignoring invalid -log-level %q, %v", logLevel, err)
		}
	}

	var opts []textlogger.ConfigOption
	// Keyed off the effective level, not the flag: debug set through the
	// environment already raises this daemon's own level via zlog, and
	// controller-runtime's verbosity has to follow it.
	if strings.EqualFold(effectiveLevel, "debug") {
		opts = append(opts, textlogger.Verbosity(4))
	}
	ctrl.SetLogger(textlogger.NewLogger(textlogger.NewConfig(opts...)))

	ctx := ctrl.SetupSignalHandler()
	ctx = ctrl.LoggerInto(ctx, ctrl.Log)
	err := daemon.Run(ctx)
	if err != nil {
		// Fatalf already terminates the process.
		log.Fatalf("Launch ipam service error: %v", err)
	}
}
