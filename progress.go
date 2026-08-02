// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"os"
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
	Phase   runPhase
	Elapsed time.Duration

	PackagesDiscovered int
	PackagesTotal      int
	PackagesBuilt      int
	PackagesListed     int
	PackagesDone       int

	TestsTotal   int
	TestsDone    int
	TestsRunning int

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
		fmt.Fprintf(&b, "%d pkgs matched, %d with tests; %d/%d test pkgs compiled", p.PackagesDiscovered, p.PackagesTotal, p.PackagesBuilt, p.PackagesTotal)
	case phaseListing:
		fmt.Fprintf(&b, "%d pkgs matched, %d with tests; %d/%d test pkgs listed; %d tests found", p.PackagesDiscovered, p.PackagesTotal, p.PackagesListed, p.PackagesTotal, p.TestsTotal)
	default:
		fmt.Fprintf(&b, "%d/%d test pkgs, %d/%d tests; %d running", p.PackagesDone, p.PackagesTotal, p.TestsDone, p.TestsTotal, p.TestsRunning)
	}
	if p.Phase == phaseTesting || p.Phase == phaseDone || p.Phase == phaseFailed {
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
