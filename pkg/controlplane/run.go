// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/apis/network/v1alpha1"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/factory"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/k8s"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/metrics"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/tracing"
)

// Options are the controlplane's command-line settings.
type Options struct {
	ConfigPath     string
	RestConfig     *rest.Config
	LeaderElect    bool
	LeaseNamespace string
	// HealthAddr serves /healthz and /metrics.
	HealthAddr string
}

const dependencyRetry = 30 * time.Second

// Run starts the controlplane and blocks until ctx ends. A dependency that is
// not there yet -- the credentials, the platform, the CRD -- is waited for,
// not treated as fatal: the controlplane is deployed before the cluster it
// serves is complete, and a crash loop would only delay it.
func Run(ctx context.Context, o Options) error {
	cfg, err := LoadConfig(o.ConfigPath)
	if err != nil {
		return err
	}
	metrics.PrometheusRegister()
	// The platform code shared with the daemon raises node events through
	// tracing, which names no node: one controlplane serves them all. What a
	// node must see, the reconciler reports on that Node itself (warnOnce);
	// the rest is logged here rather than dropped without a trace.
	tracing.RegisterEventRecorder(func(eventType, reason, message string) {
		log.Infof("Platform event %s %s: %s", eventType, reason, message)
	}, func(_, _, _, _, _ string) error { return nil })

	var platform *Platform
	// The health handler runs on its own goroutine: it reads what Run has
	// reached through atomics, and reports 503 until reconciling has started.
	var ready atomic.Pointer[Platform]
	var waiting atomic.Pointer[string]
	setWaiting := func(s string) { waiting.Store(&s) }
	setWaiting("starting")
	health := func() string {
		if w := waiting.Load(); w != nil && *w != "" {
			return *w
		}
		if p := ready.Load(); p != nil {
			return p.Health()
		}
		return healthCredentialInvalid
	}
	go serveHealth(ctx, o.HealthAddr, health)

	setWaiting("waiting for OpenAPI credentials")
	if err := waitFor(ctx, "OpenAPI credentials", func() error {
		p, err := NewPlatform(cfg)
		if err == nil {
			platform = p
		}
		return err
	}); err != nil {
		return quiet(ctx, err)
	}
	ready.Store(platform)
	go platform.RunCredentialReload(ctx)

	stack := types.IPStack(cfg.IPStack)
	var subnets factory.SubnetManager
	setWaiting("waiting for the pod subnets")
	if err := waitFor(ctx, "pod subnets", func() error {
		s, err := factory.NewPodSubnetManager(cfg.RegionID, cfg.VpcID, platform, factory.WithIPStack(stack))
		if err != nil {
			return err
		}
		if err := s.FlushSubnets(cfg.Subnets...); err != nil {
			return err
		}
		if err := factory.ValidateSubnetsForStack(s, stack, cfg.Subnets); err != nil {
			return err
		}
		subnets = s
		return nil
	}); err != nil {
		return quiet(ctx, err)
	}

	// The sweep tells this cluster's interfaces from others' in a shared VPC by
	// it, so the controlplane does not run without it.
	var clusterID string
	setWaiting("waiting for the cluster id")
	if err := waitFor(ctx, "the cluster id", func() error {
		cs, err := kubernetes.NewForConfig(o.RestConfig)
		if err != nil {
			return err
		}
		clusterID, err = k8s.ClusterID(ctx, cs)
		return err
	}); err != nil {
		return quiet(ctx, err)
	}

	setWaiting("waiting for the ZecNode CRD")
	if err := waitFor(ctx, "the ZecNode CRD", func() error { return crdServed(o.RestConfig) }); err != nil {
		return quiet(ctx, err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(o.RestConfig, ctrl.Options{
		Scheme:                  scheme,
		LeaderElection:          o.LeaderElect,
		LeaderElectionID:        "zenlayer-vpc-cni-controlplane",
		LeaderElectionNamespace: o.LeaseNamespace,
		// Metrics are served with /healthz from the platform metrics registry.
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	billing := cfg.IPv6Billing()
	manage := *cfg.ManageUntaggedENI
	r := &Reconciler{
		Client:    mgr.GetClient(),
		Cfg:       cfg,
		Lookup:    platform,
		Subnets:   subnets,
		Recorder:  mgr.GetEventRecorderFor("zenlayer-vpc-cni-controlplane"),
		APIReader: mgr.GetAPIReader(),
		NewNodePlatform: func(id *identity) nodePlatform {
			region := id.regionID
			if region == "" {
				region = cfg.RegionID
			}
			return factory.NewForInstance(platform, stack, subnets,
				&factory.InstanceMetadata{InstanceId: id.instanceID, RegionId: region, ZoneId: id.zoneID},
				billing, manage, clusterID)
		},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up reconciler: %w", err)
	}
	// One sweep for the whole VPC, on the leader only: interfaces left by a
	// deleted instance belong to no node any pass would look at.
	sweeper := factory.NewForInstance(platform, stack, subnets,
		&factory.InstanceMetadata{RegionId: cfg.RegionID}, billing, manage, clusterID)
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		wait.JitterUntilWithContext(ctx, func(ctx context.Context) {
			sweeper.CleanUpLeakedENIsInVPC(cfg.VpcID)
			r.gcZecNodes(ctx)
		}, leakSweepPeriod, 0.1, true)
		return nil
	})); err != nil {
		return fmt.Errorf("set up the leaked interface sweep: %w", err)
	}
	log.Infof("controlplane starting: vpc %s, subnets %v, ipStack %s, providerID %s",
		cfg.VpcID, cfg.Subnets, cfg.IPStack, cfg.Identity.ProviderID)
	setWaiting("")
	return quiet(ctx, mgr.Start(ctx))
}

// quiet turns the error of a wait cut short by shutdown into a clean exit.
func quiet(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// waitFor retries f until it succeeds or ctx ends.
func waitFor(ctx context.Context, what string, f func() error) error {
	return wait.PollUntilContextCancel(ctx, dependencyRetry, true, func(context.Context) (bool, error) {
		if err := f(); err != nil {
			log.Warnf("Waiting for %s: %v", what, err)
			return false, nil
		}
		return true, nil
	})
}

func crdServed(cfg *rest.Config) error {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}
	list, err := dc.ServerResourcesForGroupVersion(v1alpha1.SchemeGroupVersion.String())
	if err != nil {
		return err
	}
	for _, r := range list.APIResources {
		if r.Name == v1alpha1.Resource {
			return nil
		}
	}
	return errors.New("zecnodes is not served yet")
}

func serveHealth(ctx context.Context, addr string, health func() string) {
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if h := health(); h != healthOK {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, h)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	metrics.ServeMetrics(mux)
	srv := &http.Server{Addr: addr, Handler: mux, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Errorf("health server on %s: %v", addr, err)
	}
}
