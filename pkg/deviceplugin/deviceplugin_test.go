// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package deviceplugin

import (
	"context"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func TestResourceNameConstants(t *testing.T) {
	// Namespace plus short name is what reaches kubelet and what a pod requests
	// in resources.requests, so renaming either half breaks manifests that
	// already ask for the resource.
	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "namespace ends with a slash", got: ZenlayerNameSpace, want: "zenlayer.com/"},
		{name: "eni resource", got: ZenlayerNameSpace + ENIResourceName, want: "zenlayer.com/eni"},
		{name: "eni ip resource", got: ZenlayerNameSpace + ENIIPResourceName, want: "zenlayer.com/eni-ip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}

	t.Run("kubelet socket lives under the device plugin path", func(t *testing.T) {
		if !strings.HasPrefix(KubeletSocket, DevicePluginPath) {
			t.Errorf("KubeletSocket %q is not under DevicePluginPath %q", KubeletSocket, DevicePluginPath)
		}
		if !strings.HasSuffix(KubeletSocket, "kubelet.sock") {
			t.Errorf("KubeletSocket = %q, want it to end with kubelet.sock", KubeletSocket)
		}
	})
}

func TestNewENIDevicePlugin(t *testing.T) {
	tests := []struct {
		name      string
		resName   string
		initCount int
	}{
		{name: "eni", resName: ENIResourceName, initCount: 0},
		{name: "eni ip with a positive count", resName: ENIIPResourceName, initCount: 10},
		{name: "empty resource name", resName: "", initCount: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := NewENIDevicePlugin(tt.resName, tt.initCount)
			if plugin == nil {
				t.Fatal("NewENIDevicePlugin returned nil")
			}
			if got := plugin.ResourceName(); got != tt.resName {
				t.Errorf("ResourceName() = %q, want %q", got, tt.resName)
			}
			wantEndpoint := path.Join(DevicePluginPath, tt.resName+".sock")
			if got := plugin.Endpoint(); got != wantEndpoint {
				t.Errorf("Endpoint() = %q, want %q", got, wantEndpoint)
			}
			if !strings.HasSuffix(plugin.Endpoint(), ".sock") {
				t.Errorf("Endpoint() = %q, want a .sock path", plugin.Endpoint())
			}
			if plugin.Server() == nil {
				t.Error("Server() = nil, want a grpc server")
			}
		})
	}

	t.Run("distinct resources get distinct endpoints", func(t *testing.T) {
		a := NewENIDevicePlugin(ENIResourceName, 0)
		b := NewENIDevicePlugin(ENIIPResourceName, 0)
		if a.Endpoint() == b.Endpoint() {
			t.Errorf("both plugins share the endpoint %q", a.Endpoint())
		}
	})
}

func TestENIDevicePluginResetServer(t *testing.T) {
	plugin := NewENIDevicePlugin(ENIResourceName, 0)
	first := plugin.Server()
	plugin.ResetServer()
	second := plugin.Server()

	if second == nil {
		t.Fatal("Server() after ResetServer() = nil, want a new grpc server")
	}
	if first == second {
		t.Error("ResetServer() kept the previous grpc server, want a fresh one")
	}
}

