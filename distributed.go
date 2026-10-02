// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/tailscale/gotst/history"
)

const (
	workerProtocolVersion = 1
	workerBatchLimit      = 10
	workerBatchTarget     = 3 * time.Second
	workerHeartbeat       = 500 * time.Millisecond
	workerTimeout         = 15 * time.Second
)

// The work protocol exchanges leases, not test identities. A disconnected or
// overdue execution can be duplicated, but only the first result of a live
// lease is committed. The leader's s.mu guards all fleet state.
type fleet struct {
	s       *Server
	workers []*fleetWorker
	work    []*fleetTask
	leases  map[uint64]*workLease
	nextID  uint64
	ready   bool
	stopped bool
	done    int
	infra   []error
	wake    chan struct{}
}

type fleetWorker struct {
	ID        string
	Name      string
	Slots     int
	Connected bool
	LastSeen  time.Time
	Joined    time.Time
	Left      time.Time // zero while connected
	Completed int
	Cached    int
	Failed    int
	Duration  time.Duration
	Error     string
	binaries  map[string]bool
	leases    map[uint64]*workLease
}

type fleetTask struct {
	task     testTask
	estimate time.Duration
	done     bool
	leases   map[uint64]*workLease
}

type workLease struct {
	id       uint64
	work     *fleetTask
	worker   *fleetWorker
	assigned time.Time
	started  time.Time
	state    string
	cancel   context.CancelFunc // local execution only
}

type workerHello struct {
	Version int
	Name    string
	Slots   int
	GOOS    string
	GOARCH  string
}

type workerConfig struct {
	Version    int
	ID         string
	Error      string `json:",omitempty"`
	Tags       []string
	Race       bool
	Count      int
	CountSet   bool
	MaxRetries int
	MaxOutput  int64
	Cache      bool
	History    bool
	FailFast   bool
	JSON       bool
}

type workerRequest struct {
	Active   map[uint64]string // queued, building, or running
	Binaries []string          // newly available SHA-256s
	Results  []workerResult
}

type workerResponse struct {
	Work   []workerWork
	Cancel []uint64
	Done   bool
	Error  string `json:",omitempty"`
}

type workerWork struct {
	Lease   uint64
	Package string
	Test    string
	Binary  string
	Args    []string // resolved leader arguments, excluding per-attempt flags
}

type workerFailure struct {
	Duration time.Duration
	Output   string
}

type workerResult struct {
	Lease       uint64
	Passed      bool
	Cached      bool
	Duration    time.Duration
	Attempts    int
	CacheChecks int
	Failures    []workerFailure
	Attrs       map[string]string
	History     []history.Observation
	Error       string
	Infra       bool
}

func newFleet(s *Server) *fleet {
	return &fleet{s: s, leases: make(map[uint64]*workLease), wake: make(chan struct{}, 1)}
}

func (f *fleet) notify() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *fleet) addWorker(name string, slots int, local bool) *fleetWorker {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	id := fmt.Sprintf("h%d", len(f.workers)+1)
	now := time.Now()
	if local {
		id = "leader"
	}
	w := &fleetWorker{
		ID: id, Name: name, Slots: slots, Connected: true, LastSeen: now, Joined: now,
		binaries: make(map[string]bool), leases: make(map[uint64]*workLease),
	}
	f.workers = append(f.workers, w)
	return w
}

func (f *fleet) removeLeaseLocked(l *workLease) {
	delete(f.leases, l.id)
	delete(l.worker.leases, l.id)
	delete(l.work.leases, l.id)
	if l.cancel != nil {
		l.cancel()
	}
	f.updateRunningLocked(l.work)
}

func (f *fleet) updateRunningLocked(t *fleetTask) {
	ps := f.s.pkgs[t.task.bin.pkg]
	ts := ps.tests[t.task.test]
	if ts.done {
		return
	}
	ts.running = false
	ts.started = time.Time{}
	for _, l := range t.leases {
		if l.state == "running" {
			ts.running = true
			if ts.started.IsZero() || l.started.Before(ts.started) {
				ts.started = l.started
			}
		}
	}
	ts.changed = time.Now()
	ps.changed = ts.changed
	if ts.running {
		ps.pkgState = pkgStateTesting
	}
}

