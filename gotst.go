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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	flagListen = flag.String("listen", "127.0.0.1:5525", "if non-empty, run HTTP server on this address and serve status")

	extraSleep = flag.Duration("extra-sleep", 0, "[dev] if non-zero, sleep this long before exiting after all tests complete, to give time to explore the web UI")

	tags    = flag.String("tags", "", "comma-separated list of build tags to pass to 'go test' when building and running tests")
	verbose = flag.Bool("vlog", false, "verbose gotst debug logging")
)

func main() {
	if dir := os.Getenv("GOTST_EXEC_DEST"); dir != "" {
		// We're running as a test binary under "go test -exec".
		// Just capture our output to the given directory and exit.
		storeTestExecBinary(dir)
		return
	}
	flag.Parse()
	log.SetPrefix("gotst: ")
	log.SetFlags(log.Flags() | log.Lmsgprefix)

	s := NewServer()
	if *extraSleep > 0 {
		log.Printf("# cacheDir is %v", s.cacheDir)
	}
	defer s.Cleanup()
	if *flagListen != "" {
		ln, err := net.Listen("tcp", *flagListen)
		if err != nil {
			log.Fatalf("listening on %q: %v", *flagListen, err)
		}
		go http.Serve(ln, s)
	}
	var testPattern string
	switch flag.NArg() {
	case 0:
		testPattern = "./..."
	case 1:
		testPattern = flag.Arg(0)
	default:
		log.Fatalf("usage: gotst [pattern]")
	}
	if err := s.Run(testPattern); err != nil {
		log.Fatal(err)
	}
}

type Server struct {
	start          time.Time
	cacheDir       string
	listTestCtx    context.Context
	listTestCancel context.CancelFunc
	testsListed    chan struct{} // sent whenever a -test.list completes

	execSem chan bool // buffered chan semaphore to limit subprocesses

	mu            sync.Mutex
	pkgs          map[string]*packageStatus // test package import path -> status
	pkgsWithTests int
}

type packageStatus struct {
	glp *goListPackage

	// following fields guarded by [Server.mu]
	pkgState pkgState
	exeHash  string // once known, the sha256 hex of test binary in Server.cacheDir
	tests    map[string]*testStatus
	numFails int // number of tests in tests that failed
}

type testStatus struct {
	running  bool
	done     bool // if true, then test either passed or reach max failures
	passed   bool
	passedIn time.Duration // valid if passed is true
	fails    []failInfo
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
	Dir         string
	ImportPath  string
	Name        string
	TestGoFiles []string
	Root        string
}

func NewServer() *Server {
	listTestCtx, listTestCancel := context.WithCancel(context.Background())
	return &Server{
		start:          time.Now(),
		cacheDir:       mustNewCacheDir(),
		execSem:        make(chan bool, 2*runtime.NumCPU()),
		listTestCtx:    listTestCtx,
		listTestCancel: listTestCancel,
	}
}

func (s *Server) Run(testPattern string) error {
	if err := s.learnPackagesWithTests(testPattern); err != nil {
		return fmt.Errorf("learnPackagesWithTests: %w", err)
	}

	s.mu.Lock()
	s.testsListed = make(chan struct{}, s.pkgsWithTests)
	s.mu.Unlock()

	if err := s.buildAllTestBinaries(); err != nil {
		return fmt.Errorf("buildAllTestBinaries: %w", err)
	}

	// Wait to learn the names of all the tests in all the binaries.
	// TODO(bradfitz): we don't strictly need to wait for this, but it's convenient
	// for now (2025-10-05) during development. Later we should start running
	// tests earlier.
	if err := s.awaitTestList(); err != nil {
		return fmt.Errorf("awaitTestList: %w", err)
	}

	if *extraSleep > 0 {
		log.Printf("sleeping extra %v before exiting; cacheDir is %v", *extraSleep, s.cacheDir)
		time.Sleep(*extraSleep)
	}
	return nil
}

func (s *Server) Cleanup() {
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
		glp: glp,
	}
	s.pkgs[glp.ImportPath] = st
	if len(glp.TestGoFiles) > 0 {
		s.pkgsWithTests++
	}
}

func (s *Server) learnPackagesWithTests(testPattern string) error {
	log.Printf("Discovering packages with tests...")
	t0 := time.Now()

	cmd := exec.Command(goCmd(), "list", "--tags="+*tags, "--json", testPattern)
	return processCmdOutput(cmd, func(r io.Reader) error {
		jd := json.NewDecoder(r)
		for {
			pkg := new(goListPackage)
			if err := jd.Decode(pkg); err != nil {
				if errors.Is(err, io.EOF) {
					log.Printf("Discovered packages with tests in %v", time.Since(t0).Round(time.Millisecond))
					return nil
				}
				return fmt.Errorf("decoding package: %w", err)
			}
			s.addPackage(pkg)
		}
	})
}

