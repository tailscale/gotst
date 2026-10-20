// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/gotst/history"
)

const (
	workerProtocolVersion = 3
	workerBatchLimit      = 64
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

	// Package preparation: building, and listing the tests of, each
	// selected test package is work the leader and helpers share. Tests
	// become work as their packages are prepared.
	preparing bool           // until every package is prepared
	prep      []*prepItem    // in package order
	prepared  int            // finished prep items
	buildErrs []string       // build failures reported by preparation
	local     *helperBuilder // the leader's own binaries
}

// prepItem is one test package to prepare.
type prepItem struct {
	pkg      string
	worker   *fleetWorker // nil while unassigned
	assigned time.Time
	done     bool
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
	prep      []*prepItem // being prepared by this worker
	Built     int         // packages this worker prepared
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
	Prepared []preparedPackage
}

// preparedPackage reports a test package a helper prepared.
type preparedPackage struct {
	Package string
	Binary  string // SHA-256
	Size    int64
	Cached  bool     // restored from the build cache
	Args    []string // captured test binary arguments
	Tests   []string // as listed by the binary
	Error   string   `json:",omitempty"` // build or listing failure
}

type workerResponse struct {
	Work    []workerWork
	Cancel  []uint64
	Prepare []string // test packages to prepare
	Done    bool
	Error   string `json:",omitempty"`
}

type workerWork struct {
	Lease   uint64
	Package string
	Test    string
	Binary  string
	Args    []string // resolved leader arguments, excluding per-attempt flags
	Dir     string   // package directory, relative to the checkout root
	Root    string   // module root, relative to the checkout root
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
	helpers := 0
	for _, w := range f.workers {
		if w.ID != "leader" {
			helpers++
		}
	}
	id := fmt.Sprintf("h%d", helpers+1)
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
	f.releasePrepLocked(w)
	f.s.mu.Unlock()
	f.notify()
}

// releasePrepLocked returns w's unfinished prep items to the queue.
func (f *fleet) releasePrepLocked(w *fleetWorker) {
	for _, it := range w.prep {
		if !it.done {
			it.worker = nil
		}
	}
	w.prep = nil
}

// prepBatchSize returns how many of unassigned remaining test packages a
// machine should prepare in one cmd/go invocation, with machines machines.
// This is guided self-scheduling: batches are large while much work
// remains, amortizing each invocation's package loading and cache
// lookups, and shrink to single packages at the end, so no machine is left
// holding a large batch while others idle. Alone, a machine takes
// everything at once, as an ordinary build would.
func prepBatchSize(unassigned, machines int) int {
	if machines <= 1 {
		return unassigned
	}
	return min(max((unassigned+4*machines-1)/(4*machines), 1), 8)
}

// nextPrepLocked assigns w its next batch of packages to prepare, or
// returns nil.
func (f *fleet) nextPrepLocked(w *fleetWorker, now time.Time) []string {
	if !f.preparing || len(w.prep) != 0 || w.Error != "" {
		return nil
	}
	var unassigned []*prepItem
	for _, it := range f.prep {
		if !it.done && it.worker == nil {
			unassigned = append(unassigned, it)
		}
	}
	if len(unassigned) == 0 {
		return nil
	}
	machines := 0
	for _, w := range f.workers {
		if w.Connected && w.Error == "" {
			machines++
		}
	}
	var pkgs []string
	for _, it := range unassigned[:min(len(unassigned), prepBatchSize(len(unassigned), machines))] {
		it.worker, it.assigned = w, now
		w.prep = append(w.prep, it)
		pkgs = append(pkgs, it.pkg)
	}
	return pkgs
}