func (f *fleet) disconnect(w *fleetWorker, err error) {
	f.s.mu.Lock()
	w.Connected = false
	w.Left = time.Now()
	if err != nil {
		w.Error = err.Error()
	}
	for _, l := range w.leases {
		f.removeLeaseLocked(l)
	}
	f.s.mu.Unlock()
	f.notify()
}

// nextLocked preserves the global slowest-first queue. Binary affinity may
// move a task forward by at most four places, and only within 20% of the
// longest eligible estimate. Fresh work always precedes speculative copies.
func (f *fleet) nextLocked(w *fleetWorker, now time.Time) *fleetTask {
	var first *fleetTask
	window := 0
	for _, t := range f.work {
		if t.done || len(t.leases) != 0 {
			continue
		}
		if first == nil {
			first = t
		}
		if w.binaries[binHash(t.task.bin.absBin)] && t.estimate >= first.estimate-first.estimate/5 {
			return t
		}
		window++
		if window == 5 {
			break
		}
	}
	if first != nil {
		return first
	}
	for _, t := range f.work {
		if t.done || len(t.leases) != 1 {
			continue
		}
		for _, l := range t.leases {
			// Include build time and queued work, so a live heartbeat cannot
			// indefinitely pin a batch on a VM whose build or test is stuck.
			if l.worker != w && now.Sub(l.assigned) > max(10*time.Second, 3*t.estimate) {
				return t
			}
		}
	}
	return nil
}

func (f *fleet) assignLocked(w *fleetWorker, limit int, now time.Time) []workerWork {
	if !f.ready || f.stopped || f.s.ctx.Err() != nil || w.Error != "" {
		return nil
	}
	var batch []workerWork
	var duration time.Duration
	for _, l := range w.leases {
		duration += l.work.estimate
	}
	for len(batch) < limit {
		if len(w.leases) >= w.Slots && duration >= workerBatchTarget*time.Duration(w.Slots) {
			break
		}
		t := f.nextLocked(w, now)
		if t == nil {
			break
		}
		f.nextID++
		l := &workLease{id: f.nextID, work: t, worker: w, assigned: now, state: "queued"}
		f.leases[l.id], w.leases[l.id], t.leases[l.id] = l, l, l
		batch = append(batch, workerWork{
			Lease: l.id, Package: t.task.bin.pkg, Test: t.task.test,
			Binary: binHash(t.task.bin.absBin), Args: effectiveTestArgs(t.task, f.s.profile),
		})
		duration += t.estimate
	}
	return batch
}

// accept commits one result atomically, including its history and counters.
// A helper infrastructure failure releases its work for another machine;
// a real test failure is an authoritative result, including retries there.
func (f *fleet) accept(w *fleetWorker, r workerResult) {
	f.s.mu.Lock()
	l := w.leases[r.Lease]
	if l == nil || l.work.done || f.stopped || f.s.ctx.Err() != nil {
		f.s.mu.Unlock()
		return
	}
	if r.Infra && w.ID != "leader" {
		w.Error = r.Error
		if w.Error == "" {
			w.Error = "helper execution error"
		}
		for _, lease := range w.leases {
			f.removeLeaseLocked(lease)
		}
		f.s.mu.Unlock()
		f.notify()
		return
	}
	t := l.work
	t.done = true
	f.done++
	w.Completed++
	w.Duration += r.Duration
	if r.Cached {
		w.Cached++
	}
	if !r.Passed {
		w.Failed++
	}
	var failures []failInfo
	for _, failure := range r.Failures {
		failures = append(failures, failInfo{dur: failure.Duration, out: failure.Output})
	}
	var err error
	if r.Error != "" {
		err = errors.New(r.Error)
	}
	if r.Infra {
		f.infra = append(f.infra, fmt.Errorf("%s.%s: %s", t.task.bin.pkg, t.task.test, r.Error))
	}
	ts := f.s.pkgs[t.task.bin.pkg].tests[t.task.test]
	ts.attempts, ts.attrs = r.Attempts, r.Attrs
	f.s.cacheChecks += r.CacheChecks
	if r.Cached {
		f.s.cacheHits++
	}
	for _, obs := range r.History {
		// Identity comes from the leader. Machine-local dependency shapes
		// have already been made portable by the executing runner.
		obs.Key = historyKeyForTask(t.task, f.s.profile)
		f.s.historyPending = append(f.s.historyPending, obs)
	}
	f.s.finishTestLocked(t.task, r.Passed, r.Duration, failures, err, r.Cached)
	for _, lease := range t.leases {
		f.removeLeaseLocked(lease)
	}
	if *failFast && !r.Passed {
		f.s.failFastTriggered = true
		f.s.cancel()
	}
	f.s.mu.Unlock()
	output := ""
	if len(failures) > 0 {
		output = failures[len(failures)-1].out
	}
	f.s.printTestResult(t.task, r.Duration, r.Cached, r.Passed, output)
	f.notify()
}

