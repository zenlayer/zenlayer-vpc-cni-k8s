// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	k8sTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/tracing"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

const (
	k8sSystemNamespace                      = "kube-system"
	k8sKubeadmConfigmap                     = "kubeadm-config"
	k8sKubeadmConfigmapNetworking           = "MasterConfiguration"
	k8sKubeadmConfigmapClusterconfiguration = "ClusterConfiguration"

	// zenlayerCniConfigmap is the ConfigMap initConfigMapInformer watches. It
	// must stay in sync with config.ZenlayerCniConfigName, which this package
	// cannot import: pkg/daemon/config already imports pkg/k8s.
	zenlayerCniConfigmap = "zenlayer-cni-config"

	eventSourceComponent = "zenlayer-daemon"
)

var (
	log = zlog.GetLogger().WithFields(zlog.Fields{"subsys": "k8s"})
)

type K8sService interface {
	// ListLocalPods list local pod from api server, host network pods excluded
	ListLocalPods(ctx context.Context) ([]*corev1.Pod, error)

	// GetPod get pod from api server
	GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error)

	// ListLocalCachedPods list local pod from local cached(informer)
	ListLocalCachedPods() ([]*corev1.Pod, error)

	// GetLocalCachedPod gets one pod from the local informer cache, which lags
	// the API server in both directions. Callers deciding whether a resource is
	// reclaimable must confirm a miss against the API server first: reclaiming
	// on a stale miss takes the address away from a running pod.
	GetLocalCachedPod(namespace, name string) (*corev1.Pod, error)

	// GetLocalNode get the node itself from api server
	GetLocalNode(ctx context.Context) (*corev1.Node, error)

	// GetConfigMap get configmap from api server
	GetConfigMap(ctx context.Context, namespace, name string) (*corev1.ConfigMap, error)

	// GetClusterID returns ClusterID for this cluster.
	GetClusterID(ctx context.Context) (string, error)

	SetSvcCIDR(svcCidr *zecTypes.IPNetSet) error
	GetServiceCIDR() *zecTypes.IPNetSet

	// RecordNodeEvent record events of node
	RecordNodeEvent(eventType, reason, message string)

	// RecordNodeEventSync writes a node event straight to the API server and
	// returns once it is stored. For a process about to exit: RecordNodeEvent
	// only queues, and the queue dies with the process.
	RecordNodeEventSync(ctx context.Context, eventType, reason, message string) error

	// RecordPodEvent record events of pod
	RecordPodEvent(podName, podNamespace, eventType, reason, message string) error

	// EvictPod evict the specific pod
	EvictPod(ctx context.Context, podName, namespace string) error

	// AddConfigMapEventHandler registers a handler invoked when the
	// zenlayer-cni-config ConfigMap changes. Nothing registers one today: the
	// daemon reads its config once at startup and picks up changes by
	// restarting (the chart's checksum/config annotation rolls the DaemonSet).
	// The informer is the hook for a future hot-reload, and it watches the
	// cluster-wide ConfigMap only -- a node carrying LabelNodeDynamicConfigKey
	// has a different effective ConfigMap, so hot-reload also means teaching
	// the informer to follow that label.
	AddConfigMapEventHandler(handler cache.ResourceEventHandlerFuncs) error

	// PatchPodAnnotation patch annotation to pod
	PatchPodAnnotation(ctx context.Context, namespace, name string, anno map[string]string) error
}

type k8sManager struct {
	nodeName    string
	node        *corev1.Node
	kubeClient  kubernetes.Interface
	broadcaster record.EventBroadcaster
	recorder    record.EventRecorder

	podListerOnce sync.Once
	// podListerReady is closed once podLister is assigned and its cache has
	// synced. Readers block on it rather than on podLister being non-nil,
	// because an unsynced lister answers "not found" for every pod on the node,
	// which is indistinguishable from a genuinely empty node.
	podListerReady    chan struct{}
	svcCIDR           *zecTypes.IPNetSet
	podLister         listers.PodLister
	configMapInformer cache.SharedIndexInformer

	// Guards svcCIDR. Embedded by value rather than declared as a sync.Locker
	// field, whose zero value is a nil interface that panics on Lock unless
	// NewK8sService remembers to fill it in.
	sync.Mutex
}

