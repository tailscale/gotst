// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// The gotst command is an alternative to (and wrapper of) "go test"
// to run Go tests for both humans and CI systems.
package main

import (
	"bufio"
	"bytes"
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
	"sync"
	"time"
)

var (
	flagListen = flag.String("listen", "127.0.0.1:5525", "if non-empty, run HTTP server on this address and serve status")

	extraSleep = flag.Duration("extra-sleep", 0, "[dev] if non-zero, sleep this long before exiting after all tests complete, to give time to explore the web UI")

	tags = flag.String("tags", "", "comma-separated list of build tags to pass to 'go test' when building and running tests")
)

func main() {
	flag.Parse()
	log.SetPrefix("gotst: ")

	s := NewServer()
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
	start    time.Time
	cacheDir string

	mu       sync.Mutex
	pkgs     map[string]*packageStatus // test package import path -> status
	buildOut map[string]*bytes.Buffer  // [BuildEvent.ImportPath] (not TestEvent.Package!) -> build output
}

type packageStatus struct {
	glp *goListPackage

	// following fields guarded by [Server.mu]
	pkgState pkgState
	buildBuf bytes.Buffer // output from building the test binary, not running it

	tests map[string]*testStatus
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
	return &Server{
		start:    time.Now(),
		cacheDir: mustNewCacheDir(),
	}
}

func (s *Server) Run(testPattern string) error {
	if err := s.learnPackagesWithTests(testPattern); err != nil {
		return fmt.Errorf("learnPackagesWithTests: %w", err)
	}
	if err := s.buildAllTestBinaries(); err != nil {
		return fmt.Errorf("buildAllTestBinaries: %w", err)
	}

	if *extraSleep > 0 {
		log.Printf("sleeping extra %v before exiting", *extraSleep)
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
	_ = selfExe
	pkgs := s.packagesWithTests()
	args := []string{
		"test",
		"--tags=" + *tags,
		"--json",
		"--exec=true", // + selfExe,
	}
	args = append(args, pkgs...)
	cmd := exec.Command(goCmd(), args...)
	return processCmdOutput(cmd, func(r io.Reader) error {

		var errs []error

		bs := bufio.NewScanner(r)
		for bs.Scan() {
			ev, err := parseTestOrBuildEvent(bs.Bytes())
			if err != nil {
				return fmt.Errorf("parsing event: %w", err)
			}
			switch ev := ev.(type) {
			case *BuildEvent:
				if ev.Action == "build-output" {
					s.addBuildOutput(ev.ImportPath, ev.Output)
				}
			case *TestEvent:
				if ev.Action == "fail" {
					if ev.FailedBuild != "" {
						errs = append(errs, fmt.Errorf("failed to compile tests for %q; failure building %q:\n\n%s\n", ev.Package, ev.FailedBuild, s.buildOutput(ev.FailedBuild)))
					}
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

func (s *Server) addBuildOutput(buildPkg, out string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf, ok := s.buildOut[buildPkg]
	if !ok {
		buf = new(bytes.Buffer)
		if s.buildOut == nil {
			s.buildOut = make(map[string]*bytes.Buffer)
		}
		s.buildOut[buildPkg] = buf
	}
	buf.WriteString(out)
}

func (s *Server) buildOutput(pkg string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf, ok := s.buildOut[pkg]
	if !ok {
		return ""
	}
	return buf.String()
}
