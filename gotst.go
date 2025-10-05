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
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

func NewServer() *Server {
	ucd, err := os.UserCacheDir()
	if err != nil {
		log.Fatalf("getting user cache dir: %v", err)
	}
	gotstDir := filepath.Join(ucd, "gotst")
	if err := os.MkdirAll(gotstDir, 0700); err != nil {
		log.Fatalf("creating cache dir %q: %v", gotstDir, err)
	}
	cleanOldCaches(gotstDir)

	start := time.Now()
	pid := os.Getpid()

	cacheDir := filepath.Join(gotstDir, fmt.Sprintf("pid%d-t%d", pid, start.UnixNano()))
	if err := os.Mkdir(cacheDir, 0700); err != nil {
		log.Fatalf("creating per-run cache dir %q: %v", cacheDir, err)
	}
	return &Server{
		start:    start,
		cacheDir: cacheDir,
	}
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

func cleanOldCaches(d string) {
	ents, err := os.ReadDir(d)
	if err != nil {
		log.Fatalf("error reinad cache dir %q to clean it: %v", d, err)
		return
	}
	dirRx := regexp.MustCompile(`^pid(\d+)-t(\d+)$`)
	for _, ent := range ents {
		name := ent.Name()
		if !ent.IsDir() {
			continue
		}
		m := dirRx.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		timestamp, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			continue
		}
		if pidStillrunning(pid) {
			continue
		}
		age := time.Since(time.Unix(0, timestamp)).Round(time.Second)
		if age > 3*time.Minute {
			log.Printf("removing old cache dir %q (pid %d, age %v)", name, pid, age)
			os.RemoveAll(filepath.Join(d, name))
		}
	}
}

func pidStillrunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		// If we can FindProcess it on Windows, it's running.
		return true
	}
	// On Unix, we can send signal 0 to test if it's running.
	err = proc.Signal(os.Signal(syscall.Signal(0)))
	return err == nil
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
	if err := s.learnPackagesWithTests(testPattern); err != nil {
		return fmt.Errorf("learnPackagesWithTests: %w", err)
	}

	if *extraSleep > 0 {
		log.Printf("sleeping extra %v before exiting", *extraSleep)
		time.Sleep(*extraSleep)
	}
	return nil
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