func (f *fleet) exchange(w *fleetWorker, req workerRequest) workerResponse {
	for _, r := range req.Results {
		f.accept(w, r)
	}
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	now := time.Now()
	w.LastSeen = now
	for _, hash := range req.Binaries {
		w.binaries[hash] = true
	}
	var res workerResponse
	for id, state := range req.Active {
		l := w.leases[id]
		if l == nil {
			res.Cancel = append(res.Cancel, id)
			continue
		}
		if state != "queued" && state != "building" && state != "running" {
			continue
		}
		if state == "running" && l.started.IsZero() {
			l.started = now
		}
		l.state = state
		f.updateRunningLocked(l.work)
		ps := f.s.pkgs[l.work.task.bin.pkg]
		ps.pkgState, ps.changed = pkgStateTesting, now
	}
	res.Done = f.stopped || f.s.ctx.Err() != nil || (f.ready && f.done == len(f.work))
	res.Error = w.Error
	if !res.Done && res.Error == "" {
		// Keep one short batch queued ahead of the running slots.
		limit := min(workerBatchLimit, w.Slots+workerBatchLimit-len(w.leases))
		res.Work = f.assignLocked(w, limit, now)
	}
	return res
}

func (f *fleet) run(tasks []testTask) error {
	s := f.s
	w := f.addWorker("local", *jobs, true)
	s.mu.Lock()
	for _, task := range tasks {
		f.work = append(f.work, &fleetTask{
			task: task, estimate: s.testEstimates[task.bin.pkg+"\x00"+task.test].duration,
			leases: make(map[uint64]*workLease),
		})
		w.binaries[binHash(task.bin.absBin)] = true
	}
	// Distributed runs use one duration-ordered queue across the fleet.
	// Stable sorting retains the existing history order as a tie breaker.
	slices.SortStableFunc(f.work, func(a, b *fleetTask) int {
		if a.estimate > b.estimate {
			return -1
		}
		if a.estimate < b.estimate {
			return 1
		}
		return 0
	})
	s.testSchedule = nil
	for _, t := range f.work {
		s.testSchedule = append(s.testSchedule, t.task.bin.pkg+"\x00"+t.task.test)
	}
	f.ready = true
	s.mu.Unlock()
	var wg sync.WaitGroup
	for range min(*jobs, len(tasks)) {
		wg.Go(func() { f.localWorker(w) })
	}
	for {
		s.mu.Lock()
		done := f.done == len(f.work)
		s.mu.Unlock()
		if done || s.ctx.Err() != nil {
			break
		}
		select {
		case <-f.wake:
		case <-s.ctx.Done():
		}
	}
	s.mu.Lock()
	f.stopped = true
	for _, l := range f.leases {
		f.removeLeaseLocked(l)
	}
	s.mu.Unlock()
	wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := errors.Join(f.infra...); err != nil {
		return fmt.Errorf("running tests: %w", err)
	}
	failed := 0
	for _, ps := range s.pkgs {
		failed += ps.numFails
	}
	if failed != 0 {
		return fmt.Errorf("%d test(s) failed", failed)
	}
	return s.ctx.Err()
}

