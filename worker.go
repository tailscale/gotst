// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

const workerPort = 5526

// startTailcatWorkers starts the leader's helper listener. If keyFile is
// non-empty, it names a key written by "tailcat genkey" to use instead of a
// new ephemeral key.
func startTailcatWorkers(f *fleet, keyFile string) (tailcat.Addr, func(), error) {
	logf := logger.Discard
	if *verbose {
		logf = log.Printf
	}
	srv := &tailcat.Server{
		Logf: logf,
		OnTCP: func(port uint16) func(net.Conn) {
			if port != workerPort {
				return nil
			}
			return func(c net.Conn) { go f.serveWorker(c) }
		},
		ServedTCPPorts: []filter.PortRange{{First: workerPort, Last: workerPort}},
	}
	if keyFile != "" {
		j, err := os.ReadFile(keyFile)
		if err != nil {
			return "", nil, err
		}
		var k tailcat.PrivateKey
		if err := json.Unmarshal(j, &k); err != nil {
			return "", nil, fmt.Errorf("parsing %s: %w", keyFile, err)
		}
		if k.Private.IsZero() {
			return "", nil, fmt.Errorf("%s has no private key", keyFile)
		}
		srv.Key = k.Private
		srv.PresharedKey = k.Public.PresharedKey
		srv.RegionID = k.Public.RegionID
	}
	if err := srv.Start(); err != nil {
		return "", nil, err
	}
	return srv.TailcatAddr(), func() { srv.Close() }, nil
}

func (f *fleet) config(id string) workerConfig {
	return workerConfig{
		Version: workerProtocolVersion, ID: id, Tags: f.s.profile.Tags, Race: f.s.profile.Race,
		Count: *testCount, CountSet: testCountSet, MaxRetries: *maxRetries, MaxOutput: *maxOutput,
		Cache: f.s.testCache != nil, History: f.s.history != nil, FailFast: *failFast, JSON: *jsonSummary,
		LinkSkip: *linkSkip,
	}
}

func (f *fleet) serveWorker(c net.Conn) {
	defer c.Close()
	dec, enc := json.NewDecoder(c), json.NewEncoder(c)
	c.SetDeadline(time.Now().Add(workerTimeout))
	var hello workerHello
	if err := dec.Decode(&hello); err != nil {
		return
	}
	var reject string
	switch {
	case hello.Version != workerProtocolVersion:
		reject = "incompatible gotst worker protocol"
	case hello.GOOS != runtime.GOOS || hello.GOARCH != runtime.GOARCH:
		reject = "helper GOOS/GOARCH must match the leader"
	case hello.Slots < 1 || hello.Slots > 1024:
		reject = "helper must announce between 1 and 1024 slots"
	case len(hello.Name) > 256:
		reject = "helper name exceeds 256 bytes"
	}
	if reject != "" {
		enc.Encode(workerConfig{Version: workerProtocolVersion, Error: reject})
		return
	}
	w := f.addWorker(hello.Name, hello.Slots, false)
	var connErr error
	defer func() { f.disconnect(w, connErr) }()
	if connErr = enc.Encode(f.config(w.ID)); connErr != nil {
		return
	}
	for {
		c.SetDeadline(time.Now().Add(workerTimeout))
		var req workerRequest
		if connErr = dec.Decode(&req); connErr != nil {
			if errors.Is(connErr, io.EOF) {
				connErr = nil
			}
			return
		}
		res := f.exchange(w, req)
		if connErr = enc.Encode(res); connErr != nil {
			return
		}
		if res.Done || res.Error != "" {
			return
		}
	}
}

// shutdown lets connected helpers observe completion (including build failure
// or fail-fast) before the leader closes its separate tailcat listener.
func (f *fleet) shutdown() {
	f.s.mu.Lock()
	f.stopped = true
	f.s.mu.Unlock()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		f.s.mu.Lock()
		connected := false
		for _, w := range f.workers {
			connected = connected || (w.ID != "leader" && w.Connected)
		}
		f.s.mu.Unlock()
		if !connected {
			return
		}
		select {
		case <-f.wake:
		case <-deadline.C:
			return
		}
	}
}

// helperRejoinWindow is how long a helper keeps trying to rejoin its
// leader after losing its connection, for example to a timeout while the
// leader's machine is overloaded, before giving up.
const helperRejoinWindow = 2 * time.Minute

// errLeaderConnection marks errors from a helper's leader connection, after
// which the helper tries to rejoin, as opposed to the leader rejecting it or
// the run ending.
type errLeaderConnection struct{ err error }

