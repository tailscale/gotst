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
	"unicode/utf8"

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

const webFailureOutputLimit = 500 << 10

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
	ExitStatus string
	ExitClass  string

	PackagesTotal     int
	PackagesLinked    int
	PackagesLinkFresh int
	PackagesLinkCache int
	PackagesDone      int
	PackagesTestCache int
	PackagesTestFresh int
	PackagesRemaining int
	LinksRemaining    int
	BinarySize        string
	TestsTotal        int
	TestsDone         int
	TestsPassed       int
	TestsCached       int
	TestCacheDisabled bool
	TestsFresh        int
	TestsFailed       int
	TestsFlaky        int
	TestsRunning      int
	TestsRemaining    int
	BuildDepsTotal    int
	BuildDepsFresh    int
	BuildDepsCached   int
	BuildDepsObserved int
	BuildDepsRemain   int

	Packages []packageData
	Issues   []testIssueData
}

// packageData is the html/template frozen version of a [packageStatus].
type packageData struct {
	ImportPath      string // import path
	NumTestsKnown   bool   // whether test binary has been listed and tests enumerated
	NumTests        int    // number of runnable top-level tests
	NumTestsDone    int
	Status          string
	Passed          bool // whether all tests passed
	Failed          bool // whether any tests failed
	LastChanged     string
	LastChangedUnix int64
}

type testIssueData struct {
	ID       string
	Name     string
	Status   string
	Flaky    bool
	Output   string
	Attempts int
}

func (s *Server) statusData() *statusData {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	d := &statusData{
		StartedAt:         s.start.Format(time.RFC3339),
		StartedAgo:        formatAge(now, s.start),
		Phase:             s.phase,
		PackagesTotal:     s.pkgsWithTests,
		TestsTotal:        s.testsTotal,
		TestCacheDisabled: testCountSet,
		BuildDepsTotal:    s.buildDepsTotal,
		BuildDepsFresh:    s.buildDepsFresh,
		BuildDepsCached:   s.buildDepsCached,
	}
	switch s.phase {
	case phaseDone:
		d.ExitStatus, d.ExitClass = "success", "passed"
	case phaseFailed:
		d.ExitStatus, d.ExitClass = "failure", "failed"
	default:
		d.ExitStatus = "not exited"
	}

	for _, importPath := range slices.Sorted(maps.Keys(s.pkgs)) {
		ps := s.pkgs[importPath]
		if !ps.glp.hasTests() {
			continue
		}
		pd := packageData{
			ImportPath:      importPath,
			LastChanged:     formatAge(now, ps.changed),
			LastChangedUnix: ps.changed.UnixNano(),
		}
		if ps.exeHash != "" {
			d.PackagesLinked++
			if ps.exeCached {
				d.PackagesLinkCache++
			} else {
				d.PackagesLinkFresh++
			}
		}
		switch ps.pkgState {
		case pkgStateDiscovered:
			pd.Status = "waiting to link"
		case pkgStateBuilt:
			pd.Status = "linked"
			if ps.tests != nil {
				pd.Status = "queued"
			}
		case pkgStateTesting:
			pd.Status = "testing"
		case pkgStateDone:
			d.PackagesDone++
			if ps.numFails > 0 {
				pd.Status = fmt.Sprintf("FAILED in %v", ps.runIn)
				pd.Failed = true
			} else {
				pd.Status = fmt.Sprintf("PASSED in %v", ps.runIn)
				pd.Passed = true
			}
		}
		if ps.tests != nil {
			pd.NumTestsKnown = true
			allCached := true
			hasRunnable := false
			for testName, ts := range ps.tests {
				if !isRunnableTopLevelTest(testName) {
					continue
				}
				hasRunnable = true
				pd.NumTests++
				if ts.running {
					d.TestsRunning++
				}
				if ts.done {
					pd.NumTestsDone++
					d.TestsDone++
					if ts.cached {
						d.TestsCached++
					} else {
						d.TestsFresh++
						allCached = false
					}
					if ts.passed {
						d.TestsPassed++
						if len(ts.fails) > 0 {
							d.TestsFlaky++
							d.Issues = append(d.Issues, newTestIssue(importPath, testName, ts, true, len(d.Issues)))
						}
					} else {
						d.TestsFailed++
						d.Issues = append(d.Issues, newTestIssue(importPath, testName, ts, false, len(d.Issues)))
					}
				} else {
					allCached = false
				}
			}
			if ps.pkgState == pkgStateDone && hasRunnable {
				if allCached {
					d.PackagesTestCache++
				} else {
					d.PackagesTestFresh++
				}
			}
		}
		if ps.pkgState == pkgStateTesting {
			pd.Status = fmt.Sprintf("testing; %d/%d", pd.NumTestsDone, pd.NumTests)
		}
		d.Packages = append(d.Packages, pd)
	}
	d.PackagesRemaining = max(0, d.PackagesTotal-d.PackagesDone)
	d.LinksRemaining = max(0, d.PackagesTotal-d.PackagesLinked)
	d.TestsRemaining = max(0, d.TestsTotal-d.TestsDone-d.TestsRunning)
	d.BuildDepsRemain = max(0, d.BuildDepsTotal-d.BuildDepsFresh-d.BuildDepsCached)
	d.BuildDepsObserved = d.BuildDepsFresh + d.BuildDepsCached
	var binaryBytes int64
	for _, ps := range s.pkgs {
		if ps.glp.hasTests() && ps.exeHash != "" {
			binaryBytes += ps.exeSize
		}
	}
	d.BinarySize = formatByteSize(binaryBytes)
	slices.SortFunc(d.Issues, func(a, b testIssueData) int { return strings.Compare(a.Name, b.Name) })

	return d
}

func formatAge(now, changed time.Time) string {
	if changed.IsZero() || changed.After(now) {
		return "0s ago"
	}
	return now.Sub(changed).Truncate(time.Second).String() + " ago"
}

func formatByteSize(n int64) string {
	const unit = int64(1024)
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := unit, 0
	for value := n / unit; value >= unit && exp < 3; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func newTestIssue(pkg, test string, ts *testStatus, flaky bool, id int) testIssueData {
	status := "FAILED"
	if flaky {
		status = "FLAKY"
	}
	return testIssueData{
		ID:       fmt.Sprintf("test-issue-%d", id),
		Name:     pkg + "." + test,
		Status:   status,
		Flaky:    flaky,
		Output:   boundedFailureOutput(ts.fails),
		Attempts: ts.attempts,
	}
}

func boundedFailureOutput(failures []failInfo) string {
	const marker = "\n[output truncated at 500 KiB]\n"
	var b strings.Builder
	for i, failure := range failures {
		segment := fmt.Sprintf("--- failed attempt %d (%v) ---\n%s", i+1, failure.dur, strings.ToValidUTF8(failure.out, "�"))
		if b.Len()+len(segment) <= webFailureOutputLimit {
			b.WriteString(segment)
			if !strings.HasSuffix(segment, "\n") {
				b.WriteByte('\n')
			}
			continue
		}
		remaining := webFailureOutputLimit - b.Len() - len(marker)
		if remaining > 0 {
			piece := segment[:min(remaining, len(segment))]
			for len(piece) > 0 && !utf8.ValidString(piece) {
				piece = piece[:len(piece)-1]
			}
			b.WriteString(piece)
		}
		result := b.String()
		if len(result) > webFailureOutputLimit-len(marker) {
			result = result[:webFailureOutputLimit-len(marker)]
			for len(result) > 0 && !utf8.ValidString(result) {
				result = result[:len(result)-1]
			}
		}
		return result + marker
	}
	return b.String()
}
