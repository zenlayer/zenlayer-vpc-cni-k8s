// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/wait"

	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/alexflint/go-filemutex"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/rpc"

	"github.com/go-logr/logr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/k8s"
)

const (
	// daemonRPCTimeout bounds every CNI RPC this daemon serves (see
	// cniInterceptor), so it is the budget a pod's ADD has. gcSlowThreshold is
	// derived from it, and the OpenAPI client keeps its per-attempt timeout
	// under it.
	daemonRPCTimeout = 118 * time.Second
	lockFile         = "/var/run/eni/zenlayer-cni.lock"

	prevCNIConfFile = "10-zenlayer-cni.conf"
	cniConfFile     = "10-zenlayer-cni.conflist"
	// tmpCNIConfigPath is where the chart-rendered conflist is mounted from a
	// ConfigMap; hostCNIConfigPath is the node's real /etc/cni/net.d. Only the
	// second one kubelet reads, which is what makes copying across the last step
	// of startup rather than the first.
	tmpCNIConfigPath  = "/etc/cni/net.d"
	hostCNIConfigPath = "/host-etc-net.d"
)

var (
	stackTriggerSignals = []os.Signal{
		syscall.SIGUSR1,
	}
)

// Run starts the daemon and blocks until ctx is done. The node-wide file lock
// is taken first, and given only a few seconds, so a second instance fails to
// start instead of two daemons managing the same node's interfaces.
func Run(ctx context.Context) (retErr error) {
	err := os.MkdirAll(filepath.Dir(lockFile), 0700)
	if err != nil {
		return err
	}

	lock, err := filemutex.New(lockFile)
	if err != nil {
		return fmt.Errorf("error create lock file: %s, %w", lockFile, err)
	}
	err = wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (done bool, err error) {
		innerErr := lock.TryLock()
		if innerErr != nil {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("failed to acquire lock: %v", err)
	}
	defer func(lock *filemutex.FileMutex) {
		_ = lock.Unlock()
	}(lock)

	stackTrigger()

	d, err := NewIpamDaemon(ctx)
	if err != nil {
		return err
	}
	// NewIpamDaemon reports its own failures; this covers the rest of startup,
	// such as the wait for a chained plugin's agent in ensureCNIConfig.
	defer func() { reportStartFailure(ctx, d.k8s, retErr) }()

	log.Info("zenlayer cni daemon ready, start service")

	// nil when Config.DisableDevicePlugin is set; NewIpamDaemon does not build it.
	if d.devicePluginManager != nil {
		err = d.devicePluginManager.Serve(ctx)
		if err != nil {
			return fmt.Errorf("device plugin start failed: %v", err)
		}
		defer d.devicePluginManager.Stop()
	}

	grpcServer, err := d.startGrpcServer()
	if err != nil {
		return fmt.Errorf("start grpc server failed: %v", err)
	}
	defer grpcServer.Stop()

	debugServer, err := d.startDebugServer()
	if err != nil {
		return fmt.Errorf("start debug server failed: %v", err)
	}
	defer func(debugServer *http.Server) {
		inErr := debugServer.Close()
		if inErr != nil {
			log.Errorf("DebugServer close failed, %v", inErr)
		}
	}(debugServer)

	stopGC, err := d.startGC(ctx)
	if err != nil {
		return fmt.Errorf("start gc failed: %w", err)
	}
	defer stopGC()

	err = ensureCNIConfig(ctx)
	if err != nil {
		return err
	}

	<-ctx.Done()
	return nil
}

// stackTrigger dumps every goroutine's stack to stdout on SIGUSR1.
//
// It targets hangs, not crashes: a wedged daemon -- the pool paused for GC, an
// order unanswered, a lock held across a channel send -- keeps answering
// /healthz while no pod can start, and leaves no stack behind afterwards. A
// signal asks the live process instead, and unlike a pprof endpoint it exposes
// nothing off the node (see startDebugServer).
func stackTrigger() {
	sigchain := make(chan os.Signal, 1)
	go func(c chan os.Signal) {
		for {
			<-sigchain
			var (
				buf       []byte
				stackSize int
			)
			bufferLen := 16384
			for stackSize == len(buf) {
				buf = make([]byte, bufferLen)
				stackSize = runtime.Stack(buf, true)
				bufferLen *= 2
			}
			buf = buf[:stackSize]

			_, _ = os.Stdout.Write(buf)
		}
	}(sigchain)

	signal.Notify(sigchain, stackTriggerSignals...)
}

// cniInterceptor bounds every request by daemonRPCTimeout and puts the pod the
// request names into the log context the handler sees.
func cniInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	ctx, cancel := context.WithTimeout(ctx, daemonRPCTimeout)
	defer cancel()

	switch r := req.(type) {
	case *rpc.AllocIPRequest:
		l := logf.FromContext(ctx, "pod", k8s.PodInfoKey(r.K8SPodNamespace, r.K8SPodName), "containerID", r.K8SPodInfraContainerId)
		ctx = logr.NewContext(ctx, l)
	case *rpc.ReleaseIPRequest:
		l := logf.FromContext(ctx, "pod", k8s.PodInfoKey(r.K8SPodNamespace, r.K8SPodName), "containerID", r.K8SPodInfraContainerId)
		ctx = logr.NewContext(ctx, l)
	case *rpc.GetIPInfoRequest:
		l := logf.FromContext(ctx, "pod", k8s.PodInfoKey(r.K8SPodNamespace, r.K8SPodName), "containerID", r.K8SPodInfraContainerId)
		ctx = logr.NewContext(ctx, l)
	case *rpc.RecordEventRequest:
		l := logf.FromContext(ctx, "pod", k8s.PodInfoKey(r.K8SPodNamespace, r.K8SPodName))
		ctx = logr.NewContext(ctx, l)
	default:
	}
	return handler(ctx, req)
}