func (f *fleet) localWorker(w *fleetWorker) {
	ticker := time.NewTicker(workerHeartbeat)
	defer ticker.Stop()
	for {
		f.s.mu.Lock()
		if f.stopped || f.s.ctx.Err() != nil || f.done == len(f.work) {
			f.s.mu.Unlock()
			return
		}
		batch := f.assignLocked(w, 1, time.Now())
		if len(batch) == 0 {
			f.s.mu.Unlock()
			select {
			case <-ticker.C:
			case <-f.s.ctx.Done():
				return
			}
			continue
		}
		l := w.leases[batch[0].Lease]
		ctx, cancel := context.WithCancel(f.s.ctx)
		l.cancel, l.state, l.started = cancel, "running", time.Now()
		f.updateRunningLocked(l.work)
		task := l.work.task
		f.s.mu.Unlock()
		r := executeIsolated(ctx, f.s, task)
		r.Lease = l.id
		f.accept(w, r)
		cancel()
	}
}

// executeIsolated reuses the ordinary retry, attribute, dependency, and result
// cache implementation without letting speculative attempts mutate the leader.
func executeIsolated(ctx context.Context, parent *Server, task testTask) workerResult {
	dir, err := os.MkdirTemp(parent.cacheDir, "attempt-")
	if err != nil {
		return workerResult{Infra: true, Error: err.Error()}
	}
	defer os.RemoveAll(dir)
	ts := &testStatus{}
	ps := &packageStatus{glp: &goListPackage{Root: parent.packageRoot(task.bin.pkg)}, tests: map[string]*testStatus{task.test: ts}}
	s := &Server{
		ctx: ctx, cacheDir: dir, profile: parent.profile, quiet: true,
		testCache: parent.testCache, captureHistory: parent.history != nil || parent.captureHistory,
		execSem: make(chan bool, 1), pkgs: map[string]*packageStatus{task.bin.pkg: ps},
	}
	err = s.runTest(task)
	r := workerResult{
		Passed: ts.passed, Cached: ts.cached, Duration: ps.runIn, Attempts: ts.attempts,
		CacheChecks: s.cacheChecks, Attrs: ts.attrs, History: s.historyPending,
	}
	for _, fail := range ts.fails {
		r.Failures = append(r.Failures, workerFailure{Duration: fail.dur, Output: fail.out})
	}
	if err != nil {
		r.Error = err.Error()
		var ee *exec.ExitError
		r.Infra = !errors.As(err, &ee)
	}
	return r
}

type workerData struct {
	ID, Name, Status, Error          string
	Slots, Completed, Cached, Failed int
	Duration                         time.Duration
	Work                             []string
}

func (s *Server) workerDataLocked(now time.Time) []workerData {
	if s.fleet == nil {
		return nil
	}
	var ret []workerData
	for _, w := range s.fleet.workers {
		d := workerData{
			ID: w.ID, Name: w.Name, Slots: w.Slots, Completed: w.Completed,
			Cached: w.Cached, Failed: w.Failed, Duration: w.Duration.Round(time.Millisecond), Error: w.Error,
			Status: "idle",
		}
		switch {
		case w.Error != "":
			d.Status = "unavailable"
		case s.fleet.stopped:
			d.Status = "finished"
		case !w.Connected:
			d.Status = "disconnected"
		case w.ID != "leader" && now.Sub(w.LastSeen) > workerTimeout:
			d.Status = "unresponsive"
		case len(w.leases) > 0:
			d.Status = "working"
		}
		for _, l := range w.leases {
			d.Work = append(d.Work, l.work.task.bin.pkg+"."+l.work.task.test+" ("+l.state+")")
		}
		slices.Sort(d.Work)
		ret = append(ret, d)
	}
	return ret
}
