// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Written by hand: the types are small and the repo carries no code generator.
// Any new reference-typed field (slice, map, pointer) must be copied here, or
// two objects will share it.

func (in *ZecNode) DeepCopyInto(out *ZecNode) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *ZecNode) DeepCopy() *ZecNode {
	if in == nil {
		return nil
	}
	out := new(ZecNode)
	in.DeepCopyInto(out)
	return out
}

func (in *ZecNode) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ZecNodeList) DeepCopyInto(out *ZecNodeList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ZecNode, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ZecNodeList) DeepCopy() *ZecNodeList {
	if in == nil {
		return nil
	}
	out := new(ZecNodeList)
	in.DeepCopyInto(out)
	return out
}

func (in *ZecNodeList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ZecNodeSpec) DeepCopyInto(out *ZecNodeSpec) {
	*out = *in
	if in.IPv4.Releasing != nil {
		out.IPv4.Releasing = append([]string(nil), in.IPv4.Releasing...)
	}
}

func (in *ZecNodeStatus) DeepCopyInto(out *ZecNodeStatus) {
	*out = *in
	if in.ENIs != nil {
		out.ENIs = make([]ENIStatus, len(in.ENIs))
		for i := range in.ENIs {
			in.ENIs[i].DeepCopyInto(&out.ENIs[i])
		}
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *ENIStatus) DeepCopyInto(out *ENIStatus) {
	*out = *in
	if in.IPv4 != nil {
		out.IPv4 = append([]IPAddr(nil), in.IPv4...)
	}
	if in.IPv6 != nil {
		v6 := *in.IPv6
		out.IPv6 = &v6
	}
}

func (in *ZecNodeStatus) DeepCopy() *ZecNodeStatus {
	if in == nil {
		return nil
	}
	out := new(ZecNodeStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *ZecNodeSpec) DeepCopy() *ZecNodeSpec {
	if in == nil {
		return nil
	}
	out := new(ZecNodeSpec)
	in.DeepCopyInto(out)
	return out
}