func (k *k8sManager) ListLocalPods(ctx context.Context) ([]*corev1.Pod, error) {
	options := metav1.ListOptions{
		FieldSelector:   fields.OneTermEqualSelector("spec.nodeName", k.nodeName).String(),
		ResourceVersion: "0",
	}

	list, err := k.kubeClient.CoreV1().Pods(corev1.NamespaceAll).List(ctx, options)
	if err != nil {
		// Wrapped rather than replaced. An RBAC refusal, an expired token and
		// an unreachable API server all land here and need different fixes, and
		// this call is on the PreStart path that decides which addresses the
		// node believes are in use -- so getting it wrong costs node capacity,
		// with the discarded error as the only clue about which of the three it
		// was.
		return nil, fmt.Errorf("failed to list pods on node %s from the apiserver: %w", k.nodeName, err)
	}
	var podList []*corev1.Pod
	for i := range list.Items {
		pod := list.Items[i]
		if pod.Spec.HostNetwork {
			continue
		}
		podList = append(podList, &pod)
	}

	return podList, nil
}

func (k *k8sManager) GetClusterID(ctx context.Context) (string, error) {
	return ClusterID(ctx, k.kubeClient)
}

// ClusterID is the uid of the kube-system namespace. It is set when the
// cluster is built and never changes, so it tells this cluster's interfaces
// from another's in a shared VPC with nothing to configure.
func ClusterID(ctx context.Context, c kubernetes.Interface) (string, error) {
	ns, err := c.CoreV1().Namespaces().Get(ctx, metav1.NamespaceSystem, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read the kube-system namespace for the cluster id: %w", err)
	}
	return string(ns.UID), nil
}

func (k *k8sManager) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return k.kubeClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{ResourceVersion: "0"})
}

func (k *k8sManager) GetLocalNode(ctx context.Context) (*corev1.Node, error) {
	return k.kubeClient.CoreV1().Nodes().Get(ctx, k.nodeName, metav1.GetOptions{ResourceVersion: "0"})
}

func (k *k8sManager) GetConfigMap(ctx context.Context, namespace, name string) (*corev1.ConfigMap, error) {
	return k.kubeClient.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{ResourceVersion: "0"})
}

// serviceCidrFromAPIServer reads the service CIDR from the kubeadm config map.
// It is only reached when serviceCidr was left unset, and a cluster that kubeadm
// did not bootstrap has no such config map, so every error here says how to
// supply the value directly instead -- the failure is fatal to daemon startup.
// The value is a comma separated list, not a single CIDR: a dual-stack cluster
// carries one entry per family (10.96.0.0/12,fd00::/108), which is why it is
// split before parsing rather than parsed whole.
func (k *k8sManager) serviceCidrFromAPIServer() (*zecTypes.IPNetSet, error) {
	kubeadmConfigMap, err := k.GetConfigMap(context.Background(), k8sSystemNamespace, k8sKubeadmConfigmap)
	if err != nil {
		return nil, err
	}

	kubeNetworkingConfig, ok := kubeadmConfigMap.Data[k8sKubeadmConfigmapNetworking]
	if !ok {
		kubeNetworkingConfig, ok = kubeadmConfigMap.Data[k8sKubeadmConfigmapClusterconfiguration]
		if !ok {
			return nil, fmt.Errorf("configmap %s/%s has neither a %s nor a %s key, "+
				"so the service cidr cannot be read from it; set serviceCidr in the cni "+
				"configmap instead (chart value zec.serviceCidr)",
				k8sSystemNamespace, k8sKubeadmConfigmap,
				k8sKubeadmConfigmapNetworking, k8sKubeadmConfigmapClusterconfiguration)
		}
	}

	// Decoded into a typed struct rather than walked as a generic map: a map
	// walk has to assert the nested value to map[interface{}]interface{}, which
	// is what gopkg.in/yaml.v2 produces, while the yaml.v3 this module imports
	// yields map[string]interface{}. A failed assertion reads as "no service
	// subnet configured" and is indistinguishable from one. A struct does not
	// depend on which generic map type the yaml package produces.
	var kubeadmCfg struct {
		Networking struct {
			ServiceSubnet string `yaml:"serviceSubnet"`
		} `yaml:"networking"`
	}
	if err = yaml.Unmarshal([]byte(kubeNetworkingConfig), &kubeadmCfg); err != nil {
		return nil, fmt.Errorf("configmap unmarshal failed, %w", err)
	}

	svcStr := kubeadmCfg.Networking.ServiceSubnet
	if svcStr == "" {
		return nil, fmt.Errorf("configmap %s/%s carries no networking.serviceSubnet; "+
			"set serviceCidr in the cni configmap instead (chart value zec.serviceCidr)",
			k8sSystemNamespace, k8sKubeadmConfigmap)
	}

	parsed := &zecTypes.IPNetSet{}
	for _, cidr := range strings.Split(svcStr, ",") {
		parsed.SetIPNet(strings.TrimSpace(cidr))
	}
	if parsed.IPv4 == nil && parsed.IPv6 == nil {
		return nil, fmt.Errorf("service subnet %q holds no usable cidr", svcStr)
	}
	return parsed, nil
}

