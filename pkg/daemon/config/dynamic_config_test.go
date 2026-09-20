// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// fakeK8s implements k8s.K8sService with just enough behavior for
// getDynamicConfig. Everything the function does not call panics, so a change
// that starts calling more of the interface is noticed rather than silently
// getting a zero value.
type fakeK8s struct {
	nodeLabels map[string]string
	nodeErr    error

	configMaps map[string]*corev1.ConfigMap // keyed by "namespace/name"
	cmErr      error

	// gotNamespace / gotName record what GetConfigMap was asked for.
	gotNamespace string
	gotName      string
}

func (f *fakeK8s) GetLocalNode(context.Context) (*corev1.Node, error) {
	if f.nodeErr != nil {
		return nil, f.nodeErr
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: f.nodeLabels}}, nil
}

func (f *fakeK8s) GetConfigMap(_ context.Context, namespace, name string) (*corev1.ConfigMap, error) {
	f.gotNamespace, f.gotName = namespace, name
	if f.cmErr != nil {
		return nil, f.cmErr
	}
	cm, ok := f.configMaps[namespace+"/"+name]
	if !ok {
		return nil, errors.New("configmap " + namespace + "/" + name + " not found")
	}
	return cm, nil
}

func (f *fakeK8s) ListLocalPods(context.Context) ([]*corev1.Pod, error) { panic("not called") }
func (f *fakeK8s) GetPod(context.Context, string, string) (*corev1.Pod, error) {
	panic("not called")
}
func (f *fakeK8s) ListLocalCachedPods() ([]*corev1.Pod, error)           { panic("not called") }
func (f *fakeK8s) GetLocalCachedPod(string, string) (*corev1.Pod, error) { panic("not called") }
func (f *fakeK8s) SetSvcCIDR(*zecTypes.IPNetSet) error                   { panic("not called") }
func (f *fakeK8s) GetServiceCIDR() *zecTypes.IPNetSet                    { panic("not called") }
func (f *fakeK8s) RecordNodeEvent(string, string, string)                { panic("not called") }
func (f *fakeK8s) RecordPodEvent(string, string, string, string, string) error {
	panic("not called")
}
func (f *fakeK8s) EvictPod(context.Context, string, string) error { panic("not called") }
func (f *fakeK8s) AddConfigMapEventHandler(cache.ResourceEventHandlerFuncs) error {
	panic("not called")
}
func (f *fakeK8s) PatchPodAnnotation(context.Context, string, string, map[string]string) error {
	panic("not called")
}

func configMap(namespace, name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data:       data,
	}
}