func (e errLeaderConnection) Error() string { return "leader connection: " + e.err.Error() }
func (e errLeaderConnection) Unwrap() error { return e.err }

func runHelper(addr, name string) error {
	if _, err := tailcat.ParseAddr(tailcat.Addr(addr)); err != nil {
		return fmt.Errorf("invalid leader address: %w", err)
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	logf := logger.Discard
	if *verbose {
		logf = log.Printf
	}
	client := &tailcat.Client{Server: tailcat.Addr(addr), Logf: logf}
	defer client.Close()
	joined := false
	lastConnected := time.Now()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		c, err := client.DialTCPPort(ctx, workerPort)
		cancel()
		if err != nil {
			if time.Since(lastConnected) > helperRejoinWindow {
				return fmt.Errorf("connecting to leader: %w", err)
			}
			log.Printf("connecting to leader: %v; retrying", err)
			time.Sleep(2 * time.Second)
			continue
		}
		err = runWorkerConnection(c, name, joined)
		c.Close()
		var connErr errLeaderConnection
		if !errors.As(err, &connErr) {
			return err
		}
		joined = true
		lastConnected = time.Now()
		log.Printf("%v; rejoining", err)
	}
}

// runWorkerConnection serves the leader over c until the run ends. rejoin
// says whether this helper was connected before; errors from c itself are
// errLeaderConnection.
func runWorkerConnection(c net.Conn, name string, rejoin bool) error {
	dec, enc := json.NewDecoder(c), json.NewEncoder(c)
	c.SetDeadline(time.Now().Add(workerTimeout))
	if err := enc.Encode(workerHello{
		Version: workerProtocolVersion, Name: name, Slots: *jobs, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
	}); err != nil {
		return errLeaderConnection{err}
	}
	var config workerConfig
	if err := dec.Decode(&config); err != nil {
		return errLeaderConnection{err}
	}
	if config.Error != "" {
		return errors.New(config.Error)
	}
	if config.Version != workerProtocolVersion || config.Count < 1 || config.MaxRetries < 0 || config.MaxOutput < 0 {
		return errors.New("invalid leader configuration")
	}
	// This is a separate helper process. Its local resource/cache settings
	// remain local; all test semantics come from the leader.
	*testCount, testCountSet, *maxRetries = config.Count, config.CountSet, config.MaxRetries
	*maxOutput, *useCache = config.MaxOutput, config.Cache
	*failFast, *jsonSummary, *historyConfig = config.FailFast, config.JSON, "off"
	*linkSkip = config.LinkSkip
	defs, err := loadProfileDefinitions(*configFile)
	if err != nil {
		return err
	}
	profile := runProfile{Root: defs.root, Tags: config.Tags, Race: config.Race}
	s := NewServer(profile, testSelection{})
	s.captureHistory = config.History
	defer s.Cleanup()
	verb := "joined"
	if rejoin {
		verb = "rejoined"
	}
	log.Printf("%s leader as %s (%s), %d execution slots", verb, config.ID, name, *jobs)
	return workerLoop(c, dec, enc, s)
}

type helperExecution struct {
	work   workerWork
	cancel context.CancelFunc
	state  string
}

// helperBuilder serializes cmd/go invocations and retains binaries for the
// life of the helper. Tests run concurrently with later builds. It deliberately
// uses cmd/go and the existing linked-executable broker, so GOCACHEPROG remains
// the distribution mechanism for executable bytes.
type helperBuilder struct {
	s       *Server
	leader  bool // s is the leader, whose package map is authoritative
	mu      sync.Mutex
	bins    map[string]capturedTestBinary
	fetches keyedMutex // by package, for binaries the build skipped
}

// buildScratch builds the test binaries of pkgs as base's machine would,
// using a separate package map so discovery doesn't rebuild base's earlier
// packages or race with execution reading their metadata. Captured
// binaries are placed in base's run directory and recorded in the returned
// Server.
func buildScratch(base *Server, pkgs []string) (*Server, error) {
	build := &Server{
		ctx: base.ctx, cacheDir: base.cacheDir, profile: base.profile, scratch: true, linkSkip: base.linkSkip,
		buildDepFresh: make(map[string]bool), buildDepCached: make(map[string]bool),
		buildActionToPkg: make(map[string]string), buildActionMapDir: base.buildActionMapDir,
	}
	build.profile.Packages = pkgs
	if err := build.learnPackagesWithTests(); err != nil {
		return build, err
	}
	return build, build.buildAllTestBinaries()
}

