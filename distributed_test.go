// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/gotst/history"
)

func testFleet(t *testing.T, durations ...time.Duration) *fleet {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Server{ctx: ctx, cancel: cancel, start: time.Now(), phase: phaseTesting, quiet: true,
		pkgs: make(map[string]*packageStatus), testEstimates: make(map[string]testEstimate), testEstimateReady: true}
	f := newFleet(s)
	s.fleet = f
	for i, d := range durations {
		pkg := fmt.Sprintf("p%d", i)
		task := testTask{test: "TestX", bin: capturedTestBinary{pkg: pkg, absBin: fmt.Sprintf("hash%d", i)}}
		s.pkgs[pkg] = &packageStatus{glp: &goListPackage{ImportPath: pkg, TestGoFiles: []string{"x_test.go"}},
			tests: map[string]*testStatus{"TestX": {}}}
		s.pkgsWithTests++
		s.testsTotal++
		id := pkg + "\x00TestX"
		s.testSchedule = append(s.testSchedule, id)
		s.testEstimates[id] = testEstimate{duration: d, known: true}
		f.work = append(f.work, &fleetTask{task: task, estimate: d, leases: make(map[uint64]*workLease)})
	}
	f.ready = true
	return f
}

func TestFleetBatchAndAffinity(t *testing.T) {
	t.Run("slow work beats warm short binary", func(t *testing.T) {
		f := testFleet(t, 20*time.Second, 5*time.Second, time.Second)
		w := f.addWorker("vm", 1, false)
		w.binaries["hash2"] = true
		got := f.assignLocked(w, 10, time.Now())
		if len(got) != 1 || got[0].Package != "p0" {
			t.Fatalf("batch = %+v", got)
		}
		if more := f.assignLocked(w, 10, time.Now()); len(more) != 0 {
			t.Fatalf("overfilled busy helper: %+v", more)
		}
	})
	t.Run("small locality bias", func(t *testing.T) {
		f := testFleet(t, 5*time.Second, 4*time.Second, time.Second)
		w := f.addWorker("vm", 1, false)
		w.binaries["hash1"] = true
		if got := f.assignLocked(w, 10, time.Now()); len(got) != 1 || got[0].Package != "p1" {
			t.Fatalf("batch = %+v", got)
		}
	})
	t.Run("few seconds", func(t *testing.T) {
		f := testFleet(t, time.Second, time.Second, time.Second, time.Second, time.Second)
		w := f.addWorker("vm", 1, false)
		if got := f.assignLocked(w, 10, time.Now()); len(got) != 3 {
			t.Fatalf("batch length = %d; want 3", len(got))
		}
	})
	t.Run("ten short tests", func(t *testing.T) {
		durations := make([]time.Duration, 25)
		for i := range durations {
			durations[i] = time.Millisecond
		}
		f := testFleet(t, durations...)
		w := f.addWorker("vm", 2, false)
		res := f.exchange(w, workerRequest{})
		if len(res.Work) != 10 {
			t.Fatalf("batch length = %d", len(res.Work))
		}
		res = f.exchange(w, workerRequest{})
		if len(res.Work) != 2 {
			t.Fatalf("queue overflow: %d new assignments", len(res.Work))
		}
	})
}

func TestFleetDisconnectAndLateResult(t *testing.T) {
	f := testFleet(t, time.Second)
	w1, w2 := f.addWorker("same-name", 1, false), f.addWorker("same-name", 1, false)
	if w1.ID == w2.ID {
		t.Fatal("duplicate helper IDs")
	}
	old := f.assignLocked(w1, 1, time.Now())[0]
	f.disconnect(w1, errors.New("lost VM"))
	work := f.assignLocked(w2, 1, time.Now())[0]
	f.accept(w1, workerResult{Lease: old.Lease, Passed: true})
	if f.done != 0 {
		t.Fatal("accepted revoked lease")
	}
	f.accept(w2, workerResult{Lease: work.Lease, Passed: true, Attempts: 1})
	f.accept(w2, workerResult{Lease: work.Lease, Passed: false})
	if f.done != 1 || w2.Completed != 1 || w2.Failed != 0 {
		t.Fatalf("duplicate result changed counters: %+v", w2)
	}
}

