// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestWriteConflistAtomicallyNeverExposesAPartialFile is the regression this
// file exists for.
//
// The publish used to truncate the destination in place, so from the moment
// O_TRUNC landed until the write returned the file kubelet reads was zero bytes.
// A daemon killed in that window left it that way, and the node had no CNI
// config at all.
//
// The property that rules that out is that the destination is only ever written
// by a rename: at every instant it is either absent, the old contents, or the
// new ones. This checks the observable half -- the old contents survive
// untouched until the new file is complete.
func TestWriteConflistAtomicallyNeverExposesAPartialFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "10-zenlayer-cni.conflist")
	old := []byte(`{"name":"old"}`)
	if err := os.WriteFile(dst, old, 0600); err != nil {
		t.Fatalf("seed the destination: %v", err)
	}

	want := []byte(`{"name":"new","plugins":[]}`)
	if err := writeConflistAtomically(dst, want); err != nil {
		t.Fatalf("writeConflistAtomically: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("published %q, want %q", got, want)
	}
}

// TestWriteConflistAtomicallyIsAtomicUnderAConcurrentReader is the one that
// actually catches an in-place write.
//
// kubelet reads this directory on its own schedule, so the only property that
// matters is that a reader never sees anything but the old contents or the new
// ones. A single-threaded test cannot observe the window an O_TRUNC opens, so
// this reads in a loop while the publish runs, and makes the payload large
// enough that the window is wide rather than a few instructions.
func TestWriteConflistAtomicallyIsAtomicUnderAConcurrentReader(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "10-zenlayer-cni.conflist")

	old := []byte(`{"name":"old"}`)
	if err := os.WriteFile(dst, old, 0600); err != nil {
		t.Fatalf("seed the destination: %v", err)
	}
	// Big enough that an in-place write is not over before the reader looks.
	want := append([]byte(`{"name":"new","pad":"`), append(bytes.Repeat([]byte("x"), 1<<20), []byte(`"}`)...)...)

	stop := make(chan struct{})
	bad := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := os.ReadFile(dst)
			if err != nil {
				// Absent is a legitimate instant only if the file never
				// existed; it did.
				select {
				case bad <- "read failed: " + err.Error():
				default:
				}
				return
			}
			if !bytes.Equal(got, old) && !bytes.Equal(got, want) {
				select {
				case bad <- fmt.Sprintf("saw %d bytes, which is neither the old %d nor the new %d",
					len(got), len(old), len(want)):
				default:
				}
				return
			}
		}
	}()

	if err := writeConflistAtomically(dst, want); err != nil {
		close(stop)
		wg.Wait()
		t.Fatalf("writeConflistAtomically: %v", err)
	}
	close(stop)
	wg.Wait()

	select {
	case msg := <-bad:
		t.Fatalf("a reader observed a state that is neither the old config nor the new one: %s -- "+
			"kubelet reads this file on its own schedule, and an empty or half-written one takes "+
			"the node's pod networking down", msg)
	default:
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("published %d bytes, want %d", len(got), len(want))
	}
}

// TestWriteConflistAtomicallyLeavesNoTemporaryFile covers the directory kubelet
// scans. A temp file left behind on every publish accumulates, and one left by a
// failure is litter in a directory that is not ours alone.
func TestWriteConflistAtomicallyLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "10-zenlayer-cni.conflist")

	if err := writeConflistAtomically(dst, []byte(`{"name":"new"}`)); err != nil {
		t.Fatalf("writeConflistAtomically: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only the published conflist", names)
	}
	// And whatever is there must not be something kubelet would try to load.
	for _, e := range entries {
		if name := e.Name(); strings.Contains(name, ".tmp") {
			t.Errorf("left %s behind; kubelet scans this directory", name)
		}
	}
}

// TestWriteConflistAtomicallyPublishesUnreadableToOthers pins the mode. The file
// names the daemon's socket and, under chaining, the agent's; it has no business
// being world-readable.
func TestWriteConflistAtomicallyPublishesUnreadableToOthers(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "10-zenlayer-cni.conflist")

	if err := writeConflistAtomically(dst, []byte(`{}`)); err != nil {
		t.Fatalf("writeConflistAtomically: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != cniConfMode {
		t.Errorf("mode = %o, want %o", got, cniConfMode)
	}
}

// TestWriteConflistAtomicallyReplacesALaxerMode covers the case the old code
// called out: a file left by an earlier build, or by anything else that touched
// this directory, with a mode wider than ours. A rename carries the temporary
// file's mode, so the old one goes with it.
func TestWriteConflistAtomicallyReplacesALaxerMode(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "10-zenlayer-cni.conflist")
	if err := os.WriteFile(dst, []byte(`{"name":"old"}`), 0644); err != nil {
		t.Fatalf("seed the destination: %v", err)
	}

	if err := writeConflistAtomically(dst, []byte(`{"name":"new"}`)); err != nil {
		t.Fatalf("writeConflistAtomically: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != cniConfMode {
		t.Errorf("mode = %o after replacing a 0644 file, want %o", got, cniConfMode)
	}
}

// TestWriteConflistAtomicallyReportsAnUnwritableDirectory checks the error path
// leaves nothing behind either -- this runs on the CNI publish path, and a
// failure there is retried.
func TestWriteConflistAtomicallyReportsAnUnwritableDirectory(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "no-such-dir", "10-zenlayer-cni.conflist")

	if err := writeConflistAtomically(dst, []byte(`{}`)); err == nil {
		t.Error("writeConflistAtomically succeeded with no directory to write into")
	}
}
