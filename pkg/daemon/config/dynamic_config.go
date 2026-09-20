// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/k8s"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

const (
	Namespace             = "kube-system"
	ZenlayerCniConfigName = "zenlayer-cni-config"
)

func getNodeDynamicConfigName(ctx context.Context, k8s k8s.K8sService) (string, error) {
	node, err := k8s.GetLocalNode(ctx)
	if err != nil {
		return "", err
	}
	cfName, exist := node.Labels[zecTypes.LabelNodeDynamicConfigKey]
	if !exist {
		return "", nil
	}
	return cfName, nil
}

func getDynamicConfig(ctx context.Context, k8s k8s.K8sService, namespace, nodeName string) (string, error) {
	cfName := ZenlayerCniConfigName
	nodeCfName, err := getNodeDynamicConfigName(ctx, k8s)
	if err != nil {
		return "", err
	}
	if nodeCfName != "" {
		nsAndName := strings.SplitN(nodeCfName, ".", 2)
		if length := len(nsAndName); length != 2 {
			log.Errorf("Dynamic config label %s err[must like nameSpace.name]", nodeCfName)
			return "", fmt.Errorf("dynamic config label %s err[must like nameSpace.name]", nodeCfName)
		}
		// The namespace has to be this daemon's own, and the label may not send
		// it elsewhere. The label sits on the Node, and a kubelet is allowed to
		// set labels on its own Node as long as the prefix is not kubernetes.io
		// -- this one is not -- so a node that has been taken over could
		// otherwise point its own daemon at a ConfigMap in a namespace the
		// attacker writes to. What that buys is not merely different pool
		// sizing: eni_conf carries openAPIDomain, and the daemon sends the
		// OpenAPI credentials it holds to whatever host that names.
		//
		// Refused rather than quietly falling back to the cluster-wide config,
		// which would hide a typo and an attempt equally well.
		if nsAndName[0] != namespace {
			return "", fmt.Errorf("node label %s names a config in namespace %q, but per-node config "+
				"is only read from this daemon's own namespace %q",
				zecTypes.LabelNodeDynamicConfigKey, nsAndName[0], namespace)
		}
		cfName = nsAndName[1]
	}
	cniConfigMap, err := k8s.GetConfigMap(ctx, namespace, cfName)
	if err != nil {
		return "", err
	}
	content, ok := cniConfigMap.Data["eni_conf"]
	if ok {
		return content, nil
	}
	return "", errors.New("configmap not included eni_conf")
}
