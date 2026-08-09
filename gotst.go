// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// The gotst command is an alternative to (and wrapper of) "go test"
// to run Go tests for both humans and CI systems.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/gotst/history"
	"tailscale.com/client/local"
)

var (
	flagListen = flag.String("listen", "127.0.0.1:5525", "if non-empty, run HTTP server on this address and serve status")
	configFile = flag.String("config", "", "path to .gotst.yml (default: search parent directories)")

	extraSleep = flag.Duration("extra-sleep", 0, "[dev] if non-zero, sleep this long before exiting after all tests complete, to give time to explore the web UI")

	tags          = flag.String("tags", "", "comma-separated list of build tags to pass to 'go test' when building and running tests")
	verbose       = flag.Bool("vlog", false, "verbose gotst debug logging")
	jobs          = flag.Int("j", min(runtime.NumCPU(), 4), "maximum concurrent build or test processes")
	maxOutput     = flag.Int64("max-output", 4<<20, "maximum bytes of failure output retained per package")
	useCache      = flag.Bool("cache", true, "reuse successful tests whose recorded inputs are unchanged")
	cacheRoot     = flag.String("cache-dir", "", "cache root (default: user cache directory/gotst)")
	progressEvery = flag.Duration("progress", time.Second, "interval between aggregate progress updates (0 disables periodic updates)")
	failFast      = flag.Bool("failfast", false, "stop queued and running tests after the first failure")
	testCount     = flag.Int("count", 1, "run each test n times; explicitly setting this disables result caching")
	maxRetries    = flag.Int("max-retries", 3, "maximum additional attempts after a test failure")
	buildOnly     = flag.Bool("build-only", false, "build and capture selected test binaries without listing or running tests")
	debugUncached = flag.Bool("debug-uncached", false, "run a cache-seeding pass, then diagnose tests that do not reuse it")
	jsonSummary   = flag.Bool("json-summary", false, "emit a machine-readable flaky-test summary including testing.T attributes")
	historyConfig = flag.String("history", "local", "history store: local, off, or an http(s) base URL")
	testCountSet  bool
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == cacheShimArg {
		if err := runCacheShim(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == localCacheProgArg {
		if err := runLocalCacheProg(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if dir := os.Getenv("GOTST_EXEC_DEST"); dir != "" {
		// We're running as a test binary under "go test -exec".
		// Just capture our output to the given directory and exit.
		storeTestExecBinary(dir)
		return
	}
	flag.Parse()
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "count" {
			testCountSet = true
		}
	})
	if *jobs < 1 {
		log.Fatal("-j must be at least 1")
	}
	if *maxOutput < 0 {
		log.Fatal("-max-output must not be negative")
	}
	if *progressEvery < 0 {
		log.Fatal("-progress must not be negative")
	}
	if *testCount < 1 {
		log.Fatal("-count must be at least 1")
	}
	if *maxRetries < 0 {
		log.Fatal("-max-retries must not be negative")
	}
	if *debugUncached {
		if testCountSet {
			log.Fatal("-debug-uncached and -count are mutually exclusive")
		}
		if !*useCache {
			log.Fatal("-debug-uncached requires -cache=true")
		}
		if *buildOnly {
			log.Fatal("-debug-uncached and -build-only are mutually exclusive")
		}
		if *failFast {
			log.Fatal("-debug-uncached and -failfast are mutually exclusive")
		}
	}
	log.SetPrefix("gotst: ")
	log.SetFlags(log.Flags() | log.Lmsgprefix)

	defs, err := loadProfileDefinitions(*configFile)
	if err != nil {
		log.Fatal(err)
	}
	inv, err := parseInvocation(defs, flag.Args())
	if err != nil {
		log.Fatal(err)
	}
	profile := inv.profile
	if *tags != "" {
		profile.Tags = appendUnique(profile.Tags, splitCommaList(*tags)...)
	}
	if *verbose {
		log.Printf("Using profile %q from %s", profile.Name, profile.Root)
	}

	s := NewServer(profile, inv.tests)
	if *extraSleep > 0 {
		log.Printf("# cacheDir is %v", s.cacheDir)
	}
	defer s.Cleanup()
	if *flagListen != "" {
		lns, statusURL, err := startStatusListeners(s.ctx, *flagListen, s, &local.Client{})
		if err != nil {
			log.Fatalf("listening on %q: %v", *flagListen, err)
		}
		defer closeListeners(lns)
		if statusURL != "" {
			fmt.Fprintf(os.Stderr, "# Status: %s\n", statusURL)
		}
	}
	if err := s.Run(); err != nil {
		log.Fatal(err)
	}
}

type Server struct {
	start     time.Time
	cacheDir  string
	ctx       context.Context
	cancel    context.CancelFunc
	profile   runProfile
	tests     testSelection
	testCache testResultCache
	history   history.Store

	execSem chan bool // buffered chan semaphore to limit subprocesses
	outMu   sync.Mutex
	webMu   sync.Mutex
	webLive map[*liveClient]struct{}

	mu                sync.Mutex
	pkgs              map[string]*packageStatus // test package import path -> status
	pkgsWithTests     int
	buildDepsTotal    int
	phase             runPhase
	testsTotal        int
	cacheChecks       int
	cacheHits         int
	failFastTriggered bool
	debugPass         int
	debugMisses       int
	debugSeedErrors   map[string]string // test cache key ID -> why pass 1 did not seed it
	historyLoaded     bool
	histories         map[string]*history.History
	historyPending    []history.Observation
}

type packageStatus struct {
	glp *goListPackage

	// following fields guarded by [Server.mu]
	pkgState  pkgState
	exeHash   string // once known, the sha256 hex of test binary in Server.cacheDir
	exeSize   int64
	exeCached bool // executable was restored from the linked-binary cache
	workDir   string
	exeArgs   []string
	tests     map[string]*testStatus
	numFails  int // number of tests in tests that failed
	runErr    string
	runIn     time.Duration
	changed   time.Time
}

type testStatus struct {
	running  bool
	done     bool // if true, then test either passed or reach max failures
	passed   bool
	passedIn time.Duration // valid if passed is true
	cached   bool
	fails    []failInfo
	attempts int
	attrs    map[string]string
	changed  time.Time
}

type failInfo struct {
	dur time.Duration
	out string // failure output
}

type pkgState int

const (
	pkgStateDiscovered pkgState = iota
	pkgStateBuilt
	pkgStateTesting // tests actively running
	pkgStateDone    // tests done; might've failed
)

type goListPackage struct {
	Dir          string
	ImportPath   string
	Name         string
	TestGoFiles  []string
	XTestGoFiles []string
	Root         string
}

func NewServer(profile runProfile, tests testSelection) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	var cache testResultCache
	if *useCache && !testCountSet {
		var err error
		cache, err = newDiskTestCache(mustCacheRoot())
		if err != nil {
			log.Fatalf("initializing test result cache: %v", err)
		}
	}
	var historyStore history.Store
	switch {
	case *historyConfig == "off":
	case *historyConfig == "local" || *historyConfig == "":
		localHistory, err := newDiskHistoryStore(mustCacheRoot())
		if err != nil && *verbose {
			log.Printf("disabling local test history: %v", err)
		} else if err == nil {
			historyStore = localHistory
		}
	case strings.HasPrefix(*historyConfig, "http://") || strings.HasPrefix(*historyConfig, "https://"):
		var err error
		historyStore, err = newHTTPHistoryStore(*historyConfig)
		if err != nil {
			log.Fatalf("initializing HTTP test history: %v", err)
		}
	default:
		log.Fatalf("invalid -history value %q; want local, off, or an http(s) URL", *historyConfig)
	}
	return &Server{
		start:           time.Now(),
		cacheDir:        mustNewCacheDir(),
		ctx:             ctx,
		cancel:          cancel,
		profile:         profile,
		tests:           tests,
		testCache:       cache,
		history:         historyStore,
		execSem:         make(chan bool, *jobs),
		debugSeedErrors: make(map[string]string),
		histories:       make(map[string]*history.History),
	}
}

func (s *Server) Run() (retErr error) {
	s.setPhase(phaseDiscovering)
	stopProgress := s.startProgressReporter()
	defer func() {
		stopProgress()
		if retErr != nil {
			s.setPhase(phaseFailed)
		} else {
			s.setPhase(phaseDone)
		}
		s.flushFinalLiveStatus()
		s.printProgress()
		s.printFlakySummary()
		s.flushHistory()
	}()

	if err := s.resolveQualifiedTests(); err != nil {
		return fmt.Errorf("resolving test packages: %w", err)
	}
	if err := s.learnPackagesWithTests(); err != nil {
		return fmt.Errorf("learnPackagesWithTests: %w", err)
	}

	s.setPhase(phaseBuilding)
	if err := s.buildAllTestBinaries(); err != nil {
		return fmt.Errorf("buildAllTestBinaries: %w", err)
	}
	if *buildOnly {
		return nil
	}

	s.setPhase(phaseListing)
	if err := s.listAllTests(); err != nil {
		return fmt.Errorf("listAllTests: %w", err)
	}

	s.setPhase(phaseTesting)
	if *debugUncached {
		if err := s.runDebugUncached(); err != nil {
			return err
		}
	} else {
		if err := s.runAllTests(); err != nil {
			return err
		}
	}

	if *extraSleep > 0 {
		log.Printf("sleeping extra %v before exiting; cacheDir is %v", *extraSleep, s.cacheDir)
		time.Sleep(*extraSleep)
	}
	return nil
}

func (s *Server) runDebugUncached() error {
	s.mu.Lock()
	s.debugPass = 1
	s.mu.Unlock()
	log.Printf("debug-uncached: pass 1/2: running all tests to seed the result cache")
	if err := s.runAllTests(); err != nil {
		return fmt.Errorf("debug-uncached seed pass: %w", err)
	}

	s.mu.Lock()
	s.debugPass = 2
	s.cacheChecks = 0
	s.cacheHits = 0
	for _, ps := range s.pkgs {
		if ps.exeHash == "" {
			continue
		}
		ps.pkgState = pkgStateBuilt
		ps.numFails = 0
		ps.runErr = ""
		ps.runIn = 0
		now := time.Now()
		ps.changed = now
		for _, ts := range ps.tests {
			attrs := ts.attrs
			*ts = testStatus{attrs: attrs, changed: now}
		}
	}
	s.mu.Unlock()
	log.Printf("debug-uncached: pass 2/2: verifying every seeded result is reused")
	s.verifyDebugCache()
	s.mu.Lock()
	misses, checks, hits := s.debugMisses, s.cacheChecks, s.cacheHits
	s.mu.Unlock()
	if misses != 0 {
		return fmt.Errorf("debug-uncached: %d/%d tests did not reuse the result cache (%d hits)", misses, checks, hits)
	}
	log.Printf("debug-uncached: all %d tests reused the seeded result cache", hits)
	return nil
}

func (s *Server) verifyDebugCache() {
	tasks := s.allTestTasks()
	work := make(chan testTask)
	var wg sync.WaitGroup
	for range min(*jobs, len(tasks)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range work {
				s.verifyDebugCacheTask(task)
			}
		}()
	}
	for _, task := range tasks {
		work <- task
	}
	close(work)
	wg.Wait()
}

