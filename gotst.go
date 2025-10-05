// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// The gotst command is an alternative to (and wrapper of) "go test"
// to run Go tests for both humans and CI systems.
package main

import (
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

	mu   sync.Mutex
	pkgs map[string]*packageStatus // import path -> status
}

type packageStatus struct {
	glp *goListPackage

	// following fields guarded by [Server.mu]
	pkgState pkgState

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
	cmd := exec.Command(goCmd(), "list", "-json", testPattern)
	return processCmdOutput(cmd, func(r io.Reader) error {
		jd := json.NewDecoder(r)
		for {
			pkg := new(goListPackage)
			if err := jd.Decode(pkg); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return fmt.Errorf("decoding package: %w", err)
			}
			s.addPackage(pkg)
		}
	})
}