func (k *k8sManager) SetSvcCIDR(svcCidr *zecTypes.IPNetSet) error {
	k.Lock()
	defer k.Unlock()

	// Only ask the API server for what the caller did not supply, and take from
	// the answer only the families still missing -- an explicitly configured
	// serviceCIDR stays authoritative for its own family.
	if svcCidr.IPv4 == nil || svcCidr.IPv6 == nil {
		fromAPI, err := k.serviceCidrFromAPIServer()
		if err != nil {
			// A missing IPv4 CIDR is an error, a missing IPv6 one is not: an
			// IPv4-only cluster has none to find. The caller builds the daemon
			// with this, so returning an error here fails startup.
			if svcCidr.IPv4 == nil {
				return fmt.Errorf("error retrieving service cidr: %w", err)
			}
			log.Infof("Could not read the ipv6 service cidr from the api server (%v); continuing without it", err)
		} else {
			if svcCidr.IPv4 == nil {
				svcCidr.IPv4 = fromAPI.IPv4
			}
			if svcCidr.IPv6 == nil {
				svcCidr.IPv6 = fromAPI.IPv6
			}
			if svcCidr.IPv4 == nil {
				return fmt.Errorf("error retrieving service cidr: kubeadm config holds no ipv4 service subnet")
			}
		}
	}

	k.svcCIDR = svcCidr
	return nil
}

func (k *k8sManager) GetServiceCIDR() *zecTypes.IPNetSet {
	k.Lock()
	defer k.Unlock()

	return k.svcCIDR
}

func (k *k8sManager) RecordNodeEvent(eventType, reason, message string) {
	ref := &corev1.ObjectReference{
		Kind:      "Node",
		Name:      k.node.Name,
		UID:       k.node.UID,
		Namespace: "",
	}

	k.recorder.Event(ref, eventType, reason, tracing.TruncateMessage(message))
}

func (k *k8sManager) RecordNodeEventSync(ctx context.Context, eventType, reason, message string) error {
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s.%x", k.node.Name, now.UnixNano()),
			Namespace: metav1.NamespaceDefault,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Node",
			Name: k.node.Name,
			UID:  k.node.UID,
		},
		Reason:         reason,
		Message:        tracing.TruncateMessage(message),
		Type:           eventType,
		Source:         corev1.EventSource{Component: eventSourceComponent, Host: k.node.Name},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}
	_, err := k.kubeClient.CoreV1().Events(ev.Namespace).Create(ctx, ev, metav1.CreateOptions{})
	return err
}

func (k *k8sManager) RecordPodEvent(podName, podNamespace, eventType, reason, message string) error {
	pod, err := k.kubeClient.CoreV1().Pods(podNamespace).Get(context.TODO(), podName, metav1.GetOptions{
		ResourceVersion: "0",
	})

	if err != nil {
		return err
	}

	ref := &corev1.ObjectReference{
		Kind:      "Pod",
		Name:      pod.Name,
		UID:       pod.UID,
		Namespace: pod.Namespace,
	}

	k.recorder.Event(ref, eventType, reason, tracing.TruncateMessage(message))
	return nil
}