func (s *Server) verifyDebugCacheTask(task testTask) {
	key := s.cacheKeyForTask(task)
	s.mu.Lock()
	s.cacheChecks++
	seedError := s.debugSeedErrors[key.id()]
	s.mu.Unlock()
	if seedError != "" {
		s.reportDebugCacheMiss(task, key, "seed pass did not create a clean result", nil)
		s.finishTest(task, true, 0, nil, nil, false)
		return
	}
	entry, err := s.testCache.Get(s.ctx, key)
	if err != nil {
		reason := "cache entry missing after seed pass"
		if !errors.Is(err, errTestCacheMiss) {
			reason = "reading seeded cache entry: " + err.Error()
		}
		s.reportDebugCacheMiss(task, key, reason, nil)
		s.finishTest(task, true, 0, nil, nil, false)
		return
	}
	changes, err := changedDependencies(entry.Dependencies)
	if err != nil {
		s.reportDebugCacheMiss(task, key, "validating cached dependencies: "+err.Error(), nil)
		s.finishTest(task, true, 0, nil, nil, false)
		return
	}
	if len(changes) != 0 {
		s.reportDebugCacheMiss(task, key, "recorded inputs changed", changes)
		s.finishTest(task, true, 0, nil, nil, false)
		return
	}
	s.mu.Lock()
	s.cacheHits++
	s.mu.Unlock()
	s.finishTest(task, true, entry.PassedIn, nil, nil, true)
}

func (s *Server) Cleanup() {
	s.cancel()
	if s.cacheDir != "" {
		t0 := time.Now()
		if err := os.RemoveAll(s.cacheDir); err != nil {
			log.Printf("removing cache dir %q: %v", s.cacheDir, err)
		}
		d := time.Since(t0)
		if d > 2*time.Second {
			log.Printf("removed cache dir %q in %v", s.cacheDir, d)
		}
	}
}