// applyPrepared records a package w prepared: its binary, its tests, which
// become work, or its build failure. Results for packages already prepared
// are ignored.
func (f *fleet) applyPrepared(w *fleetWorker, pp preparedPackage) {
	s := f.s
	s.mu.Lock()
	var item *prepItem
	for _, it := range f.prep {
		if it.pkg == pp.Package {
			item = it
		}
	}
	w.prep = slices.DeleteFunc(w.prep, func(it *prepItem) bool { return it.pkg == pp.Package })
	if item == nil || item.done {
		s.mu.Unlock()
		return
	}
	item.done = true
	f.prepared++
	w.Built++
	if pp.Error != "" {
		f.buildErrs = append(f.buildErrs, pp.Error)
		s.mu.Unlock()
		s.cancel()
		f.notify()
		return
	}
	ps := s.pkgs[pp.Package]
	now := time.Now()
	ps.pkgState = pkgStateBuilt
	ps.exeHash, ps.exeSize, ps.exeCached = pp.Binary, pp.Size, pp.Cached
	ps.workDir, ps.exeArgs = ps.glp.Dir, slices.Clone(pp.Args)
	ps.changed = now
	ps.tests = make(map[string]*testStatus)
	var tasks []testTask
	bin := capturedTestBinary{pkg: pp.Package, absBin: filepath.Join(s.cacheDir, pp.Binary), workDir: ps.glp.Dir, args: ps.exeArgs}
	for _, test := range pp.Tests {
		if !s.tests.wants(pp.Package, test) {
			continue
		}
		ps.tests[test] = &testStatus{changed: now}
		if isRunnableTopLevelTest(test) {
			s.testsTotal++
			tasks = append(tasks, testTask{bin: bin, test: test})
		}
	}
	if len(tasks) == 0 {
		ps.pkgState = pkgStateDone
	}
	w.binaries[pp.Binary] = true
	s.mu.Unlock()

	if len(tasks) > 0 {
		s.loadHistory(tasks)
		s.mu.Lock()
		f.addTasksLocked(tasks)
		s.mu.Unlock()
	}
	f.notify()
}

// addTasksLocked adds tasks to the fleet's queue, which stays ordered
// slowest first by estimate.
func (f *fleet) addTasksLocked(tasks []testTask) {
	s := f.s
	for _, task := range tasks {
		id := task.bin.pkg + "\x00" + task.test
		est := s.estimateLocked(task)
		s.testEstimates[id] = est
		f.work = append(f.work, &fleetTask{task: task, estimate: est.duration, leases: make(map[uint64]*workLease)})
	}
	slices.SortStableFunc(f.work, func(a, b *fleetTask) int { return cmp.Compare(b.estimate, a.estimate) })
	s.testSchedule = s.testSchedule[:0]
	for _, t := range f.work {
		s.testSchedule = append(s.testSchedule, t.task.bin.pkg+"\x00"+t.task.test)
	}
	s.testEstimateReady = true
}

// runDistributed prepares pkgs and runs their tests across the fleet: the
// leader's own slots and its helpers share one queue of packages to build
// and list and one of tests, and tests become eligible as soon as their
// package is prepared. A build failure stops the run.
func (f *fleet) runDistributed(pkgs []string) error {
	s := f.s
	s.mu.Lock()
	for _, pkg := range pkgs {
		f.prep = append(f.prep, &prepItem{pkg: pkg})
	}
	f.preparing = len(pkgs) > 0
	f.ready = true
	if s.testEstimates == nil {
		s.testEstimates = make(map[string]testEstimate)
	}
	if s.testEstimateBase == 0 {
		s.testEstimateBase = defaultTestEstimate * time.Duration(*testCount)
	}
	local := f.leaderWorkerLocked()
	f.local = &helperBuilder{s: s, bins: make(map[string]capturedTestBinary), leader: true}
	s.mu.Unlock()
	s.prepareBinary = func(task testTask) error {
		_, err := f.local.binary(workerWork{Package: task.bin.pkg, Binary: binHash(task.bin.absBin)})
		return err
	}

	var wg sync.WaitGroup
	wg.Go(func() { f.localPreparer(local) })
	err := f.run(local)
	wg.Wait()
	s.mu.Lock()
	buildErrs := slices.Clone(f.buildErrs)
	s.mu.Unlock()
	if len(buildErrs) > 0 {
		slices.Sort(buildErrs)
		return fmt.Errorf("building test binaries: %s", strings.Join(slices.Compact(buildErrs), "\n"))
	}
	return err
}

// localPreparer prepares packages on the leader until none remain.
func (f *fleet) localPreparer(w *fleetWorker) {
	s := f.s
	for {
		s.mu.Lock()
		if !f.preparing || s.ctx.Err() != nil {
			s.mu.Unlock()
			return
		}
		pkgs := f.nextPrepLocked(w, time.Now())
		s.mu.Unlock()
		if pkgs == nil {
			select {
			case <-f.wake:
			case <-time.After(time.Second):
			case <-s.ctx.Done():
			}
			continue
		}
		for _, pp := range f.local.prepare(pkgs) {
			f.applyPrepared(w, pp)
		}
		f.finishPreparingIfDone()
	}
}

