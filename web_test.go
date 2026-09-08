// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"tailscale.com/ipn/ipnstate"
)

type fakeTailscaleStatusClient struct {
	status *ipnstate.Status
	err    error
}

func (c fakeTailscaleStatusClient) StatusWithoutPeers(context.Context) (*ipnstate.Status, error) {
	return c.status, c.err
}

func TestStartStatusListenersTailscale(t *testing.T) {
	st := &ipnstate.Status{
		BackendState: "Running",
		TailscaleIPs: []netip.Addr{
			netip.MustParseAddr("127.0.0.2"),
			netip.MustParseAddr("127.0.0.2"), // duplicates do not create duplicate listeners
		},
		Self: &ipnstate.PeerStatus{DNSName: "host.example.ts.net."},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "status page")
	})
	listeners, statusURL, err := startStatusListeners(t.Context(), "127.0.0.1:0", handler, fakeTailscaleStatusClient{status: st})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeListeners(listeners) })
	if len(listeners) != 2 {
		t.Fatalf("got %d listeners; want configured plus Tailscale listener", len(listeners))
	}
	port := listeners[0].Addr().(*net.TCPAddr).Port
	if want := fmt.Sprintf("http://host.example.ts.net:%d", port); statusURL != want {
		t.Fatalf("status URL = %q; want %q", statusURL, want)
	}
	for _, ln := range listeners {
		res, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			t.Fatalf("GET from %v: %v", ln.Addr(), err)
		}
		body, readErr := io.ReadAll(res.Body)
		res.Body.Close()
		if readErr != nil || string(body) != "status page" {
			t.Fatalf("GET from %v = %q, %v", ln.Addr(), body, readErr)
		}
	}
}

func TestStartStatusListenersWithoutTailscale(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, tc := range []fakeTailscaleStatusClient{
		{err: errors.New("tailscaled unavailable")},
		{status: &ipnstate.Status{BackendState: "Stopped"}},
	} {
		listeners, statusURL, err := startStatusListeners(t.Context(), "127.0.0.1:0", handler, tc)
		if err != nil {
			t.Fatal(err)
		}
		closeListeners(listeners)
		if len(listeners) != 1 || statusURL != "" {
			t.Fatalf("listeners=%d, statusURL=%q; want configured listener only", len(listeners), statusURL)
		}
	}
}

func TestListenerCoversIP(t *testing.T) {
	for _, tt := range []struct {
		addr string
		ip   string
		want bool
	}{
		{"0.0.0.0:5525", "100.64.0.1", true},
		{"0.0.0.0:5525", "fd7a:115c:a1e0::1", false},
		{"[::]:5525", "fd7a:115c:a1e0::1", true},
		{"[::]:5525", "100.64.0.1", false},
		{"127.0.0.1:5525", "127.0.0.1", true},
		{"127.0.0.1:5525", "100.64.0.1", false},
	} {
		tcpAddr, err := net.ResolveTCPAddr("tcp", tt.addr)
		if err != nil {
			t.Fatal(err)
		}
		if got := listenerCoversIP(tcpAddr, netip.MustParseAddr(tt.ip)); got != tt.want {
			t.Errorf("listenerCoversIP(%q, %q) = %v; want %v", tt.addr, tt.ip, got, tt.want)
		}
	}
}

func TestHTMLPatch(t *testing.T) {
	for _, tt := range []struct {
		old, new string
	}{
		{"", "complete document"},
		{"same", "same"},
		{"prefix old suffix", "prefix new suffix"},
		{"hello, 世界", "hello, 🌍"},
		{"short", "shorter"},
		{"longer", "long"},
	} {
		patch := makeHTMLPatch(tt.old, tt.new)
		if got := applyHTMLPatch(tt.old, patch); got != tt.new {
			t.Errorf("patch %q -> %q = %q; want %q (%+v)", tt.old, tt.new, got, tt.new, patch)
		}
	}
}