// adoptLocked records the binaries build captured as available to this
// machine. On a helper, it also records their package metadata. b.mu must
// be held.
func (b *helperBuilder) adoptLocked(build *Server) []capturedTestBinary {
	bins := build.capturedTestBinaries()
	for _, bin := range bins {
		b.bins[bin.pkg] = bin
		if b.leader {
			continue
		}
		build.mu.Lock()
		ps := build.pkgs[bin.pkg]
		build.mu.Unlock()
		b.s.mu.Lock()
		if b.s.pkgs == nil {
			b.s.pkgs = make(map[string]*packageStatus)
		}
		b.s.pkgs[bin.pkg] = ps
		b.s.mu.Unlock()
	}
	return bins
}

// prepare builds pkgs in one cmd/go invocation and lists their tests,
// reporting each package's binary and tests, or why it failed.
func (b *helperBuilder) prepare(pkgs []string) []preparedPackage {
	b.mu.Lock()
	build, buildErr := buildScratch(b.s, pkgs)
	bins := b.adoptLocked(build)
	b.mu.Unlock()
	byPkg := make(map[string]capturedTestBinary)
	for _, bin := range bins {
		byPkg[bin.pkg] = bin
	}
	ret := make([]preparedPackage, len(pkgs))
	var wg sync.WaitGroup
	sem := make(chan bool, max(1, *jobs))
	for i, pkg := range pkgs {
		ret[i].Package = pkg
		bin, ok := byPkg[pkg]
		if !ok {
			msg := fmt.Sprintf("package %q did not produce a test binary", pkg)
			if buildErr != nil {
				msg = buildErr.Error()
			}
			ret[i].Error = msg
			continue
		}
		build.mu.Lock()
		ps := build.pkgs[pkg]
		ret[i].Binary, ret[i].Size, ret[i].Cached = binHash(bin.absBin), ps.exeSize, ps.exeCached
		ret[i].Args = slices.Clone(ps.exeArgs)
		stubTests := ps.stubTests
		build.mu.Unlock()
		if stubTests != nil {
			ret[i].Tests = stubTests // from the link record; no binary to list
			continue
		}
		wg.Go(func() {
			sem <- true
			defer func() { <-sem }()
			tests, err := listTests(b.s.ctx, pkg, bin.absBin, bin.workDir)
			if err != nil {
				ret[i].Error = err.Error()
				return
			}
			ret[i].Tests = tests
			build.recordLinkRecord(bin, tests)
		})
	}
	wg.Wait()
	return ret
}

func (b *helperBuilder) binary(work workerWork) (capturedTestBinary, error) {
	b.mu.Lock()
	bin, ok := b.bins[work.Package]
	if !ok {
		build, err := buildScratch(b.s, []string{work.Package})
		if err != nil {
			b.mu.Unlock()
			return capturedTestBinary{}, err
		}
		b.adoptLocked(build)
		bin, ok = b.bins[work.Package]
	}
	b.mu.Unlock()
	if !ok {
		return capturedTestBinary{}, fmt.Errorf("package %q did not produce a test binary", work.Package)
	}
	if binHash(bin.absBin) != work.Binary {
		return capturedTestBinary{}, fmt.Errorf("binary mismatch for %s; helper must have the same source, toolchain, and build environment as leader", work.Package)
	}
	unlock := b.fetches.lock(work.Package)
	defer unlock()
	if err := ensureBinary(b.s.ctx, bin); err != nil {
		return capturedTestBinary{}, err
	}
	return bin, nil
}

