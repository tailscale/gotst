// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func testHistoryKey(test string) historyKey {
	return historyKey{Package: "example.com/p", Test: test, GOOS: "linux", GOARCH: "amd64", Tags: []string{"one"}}
}

func testHistoryObservation(key historyKey, id string, at time.Time) historyObservation {
	return historyObservation{
		ID: id, Key: key, ObservedAt: at, Outcome: historyPass,
		Duration: 5 * time.Millisecond, Attempts: 1,
		Dependencies: []historyDependency{{Operation: "getenv", Name: "GODEBUG"}},
	}
}

func TestDiskHistoryStore(t *testing.T) {
	store, err := newDiskHistoryStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := testHistoryKey("TestOne")
	older := testHistoryObservation(key, "older", time.Unix(100, 0).UTC())
	newer := testHistoryObservation(key, "newer", time.Unix(200, 0).UTC())
	if err := store.Record(context.Background(), []historyObservation{older, newer, newer}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Lookup(context.Background(), []historyKey{key, testHistoryKey("missing")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	h := got[key.id()]
	if h == nil || len(h.Observations) != 1 || h.Observations[0].ID != "newer" {
		t.Fatalf("Lookup = %+v; want newest observation only", h)
	}
	data, err := os.ReadFile(store.observationPath(newer))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"duration_ns": 5000000`, `"GODEBUG"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("disk history missing %q:\n%s", want, data)
		}
	}
}

func TestDiskHistoryStoreConcurrentWriters(t *testing.T) {
	root := t.TempDir()
	store1, err := newDiskHistoryStore(root)
	if err != nil {
		t.Fatal(err)
	}
	store2, err := newDiskHistoryStore(root)
	if err != nil {
		t.Fatal(err)
	}
	key := testHistoryKey("TestConcurrent")
	start := make(chan struct{})
	errs := make(chan error, 2)
	for writer, store := range []*diskHistoryStore{store1, store2} {
		go func() {
			var observations []historyObservation
			for i := range localHistoryLimit {
				id := fmt.Sprintf("writer-%d-%02d", writer, i)
				observations = append(observations, testHistoryObservation(key, id, time.Unix(int64(writer*100+i), 0).UTC()))
			}
			<-start
			errs <- store.Record(context.Background(), observations)
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	// Pruning is opportunistic: simultaneous writers can each finish pruning
	// before observing all of the other's files. Any later record converges the
	// directory to the configured bound.
	if err := store1.Record(context.Background(), []historyObservation{
		testHistoryObservation(key, "writer-1-31", time.Unix(131, 0).UTC()),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store1.Lookup(context.Background(), []historyKey{key}, localHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	observations := got[key.id()].Observations
	if len(observations) != localHistoryLimit {
		t.Fatalf("got %d observations; want %d", len(observations), localHistoryLimit)
	}
	if observations[0].ID != "writer-1-31" {
		t.Fatalf("newest observation = %q; want writer-1-31", observations[0].ID)
	}
}

func TestHTTPHistoryStore(t *testing.T) {
	key := testHistoryKey("TestHTTP")
	obs := testHistoryObservation(key, "observation-id", time.Unix(200, 0).UTC())
	var gotLookup historyLookupRequest
	var gotRecord historyRecordRequest
	mux := http.NewServeMux()
	mux.HandleFunc("POST /base/v1/history/lookup", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotLookup); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(historyLookupResponse{
			Version:   historyVersion,
			Histories: []*testHistory{{Version: historyVersion, Key: key, Observations: []historyObservation{obs}}},
		})
	})
	mux.HandleFunc("POST /base/v1/history/record", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotRecord); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	t.Setenv("GOTST_HISTORY_TOKEN", "secret")
	store, err := newHTTPHistoryStore(server.URL + "/base")
	if err != nil {
		t.Fatal(err)
	}
	histories, err := store.Lookup(context.Background(), []historyKey{key}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if histories[key.id()] == nil || gotLookup.Limit != 7 || !reflect.DeepEqual(gotLookup.Keys, []historyKey{key}) {
		t.Fatalf("Lookup response=%+v request=%+v", histories, gotLookup)
	}
	if err := store.Record(context.Background(), []historyObservation{obs}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotRecord.Observations, []historyObservation{obs}) {
		t.Fatalf("Record request = %+v; want %+v", gotRecord, obs)
	}
}

func TestHistoryDependenciesPortable(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "pkg", "one")
	deps := []cacheDependency{
		{Operation: "getenv", Name: "GODEBUG"},
		{Operation: "open", Name: "fixture", Path: filepath.Join(pkgDir, "testdata", "fixture")},
		{Operation: "stat", Name: "module", Path: filepath.Join(root, "shared", "input")},
		{Operation: "open", Name: "/private/user/tmp/input", Path: "/private/user/tmp/input"},
	}
	got := historyDependencies(deps, pkgDir, root)
	want := []historyDependency{
		{Operation: "getenv", Name: "GODEBUG"},
		{Operation: "open", Path: portableHistoryPath("/private/user/tmp/input", pkgDir, root)},
		{Operation: "open", Path: "$PACKAGE/testdata/fixture"},
		{Operation: "stat", Path: "$MODULE/shared/input"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("historyDependencies = %#v; want %#v", got, want)
	}
}

func TestHistoryDependenciesUnknown(t *testing.T) {
	if got := historyDependencies(nil, "/package", "/module"); got != nil {
		t.Fatalf("historyDependencies(nil) = %#v; want nil", got)
	}
}

func TestCompleteHistoryDeps(t *testing.T) {
	deps := []cacheDependency{{Operation: "getenv", Name: "ONE"}}
	if got := completeHistoryDeps(deps, true); !reflect.DeepEqual(got, deps) {
		t.Fatalf("completeHistoryDeps(complete) = %#v; want %#v", got, deps)
	}
	if got := completeHistoryDeps(deps, false); got != nil {
		t.Fatalf("completeHistoryDeps(incomplete) = %#v; want nil", got)
	}
}

func TestHistoryAttemptCapturesDependenciesWithoutResultCache(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	const pkg = "github.com/tailscale/gotst"
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%v", fail), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &Server{
				ctx: ctx, cacheDir: t.TempDir(), history: &diskHistoryStore{},
				pkgs:            map[string]*packageStatus{pkg: {glp: &goListPackage{Root: wd}}},
				debugSeedErrors: make(map[string]string),
			}
			task := testTask{bin: capturedTestBinary{pkg: pkg, absBin: exe, workDir: wd}, test: "TestHistoryAttemptFixture"}
			if fail {
				t.Setenv("GOTST_HISTORY_FIXTURE", "fail")
			} else {
				t.Setenv("GOTST_HISTORY_FIXTURE", "pass")
			}
			_, _, _, deps, runErr := s.runTestAttempt(task, testCacheKey{Package: pkg, Test: task.test}, nil, 0, 0)
			if (runErr != nil) != fail {
				t.Fatalf("run error = %v; fail=%v", runErr, fail)
			}
			if !slices.ContainsFunc(deps, func(dep cacheDependency) bool {
				return dep.Operation == "getenv" && dep.Name == "GOTST_HISTORY_FIXTURE"
			}) {
				t.Fatalf("dependencies = %#v; want GOTST_HISTORY_FIXTURE", deps)
			}
		})
	}
}

func TestHistoryAttemptFixture(t *testing.T) {
	if os.Getenv("GOTST_HISTORY_FIXTURE") == "fail" {
		t.Fatal("requested failure")
	}
}

func TestHistoryKeyIgnoresBinaryAndCheckout(t *testing.T) {
	profile := runProfile{Root: "/checkout/one", Tags: []string{"z", "a"}, Short: true, shortSet: true}
	task1 := testTask{bin: capturedTestBinary{pkg: "example.com/p", absBin: "/cache/hash-one", workDir: "/checkout/one/p"}, test: "TestOne"}
	task2 := testTask{bin: capturedTestBinary{pkg: "example.com/p", absBin: "/cache/hash-two", workDir: "/checkout/two/p"}, test: "TestOne"}
	k1 := historyKeyForTask(task1, profile)
	profile.Root = "/checkout/two"
	k2 := historyKeyForTask(task2, profile)
	if k1.id() != k2.id() {
		t.Fatalf("history IDs differ across binaries/checkouts: %s != %s", k1.id(), k2.id())
	}
}