func (s *Server) addPackage(glp *goListPackage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pkgs == nil {
		s.pkgs = make(map[string]*packageStatus)
	}
	st := &packageStatus{
		glp:     glp,
		changed: time.Now(),
	}
	s.pkgs[glp.ImportPath] = st
	if glp.hasTests() {
		s.pkgsWithTests++
	}
}

func (p *goListPackage) hasTests() bool {
	return len(p.TestGoFiles) > 0 || len(p.XTestGoFiles) > 0
}

func (s *Server) resolveQualifiedTests() error {
	for i := range s.tests.qualified {
		q := &s.tests.qualified[i]
		args := []string{"list", "-buildvcs=false", "--tags=" + strings.Join(s.profile.Tags, ","), "-f={{.ImportPath}}", q.packageSpec}
		cmd := exec.Command(goCmd(), args...)
		cmd.Dir = s.profile.Root
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("resolving package %q: %w: %s", q.packageSpec, err, strings.TrimSpace(string(out)))
		}
		paths := strings.Fields(string(out))
		if len(paths) != 1 {
			return fmt.Errorf("package %q resolved to %d packages; want exactly one", q.packageSpec, len(paths))
		}
		q.packagePath = paths[0]
		if !slices.Contains(s.profile.Packages, q.packageSpec) {
			s.profile.Packages = append(s.profile.Packages, q.packageSpec)
		}
	}
	return nil
}

func (s *Server) learnPackagesWithTests() error {
	if *verbose {
		log.Printf("Discovering packages with tests...")
	}
	t0 := time.Now()

	excluded, err := s.resolveExcludedPackages()
	if err != nil {
		return err
	}
	// Package discovery does not use VCS stamping. Disabling it also prevents
	// Git from briefly creating .git/index.lock while checking repository
	// status, which would invalidate cached tests that inspect the source tree.
	args := []string{"list", "-buildvcs=false", "--tags=" + strings.Join(s.profile.Tags, ","), "--json"}
	args = append(args, s.profile.Packages...)
	cmd := exec.Command(goCmd(), args...)
	cmd.Dir = s.profile.Root
	return processCmdOutput(cmd, func(r io.Reader) error {
		jd := json.NewDecoder(r)
		for {
			pkg := new(goListPackage)
			if err := jd.Decode(pkg); err != nil {
				if errors.Is(err, io.EOF) {
					if *verbose {
						log.Printf("Discovered packages with tests in %v", time.Since(t0).Round(time.Millisecond))
					}
					return nil
				}
				return fmt.Errorf("decoding package: %w", err)
			}
			if !excluded[pkg.ImportPath] && s.packageMayContainSelectedTest(pkg) {
				s.addPackage(pkg)
			}
		}
	})
}

func (s *Server) packageMayContainSelectedTest(pkg *goListPackage) bool {
	if !s.tests.active() {
		return true
	}
	if len(s.tests.packageTests(pkg.ImportPath)) > 0 {
		return true
	}
	if len(s.tests.unqualified) == 0 {
		return false
	}
	entries, err := os.ReadDir(pkg.Dir)
	if err != nil {
		return true
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(pkg.Dir, entry.Name()))
		if err != nil {
			// The source tree might be changing under us. Keep the package as a
			// candidate and let compilation and -test.list be authoritative.
			return true
		}
		for _, name := range s.tests.unqualified {
			if bytes.Contains(data, []byte(name)) {
				return true
			}
		}
	}
	return false
}

func (s *Server) resolveExcludedPackages() (map[string]bool, error) {
	ret := make(map[string]bool)
	if len(s.profile.ExcludePackages) == 0 {
		return ret, nil
	}
	args := []string{"list", "-buildvcs=false", "--tags=" + strings.Join(s.profile.Tags, ","), "-f={{.ImportPath}}"}
	args = append(args, s.profile.ExcludePackages...)
	cmd := exec.Command(goCmd(), args...)
	cmd.Dir = s.profile.Root
	var out bytes.Buffer
	cmd.Stdout = &out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("resolving excluded packages: %w: %s", err, stderr.String())
	}
	for _, pkg := range strings.Fields(out.String()) {
		ret[pkg] = true
	}
	return ret, nil
}

func (s *Server) packagesWithTests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ret []string
	for imp, ps := range s.pkgs {
		if ps.glp.hasTests() {
			ret = append(ret, imp)
		}
	}
	sort.Strings(ret)
	return ret
}

// learnBuildDependencyCount records the number of distinct packages in the
// build graph for the selected test binaries. cmd/go does not report live
// successful compile/cache progress, so this is a total only.
func (s *Server) learnBuildDependencyCount(pkgs []string) error {
	args := []string{
		"list", "-buildvcs=false", "-deps", "-test",
		"--tags=" + strings.Join(s.profile.Tags, ","), "-f={{.ImportPath}}",
	}
	args = append(args, pkgs...)
	cmd := exec.Command(goCmd(), args...)
	cmd.Dir = s.profile.Root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go list -deps -test: %w: %s", err, strings.TrimSpace(string(out)))
	}
	deps := make(map[string]struct{})
	for _, dep := range strings.Fields(string(out)) {
		deps[dep] = struct{}{}
	}
	s.mu.Lock()
	s.buildDepsTotal = len(deps)
	s.mu.Unlock()
	return nil
}

// TestEvent is the text2json representation of a test event,
// as documented in $GO/src/cmd/test2json/main.go, unioned with
// the fields (well field singular) of BuildEvent, as "go test -json"
// includes both types of events, distinguished by the Action field.
//
// The docs from Go's source are copied down below, into their fields.
type TestEvent struct {
	// The Time field holds the time the event happened.
	// It is conventionally omitted for cached test results.
	Time time.Time

	// The Action field is one of a fixed set of action descriptions:
	//
	//	start  - the test binary is about to be executed
	//	run    - the test has started running
	//	pause  - the test has been paused
	//	cont   - the test has continued running
	//	pass   - the test passed
	//	bench  - the benchmark printed log output but did not fail
	//	fail   - the test or benchmark failed
	//	output - the test printed output
	//	skip   - the test was skipped or the package contained no tests
	//
	// Every JSON stream begins with a "start" event.
	//
	// If you see the Action "build-output" or "build-fail", then
	// the event is not a TestEvent, but a BuildEvent, which has fields
	// "Action", "Output", and "ImportPath".
	Action string

	// The Package field, if present, specifies the package being tested.
	// When the go command runs parallel tests in -json mode, events from
	// different tests are interlaced; the Package field allows readers to
	// separate them.
	Package string

	// The Test field, if present, specifies the test, example, or benchmark
	// function that caused the event. Events for the overall package test
	// do not set Test.
	Test string

	// The Elapsed field is set for "pass" and "fail" events. It gives the time
	// elapsed for the specific test or the overall package test that passed or failed.
	Elapsed float64 // seconds

	// The Output field is set for Action == "output" and is a portion of the test's output
	// (standard output and standard error merged together). The output is
	// unmodified except that invalid UTF-8 output from a test is coerced
	// into valid UTF-8 by use of replacement characters. With that one exception,
	// the concatenation of the Output fields of all output events is the exact
	// output of the test execution.
	Output string

	// FailedBuild is set for Action == "fail" if the test failure was caused by
	// a build failure. It contains the package ID of the package that failed to
	// build. This matches the ImportPath field of the "go list" output, as well
	// as the BuildEvent.ImportPath field as emitted by "go build -json".
	FailedBuild string
}