func TestFleetSpeculation(t *testing.T) {
	f := testFleet(t, time.Second)
	w1, w2, w3 := f.addWorker("a", 1, false), f.addWorker("b", 1, false), f.addWorker("c", 1, false)
	now := time.Now()
	first := f.assignLocked(w1, 1, now)[0]
	if got := f.assignLocked(w2, 1, now); len(got) != 0 {
		t.Fatal("duplicated work too early")
	}
	second := f.assignLocked(w2, 1, now.Add(11*time.Second))[0]
	if got := f.assignLocked(w3, 1, now.Add(time.Minute)); len(got) != 0 {
		t.Fatal("more than two live copies")
	}
	canceled := false
	f.leases[first.Lease].cancel = func() { canceled = true }
	f.accept(w2, workerResult{Lease: second.Lease, Passed: true, Attempts: 2, Duration: time.Second,
		Failures: []workerFailure{{Duration: time.Millisecond, Output: "first attempt"}},
		Attrs:    map[string]string{"issue": "123"}, History: []history.Observation{{ID: "obs"}}})
	f.accept(w1, workerResult{Lease: first.Lease, Passed: false})
	ts := f.s.pkgs["p0"].tests["TestX"]
	if !canceled || f.done != 1 || !ts.passed || ts.attempts != 2 || len(ts.fails) != 1 || ts.attrs["issue"] != "123" || len(f.s.historyPending) != 1 {
		t.Fatalf("incorrect winner state: %+v; canceled=%v", ts, canceled)
	}
	res := f.exchange(w1, workerRequest{Active: map[uint64]string{first.Lease: "running"}})
	if !res.Done || len(res.Cancel) != 1 {
		t.Fatalf("loser response = %+v", res)
	}
}

func TestFleetInfrastructureFailureAndFailfast(t *testing.T) {
	f := testFleet(t, time.Second, time.Second)
	w := f.addWorker("broken", 2, false)
	work := f.assignLocked(w, 10, time.Now())
	f.accept(w, workerResult{Lease: work[0].Lease, Infra: true, Error: "wrong binary"})
	if f.done != 0 || len(f.leases) != 0 || w.Error == "" {
		t.Fatal("helper infrastructure error was not released")
	}
	if res := f.exchange(w, workerRequest{}); res.Error == "" || len(res.Work) != 0 {
		t.Fatalf("response = %+v", res)
	}
	old := *failFast
	*failFast = true
	t.Cleanup(func() { *failFast = old })
	w2 := f.addWorker("good", 2, false)
	work = f.assignLocked(w2, 10, time.Now())
	f.accept(w2, workerResult{Lease: work[0].Lease, Error: "exit status 1", Attempts: 4})
	if f.s.ctx.Err() == nil || !f.s.failFastTriggered {
		t.Fatal("remote failure did not cancel run")
	}
	f.accept(w2, workerResult{Lease: work[1].Lease, Infra: true, Error: "canceled"})
	if f.done != 1 {
		t.Fatal("canceled sibling counted as failure")
	}
}

func TestFleetStatus(t *testing.T) {
	f := testFleet(t, 20*time.Second, 20*time.Second)
	w := f.addWorker("<vm>", 2, false)
	work := f.assignLocked(w, 1, time.Now())[0]
	f.exchange(w, workerRequest{Active: map[uint64]string{work.Lease: "running"}, Binaries: []string{work.Binary}})
	d := f.s.statusData()
	if len(d.Workers) != 1 || d.Workers[0].Status != "working" || len(d.Workers[0].Work) != 2 || !w.binaries[work.Binary] {
		t.Fatalf("worker status = %+v", d.Workers)
	}
	var out bytes.Buffer
	if err := rootTmpl.Execute(&out, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "&lt;vm&gt;") || !strings.Contains(out.String(), "p0.TestX (running)") {
		t.Fatal("missing escaped helper status")
	}
	if p := f.s.progressSnapshot(); p.Helpers != 1 || p.TestsRunning != 1 {
		t.Fatalf("progress = %+v", p)
	}
}

