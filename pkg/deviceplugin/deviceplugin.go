// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package deviceplugin advertises this node's address capacity to kubelet.
//
// The count is a scheduling hint, not an allocation: it constrains only
// workloads that name the resource in their own requests. The daemon hands out
// addresses regardless of what is advertised here.
package deviceplugin

import (
	"context"
	"fmt"
	"path"
	"sync"
	"time"

	"google.golang.org/grpc"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// ENIDevicePlugin implements the kubelet device plugin API for
// ZenlayerNameSpace+ENIIPResourceName. The count it reports is supplied by the
// daemon (addresses this node can still serve), so a pod that requests the
// resource is only scheduled where one is left.
type ENIDevicePlugin struct {
	resourceName string
	apiEndPoint  string
	updateSignal chan int
	server       *grpc.Server

	// ctx is replaced on every kubelet restart while a ListAndWatch handler
	// from the previous round may still be reading it. Unsynchronized on
	// purpose: both values come from context.WithCancel, so the interface's
	// type word never changes and a torn read yields a valid context either
	// way. A context built some other way would break that.
	ctx context.Context

	// countMu guards count, which two goroutines touch: the gRPC handler
	// running ListAndWatch, and whoever calls Update -- in the daemon that is
	// the goroutine watching the instance's quota. Go past this and the race
	// detector fires; without it, a torn read would make the node advertise a
	// count it never held.
	countMu sync.Mutex
	count   int
}

func (eniPlugin *ENIDevicePlugin) getCount() int {
	eniPlugin.countMu.Lock()
	defer eniPlugin.countMu.Unlock()
	return eniPlugin.count
}

func (eniPlugin *ENIDevicePlugin) setCount(count int) {
	eniPlugin.countMu.Lock()
	defer eniPlugin.countMu.Unlock()
	eniPlugin.count = count
}

// NewENIDevicePlugin returns a plugin serving resName on its own socket under
// DevicePluginPath, with initCount as the initial device count.
func NewENIDevicePlugin(resName string, initCount int) *ENIDevicePlugin {
	return &ENIDevicePlugin{
		resourceName: resName,
		apiEndPoint:  path.Join(DevicePluginPath, resName+".sock"),
		count:        initCount,
		updateSignal: make(chan int, 1),
		server:       grpc.NewServer(),
		ctx:          context.TODO(),
	}
}

// GetDevicePluginOptions declares no optional features, so kubelet calls
// neither PreStartContainer nor GetPreferredAllocation; both are stubs below.
func (eniPlugin *ENIDevicePlugin) GetDevicePluginOptions(_ context.Context, _ *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// ListAndWatch streams one synthetic device per reported address, resent on
// every count update and every reportPeriod.
func (eniPlugin *ENIDevicePlugin) ListAndWatch(_ *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	count := eniPlugin.getCount()

	sendResponse := func(count int, s pluginapi.DevicePlugin_ListAndWatchServer) error {
		res := make([]*pluginapi.Device, count)
		for i := 0; i < count; i++ {
			res[i] = &pluginapi.Device{
				ID:     fmt.Sprintf("%v-%d", eniPlugin.resourceName, i),
				Health: pluginapi.Healthy,
			}
		}

		resp := &pluginapi.ListAndWatchResponse{
			Devices: res,
		}
		err := stream.Send(resp)
		log.Infof("Report resources: %v of %v", eniPlugin.resourceName, count)
		if err != nil {
			log.Errorf("Send devices error: %v", err)
			return err
		}
		return nil
	}

	if err := sendResponse(count, stream); err != nil {
		return err
	}
	ticker := time.NewTicker(reportPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			count = eniPlugin.getCount()
			err := sendResponse(count, stream)
			if err != nil {
				return err
			}
		case received := <-eniPlugin.updateSignal:
			// The field and the local must stay in step, or the ticker branch
			// above re-sends the stale field and undoes this update on its
			// next tick.
			eniPlugin.setCount(received)
			count = received
			err := sendResponse(count, stream)
			if err != nil {
				return err
			}
		case <-eniPlugin.ctx.Done():
			return nil
		}
	}
}

// Allocate returns an empty response per container request: the reported
// devices are a count, not real devices, so there is nothing to hand over.
func (eniPlugin *ENIDevicePlugin) Allocate(_ context.Context, request *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	resp := pluginapi.AllocateResponse{
		ContainerResponses: []*pluginapi.ContainerAllocateResponse{},
	}

	for range request.GetContainerRequests() {
		resp.ContainerResponses = append(resp.ContainerResponses,
			&pluginapi.ContainerAllocateResponse{},
		)
	}

	return &resp, nil
}

// PreStartContainer is a stub: GetDevicePluginOptions leaves PreStartRequired
// unset, so kubelet does not call it.
func (eniPlugin *ENIDevicePlugin) PreStartContainer(_ context.Context, _ *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

// GetPreferredAllocation is a stub: GetDevicePluginOptions leaves
// GetPreferredAllocationAvailable unset, so kubelet does not call it.
func (eniPlugin *ENIDevicePlugin) GetPreferredAllocation(_ context.Context, _ *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

// Endpoint returns the path of grpc UDS endpoint
func (eniPlugin *ENIDevicePlugin) Endpoint() string {
	return eniPlugin.apiEndPoint
}

func (eniPlugin *ENIDevicePlugin) ResourceName() string {
	return eniPlugin.resourceName
}

func (eniPlugin *ENIDevicePlugin) Server() *grpc.Server {
	return eniPlugin.server
}

func (eniPlugin *ENIDevicePlugin) ResetServer() {
	eniPlugin.server = grpc.NewServer()
}

func (eniPlugin *ENIDevicePlugin) SetContext(ctx context.Context) {
	eniPlugin.ctx = ctx
}

// Update hands a new count to ListAndWatch. The signal channel holds one value:
// an update still queued is dropped in favor of this one, and if nothing reads
// within 5s only the field is written, which a stream picks up on its next
// read.
func (eniPlugin *ENIDevicePlugin) Update(count int) {
	// A queued value counts as much as the stored one: an earlier Update still
	// waiting to be read carries a different count, and returning here leaves
	// ListAndWatch to deliver it as the current one. The node then advertises a
	// figure it cannot serve until the next poll, five minutes away.
	if count == eniPlugin.getCount() && len(eniPlugin.updateSignal) == 0 {
		return
	}
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	select {
	case eniPlugin.updateSignal <- count:
		return
	case <-t.C:
		eniPlugin.setCount(count)
	case <-eniPlugin.updateSignal:
		// Nothing is reading the channel, so the value sitting in it is stale.
		// Displacing it with this count and storing the count is the update
		// succeeding, not failing.
		eniPlugin.updateSignal <- count
		eniPlugin.setCount(count)
		log.Infof("Replaced an unread resource count with %d", count)
		return
	}
}