// BuildEvent is an event representing a build process. Field
// docs below are copied from $GO/src/cmd/go/alldocs.go.
//
// See `go help buildjson`.
type BuildEvent struct {
	// The ImportPath field gives the package ID of the package being built.
	// This matches the Package.ImportPath field of go list -json and the
	// TestEvent.FailedBuild field of go test -json. Note that it does not
	// match TestEvent.Package.
	ImportPath string

	// Action is one of the following:
	//
	//	build-output - The toolchain printed output
	//	build-fail - The build failed
	Action string

	// The Output field is set for Action == "build-output" and is a portion of
	// the build's output. The concatenation of the Output fields of all output
	// events is the exact output of the build. A single event may contain one
	// or more lines of output and there may be more than one output event for
	// a given ImportPath. This matches the definition of the TestEvent.Output
	// field produced by go test -json.
	Output string
}

// parseTestOrBuildEvent parses a line of JSON output from "go test -json"
// and returns either a *TestEvent or a *BuildEvent depending on its Action.
func parseTestOrBuildEvent(line []byte) (any, error) {
	var a struct {
		Action string
	}
	if err := json.Unmarshal(line, &a); err != nil {
		return nil, fmt.Errorf("unmarshal action: %w", err)
	}
	switch a.Action {
	case "":
		return nil, fmt.Errorf("empty action in event")
	case "build-output", "build-fail":
		var ev BuildEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("unmarshal build event: %w", err)
		}
		return &ev, nil
	default:
		var ev TestEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("unmarshal test event: %w", err)
		}
		return &ev, nil
	}
}

func (s *Server) buildAllTestBinaries() error {
	if *verbose {
		log.Printf("Building test binaries...")
	}
	t0 := time.Now()

	selfExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("getting self executable path: %w", err)
	}
	pkgs := s.packagesWithTests()
	if len(pkgs) == 0 {
		if *verbose {
			log.Printf("No packages with tests")
		}
		return nil
	}
	if err := s.learnBuildDependencyCount(pkgs); err != nil && *verbose {
		log.Printf("discovering build dependencies: %v", err)
	}
	args := []string{
		"test",
		"-count=1", // disable cmd/go test-result caching; gotst owns this layer
		"-p=" + fmt.Sprint(*jobs),
		"--trimpath",
		"--tags=" + strings.Join(s.profile.Tags, ","),
		"--json",
		"--exec=" + selfExe,
	}
	args = append(args, pkgs...)
	cmd := exec.Command(goCmd(), args...)
	cmd.Dir = s.profile.Root
	cmd.Env = append(envWithout(os.Environ(), "GOTST_EXEC_DEST"), "GOTST_EXEC_DEST="+s.cacheDir)
	var shim *cacheShim
	if realCacheProg := os.Getenv("GOCACHEPROG"); realCacheProg != "" {
		shim, err = startRunCacheShim(realCacheProg)
		if err != nil {
			return fmt.Errorf("starting GOCACHEPROG shim: %w", err)
		}
		cmd.Env = append(envWithout(cmd.Env, "GOCACHEPROG", cacheShimSocketEnv),
			"GOCACHEPROG="+quoteCacheProgArg(selfExe)+" "+cacheShimArg,
			cacheShimSocketEnv+"="+shim.endpoint,
		)
	} else {
		stockCache, err := goBuildCacheDir(s.profile.Root)
		if err != nil {
			return fmt.Errorf("locating Go build cache: %w", err)
		}
		localDir := filepath.Join(mustCacheRoot(), "build-cache", "v1")
		shim, err = startRunCacheShim(quoteCacheProgArg(selfExe)+" "+localCacheProgArg,
			localCacheDirEnv+"="+localDir,
			stockGoCacheDirEnv+"="+stockCache,
		)
		if err != nil {
			return fmt.Errorf("starting local build cache: %w", err)
		}
		shim.directExecutables = true
		cmd.Env = append(envWithout(cmd.Env, "GOCACHEPROG", cacheShimSocketEnv),
			"GOCACHEPROG="+quoteCacheProgArg(selfExe)+" "+cacheShimArg,
			cacheShimSocketEnv+"="+shim.endpoint,
		)
	}
	err = processCmdOutput(cmd, func(r io.Reader) error {

		var errs []error
		// This contains gotst's own small ExecSnarf record, not user test
		// output, so it must remain available even when -max-output=0.
		testOut := outputMap{max: 1 << 20}
		buildOut := outputMap{max: *maxOutput}
		bs := bufio.NewScanner(r)
		for bs.Scan() {
			ev, err := parseTestOrBuildEvent(bs.Bytes())
			if err != nil {
				return fmt.Errorf("parsing event: %w", err)
			}
			switch ev := ev.(type) {
			case *BuildEvent:
				if ev.Action == "build-output" {
					buildOut.Add(ev.ImportPath, ev.Output)
				}
			case *TestEvent:
				switch ev.Action {
				case "fail":
					if ev.FailedBuild != "" {
						errs = append(errs, fmt.Errorf("failed to compile tests for %q; failure building %q:\n\n%s\n", ev.Package, ev.FailedBuild, buildOut.Get(ev.FailedBuild)))

					}
				case "output":
					testOut.Add(ev.Package, ev.Output)
				case "pass":
					pkgOutput := testOut.Get(ev.Package)
					es, ok, err := findExecSnarf(pkgOutput)
					if !ok {
						errs = append(errs, fmt.Errorf("test package %q built but test wrapper did not emit ExecSnarf line; wrapper output:\n%s", ev.Package, pkgOutput))
						continue
					}
					if err != nil {
						errs = append(errs, fmt.Errorf("unmarshal ExecSnarf JSON from package %q: %w", ev.Package, err))
						continue
					}
					s.addTestBinary(ev.Package, es, shim.executableWasCached(es.ExeHash))
				}
			}
		}
		if err := bs.Err(); err != nil {
			return err
		}
		if err := errors.Join(errs...); err != nil {
			return err
		}
		if *verbose {
			log.Printf("Built %d test binaries in %v", len(pkgs), time.Since(t0).Round(time.Millisecond))
		}
		return nil
	})
	if shim != nil {
		err = errors.Join(err, shim.close())
		if *verbose {
			log.Printf("linked executable cache: %d hit(s), %d put(s)", shim.execHits.Load(), shim.execPuts.Load())
		}
	}
	return err
}