func TestWorkerHandshake(t *testing.T) {
	f := testFleet(t)
	for _, hello := range []workerHello{
		{Version: 999, Slots: 1, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH},
		{Version: workerProtocolVersion, Slots: 0, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH},
		{Version: workerProtocolVersion, Slots: 1, GOOS: "wrong", GOARCH: runtime.GOARCH},
	} {
		a, b := net.Pipe()
		go f.serveWorker(a)
		b.SetDeadline(time.Now().Add(time.Second))
		if err := json.NewEncoder(b).Encode(hello); err != nil {
			t.Fatal(err)
		}
		var res workerConfig
		if err := json.NewDecoder(b).Decode(&res); err != nil {
			t.Fatal(err)
		}
		b.Close()
		if res.Error == "" {
			t.Fatalf("accepted invalid hello %+v", hello)
		}
	}
}

func TestFleetLocalExecution(t *testing.T) {
	// Exercise the shared queue's local workers without a tailcat connection.
	// These existing unit tests are also safe, real test-binary payloads.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	s := &Server{ctx: ctx, cancel: cancel, cacheDir: t.TempDir(), quiet: true,
		pkgs: map[string]*packageStatus{"fixture": {
			glp: &goListPackage{}, tests: map[string]*testStatus{"TestGoListPackageHasTests": {}, "TestDirectTestArgs": {}},
		}}, testEstimates: make(map[string]testEstimate)}
	f := newFleet(s)
	var tasks []testTask
	for _, name := range []string{"TestGoListPackageHasTests", "TestDirectTestArgs"} {
		tasks = append(tasks, testTask{bin: capturedTestBinary{pkg: "fixture", absBin: self}, test: name})
		s.testEstimates["fixture\x00"+name] = testEstimate{duration: time.Second}
	}
	if err := f.run(tasks); err != nil {
		t.Fatal(err)
	}
	if f.done != 2 || f.workers[0].Completed != 2 || !f.stopped {
		t.Fatalf("local run did not complete: %+v", f)
	}
	if s.pkgs["fixture"].pkgState != pkgStateDone {
		t.Fatal("package not complete")
	}
}

func TestWorkerDisconnectRequeues(t *testing.T) {
	f := testFleet(t, time.Second)
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() { f.serveWorker(a); close(done) }()
	b.SetDeadline(time.Now().Add(time.Second))
	enc, dec := json.NewEncoder(b), json.NewDecoder(b)
	if err := enc.Encode(workerHello{Version: workerProtocolVersion, Name: "vm", Slots: 1, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}); err != nil {
		t.Fatal(err)
	}
	var config workerConfig
	if err := dec.Decode(&config); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(workerRequest{}); err != nil {
		t.Fatal(err)
	}
	var res workerResponse
	if err := dec.Decode(&res); err != nil {
		t.Fatal(err)
	}
	if len(res.Work) != 1 {
		t.Fatalf("assignment = %+v", res)
	}
	b.Close()
	<-done
	if len(f.leases) != 0 || f.workers[0].Connected {
		t.Fatal("connection loss kept its work")
	}
	w := f.addWorker("replacement", 1, false)
	if batch := f.assignLocked(w, 1, time.Now()); len(batch) != 1 || batch[0].Lease == res.Work[0].Lease {
		t.Fatalf("replacement work = %+v", batch)
	}
}

// Allow nested cmd/go wrappers to use this test executable as gotst, avoiding
// another complete gotst build for the distributed integration fixture.
func TestMain(m *testing.M) {
	if os.Getenv("GOTST_TEST_DISTRIBUTED") == "1" && len(os.Args) > 1 &&
		(os.Getenv("GOTST_EXEC_DEST") != "" || os.Args[1] == cacheShimArg || os.Args[1] == localCacheProgArg || os.Args[1] == toolExecArg) {
		if os.Args[1] == localCacheProgArg {
			os.Setenv(localCacheDirEnv, os.Getenv("GOTST_TEST_SHARED_CACHE"))
			os.Setenv(stockGoCacheDirEnv, os.Getenv("GOTST_TEST_STOCK_CACHE"))
		}
		main()
		return
	}
	os.Exit(m.Run())
}

