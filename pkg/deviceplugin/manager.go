// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package deviceplugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"path"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
)

var log = zlog.GetLogger().WithFields(zlog.Fields{"subsys": "deviceplugin"})

// PluginManager manages all device plugins.
type PluginManager struct {
	plugins map[string]Plugin

	// ctxMu guards ctx and cancel. Both are replaced wholesale when kubelet
	// restarts, and that happens on the fsnotify watcher's goroutine while
	// Stop, AddPlugin and start read them from whoever called in. Reach them
	// through context() and resetContext rather than directly.
	ctxMu  sync.Mutex
	cancel context.CancelFunc
	ctx    context.Context
}

// context returns the manager's current context. It is read rather than stored
// by callers because a kubelet restart replaces it.
func (manager *PluginManager) context() context.Context {
	manager.ctxMu.Lock()
	defer manager.ctxMu.Unlock()
	return manager.ctx
}

// resetContext replaces the manager's context and returns the new one. Only the
// kubelet-restart path calls it, after stopping everything the old one owned.
func (manager *PluginManager) resetContext(parent context.Context) context.Context {
	manager.ctxMu.Lock()
	defer manager.ctxMu.Unlock()
	manager.ctx, manager.cancel = context.WithCancel(parent)
	return manager.ctx
}

func (manager *PluginManager) Plugin(resourceName string) Plugin {
	return manager.plugins[resourceName]
}

func NewResourcePluginManager(ctx context.Context, plugins ...Plugin) *PluginManager {
	mgr := PluginManager{}
	mgr.ctx, mgr.cancel = context.WithCancel(ctx)
	mgr.plugins = make(map[string]Plugin)
	for _, plugin := range plugins {
		plugin.SetContext(mgr.ctx)
		mgr.plugins[plugin.ResourceName()] = plugin
	}
	return &mgr
}

// register advertises each plugin's grpc endpoint to kubelet. Must be called
// after start(), which is what creates those endpoints.
func (manager *PluginManager) register() error {
	conn, err := dialUnix(manager.context(), KubeletSocket)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := pluginapi.NewRegistrationClient(conn)
	for _, plugin := range manager.plugins {
		_, err = client.Register(manager.context(), &pluginapi.RegisterRequest{
			Version:      pluginapi.Version,
			Endpoint:     path.Base(plugin.Endpoint()),
			ResourceName: path.Join(ZenlayerNameSpace, plugin.ResourceName()),
		})

		if err != nil {
			return err
		}
	}
	return nil
}

// Serve starts the plugin servers, registers them with kubelet, and then
// watches DevicePluginPath: kubelet.sock being created again means kubelet
// restarted, and every plugin is then stopped, started and re-registered.
func (manager *PluginManager) Serve(stopCtx context.Context) error {
	err := manager.start()
	if err != nil {
		log.Errorf("Device plugin start failed: %v", err)
		return err
	}
	err = manager.register()
	if err != nil {
		log.Errorf("Device plugin register failed: %v", err)
		return err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Errorf("Create watcher failed: %v", err)
		return err
	}
	err = watcher.Add(path.Clean(DevicePluginPath))
	if err != nil {
		log.Errorf("Watch kubelet path %s failed, %v", DevicePluginPath, err)
		return err
	}

	// Snapshotted once and reassigned in the kubelet-restart branch below. The
	// select needs a context whose Done channel does not change under it mid
	// iteration, which is also why the restart branch reassigns it explicitly.
	managerCtx := manager.context()

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					log.Error("Watch kubelet failed.")
					return
				}
				if event.Name == KubeletSocket && event.Has(fsnotify.Create) {
					log.Infof(" %s created, restarting.", KubeletSocket)
					manager.Stop()
					managerCtx = manager.resetContext(context.Background())
					_ = manager.start()
					err = manager.register()
					if err != nil {
						log.Errorf("Register failed after kubelet restart, %v", err)
					}
				} else if event.Name == "kubelet.sock" && event.Op&fsnotify.Remove == fsnotify.Remove {
					// Never reached: watching a directory reports full paths,
					// while this compares against a bare file name.
					log.Infof("Kubelet stopped")
				}

			case err := <-watcher.Errors:
				if err != nil {
					log.Errorf("Watch kubelet failed, %v", err)
				}
			case <-stopCtx.Done():
				log.Info("Stop signal received (stopCtx.Done).")
				return
			case <-managerCtx.Done():
				// Returning here is what keeps this from spinning: a canceled
				// context stays ready forever, so a case that only logs turns
				// the select into a busy loop.
				//
				// The kubelet-restart branch above also cancels this context,
				// but it reassigns managerCtx before the loop reaches select
				// again, so the case is not ready on the next iteration and the
				// watcher survives the restart. Reaching it therefore means an
				// external Stop(), after which the plugins are down and there
				// is nothing left to watch for.
				log.Info("Manager context canceled, stopping kubelet watcher.")
				return
			}
		}
	}()

	return nil
}