func (s *Server) packagesWithTests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ret []string
	for imp, ps := range s.pkgs {
		if len(ps.glp.TestGoFiles) > 0 {
			ret = append(ret, imp)
		}
	}
	return ret
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
	log.Printf("Building test binaries...")
	t0 := time.Now()

	selfExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("getting self executable path: %w", err)
	}
	pkgs := s.packagesWithTests()
	args := []string{
		"test",
		"--trimpath",
		"--tags=" + *tags,
		"--json",
		"--exec=" + selfExe,
	}
	args = append(args, pkgs...)
	cmd := exec.Command(goCmd(), args...)
	cmd.Env = append(os.Environ(), "GOTST_EXEC_DEST="+s.cacheDir)
	return processCmdOutput(cmd, func(r io.Reader) error {

		var errs []error
		var testOut outputMap
		var buildOut outputMap
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

						// Stop processing further test-names-in-binaries
						// discovery, as we won't need to run any tests if
						// builds are failing.
						s.listTestCancel()
					}
				case "output":
					testOut.Add(ev.Package, ev.Output)
				case "pass":
					ej, ok := bytes.CutPrefix(testOut.Get(ev.Package), []byte("ExecSnarf:"))
					if !ok {
						errs = append(errs, fmt.Errorf("test package %q built but test wrapper did not emit ExecSnarf line", ev.Package))
						continue
					}
					ej, _, _ = bytes.Cut(ej, []byte{'\n'})
					var es ExecSnarf
					if err := json.Unmarshal(ej, &es); err != nil {
						errs = append(errs, fmt.Errorf("unmarshal ExecSnarf JSON from package %q: %w", ev.Package, err))
						continue
					}
					s.addTestBinary(ev.Package, es)
				}
			}
		}
		if err := bs.Err(); err != nil {
			return err
		}
		if err := errors.Join(errs...); err != nil {
			return err
		}
		log.Printf("Built %d test binaries in %v", len(pkgs), time.Since(t0).Round(time.Millisecond))
		return nil
	})
}

type outputMap map[string]*bytes.Buffer

func (m *outputMap) Add(pkg, out string) {
	if *m == nil {
		*m = make(map[string]*bytes.Buffer)
	}
	buf, ok := (*m)[pkg]
	if !ok {
		buf = new(bytes.Buffer)
		(*m)[pkg] = buf
	}
	buf.WriteString(out)
}

func (m *outputMap) Get(pkg string) []byte {
	buf, ok := (*m)[pkg]
	if !ok {
		return nil
	}
	return buf.Bytes()
}

func (s *Server) addTestBinary(pkg string, es ExecSnarf) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, ok := s.pkgs[pkg]
	if !ok {
		log.Printf("internal error: addTestBinary: unknown package %q", pkg)
		return
	}
	ps.pkgState = pkgStateBuilt
	ps.exeHash = es.ExeHash

	absBin := filepath.Join(s.cacheDir, es.ExeHash)
	fi, err := os.Stat(absBin)
	if err != nil {
		log.Fatalf("statting test binary for package %q: %v", pkg, err)
	}
	size := fi.Size()
	mB := float64(size) / (1 << 20)

	if *verbose {
		log.Printf("test binary for package %q is %0.1f MB, hash %s", pkg, mB, es.ExeHash)
	}

	if es.WorkingDir != ps.glp.Dir {
		log.Fatalf("unexpected pkg %q wd=%q vs golist=%q", pkg, es.WorkingDir, ps.glp.Dir)
	}
	go s.listTestsInBnary(pkg, absBin)
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

func (s *Server) listTestsInBnary(pkg, absBin string) {
	if !s.awaitExecSem(s.listTestCtx) {
		return
	}
	defer s.releaseExecSem()

	t0 := time.Now()
	out, err := exec.Command(absBin, "-test.list=.").CombinedOutput()
	if err != nil {
		log.Fatalf("error listing tests in %q: %v\noutput:\n%s", pkg, err, out)
	}
	d := time.Since(t0).Round(time.Millisecond)
	if *verbose {
		log.Printf("Listed tests in package %q in %v (%s)", pkg, d, absBin)
	}
	tests := strings.Fields(string(out))
	s.setPackageTests(pkg, tests)
}

func (s *Server) setPackageTests(pkg string, tests []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ps, ok := s.pkgs[pkg]
	if !ok {
		log.Fatalf("internal error: setPackageTests: unknown package %q", pkg)
	}
	ps.tests = make(map[string]*testStatus)
	for _, test := range tests {
		ps.tests[test] = &testStatus{}
	}
	select {
	case s.testsListed <- struct{}{}:
	default:
		panic("unexpected testsListed channel full")
	}
}

func (s *Server) awaitTestList() error {
	t0 := time.Now()

	s.mu.Lock()
	ch := s.testsListed
	n := s.pkgsWithTests
	s.mu.Unlock()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	remain := n

	for {
		select {
		case <-ch:
			remain--
			if remain == 0 {
				d := time.Since(t0).Round(time.Millisecond)
				log.Printf("Listed tests in %d packages in %v", n, d)
				return nil
			}
		case <-ticker.C:
			log.Printf("Waiting for test listing; did %d/%d ...", n-remain, n)
		}
	}
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
	f, err := os.Open(os.Args[1])
	if err != nil {
		log.Fatalf("opening test binary: %v", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		log.Fatalf("hashing test binary: %v", err)
	}

	co := &ExecSnarf{
		WorkingDir: pwd,
		ExeHash:    fmt.Sprintf("%x", h.Sum(nil)),
		Args:       os.Args[2:],
	}
	coj, err := json.Marshal(co)
	if err != nil {
		log.Fatalf("marshaling capture metadata: %v", err)
	}

	target := filepath.Join(dir, co.ExeHash)
	if err := os.Link(os.Args[1], target); err != nil {
		// Hardlinked failed. Maybe we're on Windows, or maybe we're going
		// across filesystems. Just copy instead.
		of, err := os.CreateTemp(dir, co.ExeHash+"*")
		if err != nil {
			log.Fatalf("creating temp file in capture dir: %v", err)
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
	fmt.Printf("ExecSnarf:%s\n", coj)
}