func findExecSnarf(output string) (es ExecSnarf, found bool, err error) {
	for line := range strings.SplitSeq(output, "\n") {
		jsonText, ok := strings.CutPrefix(line, "ExecSnarf:")
		if !ok {
			continue
		}
		err := json.Unmarshal([]byte(jsonText), &es)
		return es, true, err
	}
	return ExecSnarf{}, false, nil
}

type outputMap struct {
	max int64
	m   map[string]*cappedBuffer
}

func (m *outputMap) Add(pkg, out string) {
	if m.m == nil {
		m.m = make(map[string]*cappedBuffer)
	}
	buf, ok := m.m[pkg]
	if !ok {
		buf = &cappedBuffer{max: m.max}
		m.m[pkg] = buf
	}
	_, _ = buf.Write([]byte(out))
}

func (m *outputMap) Get(pkg string) string {
	buf, ok := m.m[pkg]
	if !ok {
		return ""
	}
	return buf.String()
}

func (s *Server) addTestBinary(pkg string, es ExecSnarf, cached bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, ok := s.pkgs[pkg]
	if !ok {
		log.Printf("internal error: addTestBinary: unknown package %q", pkg)
		return
	}
	ps.pkgState = pkgStateBuilt
	ps.exeHash = es.ExeHash
	ps.exeCached = cached
	ps.workDir = es.WorkingDir
	ps.exeArgs = slices.Clone(es.Args)
	ps.changed = time.Now()

	absBin := filepath.Join(s.cacheDir, es.ExeHash)
	fi, err := os.Stat(absBin)
	if err != nil {
		log.Fatalf("statting test binary for package %q: %v", pkg, err)
	}
	size := fi.Size()
	ps.exeSize = size
	mB := float64(size) / (1 << 20)

	if *verbose {
		log.Printf("test binary for package %q is %0.1f MB, hash %s", pkg, mB, es.ExeHash)
	}

	if es.WorkingDir != ps.glp.Dir {
		log.Fatalf("unexpected pkg %q wd=%q vs golist=%q", pkg, es.WorkingDir, ps.glp.Dir)
	}
}

func (s *Server) awaitExecSem(ctx context.Context) bool {
	t0 := time.Now()
	select {
	case s.execSem <- true:
		d := time.Since(t0).Round(time.Millisecond)
		if *verbose {
			log.Printf("acquired exec semaphore after %v", d)
		}
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Server) releaseExecSem() { <-s.execSem }

func (s *Server) listTestsInBinary(pkg, absBin, workDir string) error {
	if !s.awaitExecSem(s.ctx) {
		return s.ctx.Err()
	}
	defer s.releaseExecSem()

	t0 := time.Now()
	cmd := exec.CommandContext(s.ctx, absBin, "-test.list=.")
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("listing tests in %q: %w\noutput:\n%s", pkg, err, out)
	}
	d := time.Since(t0).Round(time.Millisecond)
	if *verbose {
		log.Printf("Listed tests in package %q in %v (%s)", pkg, d, absBin)
	}
	tests := strings.Fields(string(out))
	s.setPackageTests(pkg, tests)
	return nil
}

func (s *Server) setPackageTests(pkg string, tests []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ps, ok := s.pkgs[pkg]
	if !ok {
		log.Fatalf("internal error: setPackageTests: unknown package %q", pkg)
	}
	ps.tests = make(map[string]*testStatus)
	now := time.Now()
	for _, test := range tests {
		if !s.tests.wants(pkg, test) {
			continue
		}
		ps.tests[test] = &testStatus{changed: now}
		if isRunnableTopLevelTest(test) {
			s.testsTotal++
		}
	}
	ps.changed = now
}

type capturedTestBinary struct {
	pkg     string
	absBin  string
	workDir string
	args    []string
}

func (s *Server) capturedTestBinaries() []capturedTestBinary {
	s.mu.Lock()
	defer s.mu.Unlock()
	ret := make([]capturedTestBinary, 0, s.pkgsWithTests)
	for pkg, ps := range s.pkgs {
		if ps.exeHash == "" {
			continue
		}
		ret = append(ret, capturedTestBinary{
			pkg:     pkg,
			absBin:  filepath.Join(s.cacheDir, ps.exeHash),
			workDir: ps.workDir,
			args:    slices.Clone(ps.exeArgs),
		})
	}
	sort.Slice(ret, func(i, j int) bool { return ret[i].pkg < ret[j].pkg })
	return ret
}

func (s *Server) listAllTests() error {
	t0 := time.Now()
	bins := s.capturedTestBinaries()
	if len(bins) == 0 {
		return s.validateSelectedTests()
	}
	errs := make(chan error, len(bins))
	for _, bin := range bins {
		bin := bin
		go func() {
			errs <- s.listTestsInBinary(bin.pkg, bin.absBin, bin.workDir)
		}()
	}
	var all []error
	for range bins {
		if err := <-errs; err != nil {
			all = append(all, err)
		}
	}
	if err := errors.Join(all...); err != nil {
		return err
	}
	if err := s.validateSelectedTests(); err != nil {
		return err
	}
	if *verbose {
		log.Printf("Listed tests in %d packages in %v", len(bins), time.Since(t0).Round(time.Millisecond))
	}
	return nil
}

func (s *Server) validateSelectedTests() error {
	if !s.tests.active() {
		return nil
	}
	found := make(map[string]bool)
	s.mu.Lock()
	defer s.mu.Unlock()
	for pkg, ps := range s.pkgs {
		for test := range ps.tests {
			if slices.Contains(s.tests.unqualified, test) {
				found[test] = true
			}
			for _, q := range s.tests.qualified {
				if q.packagePath == pkg && q.name == test {
					found[q.packageSpec+"."+q.name] = true
				}
			}
		}
	}
	return s.tests.describeMissing(found)
}

