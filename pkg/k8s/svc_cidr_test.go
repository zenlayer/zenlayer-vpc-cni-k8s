// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// managerWithKubeadmConfig builds a k8sManager whose only reachable state is a
// kubeadm-config carrying the given ClusterConfiguration body.
func managerWithKubeadmConfig(body string) *k8sManager {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: k8sKubeadmConfigmap, Namespace: k8sSystemNamespace},
		Data:       map[string]string{k8sKubeadmConfigmapClusterconfiguration: body},
	}
	return &k8sManager{kubeClient: fake.NewSimpleClientset(cm)}
}

// realKubeadmBody is the shape kubeadm actually writes on a v1.34 cluster, with
// only the certSAN replaced by a documentation address. The nesting is what
// matters: serviceSubnet sits under networking, so reading it exercises the
// decode of a nested mapping -- the step that used to fail. The surrounding
// keys are kept because the decode has to skip them.
const realKubeadmBody = `apiServer:
  certSANs:
  - 192.0.2.10
apiVersion: kubeadm.k8s.io/v1beta4
clusterName: kubernetes
kind: ClusterConfiguration
kubernetesVersion: v1.34.10
networking:
  dnsDomain: cluster.local
  serviceSubnet: %s
proxy: {}
scheduler: {}
`

func kubeadmBodyWith(subnet string) string {
	return fmt.Sprintf(realKubeadmBody, subnet)
}

// TestServiceCidrFromAPIServerReadsNestedServiceSubnet is the regression this
// file exists for.
//
// The reader used to decode into map[interface{}]interface{} and assert the
// nested value to the same type. That is gopkg.in/yaml.v2 behavior; v3, which
// this module uses, produces map[string]interface{}, so the assertion was always
// false and every read reported the service cidr as missing -- on a kubeadm
// config that was perfectly well formed.
//
// It stayed invisible because the chart makes zec.serviceCidr required, so
// deployments always supplied IPv4 explicitly. Only a cluster without it saw the
// real symptom: the daemon CrashLooping at startup.
func TestServiceCidrFromAPIServerReadsNestedServiceSubnet(t *testing.T) {
	for _, tc := range []struct {
		name     string
		subnet   string
		wantV4   string
		wantV6   string
		wantFail bool
	}{
		{
			name:   "single stack ipv4",
			subnet: "10.96.0.0/12",
			wantV4: "10.96.0.0/12",
		},
		{
			// The reason the comma split exists: every dual-stack cluster writes
			// this, and parsing the whole string as one CIDR fails on all of them.
			name:   "dual stack comma list",
			subnet: "10.96.0.0/12,fd00::/108",
			wantV4: "10.96.0.0/12",
			wantV6: "fd00::/108",
		},
		{
			name:   "dual stack with spaces after the comma",
			subnet: "10.96.0.0/12, fd00::/108",
			wantV4: "10.96.0.0/12",
			wantV6: "fd00::/108",
		},
		{
			name:   "ipv6 first",
			subnet: "fd00::/108,10.96.0.0/12",
			wantV4: "10.96.0.0/12",
			wantV6: "fd00::/108",
		},
		{
			name:     "nothing parseable",
			subnet:   "not-a-cidr",
			wantFail: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := managerWithKubeadmConfig(kubeadmBodyWith(tc.subnet)).serviceCidrFromAPIServer()
			if tc.wantFail {
				if err == nil {
					t.Fatalf("serviceCidrFromAPIServer() = %v, want an error for %q", got, tc.subnet)
				}
				return
			}
			if err != nil {
				t.Fatalf("serviceCidrFromAPIServer(%q): %v", tc.subnet, err)
			}
			gotV4, gotV6 := "", ""
			if got.IPv4 != nil {
				gotV4 = got.IPv4.String()
			}
			if got.IPv6 != nil {
				gotV6 = got.IPv6.String()
			}
			if gotV4 != tc.wantV4 || gotV6 != tc.wantV6 {
				t.Errorf("serviceCidrFromAPIServer(%q) = v4:%q v6:%q, want v4:%q v6:%q",
					tc.subnet, gotV4, gotV6, tc.wantV4, tc.wantV6)
			}
		})
	}
}

// TestServiceCidrFromAPIServerRejectsConfigWithoutNetworking covers the genuine
// "not there" case, so the error the bug used to produce still means what it
// says.
func TestServiceCidrFromAPIServerRejectsConfigWithoutNetworking(t *testing.T) {
	body := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nproxy: {}\n"
	if got, err := managerWithKubeadmConfig(body).serviceCidrFromAPIServer(); err == nil {
		t.Fatalf("serviceCidrFromAPIServer() = %v, want an error when networking is absent", got)
	}
}
