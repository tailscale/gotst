// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

type runPhase string

const (
	phaseDiscovering runPhase = "discovering"
	phaseBuilding    runPhase = "building"
	phaseListing     runPhase = "listing"
	phaseTesting     runPhase = "testing"
	phaseDone        runPhase = "done"
	phaseFailed      runPhase = "failed"
)

type progressSnapshot struct {
	Phase     runPhase
	Elapsed   time.Duration
	BuildOnly bool

	PackagesDiscovered int
	PackagesTotal      int
	PackagesBuilt      int
	PackagesCached     int
	PackagesListed     int
	PackagesDone       int

	TestsTotal   int
	TestsDone    int
	TestsRunning int
	TestsFlaky   int

	CacheEnabled bool
	CacheChecks  int
	CacheHits    int
}

func (s *Server) setPhase(phase runPhase) {
	s.mu.Lock()
	s.phase = phase
	s.mu.Unlock()
}

func (s *Server) progressSnapshot() progressSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := progressSnapshot{
		Phase:              s.phase,
		Elapsed:            time.Since(s.start).Round(time.Second),
		BuildOnly:          *buildOnly,
		PackagesDiscovered: len(s.pkgs),
		PackagesTotal:      s.pkgsWithTests,
		TestsTotal:         s.testsTotal,
		CacheEnabled:       s.testCache != nil,
		CacheChecks:        s.cacheChecks,
		CacheHits:          s.cacheHits,
	}
	for _, ps := range s.pkgs {
		if !ps.glp.hasTests() {
			continue
		}
		if ps.exeHash != "" {
			p.PackagesBuilt++
			if ps.exeCached {
				p.PackagesCached++
			}
		}
		if ps.tests != nil {
			p.PackagesListed++
		}
		if ps.pkgState == pkgStateDone {
			p.PackagesDone++
		}
		for name, ts := range ps.tests {
			if !isRunnableTopLevelTest(name) {
				continue
			}
			if ts.done {
				p.TestsDone++
			}
			if ts.running {
				p.TestsRunning++
			}
			if ts.passed && len(ts.fails) > 0 {
				p.TestsFlaky++
			}
		}
	}
	return p
}

func (p progressSnapshot) line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "gotst: %s: ", p.Phase)
	switch p.Phase {
	case phaseDiscovering:
		fmt.Fprintf(&b, "%d pkgs matched; %d with tests", p.PackagesDiscovered, p.PackagesTotal)
	case phaseBuilding:
		p.writeBuildProgress(&b)
	case phaseListing:
		fmt.Fprintf(&b, "%d pkgs matched, %d with tests; %d/%d test pkgs listed; %d tests found", p.PackagesDiscovered, p.PackagesTotal, p.PackagesListed, p.PackagesTotal, p.TestsTotal)
	case phaseDone, phaseFailed:
		if p.BuildOnly {
			p.writeBuildProgress(&b)
			break
		}
		fallthrough
	default:
		fmt.Fprintf(&b, "%d/%d test pkgs, %d/%d tests; %d running", p.PackagesDone, p.PackagesTotal, p.TestsDone, p.TestsTotal, p.TestsRunning)
	}
	if (p.Phase == phaseTesting || p.Phase == phaseDone || p.Phase == phaseFailed) && !p.BuildOnly {
		if p.TestsFlaky > 0 {
			fmt.Fprintf(&b, "; %d flaky", p.TestsFlaky)
		}
		if !p.CacheEnabled {
			b.WriteString("; cache off")
		} else if p.CacheChecks == 0 {
			b.WriteString("; cache hits 0/0")
		} else {
			pct := 100 * float64(p.CacheHits) / float64(p.CacheChecks)
			fmt.Fprintf(&b, "; cache hits %d/%d (%.1f%%)", p.CacheHits, p.CacheChecks, pct)
		}
	}
	fmt.Fprintf(&b, "; %s", p.Elapsed)
	return b.String()
}

func (p progressSnapshot) writeBuildProgress(b *strings.Builder) {
	fmt.Fprintf(b, "%d/%d test pkgs; %d/%d built", p.PackagesTotal, p.PackagesDiscovered, p.PackagesBuilt, p.PackagesTotal)
	if p.PackagesBuilt == 0 {
		fmt.Fprintf(b, " (%d cached)", p.PackagesCached)
		return
	}
	pct := 100 * float64(p.PackagesCached) / float64(p.PackagesBuilt)
	fmt.Fprintf(b, " (%d cached, %.1f%%)", p.PackagesCached, pct)
}

type flakyTestSummary struct {
	Package        string
	Test           string
	Attempts       int
	Failures       int
	Attrs          map[string]string `json:",omitempty"`
	FailedAttempts []failInfo        `json:"-"`
}

func (s *Server) flakyTests() []flakyTestSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ret []flakyTestSummary
	for pkg, ps := range s.pkgs {
		for test, ts := range ps.tests {
			if ts.passed && len(ts.fails) > 0 {
				ret = append(ret, flakyTestSummary{
					Package: pkg, Test: test, Attempts: ts.attempts, Failures: len(ts.fails),
					Attrs: ts.attrs, FailedAttempts: slices.Clone(ts.fails),
				})
			}
		}
	}
	slices.SortFunc(ret, func(a, b flakyTestSummary) int {
		if c := strings.Compare(a.Package, b.Package); c != 0 {
			return c
		}
		return strings.Compare(a.Test, b.Test)
	})
	return ret
}

func (s *Server) printFlakySummary() {
	flakes := s.flakyTests()
	if len(flakes) == 0 {
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if *jsonSummary {
		fmt.Fprintf(os.Stdout, "\nFLAKY TEST DIAGNOSTICS:\n")
		for _, f := range flakes {
			for i, failure := range f.FailedAttempts {
				fmt.Fprintf(os.Stdout, "\n[gotst: %s %s failed attempt %d/%d, %s]\n",
					f.Package, f.Test, i+1, f.Failures, failure.dur)
				fmt.Fprint(os.Stdout, failure.out)
				if !strings.HasSuffix(failure.out, "\n") {
					fmt.Fprintln(os.Stdout)
				}
			}
		}
	}
	fmt.Fprintf(os.Stdout, "\nFLAKY TESTS (%d):\n", len(flakes))
	for _, f := range flakes {
		fmt.Fprintf(os.Stdout, "  %s %s: passed after %d attempts (%d failed)\n", f.Package, f.Test, f.Attempts, f.Failures)
	}
	if *jsonSummary {
		j, _ := json.Marshal(flakes)
		fmt.Fprintf(os.Stdout, "gotst flaky tests JSON: %s\n", j)
	}
}

func (s *Server) startProgressReporter() func() {
	if *progressEvery == 0 {
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(*progressEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.printProgress()
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

func (s *Server) printProgress() {
	line := s.progressSnapshot().line()
	s.outMu.Lock()
	defer s.outMu.Unlock()
	fmt.Fprintln(os.Stdout, line)
}
