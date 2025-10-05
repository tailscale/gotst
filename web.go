// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"html/template"
	"log"
	"net/http"
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

type statusData struct {
	StartedAt  string
	StartedAgo string

	Packages []packageStatus
}

type packageStatus struct {
	Path   string // import path
	Tests  int    // number of tests
	Status string // TODO
}

func (s *Server) statusData() *statusData {
	now := time.Now()
	d := &statusData{
		StartedAt:  now.Format(time.RFC3339),
		StartedAgo: time.Since(now).Round(time.Second).String(),
	}
	return d
}
