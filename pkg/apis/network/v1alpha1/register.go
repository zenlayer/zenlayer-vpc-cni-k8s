// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	GroupName = "network.zenlayer.com"
	Version   = "v1alpha1"
	Resource  = "zecnodes"
	Kind      = "ZecNode"
)

var (
	SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}
	// GVR is the resource both sides read and write.
	GVR = SchemeGroupVersion.WithResource(Resource)

	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, &ZecNode{}, &ZecNodeList{})
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