// Stop stops the plugins' grpc servers (see stop for a caveat) and removes
// their endpoint sockets.
func (manager *PluginManager) Stop() {
	manager.stop()
	_ = manager.cleanUp()
}

// Update publishes a new resource count to the named plugin.
//
// The plugin's update channel is buffered with one entry: the count either goes
// into that buffer or replaces the one already queued, so this does not wait
// for a ListAndWatch stream to read it.
func (manager *PluginManager) Update(resName string, count int) error {
	plugin, ok := manager.plugins[resName]
	if !ok {
		return fmt.Errorf("plugin not found")
	}
	plugin.Update(count)
	return nil
}

func (manager *PluginManager) AddPlugin(plugin Plugin) {
	plugin.SetContext(manager.context())
	manager.plugins[plugin.ResourceName()] = plugin
}

// start boots one grpc server per plugin, each listening on its own socket
// under DevicePluginPath (/var/lib/kubelet/device-plugins/<res>.sock).
//
// Each socket is dialed once right after Serve and closed again: a plugin that
// cannot be reached over its own endpoint would otherwise be registered with
// kubelet and fail only later, inside kubelet's own call.
func (manager *PluginManager) start() error {
	if err := manager.cleanUp(); err != nil {
		return err
	}
	// Read once so every plugin started in this pass gets the same context,
	// even if a kubelet restart replaces the manager's while the loop runs.
	managerCtx := manager.context()
	for _, plugin := range manager.plugins {
		sock, err := net.Listen("unix", plugin.Endpoint())
		if err != nil {
			return err
		}
		plugin.ResetServer()
		plugin.SetContext(managerCtx)
		pluginapi.RegisterDevicePluginServer(plugin.Server(), plugin)

		go func() {
			err := plugin.Server().Serve(sock)
			if err != nil {
				log.Errorf("Failed to serve deviceplugin grpc server, %v", err)
			}
		}()

		conn, err := dialUnix(managerCtx, plugin.Endpoint())
		if err != nil {
			return err
		}
		err = conn.Close()
		if err != nil {
			return err
		}
		log.Infof("Start device plugin for %v", ZenlayerNameSpace+plugin.ResourceName())
	}
	return nil
}

// stop cancels the manager context and stops the plugins' grpc servers. The
// endpoint sockets are left behind; cleanUp removes them.
//
// The loop below returns instead of continuing when a plugin has no server, so
// the plugins not yet visited keep serving.
func (manager *PluginManager) stop() {
	manager.ctxMu.Lock()
	cancel := manager.cancel
	manager.ctxMu.Unlock()
	cancel()
	for _, eniPlugin := range manager.plugins {
		if eniPlugin.Server() == nil {
			return
		}
		eniPlugin.Server().Stop()
	}
}

// cleanUp removes the plugins' endpoint sockets. A leftover socket makes
// net.Listen in start() fail with "address already in use".
func (manager *PluginManager) cleanUp() error {
	for _, res := range manager.plugins {
		if err := os.Remove(res.Endpoint()); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func dialUnix(ctx context.Context, path string) (*grpc.ClientConn, error) {
	// Kept on DialContext + WithBlock deliberately. grpc.NewClient has no
	// equivalent of WithBlock: it returns before the connection is up and only
	// dials on the first RPC. start() dials purely to prove a plugin endpoint is
	// reachable and closes the connection immediately, so without WithBlock that
	// check would succeed on a socket nothing is serving and the failure would
	// surface later, inside kubelet's own call. (register() also dials here, for
	// a connection it really uses.) Migrating means replacing the check itself,
	// so it is a change to make on its own.
	//nolint:staticcheck // SA1019: no NewClient equivalent for WithBlock; see above
	conn, err := grpc.DialContext(ctx, path,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		//nolint:staticcheck // SA1019: see the note at the top of dialUnix
		grpc.WithBlock(),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return net.DialTimeout("unix", path, time.Second*10)
		}),
	)
	return conn, err
}
