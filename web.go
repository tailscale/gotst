// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	"sync"
	"time"

	"github.com/coder/websocket"
	"tailscale.com/ipn/ipnstate"

	_ "embed"
)

//go:embed root.tmpl.html
var rootTemplateHTML string

//go:embed live.js
var liveJS []byte

var rootTmpl = template.Must(template.New("root").Parse(rootTemplateHTML))

const liveUpdateInterval = 500 * time.Millisecond

type htmlPatch struct {
	Seq    uint64 `json:"seq"`
	Prefix int    `json:"prefix"`
	Suffix int    `json:"suffix"`
	Insert string `json:"insert"`
	Final  bool   `json:"final,omitempty"`
}

type liveCommand struct {
	ctx   context.Context
	html  string
	final bool
	done  chan error
}

type liveClient struct {
	server *Server
	conn   *websocket.Conn
	cmd    chan liveCommand
	done   chan struct{}
	acks   chan uint64
}

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
	switch r.URL.Path {
	case "/":
		s.serveStatusRoot(w, r)
	case "/live.js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(liveJS)
	case "/live-ws":
		s.serveLiveWS(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) serveStatusRoot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	html, err := s.renderStatusHTML()
	if err != nil {
		log.Printf("executing template: %v", err)
		http.Error(w, "executing template: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(html))
}

func (s *Server) renderStatusHTML() (string, error) {
	var buf bytes.Buffer
	if err := rootTmpl.Execute(&buf, s.statusData()); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (s *Server) serveLiveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode:      websocket.CompressionContextTakeover,
		CompressionThreshold: 1,
	})
	if err != nil {
		log.Printf("live websocket: %v", err)
		return
	}
	c := &liveClient{
		server: s, conn: conn, cmd: make(chan liveCommand),
		done: make(chan struct{}), acks: make(chan uint64, 1),
	}
	s.webMu.Lock()
	if s.webLive == nil {
		s.webLive = make(map[*liveClient]struct{})
	}
	s.webLive[c] = struct{}{}
	s.webMu.Unlock()
	c.run(r.Context())
	s.webMu.Lock()
	delete(s.webLive, c)
	s.webMu.Unlock()
}

func (c *liveClient) run(requestCtx context.Context) {
	defer close(c.done)
	defer c.conn.CloseNow()
	ctx, cancel := context.WithCancel(requestCtx)
	defer cancel()
	go c.readAcks(ctx, cancel)

	var lastHTML string
	var seq uint64
	var lastPush time.Time
	ticker := time.NewTicker(liveUpdateInterval)
	defer ticker.Stop()
	push := func(ctx context.Context, html string, final bool) error {
		if html == lastHTML && !final {
			return nil
		}
		if wait := liveUpdateInterval - time.Since(lastPush); !lastPush.IsZero() && wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		seq++
		patch := makeHTMLPatch(lastHTML, html)
		patch.Seq, patch.Final = seq, final
		msg, err := json.Marshal(patch)
		if err != nil {
			return err
		}
		if err := c.conn.Write(ctx, websocket.MessageText, msg); err != nil {
			return err
		}
		lastPush = time.Now()
		lastHTML = html
		if !final {
			return nil
		}
		for {
			select {
			case ack := <-c.acks:
				if ack == seq {
					return nil
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if html, err := c.server.renderStatusHTML(); err == nil {
		if err := push(ctx, html, false); err != nil {
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			html, err := c.server.renderStatusHTML()
			if err != nil || push(ctx, html, false) != nil {
				return
			}
		case cmd := <-c.cmd:
			err := push(cmd.ctx, cmd.html, cmd.final)
			cmd.done <- err
			if cmd.final || err != nil {
				return
			}
		}
	}
}

func (c *liveClient) readAcks(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	for {
		_, msg, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		var ack struct {
			Ack uint64 `json:"ack"`
		}
		if json.Unmarshal(msg, &ack) == nil && ack.Ack != 0 {
			select {
			case c.acks <- ack.Ack:
			default:
			}
		}
	}
}

func makeHTMLPatch(oldHTML, newHTML string) htmlPatch {
	oldRunes, newRunes := []rune(oldHTML), []rune(newHTML)
	prefix := 0
	for prefix < len(oldRunes) && prefix < len(newRunes) && oldRunes[prefix] == newRunes[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldRunes)-prefix && suffix < len(newRunes)-prefix &&
		oldRunes[len(oldRunes)-1-suffix] == newRunes[len(newRunes)-1-suffix] {
		suffix++
	}
	return htmlPatch{
		Prefix: prefix,
		Suffix: suffix,
		Insert: string(newRunes[prefix : len(newRunes)-suffix]),
	}
}

func (s *Server) flushFinalLiveStatus() {
	s.webMu.Lock()
	clients := make([]*liveClient, 0, len(s.webLive))
	for c := range s.webLive {
		clients = append(clients, c)
	}
	s.webMu.Unlock()
	if len(clients) == 0 {
		return
	}
	html, err := s.renderStatusHTML()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := make(chan error, 1)
			cmd := liveCommand{ctx: ctx, html: html, final: true, done: done}
			select {
			case c.cmd <- cmd:
			case <-c.done:
				return
			case <-ctx.Done():
				return
			}
			select {
			case <-done:
			case <-c.done:
			case <-ctx.Done():
			}
		}()
	}
	wg.Wait()
}

// statusData is the data argument type for [rootTmpl].
type statusData struct {
	StartedAt  string
	StartedAgo string
	Phase      runPhase

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
		Phase:      s.phase,
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
