// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package deviceplugin

import (
	"time"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	// ZenlayerNameSpace is the prefix the names below are registered and
	// requested under, giving "zenlayer.com/eni-ip" and the like.
	ZenlayerNameSpace = "zenlayer.com/"
	// ENIResourceName is the resource name of an exclusive ENI. Nothing
	// advertises it; it is only matched against pod resource requests.
	ENIResourceName = "eni"
	// ENIIPResourceName is the resource name of an IP address shared on an ENI.
	// It is the one resource this daemon registers with kubelet.
	ENIIPResourceName = "eni-ip"
)

// reportPeriod is how often ListAndWatch re-sends the device list when nothing
// has changed; an update to the count is sent as it arrives, not on this tick.
const reportPeriod = time.Minute * 3

// DevicePluginPath is kubelet's device plugin directory: the plugin sockets are
// created there, and KubeletSocket, the socket registration is sent to, is
// watched inside it because its recreation is how a kubelet restart is noticed.
var (
	DevicePluginPath = pluginapi.DevicePluginPath
	KubeletSocket    = DevicePluginPath + "kubelet.sock"
)

// Manager is the interface of device plugin manager.
type Manager interface {
	AddPlugin(plugin Plugin)
	Plugin(resourceName string) Plugin
	Serve(ctx context.Context) error
	Stop()
	Update(resourceName string, count int) error
}

// Plugin is the interface for generic device plugin which can be managed by Manager.
type Plugin interface {
	pluginapi.DevicePluginServer
	Endpoint() string
	ResourceName() string
	Server() *grpc.Server
	ResetServer()
	SetContext(ctx context.Context)
	Update(count int)
}
