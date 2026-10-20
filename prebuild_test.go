// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPrebuildChunkSize(t *testing.T) {
	for _, tt := range []struct{ n, machines, want int }{
		{148, 4, 13},
		{10, 4, 1},
		{1000, 4, 16},
		{2, 2, 1},
	} {
		if got := prebuildChunkSize(tt.n, tt.machines); got != tt.want {
			t.Errorf("prebuildChunkSize(%d, %d) = %d; want %d", tt.n, tt.machines, got, tt.want)
		}
	}
}

func TestPrebuildChunkAssignment(t *testing.T) {
	f := testFleet(t)
	f.ready = false
	a, b := f.addWorker("a", 1, false), f.addWorker("b", 1, false)
	f.chunks = []*buildChunk{{pkgs: []string{"p1", "p2"}}, {pkgs: []string{"p3"}}}
	f.prebuildOpen, f.prebuildTotal = true, 3

	res := f.exchange(a, workerRequest{})
	if fmt.Sprint(res.Build) != "[p1 p2]" {
		t.Fatalf("a's first chunk = %q", res.Build)
	}
	if res := f.exchange(a, workerRequest{}); res.Build != nil {
		t.Fatalf("a got a second chunk %q before finishing its first", res.Build)
	}
	if res := f.exchange(b, workerRequest{}); fmt.Sprint(res.Build) != "[p3]" {
		t.Fatalf("b's chunk = %q", res.Build)
	}
	// b disconnects mid-build: its chunk goes back to the queue, for a.
	f.disconnect(b, nil)
	res = f.exchange(a, workerRequest{Built: []string{"p1", "p2"}})
	if fmt.Sprint(res.Build) != "[p3]" || f.prebuiltPkgs != 2 || a.Prebuilt != 2 {
		t.Fatalf("after a finished and b left: build %q, prebuilt %d, a.Prebuilt %d", res.Build, f.prebuiltPkgs, a.Prebuilt)
	}
	f.exchange(a, workerRequest{Built: []string{"p3"}})
	if f.prebuiltPkgs != 3 || a.chunk != nil {
		t.Fatalf("prebuilt %d, chunk %v", f.prebuiltPkgs, a.chunk)
	}
	// Once closed, no more chunks are handed out.
	f.chunks = append(f.chunks, &buildChunk{pkgs: []string{"p4"}})
	f.prebuildOpen = false
	if res := f.exchange(a, workerRequest{}); res.Build != nil {
		t.Fatalf("got chunk %q after the distributed build closed", res.Build)
	}
}

// TestDistributedBuild has a real helper process build part of the
// leader's packages into a GOCACHEPROG they share, then checks that the
// leader's full build restores every test binary from that cache.
func TestDistributedBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a nested module in a helper process")
	}
	t.Setenv("GOTST_TEST_DISTRIBUTED", "1")
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	oldCache, oldHistory, oldRoot := *useCache, *historyConfig, *cacheRoot
	*useCache, *historyConfig, *cacheRoot = false, "off", t.TempDir()
	t.Cleanup(func() { *useCache, *historyConfig, *cacheRoot = oldCache, oldHistory, oldRoot })

	const npkgs = 6
	leaderDir, helperDir := t.TempDir(), t.TempDir()
	for _, dir := range []string{leaderDir, helperDir} {
		files := map[string]string{"go.mod": "module distbuild\n\ngo 1.25.1\n"}
		for i := range npkgs {
			files[fmt.Sprintf("p%d/p_test.go", i)] = fmt.Sprintf("package p%d\nimport \"testing\"\nfunc TestP(t *testing.T) {}\n", i)
		}
		for name, data := range files {
			p := filepath.Join(dir, name)
			os.MkdirAll(filepath.Dir(p), 0700)
			if err := os.WriteFile(p, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOCACHEPROG", quoteCacheProgArg(self)+" "+localCacheProgArg)
	t.Setenv("GOTST_TEST_SHARED_CACHE", t.TempDir())
	stock, err := goBuildCacheDir(leaderDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOTST_TEST_STOCK_CACHE", stock)

	s := NewServer(runProfile{Root: leaderDir, Packages: []string{"./..."}}, testSelection{})
	t.Cleanup(s.Cleanup)
	f := newFleet(s)
	s.fleet = f
	if err := s.learnPackagesWithTests(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	served := make(chan struct{})
	go func() {
		defer close(served)
		if c, err := ln.Accept(); err == nil {
			f.serveWorker(c)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestDistributedWorkerProcess$", "-j=2", "-cache-dir="+t.TempDir())
	cmd.Dir = helperDir
	cmd.Env = append(os.Environ(), "GOTST_TEST_WORKER_ADDR="+ln.Addr().String())
	helperDone := make(chan error, 1)
	go func() {
		out, err := cmd.CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%v\n%s", err, out)
		}
		helperDone <- err
	}()
	for {
		s.mu.Lock()
		joined := len(f.workers) == 1
		s.mu.Unlock()
		if joined {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	f.prebuild(s.packagesWithTests())
	// Wait for the helper to report its last chunk, then end its session.
	for {
		s.mu.Lock()
		busy := false
		for _, w := range f.workers {
			busy = busy || w.chunk != nil
		}
		s.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.mu.Lock()
	f.stopped = true
	var helperBuilt, leaderBuilt int
	for _, w := range f.workers {
		if w.ID == "leader" {
			leaderBuilt = w.Prebuilt
		} else {
			helperBuilt = w.Prebuilt
		}
	}
	s.mu.Unlock()
	if err := <-helperDone; err != nil {
		t.Fatalf("helper: %v", err)
	}
	<-served
	if helperBuilt == 0 || helperBuilt+leaderBuilt != npkgs || f.prebuiltPkgs != npkgs {
		t.Fatalf("helper built %d, leader built %d, total %d; want %d with some from the helper", helperBuilt, leaderBuilt, f.prebuiltPkgs, npkgs)
	}

	if err := s.buildAllTestBinaries(); err != nil {
		t.Fatal(err)
	}
	cached := 0
	for _, ps := range s.pkgs {
		if ps.exeCached {
			cached++
		}
	}
	if cached != npkgs {
		t.Errorf("leader's full build restored %d of %d test binaries from the shared cache", cached, npkgs)
	}
}