// cniConfMode keeps the written config readable only by root: not a secret, but
// not something every process on the node needs to read either.
const cniConfMode = 0600

// ensureCNIConfig publishes the conflist to the host, which is what makes this
// node eligible for pods: until the file is there kubelet has no CNI to call.
// It deliberately runs last, after the gRPC server is accepting, so the plugin
// never gets exec'd before the daemon can answer it. The conflist is read into
// memory because it has to be inspected first: if it chains another plugin, that
// plugin's prerequisites are part of "ready" too. See waitForCiliumAgent.
//
// The wait gates publishing the new conflist; it does not retract one already on
// the node from an earlier run. A fresh node therefore admits no pods until the
// wait finishes, while a node being switched over keeps serving pods through its
// previous, unchained conflist -- and so with no policy enforcement -- until
// this returns. Retracting it would take the node down to gain nothing, but it
// does mean enabling enforcement is not instantaneous on a running node.
func ensureCNIConfig(ctx context.Context) error {
	conflist, err := os.ReadFile(filepath.Join(tmpCNIConfigPath, cniConfFile))
	if err != nil {
		return err
	}

	chained, err := chainsCilium(conflist)
	if err != nil {
		return err
	}
	if chained {
		if err := waitForCiliumAgent(ctx, ciliumAgentSocket, ciliumWaitTimeout()); err != nil {
			return err
		}
	}

	dstPath := filepath.Join(hostCNIConfigPath, cniConfFile)
	if err = writeConflistAtomically(dstPath, conflist); err != nil {
		return err
	}

	log.Info("write cni conf success")

	// An earlier build installed a plain .conf. kubelet takes the first config
	// in the directory by name, and "10-zenlayer-cni.conf" sorts before
	// "10-zenlayer-cni.conflist", so one left behind would keep winning over the
	// file just written. Nothing to do if it is not there.
	_ = os.Remove(filepath.Join(hostCNIConfigPath, prevCNIConfFile))
	return nil
}

// writeConflistAtomically replaces the host conflist with a rename, rather than
// truncating the file in place.
//
// kubelet reads this directory on its own schedule. Truncating meant the file
// was zero bytes from the moment O_TRUNC landed until the write returned, so a
// read in that window saw an empty or half-written config -- and a daemon killed
// mid-write left one behind for good, taking the node's pod networking with it.
//
// The temporary file has to be in the same directory: rename is atomic within a
// filesystem and fails with EXDEV across one. Its name is safe to leave in
// kubelet's path because kubelet only considers .conf, .conflist and .json, and
// CreateTemp appends random digits to the suffix given here.
//
// Sync before the rename, or a machine that loses power just after it comes back
// to a correctly named file with no contents -- the same end state as the bug
// this replaces, reached a different way.
func writeConflistAtomically(dstPath string, conflist []byte) error {
	dir := filepath.Dir(dstPath)
	tmp, err := os.CreateTemp(dir, filepath.Base(dstPath)+".tmp")
	if err != nil {
		return fmt.Errorf("create a temporary conflist in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		// A no-op once the rename has happened; on every error path it is what
		// keeps a failed publish from leaving litter in kubelet's directory.
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	if _, err = tmp.Write(conflist); err != nil {
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	// CreateTemp makes the file 0600 already; this states the intent and covers
	// a umask or a future change to that default. The published file must not be
	// world-readable: it names the daemon socket and the chained plugin's.
	if err = tmp.Chmod(cniConfMode); err != nil {
		return fmt.Errorf("set mode %o on %s: %w", cniConfMode, tmpPath, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", tmpPath, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err = os.Rename(tmpPath, dstPath); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpPath, dstPath, err)
	}
	return nil
}

func listenOnUnixSock(socketFilePath string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socketFilePath), 0700); err != nil {
		return nil, err
	}

	if err := syscall.Unlink(socketFilePath); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	// The bind creates the socket file with 0777 masked by the umask, so 0777
	// here lands it at mode 0000 and only root can connect. Restored on return.
	mask := syscall.Umask(0777)
	defer syscall.Umask(mask)

	l, err := net.Listen("unix", socketFilePath)
	if err != nil {
		return nil, fmt.Errorf("error listen at %s: %v", socketFilePath, err)
	}
	return l, nil
}
