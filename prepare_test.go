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
	"strings"
	"testing"
	"time"
)

func TestPrepBatchSize(t *testing.T) {
	for _, tt := range []struct{ unassigned, machines, want int }{
		{148, 1, 148}, // alone: one ordinary build
		{148, 4, 8},   // capped
		{40, 4, 3},
		{5, 4, 1},
		{1, 4, 1},
	} {
		if got := prepBatchSize(tt.unassigned, tt.machines); got != tt.want {
			t.Errorf("prepBatchSize(%d, %d) = %d; want %d", tt.unassigned, tt.machines, got, tt.want)
		}
	}
}

func TestPrepAssignment(t *testing.T) {
	old := *distBuild
	*distBuild = "batches"
	t.Cleanup(func() { *distBuild = old })
	f := testFleet(t)
	s := f.s
	s.testEstimates, s.testEstimateBase = make(map[string]testEstimate), time.Second
	for _, pkg := range []string{"p1", "p2"} {
		s.pkgs[pkg] = &packageStatus{glp: &goListPackage{ImportPath: pkg, Dir: "/src/" + pkg, Root: "/src"}}
		f.prep = append(f.prep, &prepItem{pkg: pkg})
	}
	f.preparing = true
	s.profile.Root = "/src"
	a, b := f.addWorker("a", 1, false), f.addWorker("b", 1, false)

	res := f.exchange(a, workerRequest{})
	if fmt.Sprint(res.Prepare) != "[p1]" {
		t.Fatalf("a's prepare = %q", res.Prepare)
	}
	if res := f.exchange(a, workerRequest{}); res.Prepare != nil {
		t.Fatalf("a got more to prepare (%q) before reporting", res.Prepare)
	}
	if res := f.exchange(b, workerRequest{}); fmt.Sprint(res.Prepare) != "[p2]" {
		t.Fatalf("b's prepare = %q", res.Prepare)
	}
	// b disconnects: its package goes back to the queue, for a.
	f.disconnect(b, nil)
	res = f.exchange(a, workerRequest{Prepared: []preparedPackage{{Package: "p1", Binary: "h1", Tests: []string{"TestA", "TestB", "BenchmarkX"}}}})
	if fmt.Sprint(res.Prepare) != "[p2]" {
		t.Fatalf("after b left, a's prepare = %q", res.Prepare)
	}
	if res.Done {
		t.Fatal("done while a package is unprepared")
	}
	// p1's tests are already work, and run on a.
	if len(f.work) != 2 || len(res.Work) != 2 || res.Work[0].Dir != "p1" || !a.binaries["h1"] {
		t.Fatalf("work %d, assigned %+v", len(f.work), res.Work)
	}
	stopped := false
	f.stopLeaderBuild = func() { stopped = true }
	res = f.exchange(a, workerRequest{Prepared: []preparedPackage{{Package: "p2", Binary: "h2", Tests: []string{"TestC"}}}, PrepTime: time.Second})
	if !stopped {
		t.Error("finishing preparation didn't stop the leader's build")
	}
	if a.PrepTime != time.Second {
		t.Errorf("a.PrepTime = %v", a.PrepTime)
	}
	if f.preparing || f.prepared != 2 || a.Built != 2 || len(f.work) != 3 {
		t.Fatalf("preparing %v, prepared %d, a.Built %d, work %d", f.preparing, f.prepared, a.Built, len(f.work))
	}
	// A duplicate report changes nothing.
	f.applyPrepared(a, preparedPackage{Package: "p2", Binary: "h2", Tests: []string{"TestC"}})
	if len(f.work) != 3 || a.Built != 2 {
		t.Fatalf("duplicate report: work %d, a.Built %d", len(f.work), a.Built)
	}
}