// finishPreparingIfDone ends package preparation once every package is
// prepared, checking that requested tests exist.
func (f *fleet) finishPreparingIfDone() {
	s := f.s
	s.mu.Lock()
	if !f.preparing || f.prepared < len(f.prep) {
		s.mu.Unlock()
		return
	}
	f.preparing = false
	s.mu.Unlock()
	if err := s.validateSelectedTests(); err != nil {
		s.mu.Lock()
		f.buildErrs = append(f.buildErrs, err.Error())
		s.mu.Unlock()
		s.cancel()
	}
	s.setPhase(phaseTesting)
	f.notify()
}

// leaderWorkerLocked returns the leader's own worker, adding it if needed.
func (f *fleet) leaderWorkerLocked() *fleetWorker {
	for _, w := range f.workers {
		if w.ID == "leader" {
			return w
		}
	}
	now := time.Now()
	w := &fleetWorker{
		ID: "leader", Name: "local", Slots: *jobs, Connected: true, LastSeen: now, Joined: now,
		binaries: make(map[string]bool), leases: make(map[uint64]*workLease),
	}
	f.workers = append(f.workers, w)
	return w
}

// nextLocked preserves the global slowest-first queue, with affinity: a
// task whose binary w already has is taken from anywhere in the queue, so
// tests mostly run where their packages were prepared, as long as its
// estimate is within 20% of the longest eligible task's. Fresh work always
// precedes speculative copies.
func (f *fleet) nextLocked(w *fleetWorker, now time.Time) *fleetTask {
	var first *fleetTask
	for _, t := range f.work {
		if t.done || len(t.leases) != 0 {
			continue
		}
		if first == nil {
			first = t
		}
		if t.estimate < first.estimate-first.estimate/5 {
			break // the queue is ordered by estimate, so no later task qualifies
		}
		if w.binaries[binHash(t.task.bin.absBin)] {
			return t
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
		work := workerWork{
			Lease: l.id, Package: t.task.bin.pkg, Test: t.task.test,
			Binary: binHash(t.task.bin.absBin), Args: effectiveTestArgs(t.task, f.s.profile),
		}
		if ps := f.s.pkgs[t.task.bin.pkg]; ps != nil {
			work.Dir, _ = filepath.Rel(f.s.profile.Root, ps.glp.Dir)
			work.Root, _ = filepath.Rel(f.s.profile.Root, ps.glp.Root)
		}
		batch = append(batch, work)
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
	if r.Cached {
		// A hit reports the original run's duration, not time spent here.
		w.Cached++
	} else {
		w.Duration += r.Duration
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
	for _, pp := range req.Prepared {
		f.applyPrepared(w, pp)
	}
	if len(req.Prepared) > 0 {
		f.finishPreparingIfDone()
	}
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
	res.Done = f.stopped || f.s.ctx.Err() != nil || (f.ready && !f.preparing && f.done == len(f.work))
	res.Error = w.Error
	if !res.Done && res.Error == "" {
		res.Prepare = f.nextPrepLocked(w, now)
		// Keep one short batch queued ahead of the running slots.
		limit := min(workerBatchLimit, w.Slots+workerBatchLimit-len(w.leases))
		res.Work = f.assignLocked(w, limit, now)
	}
	return res
}

// run executes the fleet's tests on the leader's slots and waits for
// every test, including those of packages still being prepared, to finish.
func (f *fleet) run(w *fleetWorker) error {
	s := f.s
	var wg sync.WaitGroup
	for range *jobs {
		wg.Go(func() { f.localWorker(w) })
	}
	for {
		s.mu.Lock()
		done := !f.preparing && f.done == len(f.work)
		s.mu.Unlock()
		if done || s.ctx.Err() != nil {
			break
		}
		// f.wake has several waiters, so also poll.
		select {
		case <-f.wake:
		case <-time.After(250 * time.Millisecond):
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
		if f.stopped || f.s.ctx.Err() != nil || (!f.preparing && f.done == len(f.work)) {
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
		prepareBinary: parent.prepareBinary,
		execSem:       make(chan bool, 1), pkgs: map[string]*packageStatus{task.bin.pkg: ps},
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
		if len(w.prep) > 0 {
			d.Status = "working"
			d.Work = append(d.Work, fmt.Sprintf("preparing %d packages from %s", len(w.prep), w.prep[0].pkg))
		}
		for _, l := range w.leases {
			d.Work = append(d.Work, l.work.task.bin.pkg+"."+l.work.task.test+" ("+l.state+")")
		}
		slices.Sort(d.Work)
		ret = append(ret, d)
	}
	return ret
}
