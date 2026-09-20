// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"reflect"
	"strings"
	"testing"
)

func TestPodKey(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		podName   string
		want      string
	}{
		{name: "namespaced pod", namespace: "default", podName: "nginx", want: "default/nginx"},
		{name: "empty namespace", namespace: "", podName: "nginx", want: "/nginx"},
		{name: "empty name", namespace: "default", podName: "", want: "default/"},
		{name: "both empty", want: "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PodKey(tt.namespace, tt.podName); got != tt.want {
				t.Errorf("PodKey(%q, %q) = %q, want %q", tt.namespace, tt.podName, got, tt.want)
			}
		})
	}
}

func TestPod_GetNetResourceByType(t *testing.T) {
	eniIP1 := ResourceItem{Type: NetResourceTypeEniIp, ID: "eni-1/10.0.0.1", ENIId: "eni-1", IPv4: "10.0.0.1"}
	eniIP2 := ResourceItem{Type: NetResourceTypeEniIp, ID: "eni-1/10.0.0.2", ENIId: "eni-1", IPv4: "10.0.0.2"}
	eni1 := ResourceItem{Type: NetResourceTypeEni, ID: "eni-1", ENIId: "eni-1"}

	tests := []struct {
		name  string
		pod   *Pod
		rType string
		want  []ResourceItem
	}{
		{
			name:  "nil pod yields nil",
			pod:   nil,
			rType: NetResourceTypeEniIp,
		},
		{
			name:  "pod with no resources yields nil",
			pod:   &Pod{Namespace: "default", Name: "p"},
			rType: NetResourceTypeEniIp,
		},
		{
			name:  "matching entries are returned in order",
			pod:   &Pod{Resources: []ResourceItem{eniIP1, eni1, eniIP2}},
			rType: NetResourceTypeEniIp,
			want:  []ResourceItem{eniIP1, eniIP2},
		},
		{
			name:  "the other type is filtered out",
			pod:   &Pod{Resources: []ResourceItem{eniIP1, eni1, eniIP2}},
			rType: NetResourceTypeEni,
			want:  []ResourceItem{eni1},
		},
		{
			name:  "no match yields nil",
			pod:   &Pod{Resources: []ResourceItem{eniIP1, eniIP2}},
			rType: NetResourceTypeEni,
		},
		{
			name:  "an unknown type matches nothing",
			pod:   &Pod{Resources: []ResourceItem{eniIP1, eni1}},
			rType: "other",
		},
		{
			name:  "the empty type matches only resources with an empty type",
			pod:   &Pod{Resources: []ResourceItem{eniIP1, {ID: "typeless"}}},
			rType: "",
			want:  []ResourceItem{{ID: "typeless"}},
		},
		{
			name:  "matching is case sensitive",
			pod:   &Pod{Resources: []ResourceItem{eniIP1}},
			rType: strings.ToUpper(NetResourceTypeEniIp),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.pod.GetNetResourceByType(tt.rType)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetNetResourceByType(%q) = %+v, want %+v", tt.rType, got, tt.want)
			}
		})
	}
}

func TestPod_String(t *testing.T) {
	t.Run("nil receiver yields an empty string", func(t *testing.T) {
		var p *Pod
		if got := p.String(); got != "" {
			t.Errorf("String() = %q, want empty", got)
		}
	})

	t.Run("rendered output carries the identifying fields", func(t *testing.T) {
		p := &Pod{
			Namespace:      "default",
			Name:           "nginx",
			NetNs:          "/var/run/netns/cni-1",
			PodNetworkMode: PodNetworkModeENIShare,
			Resources: []ResourceItem{
				{Type: NetResourceTypeEniIp, ID: "eni-1/10.0.0.1", ENIId: "eni-1", IPv4: "10.0.0.1"},
			},
		}
		got := p.String()
		for _, want := range []string{"default", "nginx", "/var/run/netns/cni-1", PodNetworkModeENIShare, "eni-1/10.0.0.1"} {
			if !strings.Contains(got, want) {
				t.Errorf("String() = %q, missing %q", got, want)
			}
		}
	})
}
