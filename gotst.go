// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// The gotst command is an alternative to (and wrapper of) "go test"
// to run Go tests for both humans and CI systems.
package main

import (
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
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	flagListen = flag.String("listen", "127.0.0.1:5525", "if non-empty, run HTTP server on this address and serve status")
)

func main() {
	flag.Parse()
	log.SetPrefix("gotst: ")

	s := NewServer()
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

func NewServer() *Server {
	return &Server{
		start: time.Now(),
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

func (s *Server) Run(testPattern string) error {
	cmd := exec.Command(goCmd(), "list", "-json", testPattern)
	err := processCmdOutput(cmd, func(r io.Reader) error {
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
	if err != nil {
		log.Fatalf("error listing packages: %v", err)
	}
	time.Sleep(30 * time.Second)
	return nil
}

type Server struct {
	start time.Time

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

func processCmdOutput(cmd *exec.Cmd, fn func(r io.Reader) error) (err error) {
	var errBuf bytes.Buffer
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer out.Close()
	cmd.Stderr = &errBuf
	defer func() {
		if werr := cmd.Wait(); err == nil && werr != nil {
			err = fmt.Errorf("%v: %s", werr, errBuf.String())
		}
	}()
	if err := cmd.Start(); err != nil {
		return err
	}
	return fn(out)
}

var goCmd = sync.OnceValue(func() string {
	v, err := findGo()
	if err != nil {
		log.Fatalf("error finding 'go' binary: %v\n", err)
	}
	return v
})

func findGo() (string, error) {
	var cands []string
	if mod, err := goModuleRoot(); err == nil {
		if strings.HasSuffix(mod, "/src") {
			cands = append(cands, filepath.Join(mod, "..", "bin", "go"))
		} else if strings.HasSuffix(mod, "/src/cmd") {
			cands = append(cands, filepath.Join(mod, "..", "..", "bin", "go"))
		} else {
			toolGo := filepath.Join(mod, "tool", "go")
			cands = append(cands, toolGo)
		}
	}
	cands = append(cands,
		filepath.Join(os.Getenv("HOME"), "sdk", "go", "bin", "go"),
		"/usr/local/go/bin/go",
		"/usr/local/bin/go",
		"/usr/bin/go",
	)
	for _, cand := range cands {
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	return "", fmt.Errorf("no go found in any of %q", cands)
}

func goModuleRoot() (string, error) {
	pwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(pwd, "go.mod")); err == nil {
			return pwd, nil
		}
		if pwd == "/" {
			break
		}
		pwd = filepath.Dir(pwd)
	}
	return "", errors.New("no go.mod found")
}