func TestENIDevicePluginStaticRPCs(t *testing.T) {
	plugin := NewENIDevicePlugin(ENIResourceName, 3)
	ctx := context.Background()

	t.Run("GetDevicePluginOptions", func(t *testing.T) {
		got, err := plugin.GetDevicePluginOptions(ctx, &pluginapi.Empty{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("got nil options")
		}
		if got.PreStartRequired || got.GetPreferredAllocationAvailable {
			t.Errorf("got %+v, want all options disabled", got)
		}
	})

	t.Run("PreStartContainer is a no-op", func(t *testing.T) {
		got, err := plugin.PreStartContainer(ctx, &pluginapi.PreStartContainerRequest{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("got nil response")
		}
	})

	t.Run("GetPreferredAllocation is a no-op", func(t *testing.T) {
		got, err := plugin.GetPreferredAllocation(ctx, &pluginapi.PreferredAllocationRequest{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("got nil response")
		}
		if len(got.ContainerResponses) != 0 {
			t.Errorf("got %d container responses, want 0", len(got.ContainerResponses))
		}
	})
}

func TestENIDevicePluginAllocate(t *testing.T) {
	plugin := NewENIDevicePlugin(ENIResourceName, 5)

	tests := []struct {
		name     string
		requests int
	}{
		{name: "no container requests", requests: 0},
		{name: "single container", requests: 1},
		{name: "multiple containers", requests: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &pluginapi.AllocateRequest{}
			for i := 0; i < tt.requests; i++ {
				req.ContainerRequests = append(req.ContainerRequests,
					&pluginapi.ContainerAllocateRequest{DevicesIDs: []string{"eni-0"}})
			}

			resp, err := plugin.Allocate(context.Background(), req)
			if err != nil {
				t.Fatalf("Allocate failed: %v", err)
			}
			if resp == nil {
				t.Fatal("Allocate returned a nil response")
			}
			if len(resp.ContainerResponses) != tt.requests {
				t.Fatalf("got %d container responses, want %d", len(resp.ContainerResponses), tt.requests)
			}
			// The plugin allocates nothing: every response must be empty.
			for i, cr := range resp.ContainerResponses {
				if cr == nil {
					t.Fatalf("container response %d is nil", i)
				}
				if len(cr.Devices) != 0 || len(cr.Mounts) != 0 || len(cr.Envs) != 0 {
					t.Errorf("container response %d = %+v, want an empty response", i, cr)
				}
			}
		})
	}

	t.Run("nil request yields an empty response", func(t *testing.T) {
		resp, err := plugin.Allocate(context.Background(), nil)
		if err != nil {
			t.Fatalf("Allocate(nil) failed: %v", err)
		}
		if len(resp.ContainerResponses) != 0 {
			t.Errorf("got %d container responses, want 0", len(resp.ContainerResponses))
		}
	})
}

func TestNewResourcePluginManager(t *testing.T) {
	t.Run("indexes plugins by resource name", func(t *testing.T) {
		eni := NewENIDevicePlugin(ENIResourceName, 1)
		eniIP := NewENIDevicePlugin(ENIIPResourceName, 2)

		mgr := NewResourcePluginManager(context.Background(), eni, eniIP)

		if got := mgr.Plugin(ENIResourceName); got != Plugin(eni) {
			t.Errorf("Plugin(%q) = %v, want the registered eni plugin", ENIResourceName, got)
		}
		if got := mgr.Plugin(ENIIPResourceName); got != Plugin(eniIP) {
			t.Errorf("Plugin(%q) = %v, want the registered eni-ip plugin", ENIIPResourceName, got)
		}
	})

	t.Run("unknown resource name yields nil", func(t *testing.T) {
		mgr := NewResourcePluginManager(context.Background())
		if got := mgr.Plugin("zenlayer.com/does-not-exist"); got != nil {
			t.Errorf("Plugin(unknown) = %v, want nil", got)
		}
	})

	t.Run("no plugins is valid", func(t *testing.T) {
		mgr := NewResourcePluginManager(context.Background())
		if mgr == nil {
			t.Fatal("NewResourcePluginManager returned nil")
		}
		mgr.Stop()
	})

	t.Run("later plugin with the same name wins", func(t *testing.T) {
		first := NewENIDevicePlugin(ENIResourceName, 1)
		second := NewENIDevicePlugin(ENIResourceName, 2)

		mgr := NewResourcePluginManager(context.Background(), first, second)
		if got := mgr.Plugin(ENIResourceName); got != Plugin(second) {
			t.Errorf("Plugin(%q) = %v, want the last registered plugin", ENIResourceName, got)
		}
	})
}

func TestPluginManagerAddPlugin(t *testing.T) {
	mgr := NewResourcePluginManager(context.Background())
	if got := mgr.Plugin(ENIResourceName); got != nil {
		t.Fatalf("Plugin(%q) on an empty manager = %v, want nil", ENIResourceName, got)
	}

	eni := NewENIDevicePlugin(ENIResourceName, 1)
	mgr.AddPlugin(eni)

	if got := mgr.Plugin(ENIResourceName); got != Plugin(eni) {
		t.Errorf("Plugin(%q) after AddPlugin = %v, want the added plugin", ENIResourceName, got)
	}
}

func TestPluginManagerUpdate(t *testing.T) {
	t.Run("unknown resource returns an error", func(t *testing.T) {
		mgr := NewResourcePluginManager(context.Background())
		if err := mgr.Update("zenlayer.com/does-not-exist", 1); err == nil {
			t.Error("Update on an unknown resource = nil, want an error")
		}
	})

	t.Run("known resource is updated without error", func(t *testing.T) {
		eni := NewENIDevicePlugin(ENIResourceName, 1)
		mgr := NewResourcePluginManager(context.Background(), eni)

		// No ListAndWatch stream is running, so the new count is queued on the
		// buffered update channel rather than applied immediately.
		if err := mgr.Update(ENIResourceName, 5); err != nil {
			t.Errorf("Update = %v, want nil", err)
		}
	})

	t.Run("update to the unchanged count is a no-op", func(t *testing.T) {
		eni := NewENIDevicePlugin(ENIResourceName, 7)
		mgr := NewResourcePluginManager(context.Background(), eni)

		if err := mgr.Update(ENIResourceName, 7); err != nil {
			t.Errorf("Update = %v, want nil", err)
		}
	})
}

// fakeLWStream stands in for kubelet's ListAndWatch stream. It records the
// device counts it is sent so a test can assert what the node would advertise.
type fakeLWStream struct {
	grpc.ServerStream
	mu     sync.Mutex
	counts []int
}

func (f *fakeLWStream) Send(resp *pluginapi.ListAndWatchResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts = append(f.counts, len(resp.Devices))
	return nil
}

func (f *fakeLWStream) Context() context.Context { return context.Background() }

func (f *fakeLWStream) recorded() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.counts...)
}