func (k *k8sManager) initLocalPodInformer(ctx context.Context) {
	defer runtime.HandleCrash(func(r interface{}) {
		log.Error(r)
	})

	factory := informers.NewSharedInformerFactoryWithOptions(k.kubeClient, 5*time.Minute,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.ParseSelectorOrDie("spec.nodeName=" + k.nodeName).String()
		}))
	podInformer := factory.Core().V1().Pods()
	informer := podInformer.Informer()

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		log.Error("Timeout waiting for pod informer caches to sync")
		runtime.HandleError(fmt.Errorf("timeout waiting for pod informer caches to sync"))
		return
	}
	k.podLister = podInformer.Lister()
	// Once, not a bare close: closing an already closed channel panics. Only
	// NewK8sService starts this today, so a second close cannot happen; the
	// Once is what keeps that true if another start is ever added.
	k.podListerOnce.Do(func() {
		close(k.podListerReady)
		log.Infof("K8s pod informer synced, pod lister ready")
	})
}

func (k *k8sManager) initConfigMapInformer(ctx context.Context) {
	defer runtime.HandleCrash(func(r interface{}) {
		log.Error(r)
	})

	factory := informers.NewSharedInformerFactoryWithOptions(k.kubeClient, 5*time.Minute,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.ParseSelectorOrDie(
				fmt.Sprintf("metadata.name=%s,metadata.namespace=%s",
					zenlayerCniConfigmap, k8sSystemNamespace)).String()
		}))

	k.configMapInformer = factory.Core().V1().ConfigMaps().Informer()
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), k.configMapInformer.HasSynced) {
		log.Error("Timeout waiting for configmap informer caches to sync")
		runtime.HandleError(fmt.Errorf("timeout waiting for configmap informer caches to sync"))
		return
	}
}

func (k *k8sManager) ListLocalCachedPods() ([]*corev1.Pod, error) {
	<-k.podListerReady
	return k.podLister.Pods(corev1.NamespaceAll).List(labels.Everything())
}

func (k *k8sManager) GetLocalCachedPod(namespace, name string) (*corev1.Pod, error) {
	<-k.podListerReady
	ns := corev1.NamespaceAll
	if namespace != "" {
		ns = namespace
	}
	return k.podLister.Pods(ns).Get(name)
}

func (k *k8sManager) PatchPodAnnotation(ctx context.Context, namespace, name string, anno map[string]string) error {
	log.Infof("PatchPodAnnotation ns:%s, pod name:%s, pod annotation:%v", namespace, name, anno)
	return retry.OnError(retry.DefaultBackoff, func(err error) bool {
		return true
	}, func() error {
		patch := map[string]map[string]map[string]string{
			"metadata": {
				"annotations": anno,
			},
		}
		patchData, _ := json.Marshal(patch)
		_, err := k.kubeClient.CoreV1().Pods(namespace).Patch(ctx, name, k8sTypes.StrategicMergePatchType, patchData, metav1.PatchOptions{})
		return err
	})
}

func (k *k8sManager) EvictPod(ctx context.Context, podName, namespace string) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
	}
	return k.kubeClient.PolicyV1().Evictions(namespace).Evict(ctx, eviction)
}

func (k *k8sManager) AddConfigMapEventHandler(handler cache.ResourceEventHandlerFuncs) error {
	_, err := k.configMapInformer.AddEventHandler(handler)
	return err
}

func NewK8sService(ctx context.Context, nodeName string, clientSet kubernetes.Interface) (K8sService, error) {
	node, err := clientSet.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{
		ResourceVersion: "0",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s, err: %s", nodeName, err.Error())
	}

	broadcaster := record.NewBroadcaster()
	source := corev1.EventSource{Component: eventSourceComponent}
	recorder := broadcaster.NewRecorder(scheme.Scheme, source)

	sink := &typedv1.EventSinkImpl{
		Interface: typedv1.New(clientSet.CoreV1().RESTClient()).Events(""),
	}
	broadcaster.StartRecordingToSink(sink)

	k8sM := &k8sManager{
		nodeName:       nodeName,
		kubeClient:     clientSet,
		broadcaster:    broadcaster,
		recorder:       recorder,
		node:           node,
		podListerReady: make(chan struct{}),
	}

	log.Info("Init pod informer...")
	go k8sM.initLocalPodInformer(ctx)
	k8sM.initConfigMapInformer(ctx)
	return k8sM, nil
}