func TestDistributedWorkerProcess(t *testing.T) {
	addr := os.Getenv("GOTST_TEST_WORKER_ADDR")
	if addr == "" {
		t.Skip("subprocess helper")
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := runWorkerConnection(c, "fixture-vm"); err != nil {
		t.Fatal(err)
	}
}

func TestDistributedExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and executes a nested module in a helper process")
	}
	t.Setenv("GOTST_TEST_DISTRIBUTED", "1")
	// Each cmd/go tool invocation runs this test executable as a wrapper.
	// Avoid the race runtime's default one-second exit delay in every child.
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	oldCache, oldHistory, oldRoot := *useCache, *historyConfig, *cacheRoot
	oldCountSet, oldJSON, oldRetries := testCountSet, *jsonSummary, *maxRetries
	*useCache, *historyConfig, *cacheRoot = false, "local", t.TempDir()
	testCountSet, *jsonSummary, *maxRetries = true, true, 1
	t.Cleanup(func() {
		*useCache, *historyConfig, *cacheRoot = oldCache, oldHistory, oldRoot
		testCountSet, *jsonSummary, *maxRetries = oldCountSet, oldJSON, oldRetries
	})
	leaderDir, helperDir := t.TempDir(), t.TempDir()
	for _, dir := range []string{leaderDir, helperDir} {
		for name, data := range map[string]string{
			"go.mod": "module distfixture\n\ngo 1.25.1\n",
			"x_test.go": `package distfixture
import ("os"; "testing")
func TestRetry(t *testing.T) {
 t.Attr("issue", "456")
 if _, err := os.Stat("attempt"); err != nil {
  os.WriteFile("attempt", nil, 0600)
  t.Fatal("retry diagnostic")
 }
}
func TestFail(t *testing.T) { t.Fatal("remote failure diagnostic") }
func TestPass(t *testing.T) {
 if err := os.WriteFile("passed", nil, 0600); err != nil { t.Fatal(err) }
}
`,
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Both machines use an external GOCACHEPROG backed by the same directory.
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
	s := NewServer(runProfile{Root: leaderDir, Packages: []string{"."}}, testSelection{})
	t.Cleanup(s.Cleanup)
	if err := s.learnPackagesWithTests(); err != nil {
		t.Fatal(err)
	}
	if err := s.buildAllTestBinaries(); err != nil {
		t.Fatal(err)
	}
	if err := s.listAllTests(); err != nil {
		t.Fatal(err)
	}
	tasks := s.allTestTasks()
	f := newFleet(s)
	s.fleet = f
	for _, task := range tasks {
		f.work = append(f.work, &fleetTask{task: task, estimate: time.Second, leases: make(map[uint64]*workLease)})
	}
	f.ready = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	served := make(chan struct{})
	go func() {
		defer close(served)
		c, err := ln.Accept()
		if err == nil {
			f.serveWorker(c)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestDistributedWorkerProcess$", "-vlog", "-j=2", "-cache-dir="+t.TempDir())
	cmd.Dir = helperDir
	cmd.Env = append(os.Environ(), "GOTST_TEST_WORKER_ADDR="+ln.Addr().String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("linked executable cache: 1 hit(s), 0 put(s)")) {
		t.Fatalf("helper did not restore shared executable: %s", out)
	}
	<-served
	if f.done != 3 || len(f.workers) != 1 || f.workers[0].Completed != 3 || f.workers[0].Failed != 1 {
		t.Fatalf("fleet done=%d workers=%+v", f.done, f.workers)
	}
	ts := s.pkgs["distfixture"].tests["TestRetry"]
	if !ts.passed || ts.attempts != 2 || ts.attrs["issue"] != "456" || len(ts.fails) != 1 || !strings.Contains(ts.fails[0].out, "retry diagnostic") {
		t.Fatalf("remote retry result = %+v", ts)
	}
	if len(s.historyPending) != 3 {
		t.Fatalf("history count = %d", len(s.historyPending))
	}
	if _, err := os.Stat(filepath.Join(helperDir, "passed")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(leaderDir, "passed")); !os.IsNotExist(err) {
		t.Fatal("test ran in leader checkout")
	}
}