// TestListAndWatchReportsUpdatedCount covers what the node actually advertises,
// which the other tests do not: they only assert Update's return.
func TestListAndWatchReportsUpdatedCount(t *testing.T) {
	p := NewENIDevicePlugin(ENIIPResourceName, 3)
	ctx, cancel := context.WithCancel(context.Background())
	p.SetContext(ctx)

	stream := &fakeLWStream{}
	done := make(chan error, 1)
	go func() { done <- p.ListAndWatch(&pluginapi.Empty{}, stream) }()

	// The first send happens before the loop, so wait for it rather than for a
	// duration: a fixed sleep would make this test time-dependent.
	waitFor(t, func() bool { return len(stream.recorded()) >= 1 })
	p.Update(7)
	waitFor(t, func() bool {
		got := stream.recorded()
		return len(got) >= 2 && got[len(got)-1] == 7
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("ListAndWatch returned %v, want nil after the context was canceled", err)
	}

	got := stream.recorded()
	if got[0] != 3 {
		t.Errorf("first report advertised %d devices, want the initial 3", got[0])
	}
	if last := got[len(got)-1]; last != 7 {
		t.Errorf("last report advertised %d devices, want the updated 7", last)
	}
	if c := p.getCount(); c != 7 {
		t.Errorf("count after Update = %d, want 7 -- the field and the stream must agree", c)
	}
}

// TestCountIsRaceFree pins the mutex on count. ListAndWatch runs in kubelet's
// gRPC handler while the daemon's quota watcher calls Update, so the two touch
// the field concurrently; before the mutex this failed under -race.
func TestCountIsRaceFree(t *testing.T) {
	p := NewENIDevicePlugin(ENIIPResourceName, 1)
	ctx, cancel := context.WithCancel(context.Background())
	p.SetContext(ctx)

	stream := &fakeLWStream{}
	done := make(chan error, 1)
	go func() { done <- p.ListAndWatch(&pluginapi.Empty{}, stream) }()
	waitFor(t, func() bool { return len(stream.recorded()) >= 1 })

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for n := 0; n < 25; n++ {
				p.Update(base*100 + n + 1)
				_ = p.getCount()
			}
		}(i)
	}
	wg.Wait()

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("ListAndWatch returned %v, want nil", err)
	}
}

// waitFor polls cond instead of sleeping a fixed amount, so the tests above do
// not encode an assumption about how fast the handler goroutine is scheduled.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestUpdateDisplacesAStaleQueuedCount is the regression this covers.
//
// The update channel holds one value. An Update whose count matched the stored
// one used to return immediately, without looking at what was queued -- so a
// count sent earlier and not yet read stayed there and ListAndWatch delivered
// it as if it were current. The node then advertised that stale figure to
// kubelet until something changed again, and a figure below the truth is pods
// that cannot be scheduled.
func TestUpdateDisplacesAStaleQueuedCount(t *testing.T) {
	// Stored count 5, nothing queued, no stream reading.
	eni := NewENIDevicePlugin(ENIResourceName, 5)

	// Queues 3. This branch does not touch the stored count, so it stays 5.
	eni.Update(3)
	if got := eni.getCount(); got != 5 {
		t.Fatalf("stored count = %d after queueing, want 5 -- the queue path does not store", got)
	}

	// Back to 5, which equals the stored count. The queued 3 has to go.
	eni.Update(5)

	select {
	case got := <-eni.updateSignal:
		if got != 5 {
			t.Errorf("the queue holds %d, want 5: the stale value was left for ListAndWatch to "+
				"deliver as the current count", got)
		}
	default:
		// Nothing queued is also correct -- what must not happen is the 3
		// surviving.
	}
}

// TestUpdateStaysANoOpWithNothingQueued is the control. Without it the fix
// above passes for an implementation that queues on every call, which would put
// a redundant ListAndWatch response on the wire every time the quota is polled.
func TestUpdateStaysANoOpWithNothingQueued(t *testing.T) {
	eni := NewENIDevicePlugin(ENIResourceName, 7)

	eni.Update(7)

	if n := len(eni.updateSignal); n != 0 {
		t.Errorf("%d values queued for an unchanged count, want 0", n)
	}
}