type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int64
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remain := b.max - int64(b.buf.Len())
	if remain > 0 {
		if int64(len(p)) > remain {
			p = p[:remain]
		}
		_, _ = b.buf.Write(p)
	}
	if int64(n) > remain {
		b.truncated = true
	}
	return n, nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	if b.truncated {
		s += fmt.Sprintf("\n[gotst: output truncated after %d bytes]\n", b.max)
	}
	return s
}

func (s *Server) runAllTests() error {
	bins := s.capturedTestBinaries()
	if len(bins) == 0 {
		return nil
	}
	tasks := s.allTestTasks()
	s.loadHistory(tasks)
	s.orderTestTasksByHistory(tasks)
	if *verbose {
		log.Printf("Running %d tests in %d packages with up to %d concurrent processes...", len(tasks), len(bins), *jobs)
	}
	type testRunResult struct {
		err            error
		primaryFailure bool
	}
	results := make(chan testRunResult, len(tasks))
	work := make(chan testTask)
	workers := min(*jobs, len(tasks))
	var workerWG sync.WaitGroup
	workerWG.Add(workers)
	for range workers {
		go func() {
			defer workerWG.Done()
			for {
				select {
				case <-s.ctx.Done():
					return
				case task, ok := <-work:
					if !ok {
						return
					}
					err := s.runTest(task)
					primary := false
					var ee *exec.ExitError
					if *failFast && errors.As(err, &ee) {
						primary = s.triggerFailFast()
					}
					results <- testRunResult{err: err, primaryFailure: primary}
				}
			}
		}()
	}
	go func() {
		defer close(work)
		for _, task := range tasks {
			select {
			case work <- task:
			case <-s.ctx.Done():
				return
			}
		}
	}()
	go func() {
		workerWG.Wait()
		close(results)
	}()
	failed := 0
	var infraErrs []error
	for result := range results {
		err := result.err
		if err != nil {
			if *failFast && s.failFastWasTriggered() && !result.primaryFailure {
				continue
			}
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				failed++
			} else {
				infraErrs = append(infraErrs, err)
			}
		}
	}
	if err := errors.Join(infraErrs...); err != nil {
		return fmt.Errorf("running tests: %w", err)
	}
	if failed > 0 {
		return fmt.Errorf("%d test(s) failed", failed)
	}
	return nil
}

func (s *Server) allTestTasks() []testTask {
	bins := s.capturedTestBinaries()
	var tasks []testTask
	for _, bin := range bins {
		s.mu.Lock()
		ps := s.pkgs[bin.pkg]
		tests := make([]string, 0, len(ps.tests))
		for test := range ps.tests {
			if isRunnableTopLevelTest(test) {
				tests = append(tests, test)
			}
		}
		s.mu.Unlock()
		sort.Strings(tests)
		for _, test := range tests {
			tasks = append(tasks, testTask{bin: bin, test: test})
		}
		if len(tests) == 0 {
			s.mu.Lock()
			ps.pkgState = pkgStateDone
			ps.changed = time.Now()
			s.mu.Unlock()
		}
	}
	return tasks
}

func (s *Server) triggerFailFast() bool {
	s.mu.Lock()
	if s.failFastTriggered {
		s.mu.Unlock()
		return false
	}
	s.failFastTriggered = true
	s.mu.Unlock()
	s.cancel()
	return true
}

func (s *Server) failFastWasTriggered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failFastTriggered
}

type testTask struct {
	bin  capturedTestBinary
	test string
}

func (s *Server) packageRoot(pkg string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pkgs[pkg].glp.Root
}

func isRunnableTopLevelTest(name string) bool {
	return strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Fuzz") || strings.HasPrefix(name, "Example")
}

func (s *Server) cacheKeyForTask(task testTask) testCacheKey {
	return testCacheKey{
		BinarySHA256: binHash(task.bin.absBin),
		Package:      task.bin.pkg,
		Test:         task.test,
		WorkingDir:   task.bin.workDir,
		Args:         effectiveTestArgs(task, s.profile),
	}
}

func effectiveTestArgs(task testTask, profile runProfile) []string {
	args := profileTestArgs(task.bin.args, profile)
	// Gotst owns repetition and retries. Each child process is exactly one
	// attempt even if captured/profile arguments contained another count.
	args = setTestArg(args, "-test.count", "1")
	if *failFast {
		args = setTestArg(args, "-test.failfast", "true")
	}
	if *jsonSummary {
		args = setTestArg(args, "-test.v", "true")
	}
	return args
}

func (s *Server) runTest(task testTask) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	bin := task.bin
	baseArgs := effectiveTestArgs(task, s.profile)
	key := s.cacheKeyForTask(task)
	debugPass := s.currentDebugPass()
	if s.testCache != nil && debugPass != 1 {
		s.mu.Lock()
		s.cacheChecks++
		s.mu.Unlock()
		entry, err := s.testCache.Get(s.ctx, key)
		if err == nil {
			valid, validateErr := validateDependencies(entry.Dependencies)
			if validateErr == nil && valid {
				s.mu.Lock()
				s.cacheHits++
				s.mu.Unlock()
				s.finishTest(task, true, entry.PassedIn, nil, nil, true)
				s.printTestResult(task, entry.PassedIn, true, true, "")
				return nil
			}
		} else if !errors.Is(err, errTestCacheMiss) && *verbose {
			log.Printf("cache lookup for %s/%s: %v", bin.pkg, task.test, err)
		}
	}

	if !s.awaitExecSem(s.ctx) {
		return s.ctx.Err()
	}
	defer s.releaseExecSem()

	s.mu.Lock()
	ps := s.pkgs[bin.pkg]
	ps.pkgState = pkgStateTesting
	now := time.Now()
	ps.changed = now
	ps.tests[task.test].running = true
	ps.tests[task.test].changed = now
	s.mu.Unlock()

	var total time.Duration
	var failures []failInfo
	var cacheDeps []cacheDependency
	historyDeps := make([]cacheDependency, 0)
	historyDepsComplete := true
	totalAttempts := 0
	for repetition := range *testCount {
		for retry := 0; ; retry++ {
			d, output, attrs, deps, err := s.runTestAttempt(task, key, baseArgs, repetition, retry)
			total += d
			totalAttempts++
			if deps == nil {
				historyDepsComplete = false
			} else {
				historyDeps = append(historyDeps, deps...)
			}
			s.recordTestAttrs(task, attrs)
			s.recordTestAttempt(task)
			if err == nil {
				if len(failures) == 0 && *testCount == 1 {
					cacheDeps = deps
				}
				break
			}
			if errors.Is(err, context.Canceled) {
				s.stopRunningTest(task)
				return err
			}
			failures = append(failures, failInfo{dur: d, out: output})
			if retry == *maxRetries {
				s.finishTest(task, false, total, failures, err, false)
				s.recordHistory(task, history.OutcomeFail, total, totalAttempts, completeHistoryDeps(historyDeps, historyDepsComplete))
				s.printTestResult(task, d, false, false, output)
				return err
			}
			if *verbose {
				log.Printf("retrying %s/%s after attempt %d failed", bin.pkg, task.test, retry+1)
			}
		}
	}

	if len(failures) == 0 && s.testCache != nil && cacheDeps != nil {
		entry := &testCacheEntry{
			Version: testCacheVersion, Key: key, Created: time.Now().UTC(),
			PassedIn: total, Dependencies: cacheDeps,
		}
		if putErr := s.testCache.Put(s.ctx, entry); putErr != nil {
			if debugPass == 1 {
				s.setDebugSeedError(key, "writing cache entry: "+putErr.Error())
			}
			if *verbose {
				log.Printf("caching %s/%s: %v", bin.pkg, task.test, putErr)
			}
		}
	} else if debugPass == 1 {
		reason := "test result was not cacheable"
		if len(failures) != 0 {
			reason = fmt.Sprintf("test passed only after %d failed attempt(s)", len(failures))
		} else if cacheDeps == nil {
			reason = "test input log could not be captured"
		}
		s.setDebugSeedError(key, reason)
	}
	s.finishTest(task, true, total, failures, nil, false)
	outcome := history.OutcomePass
	if len(failures) != 0 {
		outcome = history.OutcomeFlaky
	}
	s.recordHistory(task, outcome, total, totalAttempts, completeHistoryDeps(historyDeps, historyDepsComplete))
	s.printTestResult(task, total, false, true, "")
	return nil
}

