// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDiskTestCache(t *testing.T) {
	cache, err := newDiskTestCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := testCacheKey{
		BinarySHA256: strings.Repeat("a", 64),
		Package:      "example.com/p",
		Test:         "TestOne/sub",
		WorkingDir:   "/work/p",
		Args:         []string{"-test.short=true"},
	}
	if _, err := cache.Get(context.Background(), key); !errors.Is(err, errTestCacheMiss) {
		t.Fatalf("empty Get error = %v; want errTestCacheMiss", err)
	}
	entry := &testCacheEntry{
		Version:      testCacheVersion,
		Key:          key,
		Created:      time.Unix(123, 0).UTC(),
		PassedIn:     42 * time.Millisecond,
		Dependencies: []cacheDependency{environmentDependency("GOTST_TEST_CACHE_MISSING")},
	}
	if err := cache.Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	got, err := cache.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Key.id() != key.id() || got.PassedIn != entry.PassedIn {
		t.Fatalf("Get = %+v; want %+v", got, entry)
	}
	path := cache.entryPath(key)
	if !strings.Contains(filepath.Base(path), "TestOne_sub-") {
		t.Fatalf("entry path %q is not inspectable by test name", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"binary_sha256"`, `"TestOne/sub"`, `"dependencies"`, `"value_sha256"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("entry JSON missing %q:\n%s", want, data)
		}
	}
}

func TestParseAndValidateTestLog(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "input.txt")
	if err := os.WriteFile(file, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOTST_CACHE_ENV", "first")
	logPath := filepath.Join(dir, "testlog.txt")
	log := "# test log\ngetenv GOTST_CACHE_ENV\nopen input.txt\nstat input.txt\n"
	if err := os.WriteFile(logPath, []byte(log), 0600); err != nil {
		t.Fatal(err)
	}
	deps, err := parseTestLog(logPath, dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 4 { // implicit GODEBUG plus the three logged operations
		t.Fatalf("got %d dependencies: %+v", len(deps), deps)
	}
	if deps[1].ValueSHA256 == "" || deps[1].Present == nil || !*deps[1].Present {
		t.Fatalf("environment dependency = %+v", deps[1])
	}
	if deps[2].File == nil || deps[2].File.ContentSHA256 != hashString("one") {
		t.Fatalf("open dependency = %+v", deps[2])
	}
	if ok, err := validateDependencies(deps); err != nil || !ok {
		t.Fatalf("initial validation = %v, %v", ok, err)
	}
	t.Setenv("GOTST_CACHE_ENV", "second")
	if ok, err := validateDependencies(deps); err != nil || ok {
		t.Fatalf("environment-changed validation = %v, %v", ok, err)
	}
	t.Setenv("GOTST_CACHE_ENV", "first")
	if err := os.WriteFile(file, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	if ok, err := validateDependencies(deps); err != nil || ok {
		t.Fatalf("file-changed validation = %v, %v", ok, err)
	}
}

func TestParseTestLogIgnoresPathsOutsidePackageRoot(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	inside := filepath.Join(root, "inside")
	outside := filepath.Join(external, "outside")
	for _, path := range []string{inside, outside} {
		if err := os.WriteFile(path, []byte("one"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(root, "testlog.txt")
	log := fmt.Sprintf("# test log\nopen %s\nstat %s\nopen %s\nstat %s\n", inside, inside, outside, outside)
	if err := os.WriteFile(logPath, []byte(log), 0600); err != nil {
		t.Fatal(err)
	}
	deps, err := parseTestLog(logPath, root, root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(deps), 3; got != want { // GODEBUG, open inside, stat inside
		t.Fatalf("got %d dependencies, want %d: %+v", got, want, deps)
	}
	if err := os.WriteFile(outside, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	if ok, err := validateDependencies(deps); err != nil || !ok {
		t.Fatalf("external change invalidated entry: %v, %v", ok, err)
	}
	if err := os.WriteFile(inside, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	if ok, err := validateDependencies(deps); err != nil || ok {
		t.Fatalf("in-root change did not invalidate entry: %v, %v", ok, err)
	}
}

func TestDeduplicateDependenciesKeepsLast(t *testing.T) {
	a := cacheDependency{Operation: "getenv", Name: "A", ValueSHA256: "old"}
	b := cacheDependency{Operation: "getenv", Name: "B", ValueSHA256: "b"}
	c := cacheDependency{Operation: "getenv", Name: "A", ValueSHA256: "new"}
	got := deduplicateDependencies([]cacheDependency{a, b, c})
	if !slices.EqualFunc(got, []cacheDependency{b, c}, func(a, b cacheDependency) bool {
		return a.Name == b.Name && a.ValueSHA256 == b.ValueSHA256
	}) {
		t.Fatalf("deduplicated = %+v", got)
	}
}

func TestCacheEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a nested test module repeatedly")
	}
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module cachetest\n\ngo 1.25.1\n")
	write("cachetest.go", "package cachetest\n")
	write("input.txt", "one")
	write("cachetest_test.go", `package cachetest

import (
	"os"
	"testing"
)

func TestInputs(t *testing.T) {
	_ = os.Getenv("GOTST_CACHE_E2E_ENV")
	if _, err := os.ReadFile("input.txt"); err != nil {
		t.Fatal(err)
	}
}
`)

	exe := filepath.Join(t.TempDir(), "gotst")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.Command(goCmd(), "build", "-o", exe, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building gotst: %v\n%s", err, out)
	}
	cacheDir := t.TempDir()
	run := func(env string) string {
		t.Helper()
		cmd := exec.Command(exe, "-listen=", "-cache-dir="+cacheDir)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOTST_CACHE_E2E_ENV="+env)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("running gotst: %v\n%s", err, &out)
		}
		return out.String()
	}
	assertCached := func(out string, want bool) {
		t.Helper()
		got := strings.Contains(out, "cache hits 1/1 (100.0%)")
		if got != want {
			t.Fatalf("cached = %v; want %v; output:\n%s", got, want, out)
		}
	}

	assertCached(run("one"), false)
	assertCached(run("one"), true)
	write("input.txt", "two")
	assertCached(run("one"), false)
	assertCached(run("one"), true)
	assertCached(run("two"), false)

	matches, err := filepath.Glob(filepath.Join(cacheDir, "test-results", fmt.Sprintf("v%d", testCacheVersion), "*", "*", "TestInputs-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("cache entries = %q, %v; want one", matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"GOTST_CACHE_E2E_ENV"`, `"input.txt"`, `"content_sha256"`} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("cache entry missing %s:\n%s", want, data)
		}
	}
}