func TestStatusDataAndFailureOutput(t *testing.T) {
	now := time.Now()
	s := NewServer(runProfile{}, testSelection{})
	defer s.Cleanup()
	s.start = now.Add(-5 * time.Second)
	s.phase = phaseTesting
	s.pkgs = map[string]*packageStatus{
		"example.com/notests": {glp: &goListPackage{ImportPath: "example.com/notests"}, changed: now},
		"example.com/pkg": {
			glp:      &goListPackage{ImportPath: "example.com/pkg", TestGoFiles: []string{"pkg_test.go"}},
			pkgState: pkgStateDone,
			exeHash:  "hash",
			exeSize:  2 << 20,
			numFails: 1,
			runIn:    time.Second,
			changed:  now.Add(-4 * time.Second),
			tests: map[string]*testStatus{
				"TestPass":  {done: true, passed: true, passedIn: time.Second, cached: true, changed: now},
				"TestFlaky": {done: true, passed: true, passedIn: 3 * time.Second, attempts: 2, changed: now, fails: []failInfo{{dur: time.Second, out: "flaky output"}}},
				"TestFail":  {done: true, attempts: 4, changed: now, fails: []failInfo{{dur: 2 * time.Second, out: strings.Repeat("x", webFailureOutputLimit+100)}}},
			},
		},
	}
	s.pkgsWithTests = 1
	s.testsTotal = 3
	s.buildDepsTotal = 321
	s.testEstimateReady = true
	s.testEstimateBase = 40 * time.Millisecond
	s.testSchedule = []string{"example.com/pkg\x00TestETAUnknown"}
	s.testEstimates = map[string]testEstimate{"example.com/pkg\x00TestETAUnknown": {duration: 2 * time.Second}}
	s.pkgs["example.com/pkg"].tests["TestETAUnknown"] = &testStatus{}
	s.testsTotal++

	d := s.statusData()
	if len(d.Packages) != 1 || d.Packages[0].NumTests != 4 {
		t.Fatalf("packages = %+v; want one package with 4 tests", d.Packages)
	}
	if d.Packages[0].LastChanged != "4s ago" {
		t.Fatalf("last changed = %q; want 4s ago", d.Packages[0].LastChanged)
	}
	if d.PackagesLinked != 1 || d.PackagesLinkFresh != 1 || d.PackagesTestFresh != 1 || d.BinarySize != "2.0 MiB" || d.TestsDone != 3 || d.TestsPassed != 2 || d.TestsCached != 1 || d.TestsFresh != 2 || d.TestsFailed != 1 || d.TestsFlaky != 1 || d.BuildDepsTotal != 321 || d.TestETA != "2s" || d.ETAUnknown != 1 {
		t.Fatalf("status data = %+v", d)
	}
	if len(d.Issues) != 2 {
		t.Fatalf("issues = %d; want 2", len(d.Issues))
	}
	if len(d.Slowest) != 3 || d.Slowest[0].Name != "example.com/pkg.TestFlaky" || d.Slowest[0].Duration != 3*time.Second {
		t.Fatalf("slowest = %+v", d.Slowest)
	}
	for _, issue := range d.Issues {
		if len(issue.Output) > webFailureOutputLimit {
			t.Errorf("%s output is %d bytes; limit is %d", issue.Name, len(issue.Output), webFailureOutputLimit)
		}
	}
	html, err := s.renderStatusHTML()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, "example.com/notests") || !strings.Contains(html, "example.com/pkg") {
		t.Fatalf("rendered package filtering is wrong: %q", html)
	}
	for _, want := range []string{"Package binaries", "Build dependencies", "Estimated remaining", "2s", "1 unknown at &lt;1s median", "321", "2.0 MiB", "4s ago", "FAILED", "FLAKY", "flaky output", `data-sort="changed"`, "▶️ PLAY", `href="#slowest"`, "Slowest Tests", "3s", "cached-result", "(cached)"} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered status missing %q", want)
		}
	}
}

func TestStatusTestSummaryOmitsZeroIssuesAndShowsDisabledCache(t *testing.T) {
	oldTestCountSet := testCountSet
	testCountSet = true
	t.Cleanup(func() { testCountSet = oldTestCountSet })

	s := NewServer(runProfile{}, testSelection{})
	defer s.Cleanup()
	s.pkgs = map[string]*packageStatus{}
	s.testsTotal = 4
	html, err := s.renderStatusHTML()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, ">disabled</span>") {
		t.Fatal("test summary does not show disabled cache")
	}
	if strings.Contains(html, "failed</span>") || strings.Contains(html, "flaky</span>") {
		t.Fatalf("test summary shows zero failure or flake breakdown: %q", html)
	}
}