func completeHistoryDeps(deps []cacheDependency, complete bool) []cacheDependency {
	if !complete {
		return nil
	}
	return deps
}

func (s *Server) runTestAttempt(task testTask, key testCacheKey, baseArgs []string, repetition, retry int) (time.Duration, string, map[string]string, []cacheDependency, error) {
	var out cappedBuffer
	out.max = *maxOutput
	var attrOut cappedBuffer
	attrOut.max = 1 << 20
	logPath := filepath.Join(s.cacheDir, fmt.Sprintf("testlog-%s-%d-%d", key.id(), repetition, retry))
	args := setTestArg(baseArgs, "-test.run", "^"+regexp.QuoteMeta(task.test)+"$")
	captureDeps := s.testCache != nil || s.history != nil
	if captureDeps {
		args = setTestArg(args, "-test.testlogfile", logPath)
	}
	t0 := time.Now()
	cmd := exec.CommandContext(s.ctx, task.bin.absBin, args...)
	cmd.Dir = task.bin.workDir
	var outputWriter io.Writer = &out
	if *jsonSummary {
		outputWriter = io.MultiWriter(&out, &attrOut)
	}
	cmd.Stdout = outputWriter
	cmd.Stderr = outputWriter
	err := cmd.Run()
	d := time.Since(t0).Round(time.Millisecond)
	attrs := testAttrs(attrOut.String())
	if s.ctx.Err() != nil {
		if captureDeps {
			os.Remove(logPath)
		}
		return d, out.String(), attrs, nil, context.Canceled
	}
	var deps []cacheDependency
	if captureDeps {
		var depErr error
		deps, depErr = parseTestLog(logPath, task.bin.workDir, s.packageRoot(task.bin.pkg))
		if depErr != nil {
			deps = nil
			if err == nil && s.currentDebugPass() == 1 {
				s.setDebugSeedError(key, "parsing test input log: "+depErr.Error())
			}
			if *verbose {
				log.Printf("capturing test inputs for %s/%s: %v", task.bin.pkg, task.test, depErr)
			}
		}
	}
	if captureDeps {
		os.Remove(logPath)
	}
	return d, out.String(), attrs, deps, err
}

func (s *Server) currentDebugPass() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.debugPass
}

func (s *Server) setDebugSeedError(key testCacheKey, reason string) {
	s.mu.Lock()
	if s.debugSeedErrors[key.id()] == "" {
		s.debugSeedErrors[key.id()] = reason
	}
	s.mu.Unlock()
}

func (s *Server) reportDebugCacheMiss(task testTask, key testCacheKey, reason string, details []string) {
	s.mu.Lock()
	s.debugMisses++
	seedError := s.debugSeedErrors[key.id()]
	s.mu.Unlock()
	if seedError != "" {
		reason += "; seed pass: " + seedError
	}
	log.Printf("debug-uncached: MISS %s/%s: %s", task.bin.pkg, task.test, reason)
	for _, detail := range details {
		log.Printf("debug-uncached:   %s", detail)
	}
}

func (s *Server) recordTestAttempt(task testTask) {
	s.mu.Lock()
	s.pkgs[task.bin.pkg].tests[task.test].attempts++
	s.mu.Unlock()
}

func testAttrs(output string) map[string]string {
	var ret map[string]string
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimPrefix(line, "\x16") // -test.v=test2json framing marker
		line, ok := strings.CutPrefix(line, "=== ATTR  ")
		if !ok {
			continue
		}
		_, rest, ok := strings.Cut(line, " ") // test name
		if !ok {
			continue
		}
		key, value, ok := strings.Cut(rest, " ")
		if !ok || key == "" {
			continue
		}
		if ret == nil {
			ret = make(map[string]string)
		}
		ret[key] = value
	}
	return ret
}

