// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"tailscale.com/ipn/ipnstate"

	_ "embed"
)

//go:embed root.tmpl.html
var rootTemplateHTML string

var rootTmpl = template.Must(template.New("root").Parse(rootTemplateHTML))

type tailscaleStatusClient interface {
	StatusWithoutPeers(context.Context) (*ipnstate.Status, error)
}

// startStatusListeners starts the configured status listener and, when a
// local Tailscale daemon is running, listeners on each of its assigned IPs.
// Failure to detect Tailscale is deliberately non-fatal.
func startStatusListeners(ctx context.Context, addr string, handler http.Handler, lc tailscaleStatusClient) ([]net.Listener, string, error) {
	primary, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", err
	}
	listeners := []net.Listener{primary}
	for _, ln := range listeners {
		go serveStatus(ln, handler)
	}

	statusCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	st, err := lc.StatusWithoutPeers(statusCtx)
	if err != nil || st == nil || st.BackendState != "Running" || st.Self == nil {
		return listeners, "", nil
	}
	tcpAddr, ok := primary.Addr().(*net.TCPAddr)
	if !ok {
		return listeners, "", nil
	}
	port := tcpAddr.Port
	bound := false
	seen := make(map[netip.Addr]bool)
	for _, ip := range st.TailscaleIPs {
		ip = ip.Unmap()
		if !ip.IsValid() || seen[ip] {
			continue
		}
		seen[ip] = true
		if listenerCoversIP(tcpAddr, ip) {
			bound = true
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		if err != nil {
			log.Printf("listening on Tailscale address %v: %v", ip, err)
			continue
		}
		listeners = append(listeners, ln)
		bound = true
		go serveStatus(ln, handler)
	}
	if !bound {
		return listeners, "", nil
	}
	dnsName := strings.TrimSuffix(st.Self.DNSName, ".")
	if dnsName == "" {
		return listeners, "", nil
	}
	return listeners, "http://" + net.JoinHostPort(dnsName, strconv.Itoa(port)), nil
}

func listenerCoversIP(addr *net.TCPAddr, ip netip.Addr) bool {
	if addr.IP == nil {
		return false
	}
	bound, ok := netip.AddrFromSlice(addr.IP)
	if !ok {
		return false
	}
	bound, ip = bound.Unmap(), ip.Unmap()
	if bound.IsUnspecified() {
		return bound.Is4() == ip.Is4()
	}
	return bound == ip
}

func serveStatus(ln net.Listener, handler http.Handler) {
	if err := http.Serve(ln, handler); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("status server: %v", err)
	}
}

func closeListeners(listeners []net.Listener) {
	for _, ln := range listeners {
		_ = ln.Close()
	}
}

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