func TestBuildDependencyActionMapping(t *testing.T) {
	actionID := []byte("0123456789abcdef0123456789abcdef")
	prefix := actionIDPrefix(actionID)
	s := &Server{
		buildDepFresh:     make(map[string]bool),
		buildDepCached:    make(map[string]bool),
		buildActionToPkg:  make(map[string]string),
		buildActionMapDir: t.TempDir(),
	}
	s.recordToolExec(toolExecEvent{ImportPath: "example.com/dep", Tool: "compile", BuildID: prefix + "/content"})
	if s.buildDepsFresh != 1 {
		t.Fatalf("fresh dependencies = %d; want 1", s.buildDepsFresh)
	}

	s2 := &Server{
		buildDepFresh:     make(map[string]bool),
		buildDepCached:    make(map[string]bool),
		buildActionToPkg:  make(map[string]string),
		buildActionMapDir: s.buildActionMapDir,
	}
	s2.recordCacheLookup(cacheLookupEvent{ActionID: actionID, Hit: true})
	if s2.buildDepsCached != 1 || !s2.buildDepCached["example.com/dep"] {
		t.Fatalf("cached dependencies = %d, map %v; want example.com/dep", s2.buildDepsCached, s2.buildDepCached)
	}
}

func TestTestingPackageStatusIncludesProgress(t *testing.T) {
	s := NewServer(runProfile{}, testSelection{})
	defer s.Cleanup()
	s.pkgs = map[string]*packageStatus{
		"example.com/pkg": {
			glp:      &goListPackage{ImportPath: "example.com/pkg", TestGoFiles: []string{"pkg_test.go"}},
			pkgState: pkgStateTesting,
			tests: map[string]*testStatus{
				"TestDone":    {done: true, passed: true},
				"TestRunning": {running: true},
				"TestQueued":  {},
				"BenchmarkX":  {},
			},
		},
	}
	s.pkgsWithTests = 1
	s.testsTotal = 3
	d := s.statusData()
	if len(d.Packages) != 1 || d.Packages[0].Status != "testing; 1/3" {
		t.Fatalf("packages = %+v; want testing; 1/3", d.Packages)
	}
}

func TestLiveWebSocketUpdatesAndFinalFlush(t *testing.T) {
	s := NewServer(runProfile{}, testSelection{})
	defer s.Cleanup()
	httpServer := httptest.NewServer(s)
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/live-ws"
	conn, res, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if ext := res.Header.Get("Sec-WebSocket-Extensions"); !strings.Contains(ext, "permessage-deflate") {
		t.Fatalf("websocket extensions = %q; want permessage-deflate", ext)
	}

	readPatch := func() (htmlPatch, time.Time) {
		t.Helper()
		kind, msg, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if kind != websocket.MessageText {
			t.Fatalf("message kind = %v; want text", kind)
		}
		var patch htmlPatch
		if err := json.Unmarshal(msg, &patch); err != nil {
			t.Fatal(err)
		}
		return patch, time.Now()
	}

	first, firstAt := readPatch()
	html := applyHTMLPatch("", first)
	if !strings.Contains(html, "<h1>gotst</h1>") {
		t.Fatalf("initial HTML missing heading: %q", html)
	}
	s.setPhase(phaseTesting)
	second, secondAt := readPatch()
	html = applyHTMLPatch(html, second)
	if elapsed := secondAt.Sub(firstAt); elapsed < liveUpdateInterval-25*time.Millisecond {
		t.Fatalf("updates separated by %v; want at least %v", elapsed, liveUpdateInterval)
	}

	s.setPhase(phaseDone)
	flushed := make(chan struct{})
	go func() {
		s.flushFinalLiveStatus()
		close(flushed)
	}()
	var final htmlPatch
	var finalAt time.Time
	for !final.Final {
		final, finalAt = readPatch()
		html = applyHTMLPatch(html, final)
	}
	if !strings.Contains(html, "Phase: <b>done</b>") {
		t.Fatalf("final HTML missing done phase: %q", html)
	}
	if elapsed := finalAt.Sub(secondAt); elapsed < liveUpdateInterval-25*time.Millisecond {
		t.Fatalf("final update separated by %v; want at least %v", elapsed, liveUpdateInterval)
	}
	ack, _ := json.Marshal(struct {
		Ack uint64 `json:"ack"`
	}{final.Seq})
	if err := conn.Write(ctx, websocket.MessageText, ack); err != nil {
		t.Fatal(err)
	}
	select {
	case <-flushed:
	case <-ctx.Done():
		t.Fatal("final flush did not observe browser acknowledgement")
	}
}

func applyHTMLPatch(old string, patch htmlPatch) string {
	runes := []rune(old)
	return string(slices.Concat(
		runes[:patch.Prefix],
		[]rune(patch.Insert),
		runes[len(runes)-patch.Suffix:],
	))
}