func (s *Server) recordTestAttrs(task testTask, attrs map[string]string) {
	if len(attrs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.pkgs[task.bin.pkg].tests[task.test]
	if ts.attrs == nil {
		ts.attrs = make(map[string]string)
	}
	for key, value := range attrs {
		ts.attrs[key] = value
	}
}

func (s *Server) stopRunningTest(task testTask) {
	s.mu.Lock()
	ps := s.pkgs[task.bin.pkg]
	ps.tests[task.test].running = false
	ps.tests[task.test].changed = time.Now()
	ps.changed = time.Now()
	s.mu.Unlock()
}

func (s *Server) finishTest(task testTask, passed bool, d time.Duration, failures []failInfo, err error, cached bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.pkgs[task.bin.pkg]
	ts := ps.tests[task.test]
	now := time.Now()
	ts.running = false
	ts.done = true
	ts.passed = passed
	ts.cached = cached
	ts.fails = append(ts.fails, failures...)
	ts.changed = now
	ps.changed = now
	if passed {
		ts.passedIn = d
	} else {
		ps.numFails++
		if err != nil {
			ps.runErr = err.Error()
		}
	}
	ps.runIn += d
	allDone := true
	for name, status := range ps.tests {
		if isRunnableTopLevelTest(name) && !status.done {
			allDone = false
			break
		}
	}
	if allDone {
		ps.pkgState = pkgStateDone
		ps.changed = now
	}
}

func (s *Server) printTestResult(task testTask, d time.Duration, cached, passed bool, output string) {
	if passed && !*verbose {
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if !passed {
		fmt.Fprintf(os.Stdout, "FAILED: %s.%s\n", task.bin.pkg, task.test)
		fmt.Fprintf(os.Stdout, "%s", output)
		fmt.Fprintf(os.Stdout, "FAIL\t%s\t%s\t%s\n", task.bin.pkg, task.test, d)
	} else if cached {
		fmt.Fprintf(os.Stdout, "ok  \t%s\t%s\t(cached)\n", task.bin.pkg, task.test)
	} else {
		fmt.Fprintf(os.Stdout, "ok  \t%s\t%s\t%s\n", task.bin.pkg, task.test, d)
	}
}

func binHash(absBin string) string { return filepath.Base(absBin) }

// directTestArgs converts arguments emitted by "go test -json" into arguments
// suitable for running the captured test binary directly. The Go command's
// private test2json verbosity mode emits framing bytes intended for cmd/test2json,
// not terminals or gotst's package-level output collector.
func directTestArgs(args []string) []string {
	ret := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "-test.v=test2json" {
			continue
		}
		ret = append(ret, arg)
	}
	return ret
}

func profileTestArgs(args []string, profile runProfile) []string {
	ret := directTestArgs(args)
	if profile.shortSet {
		ret = setTestArg(ret, "-test.short", fmt.Sprint(profile.Short))
	}
	if profile.Timeout != 0 {
		ret = setTestArg(ret, "-test.timeout", profile.Timeout.String())
	}
	for _, arg := range profile.TestFlags {
		ret = append(ret, normalizeTestFlag(arg))
	}
	return ret
}

func normalizeTestFlag(arg string) string {
	switch arg {
	case "-short", "-testing.short", "-test.short":
		return "-test.short=true"
	}
	if rest, ok := strings.CutPrefix(arg, "-testing."); ok {
		return "-test." + rest
	}
	return arg
}

func setTestArg(args []string, name, value string) []string {
	prefix := name + "="
	ret := make([]string, 0, len(args)+1)
	for _, arg := range args {
		if arg != name && !strings.HasPrefix(arg, prefix) {
			ret = append(ret, arg)
		}
	}
	return append(ret, prefix+value)
}

func splitCommaList(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' })
}

// ExecSnarf is metadata about a test binary as seen when we're running
// as a wrapper in "go test -exec" mode.
type ExecSnarf struct {
	WorkingDir string
	ExeHash    string   // hex sha256 of os.Args[1] executable
	Args       []string // go.test args after the binary name
}

// storeTestExecBinary runs when our binary is in child process mode, under "go
// test -exec", and copies (or hardlinks) the test binary it's wrapping into a
// content-addressable directory, as given by dir (the gotst parent's
// [Server.cacheDir]). It then prints a line to stdout beginning with
// "ExecSnarf:" followed by a JSON blob of [ExecSnarf] metadata about the
// captured binary, which the parent process can parse out of the test output.
func storeTestExecBinary(dir string) {
	fi, err := os.Stat(dir)
	if err != nil {
		log.Fatalf("invalid capture directory: %v", err)
	}
	if !fi.IsDir() {
		log.Fatalf("capture path %q is not a directory", dir)
	}
	if len(os.Args) < 2 {
		log.Fatal("missing argument(s); need path to test binary and its args")
	}
	pwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("getting working directory: %v", err)
	}
	binPath := os.Args[1]
	socket := os.Getenv(cacheShimSocketEnv)
	exeHash, verifiedSize, verified := lookupVerifiedTestExecutable(socket, binPath)
	var f *os.File
	if !verified {
		f, err = os.Open(binPath)
		if err != nil {
			log.Fatalf("opening test binary: %v", err)
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			log.Fatalf("hashing test binary: %v", err)
		}
		exeHash = fmt.Sprintf("%x", h.Sum(nil))
	}

	co := &ExecSnarf{
		WorkingDir: pwd,
		ExeHash:    exeHash,
		Args:       os.Args[2:],
	}
	coj, err := json.Marshal(co)
	if err != nil {
		log.Fatalf("marshaling capture metadata: %v", err)
	}

	target := filepath.Join(dir, co.ExeHash)
	if err := os.Link(binPath, target); err != nil {
		// Hardlinked failed. Maybe we're on Windows, or maybe we're going
		// across filesystems. Just copy instead.
		of, err := os.CreateTemp(dir, co.ExeHash+"*")
		if err != nil {
			log.Fatalf("creating temp file in capture dir: %v", err)
		}
		if f == nil {
			f, err = os.Open(binPath)
			if err != nil {
				log.Fatalf("opening test binary for copy: %v", err)
			}
			defer f.Close()
		}
		if _, err := f.Seek(0, 0); err != nil {
			log.Fatalf("seeking to beginning of test binary: %v", err)
		}
		if _, err := io.Copy(of, f); err != nil {
			log.Fatalf("copying test binary to capture dir: %v", err)
		}
		if err := of.Close(); err != nil {
			log.Fatalf("closing copied test binary: %v", err)
		}
		if err := os.Chmod(of.Name(), 0700); err != nil {
			log.Fatalf("chmod +x copied test binary: %v", err)
		}
		if err := os.Rename(of.Name(), target); err != nil {
			log.Fatalf("renaming copied test binary to final name: %v", err)
		}
	}
	if socket != "" && !verified {
		fi, err := os.Stat(target)
		if err != nil {
			log.Fatalf("statting captured test binary: %v", err)
		}
		if err := registerTestExecutable(socket, target, co.ExeHash, fi.Size()); err != nil {
			log.Printf("not caching test executable: %v", err)
		}
	}
	if verified {
		if fi, err := os.Stat(target); err != nil || fi.Size() != verifiedSize {
			log.Fatalf("captured verified executable changed size: got %v, %v; want %d bytes", fi, err, verifiedSize)
		}
	}
	fmt.Printf("ExecSnarf:%s\n", coj)
}