func TestGetNodeDynamicConfigName(t *testing.T) {
	tests := []struct {
		name    string
		labels  map[string]string
		nodeErr error
		want    string
		wantErr bool
	}{
		{name: "no labels at all", labels: nil, want: ""},
		{name: "label absent", labels: map[string]string{"other": "x"}, want: ""},
		{
			name:   "label present",
			labels: map[string]string{zecTypes.LabelNodeDynamicConfigKey: "ns.name"},
			want:   "ns.name",
		},
		{
			name:   "empty label value is treated as absent",
			labels: map[string]string{zecTypes.LabelNodeDynamicConfigKey: ""},
			want:   "",
		},
		{name: "node lookup fails", nodeErr: errors.New("api down"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeK8s{nodeLabels: tt.labels, nodeErr: tt.nodeErr}
			got, err := getNodeDynamicConfigName(context.Background(), f)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("getNodeDynamicConfigName() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("getNodeDynamicConfigName() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("getNodeDynamicConfigName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetDynamicConfig(t *testing.T) {
	const cmContent = `{"securityGroup":"sg-1"}`

	tests := []struct {
		name          string
		labels        map[string]string
		nodeErr       error
		configMaps    map[string]*corev1.ConfigMap
		cmErr         error
		namespace     string
		wantContent   string
		wantErrSubstr string
		wantNamespace string
		wantName      string
	}{
		{
			name:      "unlabelled node uses the namespace and default name passed in",
			namespace: Namespace,
			configMaps: map[string]*corev1.ConfigMap{
				Namespace + "/" + ZenlayerCniConfigName: configMap(Namespace, ZenlayerCniConfigName, map[string]string{"eni_conf": cmContent}),
			},
			wantContent:   cmContent,
			wantNamespace: Namespace,
			wantName:      ZenlayerCniConfigName,
		},
		{
			// The label sits on the Node, and a kubelet may set labels on its
			// own Node with any prefix outside kubernetes.io -- this one is not.
			// Following it into another namespace would let a node that has been
			// taken over hand its own daemon an eni_conf naming an openAPIDomain
			// the attacker runs, and the daemon sends its OpenAPI credentials
			// there. Refused, not quietly ignored: falling back to the
			// cluster-wide config hides a typo and an attempt equally well.
			name:      "a config in another namespace is refused",
			namespace: Namespace,
			labels:    map[string]string{zecTypes.LabelNodeDynamicConfigKey: "custom-ns.custom-cm"},
			configMaps: map[string]*corev1.ConfigMap{
				Namespace + "/" + ZenlayerCniConfigName: configMap(Namespace, ZenlayerCniConfigName, map[string]string{"eni_conf": "cluster-wide"}),
				"custom-ns/custom-cm":                   configMap("custom-ns", "custom-cm", map[string]string{"eni_conf": "node-specific"}),
			},
			wantErrSubstr: "only read from this daemon's own namespace",
		},
		{
			name:      "a config named in this daemon's own namespace is used",
			namespace: Namespace,
			labels:    map[string]string{zecTypes.LabelNodeDynamicConfigKey: Namespace + ".custom-cm"},
			configMaps: map[string]*corev1.ConfigMap{
				Namespace + "/" + ZenlayerCniConfigName: configMap(Namespace, ZenlayerCniConfigName, map[string]string{"eni_conf": "cluster-wide"}),
				Namespace + "/custom-cm":                configMap(Namespace, "custom-cm", map[string]string{"eni_conf": "node-specific"}),
			},
			wantContent:   "node-specific",
			wantNamespace: Namespace,
			wantName:      "custom-cm",
		},
		{
			// SplitN(..., 2) means only the first dot separates; the rest is
			// part of the name, which is legal for a ConfigMap name.
			name:      "only the first dot separates namespace from name",
			namespace: Namespace,
			labels:    map[string]string{zecTypes.LabelNodeDynamicConfigKey: Namespace + ".a.b"},
			configMaps: map[string]*corev1.ConfigMap{
				Namespace + "/a.b": configMap(Namespace, "a.b", map[string]string{"eni_conf": cmContent}),
			},
			wantContent:   cmContent,
			wantNamespace: Namespace,
			wantName:      "a.b",
		},
		{
			name:          "label without a dot is rejected",
			namespace:     Namespace,
			labels:        map[string]string{zecTypes.LabelNodeDynamicConfigKey: "no-dot"},
			wantErrSubstr: "must like nameSpace.name",
		},
		{
			name:          "node lookup failure is propagated",
			namespace:     Namespace,
			nodeErr:       errors.New("api down"),
			wantErrSubstr: "api down",
		},
		{
			name:          "configmap lookup failure is propagated",
			namespace:     Namespace,
			cmErr:         errors.New("forbidden"),
			wantErrSubstr: "forbidden",
		},
		{
			name:      "configmap without the eni_conf key is rejected",
			namespace: Namespace,
			configMaps: map[string]*corev1.ConfigMap{
				Namespace + "/" + ZenlayerCniConfigName: configMap(Namespace, ZenlayerCniConfigName, map[string]string{"other": "x"}),
			},
			wantErrSubstr: "configmap not included eni_conf",
		},
		{
			name:      "configmap with no data at all is rejected",
			namespace: Namespace,
			configMaps: map[string]*corev1.ConfigMap{
				Namespace + "/" + ZenlayerCniConfigName: configMap(Namespace, ZenlayerCniConfigName, nil),
			},
			wantErrSubstr: "configmap not included eni_conf",
		},
		{
			// An eni_conf key that is present but empty is returned as-is;
			// MergeConfigAndUnmarshal is what rejects it later.
			name:      "empty eni_conf is returned without error",
			namespace: Namespace,
			configMaps: map[string]*corev1.ConfigMap{
				Namespace + "/" + ZenlayerCniConfigName: configMap(Namespace, ZenlayerCniConfigName, map[string]string{"eni_conf": ""}),
			},
			wantContent:   "",
			wantNamespace: Namespace,
			wantName:      ZenlayerCniConfigName,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeK8s{
				nodeLabels: tt.labels,
				nodeErr:    tt.nodeErr,
				configMaps: tt.configMaps,
				cmErr:      tt.cmErr,
			}

			got, err := getDynamicConfig(context.Background(), f, tt.namespace, "node-1")
			if tt.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("getDynamicConfig() = %q, want an error", got)
				}
				if !strings.Contains(err.Error(), tt.wantErrSubstr) {
					t.Fatalf("getDynamicConfig() error = %q, want it to contain %q", err.Error(), tt.wantErrSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("getDynamicConfig() error = %v", err)
			}
			if got != tt.wantContent {
				t.Errorf("getDynamicConfig() = %q, want %q", got, tt.wantContent)
			}
			if f.gotNamespace != tt.wantNamespace || f.gotName != tt.wantName {
				t.Errorf("looked up configmap %s/%s, want %s/%s", f.gotNamespace, f.gotName, tt.wantNamespace, tt.wantName)
			}
		})
	}
}

// TestEmptyDynamicConfigIsRejectedDownstream completes the empty-eni_conf case
// above: getDynamicConfig passes an empty value through, and this is where it is
// rejected.
func TestEmptyDynamicConfigIsRejectedDownstream(t *testing.T) {
	if _, err := MergeConfigAndUnmarshal([]byte(""), nil); err == nil {
		t.Error("MergeConfigAndUnmarshal() error = nil, want an empty eni_conf to be rejected")
	}
}

// TestGetDynamicConfigIgnoresNodeNameArgument locks in that getDynamicConfig
// resolves the node through the k8s service rather than from its nodeName
// argument, so a caller passing the wrong value cannot change the outcome
// unnoticed.
func TestGetDynamicConfigIgnoresNodeNameArgument(t *testing.T) {
	newFake := func() *fakeK8s {
		return &fakeK8s{
			configMaps: map[string]*corev1.ConfigMap{
				Namespace + "/" + ZenlayerCniConfigName: configMap(Namespace, ZenlayerCniConfigName, map[string]string{"eni_conf": "x"}),
			},
		}
	}
	a, errA := getDynamicConfig(context.Background(), newFake(), Namespace, "node-1")
	b, errB := getDynamicConfig(context.Background(), newFake(), Namespace, "totally-different")
	if errA != nil || errB != nil {
		t.Fatalf("getDynamicConfig() errors: %v / %v", errA, errB)
	}
	if a != b {
		t.Errorf("getDynamicConfig() depends on nodeName: %q vs %q", a, b)
	}
}
