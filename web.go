// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"html/template"
	"log"
	"maps"
	"net/http"
	"slices"
	"time"

	_ "embed"
)

//go:embed root.tmpl.html
var rootTemplateHTML string

var rootTmpl = template.Must(template.New("root").Parse(rootTemplateHTML))

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := rootTmpl.Execute(w, s.statusData()); err != nil {
		log.Printf("executing template: %v", err)
		http.Error(w, "executing template: "+err.Error(), http.StatusInternalServerError)
	}
}

// statusData is the data argument type for [rootTmpl].
type statusData struct {
	StartedAt  string
	StartedAgo string

	Packages []packageData
}

// packageData is the html/template frozen version of a [packageStatus].
type packageData struct {
	ImportPath    string // import path
	HasTests      bool
	NumTestsKnown bool   // whether test binary has been listed and tests enumerated
	NumTests      int    // number of tests
	Status        string // TODO
	Passed        bool   // whether all tests passed
	Failed        bool   // whether any tests failed
}

func (s *Server) statusData() *statusData {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	d := &statusData{
		StartedAt:  s.start.Format(time.RFC3339),
		StartedAgo: now.Sub(s.start).Round(time.Second).String(),
	}

	for _, importPath := range slices.Sorted(maps.Keys(s.pkgs)) {
		ps := s.pkgs[importPath]
		pd := packageData{
			ImportPath: importPath,
			HasTests:   ps.glp.hasTests(),
		}
		switch ps.pkgState {
		case pkgStateBuilt:
			pd.Status = "built, " + ps.exeHash[:min(len(ps.exeHash), 8)]
			if ps.tests != nil {
				pd.Status += fmt.Sprintf(", %d tests", len(ps.tests))
			}
		case pkgStateTesting:
			pd.Status = "testing"
		case pkgStateDone:
			if ps.numFails > 0 {
				pd.Status = fmt.Sprintf("FAILED in %v", ps.runIn)
				pd.Failed = true
			} else {
				pd.Status = fmt.Sprintf("PASSED %d tests in %v", len(ps.tests), ps.runIn)
				pd.Passed = true
			}
		}
		if ps.tests != nil {
			pd.NumTestsKnown = true
			pd.NumTests = len(ps.tests)
		}
		d.Packages = append(d.Packages, pd)
	}

	return d
}
