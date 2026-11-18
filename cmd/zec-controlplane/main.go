// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// zec-controlplane is the credential-holding half of ipamType crd.
package main

import (
	"flag"
	"os"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2/textlogger"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/controlplane"
	vs "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/version"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
)

var log = zlog.GetLogger().WithFields(zlog.Fields{"subsys": "controlplane"})

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		vs.Show()
		os.Exit(0)
	}
	vs.Show()

	var o controlplane.Options
	var kubeconfig, logLevel string
	fs := flag.NewFlagSet("zec-controlplane", flag.ExitOnError)
	fs.StringVar(&o.ConfigPath, "config", "/etc/zec-controlplane/config.json", "controlplane configuration file")
	fs.StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig of the cluster served; empty uses the in-cluster service account")
	fs.BoolVar(&o.LeaderElect, "leader-elect", true, "elect a leader among replicas; only the leader reconciles")
	fs.StringVar(&o.LeaseNamespace, "lease-namespace", "kube-system", "namespace of the leader election Lease")
	fs.StringVar(&o.HealthAddr, "health-addr", ":11416", "address serving /healthz and /metrics; empty disables")
	fs.StringVar(&logLevel, "log-level", "", "log level (trace|debug|info|warn|error)")
	_ = fs.Parse(os.Args[1:])
	if logLevel != "" {
		if err := zlog.GetLogger().SetLogLevel(logLevel); err != nil {
			log.Warnf("Ignoring invalid -log-level %q, %v", logLevel, err)
		}
	}
	ctrl.SetLogger(textlogger.NewLogger(textlogger.NewConfig()))

	var err error
	if kubeconfig != "" {
		o.RestConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		o.RestConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		log.Fatalf("kubernetes client config: %v", err)
	}
	o.RestConfig.UserAgent = "zec-controlplane/" + vs.CNIVersion
	// One client serves every node's status writes; client-go's default of 5
	// QPS would queue them on a cluster of any size.
	o.RestConfig.QPS, o.RestConfig.Burst = 50, 100

	if err := controlplane.Run(ctrl.SetupSignalHandler(), o); err != nil {
		log.Fatalf("controlplane: %v", err)
	}
}