func workerLoop(c net.Conn, dec *json.Decoder, enc *json.Encoder, s *Server) error {
	ctx, cancel := context.WithCancel(s.ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		s.cancel() // also cancels cmd/go while preparing a binary
		c.Close()
		wg.Wait()
	}()
	stopClose := context.AfterFunc(ctx, func() { c.Close() })
	defer stopClose()
	builder := &helperBuilder{s: s, bins: make(map[string]capturedTestBinary)}
	active := make(map[uint64]*helperExecution)
	var queue []*helperExecution
	results := make(chan workerResult, *jobs)
	type started struct{ lease uint64 }
	ready := make(chan started, *jobs)
	preparedc := make(chan []preparedPackage, 1)
	preparing := false // a batch of packages is being prepared for the leader
	running := 0
	// Tests fetch their binaries only after missing the result cache.
	var newBinsMu sync.Mutex
	var newBins []string // hashes of binaries prepared for tests
	s.prepareBinary = func(task testTask) error {
		bin, err := builder.binary(workerWork{Package: task.bin.pkg, Binary: binHash(task.bin.absBin)})
		if err != nil {
			return err
		}
		task.bin.absBin = bin.absBin
		newBinsMu.Lock()
		newBins = append(newBins, binHash(bin.absBin))
		newBinsMu.Unlock()
		return nil
	}
	start := func() {
		for running < *jobs && len(queue) != 0 {
			e := queue[0]
			queue = queue[1:]
			if active[e.work.Lease] != e {
				continue
			}
			e.state = "building"
			jobCtx, jobCancel := context.WithCancel(ctx)
			e.cancel = jobCancel
			running++
			wg.Go(func() {
				defer jobCancel()
				select {
				case ready <- started{lease: e.work.Lease}:
				case <-ctx.Done():
					return
				}
				// The binary's path is content-addressed, so this is where
				// it is or will be prepared, after a result-cache miss.
				bin := capturedTestBinary{
					pkg: e.work.Package, absBin: filepath.Join(s.cacheDir, e.work.Binary),
					workDir: filepath.Join(s.profile.Root, e.work.Dir), args: e.work.Args,
				}
				s.mu.Lock()
				if s.pkgs == nil {
					s.pkgs = make(map[string]*packageStatus)
				}
				if s.pkgs[bin.pkg] == nil {
					s.pkgs[bin.pkg] = &packageStatus{glp: &goListPackage{
						ImportPath: bin.pkg, Dir: bin.workDir, Root: filepath.Join(s.profile.Root, e.work.Root),
					}}
				}
				s.mu.Unlock()
				r := executeIsolated(jobCtx, s, testTask{bin: bin, test: e.work.Test})
				r.Lease = e.work.Lease
				select {
				case results <- r:
				case <-ctx.Done():
				}
			})
		}
	}
	ticker := time.NewTicker(workerHeartbeat)
	defer ticker.Stop()
	// Network round trips must not hold up execution of the queued batch.
	// One exchange runs at a time, while this loop keeps worker slots full.
	type exchangeResult struct {
		response workerResponse
		err      error
	}
	requests := make(chan workerRequest, 1)
	responses := make(chan exchangeResult, 1)
	wg.Go(func() {
		for {
			select {
			case req := <-requests:
				c.SetDeadline(time.Now().Add(workerTimeout))
				var res workerResponse
				err := enc.Encode(req)
				if err == nil {
					err = dec.Decode(&res)
				}
				select {
				case responses <- exchangeResult{res, err}:
				case <-ctx.Done():
					return
				}
				if err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	})
	var pending []workerResult
	var binaries []string
	var prepared []preparedPackage
	dirty, inFlight := true, false
	for {
		if dirty && !inFlight {
			newBinsMu.Lock()
			binaries = append(binaries, newBins...)
			newBins = nil
			newBinsMu.Unlock()
			req := workerRequest{Active: make(map[uint64]string), Results: pending, Binaries: binaries, Prepared: prepared}
			for id, e := range active {
				req.Active[id] = e.state
			}
			requests <- req
			pending, binaries, prepared = nil, nil, nil
			dirty, inFlight = false, true
		}
		select {
		case exchange := <-responses:
			inFlight = false
			if exchange.err != nil {
				return errLeaderConnection{exchange.err}
			}
			res := exchange.response
			if res.Error != "" {
				return errors.New(res.Error)
			}
			if res.Done {
				return nil
			}
			for _, id := range res.Cancel {
				if e := active[id]; e != nil {
					if e.cancel != nil {
						e.cancel()
					}
					delete(active, id)
				}
			}
			if len(res.Prepare) > 0 && !preparing {
				preparing = true
				pkgs := res.Prepare
				wg.Go(func() {
					pps := builder.prepare(pkgs)
					select {
					case preparedc <- pps:
					case <-ctx.Done():
					}
				})
			}
			for _, work := range res.Work {
				e := &helperExecution{work: work, state: "queued"}
				active[work.Lease] = e
				queue = append(queue, e)
			}
			start()
		case p := <-ready:
			if e := active[p.lease]; e != nil {
				e.state = "running"
			}
			dirty = true
		case pps := <-preparedc:
			preparing = false
			prepared = append(prepared, pps...)
			for _, pp := range pps {
				if pp.Binary != "" {
					binaries = append(binaries, pp.Binary)
				}
			}
			dirty = true
		case r := <-results:
			running--
			if active[r.Lease] != nil {
				delete(active, r.Lease)
				pending = append(pending, r)
			}
			start()
			dirty = true
		case <-ticker.C:
			dirty = true
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