func TestPrepAssignmentHybrid(t *testing.T) {
	old := *distBuild
	t.Cleanup(func() { *distBuild = old })
	f := testFleet(t)
	for _, pkg := range []string{"p1", "p2", "p3"} {
		f.prep = append(f.prep, &prepItem{pkg: pkg})
	}
	f.preparing = true
	f.leaderWorkerLocked()
	a := f.addWorker("a", 1, false)
	*distBuild = "hybrid"
	if got := f.nextPrepLocked(a, time.Now()); fmt.Sprint(got) != "[p3]" {
		t.Errorf("hybrid: helper got %q; want the last package", got)
	}
	f.releasePrepLocked(a)
	*distBuild = "leader"
	if got := f.nextPrepLocked(a, time.Now()); got != nil {
		t.Errorf("leader: helper got %q; want nothing", got)
	}
}

// distributedFixture writes a module with the given test packages to two
// checkouts, for a leader and a helper sharing a GOCACHEPROG.
func distributedFixture(t *testing.T, pkgs map[string]string) (leaderDir, helperDir string) {
	t.Setenv("GOTST_TEST_DISTRIBUTED", "1")
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	oldCache, oldHistory, oldRoot := *useCache, *historyConfig, *cacheRoot
	*useCache, *historyConfig, *cacheRoot = false, "off", t.TempDir()
	t.Cleanup(func() { *useCache, *historyConfig, *cacheRoot = oldCache, oldHistory, oldRoot })
	leaderDir, helperDir = t.TempDir(), t.TempDir()
	for _, dir := range []string{leaderDir, helperDir} {
		files := map[string]string{"go.mod": "module distbuild\n\ngo 1.25.1\n"}
		for name, data := range pkgs {
			files[name] = data
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
	return leaderDir, helperDir
}

// runDistributedWithHelper runs the leader's distributed pipeline for the
// checkout at leaderDir, with one helper process in helperDir.
func runDistributedWithHelper(t *testing.T, leaderDir, helperDir string) (*fleet, error) {
	self, _ := os.Executable()
	s := NewServer(runProfile{Root: leaderDir, Packages: []string{"./..."}}, testSelection{})
	s.quiet = true
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
	t.Cleanup(func() { ln.Close() })
	go func() {
		if c, err := ln.Accept(); err == nil {
			f.serveWorker(c)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
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
	t.Cleanup(func() { <-helperDone })
	for {
		s.mu.Lock()
		joined := len(f.workers) == 1
		s.mu.Unlock()
		if joined {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	err = f.runDistributed(s.packagesWithTests())
	f.shutdown()
	return f, err
}

func TestDistributedPrepare(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a nested module in a helper process")
	}
	for _, mode := range []string{"batches", "hybrid", "leader"} {
		t.Run(mode, func(t *testing.T) {
			old := *distBuild
			*distBuild = mode
			t.Cleanup(func() { *distBuild = old })
			testDistributedPrepare(t, mode)
		})
	}
}

func testDistributedPrepare(t *testing.T, mode string) {
	const npkgs = 8
	pkgs := make(map[string]string)
	for i := range npkgs {
		pkgs[fmt.Sprintf("p%d/p_test.go", i)] = fmt.Sprintf("package p%d\nimport \"testing\"\nfunc TestA(t *testing.T) {}\nfunc TestB(t *testing.T) {}\n", i)
	}
	leaderDir, helperDir := distributedFixture(t, pkgs)
	f, err := runDistributedWithHelper(t, leaderDir, helperDir)
	if err != nil {
		t.Fatal(err)
	}
	s := f.s
	s.mu.Lock()
	defer s.mu.Unlock()
	var built, completed int
	var helper *fleetWorker
	for _, w := range f.workers {
		built += w.Built
		completed += w.Completed
		if w.ID != "leader" {
			helper = w
		}
	}
	if built != npkgs || completed != 2*npkgs || f.done != 2*npkgs {
		t.Fatalf("built %d, completed %d, done %d; want %d, %d", built, completed, f.done, npkgs, 2*npkgs)
	}
	switch {
	case helper == nil:
		t.Fatal("no helper")
	case mode == "batches" && helper.Built == 0:
		t.Fatalf("helper prepared no packages: %+v", helper)
	case mode == "leader" && helper.Built != 0:
		t.Fatalf("helper prepared %d packages in leader mode", helper.Built)
	}
	for pkg, ps := range s.pkgs {
		if ps.exeHash == "" || len(ps.tests) != 2 || ps.pkgState != pkgStateDone {
			t.Errorf("%s: hash %q, %d tests, state %v", pkg, ps.exeHash, len(ps.tests), ps.pkgState)
		}
		for name, ts := range ps.tests {
			if !ts.passed {
				t.Errorf("%s.%s did not pass", pkg, name)
			}
		}
	}
}

func TestDistributedPrepareBuildFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a nested module in a helper process")
	}
	pkgs := map[string]string{"bad/bad_test.go": "package bad\nimport \"testing\"\nfunc TestBad(t *testing.T) { undefinedThing() }\n"}
	for i := range 4 {
		pkgs[fmt.Sprintf("p%d/p_test.go", i)] = fmt.Sprintf("package p%d\nimport \"testing\"\nfunc TestA(t *testing.T) {}\n", i)
	}
	leaderDir, helperDir := distributedFixture(t, pkgs)
	_, err := runDistributedWithHelper(t, leaderDir, helperDir)
	if err == nil || !strings.Contains(err.Error(), "undefinedThing") {
		t.Fatalf("err = %v; want the compile error", err)
	}
}

// TestLinkSkip runs gotst twice over a GOCACHEPROG, the second time as a
// fresh machine would. The second run's build should skip every test
// binary, taking their tests from link records, and fetch only the binary
// whose test isn't cached.
func TestLinkSkip(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a nested module")
	}
	dir, _ := distributedFixture(t, map[string]string{
		"pass/p_test.go": "package pass\nimport \"testing\"\nfunc TestPass(t *testing.T) {}\n",
		"fail/f_test.go": "package fail\nimport \"testing\"\nfunc TestFail(t *testing.T) { t.Fatal(\"always\") }\nfunc TestOK(t *testing.T) {}\n",
	})
	oldCache, oldRetries := *useCache, *maxRetries
	*useCache, *maxRetries = true, 0
	t.Cleanup(func() { *useCache, *maxRetries = oldCache, oldRetries })
	run := func() *Server {
		t.Helper()
		*cacheRoot = t.TempDir() // a fresh machine's local caches
		s := NewServer(runProfile{Root: dir, Packages: []string{"./..."}}, testSelection{})
		s.quiet = true
		t.Cleanup(s.Cleanup)
		if !s.linkSkip {
			t.Fatal("link skipping not enabled")
		}
		if err := s.Run(); err == nil || !strings.Contains(err.Error(), "1 test(s) failed") {
			t.Fatalf("Run = %v; want one failure", err)
		}
		return s
	}
	first := run()
	for pkg, ps := range first.pkgs {
		if ps.stubTests != nil || ps.linkAction == "" {
			t.Fatalf("first run: %s stub=%v linkAction=%q", pkg, ps.stubTests != nil, ps.linkAction)
		}
	}
	second := run()
	for pkg, ps := range second.pkgs {
		if ps.stubTests == nil {
			t.Errorf("second run built or restored %s instead of skipping it", pkg)
		}
		_, err := os.Stat(filepath.Join(second.cacheDir, ps.exeHash))
		switch pkg {
		case "distbuild/pass":
			if err == nil {
				t.Error("second run fetched the binary of a package whose tests were all cached")
			}
			if !ps.tests["TestPass"].cached {
				t.Error("TestPass not a cache hit")
			}
		case "distbuild/fail":
			if err != nil {
				t.Errorf("second run didn't fetch the binary to run the uncached test: %v", err)
			}
			if ts := ps.tests["TestFail"]; ts.passed || len(ts.fails) == 0 || !strings.Contains(ts.fails[0].out, "always") {
				t.Errorf("TestFail = %+v", ts)
			}
		}
		if ps.exeHash != first.pkgs[pkg].exeHash {
			t.Errorf("%s: hash %s; first run's %s", pkg, ps.exeHash, first.pkgs[pkg].exeHash)
		}
	}
}
