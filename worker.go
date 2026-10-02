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
	"runtime"
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	c, err := client.DialTCPPort(ctx, workerPort)
	cancel()
	if err != nil {
		return fmt.Errorf("connecting to leader: %w", err)
	}
	defer c.Close()
	return runWorkerConnection(c, name)
}

func runWorkerConnection(c net.Conn, name string) error {
	dec, enc := json.NewDecoder(c), json.NewEncoder(c)
	c.SetDeadline(time.Now().Add(workerTimeout))
	if err := enc.Encode(workerHello{
		Version: workerProtocolVersion, Name: name, Slots: *jobs, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
	}); err != nil {
		return err
	}
	var config workerConfig
	if err := dec.Decode(&config); err != nil {
		return err
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
	defs, err := loadProfileDefinitions(*configFile)
	if err != nil {
		return err
	}
	profile := runProfile{Root: defs.root, Tags: config.Tags, Race: config.Race}
	s := NewServer(profile, testSelection{})
	s.captureHistory = config.History
	defer s.Cleanup()
	log.Printf("joined leader as %s (%s), %d execution slots", config.ID, name, *jobs)
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
	s    *Server
	mu   sync.Mutex
	bins map[string]capturedTestBinary
}

func (b *helperBuilder) binary(work workerWork) (capturedTestBinary, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bin, ok := b.bins[work.Package]; ok {
		return bin, nil
	}
	// Use a separate package map so discovery doesn't rebuild earlier packages
	// or race with execution reading their metadata.
	build := &Server{
		ctx: b.s.ctx, cacheDir: b.s.cacheDir, profile: b.s.profile,
		buildDepFresh: make(map[string]bool), buildDepCached: make(map[string]bool),
		buildActionToPkg: make(map[string]string), buildActionMapDir: b.s.buildActionMapDir,
	}
	build.profile.Packages = []string{work.Package}
	if err := build.learnPackagesWithTests(); err != nil {
		return capturedTestBinary{}, err
	}
	if err := build.buildAllTestBinaries(); err != nil {
		return capturedTestBinary{}, err
	}
	bins := build.capturedTestBinaries()
	if len(bins) != 1 || bins[0].pkg != work.Package {
		return capturedTestBinary{}, fmt.Errorf("package %q did not produce a test binary", work.Package)
	}
	bin := bins[0]
	if binHash(bin.absBin) != work.Binary {
		return capturedTestBinary{}, fmt.Errorf("binary mismatch for %s; helper must have the same source, toolchain, and build environment as leader", work.Package)
	}
	b.s.mu.Lock()
	if b.s.pkgs == nil {
		b.s.pkgs = make(map[string]*packageStatus)
	}
	b.s.pkgs[work.Package] = build.pkgs[work.Package]
	b.s.mu.Unlock()
	b.bins[work.Package] = bin
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
	type prepared struct {
		lease uint64
		hash  string
	}
	ready := make(chan prepared, *jobs)
	running := 0
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
				bin, err := builder.binary(e.work)
				r := workerResult{Lease: e.work.Lease}
				if err != nil {
					r.Error, r.Infra = err.Error(), true
				} else if jobCtx.Err() == nil {
					select {
					case ready <- prepared{e.work.Lease, binHash(bin.absBin)}:
					case <-ctx.Done():
						return
					}
					bin.args = e.work.Args
					r = executeIsolated(jobCtx, s, testTask{bin: bin, test: e.work.Test})
					r.Lease = e.work.Lease
				}
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
	dirty, inFlight := true, false
	for {
		if dirty && !inFlight {
			req := workerRequest{Active: make(map[uint64]string), Results: pending, Binaries: binaries}
			for id, e := range active {
				req.Active[id] = e.state
			}
			requests <- req
			pending, binaries = nil, nil
			dirty, inFlight = false, true
		}
		select {
		case exchange := <-responses:
			inFlight = false
			if exchange.err != nil {
				return fmt.Errorf("leader connection: %w", exchange.err)
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
			for _, work := range res.Work {
				e := &helperExecution{work: work, state: "queued"}
				active[work.Lease] = e
				queue = append(queue, e)
			}
			start()
		case p := <-ready:
			binaries = append(binaries, p.hash)
			if e := active[p.lease]; e != nil {
				e.state = "running"
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
