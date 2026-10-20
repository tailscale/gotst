// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestProgTestCacheHelper is not a real test: with the environment
// variable below set, it serves gotst's local build cache as a
// GOCACHEPROG, standing in for a shared cache server.
func TestProgTestCacheHelper(t *testing.T) {
	dir := os.Getenv("GOTST_TEST_SHARED_CACHE_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	c := &localBuildCache{dir: dir}
	if err := c.serve(os.Stdin, os.Stdout); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestProgTestCacheSharesResults(t *testing.T) {
	shared := t.TempDir()
	t.Setenv("GOTST_TEST_SHARED_CACHE_DIR", shared)
	prog := quoteCacheProgArg(os.Args[0]) + " -test.run=^TestProgTestCacheHelper$"
	newMachine := func() *progTestCache {
		disk, err := newDiskTestCache(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		c := newProgTestCache(disk, prog)
		t.Cleanup(func() { c.Close() })
		return c
	}
	ctx := context.Background()
	key := testCacheKey{BinarySHA256: "abcd", Package: "example.com/p", Test: "TestX", WorkingDir: "/src/p"}
	a, b := newMachine(), newMachine()

	if _, err := b.Get(ctx, key); !errors.Is(err, errTestCacheMiss) {
		t.Fatalf("Get before Put = %v; want miss", err)
	}
	entry := &testCacheEntry{Version: testCacheVersion, Key: key, Created: time.Now().UTC(), PassedIn: time.Second,
		Dependencies: []cacheDependency{environmentDependency("HOME")}}
	if err := a.Put(ctx, entry); err != nil {
		t.Fatal(err)
	}
	got, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get from other machine: %v", err)
	}
	if got.PassedIn != time.Second || len(got.Dependencies) != 1 {
		t.Errorf("got %+v", got)
	}
	// b now has its own local copy.
	if _, err := b.local.Get(ctx, key); err != nil {
		t.Errorf("local copy: %v", err)
	}
	other := key
	other.Test = "TestY"
	if _, err := b.Get(ctx, other); !errors.Is(err, errTestCacheMiss) {
		t.Errorf("Get of other key = %v; want miss", err)
	}
}

func TestProgTestCacheWithoutHelper(t *testing.T) {
	disk, err := newDiskTestCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := newProgTestCache(disk, filepath.Join(t.TempDir(), "no-such-cacheprog"))
	defer c.Close()
	ctx := context.Background()
	key := testCacheKey{BinarySHA256: "abcd", Test: "TestX"}
	if err := c.Put(ctx, &testCacheEntry{Version: testCacheVersion, Key: key}); err != nil {
		t.Fatalf("Put with unavailable GOCACHEPROG: %v", err)
	}
	if _, err := c.Get(ctx, key); err != nil {
		t.Fatalf("Get with unavailable GOCACHEPROG: %v", err)
	}
}

// TestFingerprintIgnoresModTimes checks that equivalent files and
// directories in different checkouts, with different modification times,
// have the same fingerprints, so results can be shared between machines.
func TestFingerprintIgnoresModTimes(t *testing.T) {
	mk := func(mtime time.Time) string {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "f"), []byte("data"), 0644)
		os.Mkdir(filepath.Join(dir, "sub"), 0755)
		for _, p := range []string{"f", "sub", "."} {
			os.Chtimes(filepath.Join(dir, p), mtime, mtime)
		}
		return dir
	}
	d1, d2 := mk(time.Unix(1e9, 0)), mk(time.Unix(2e9, 0))
	for _, tt := range []struct {
		name string
		open bool
	}{{"f", true}, {"f", false}, {"sub", false}, {".", true}} {
		f1, err1 := fingerprintPath(filepath.Join(d1, tt.name), tt.open)
		f2, err2 := fingerprintPath(filepath.Join(d2, tt.name), tt.open)
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		if f1.FingerprintSHA256 != f2.FingerprintSHA256 {
			t.Errorf("%s (open=%v): fingerprints differ across mtimes", tt.name, tt.open)
		}
	}
	// Content changes still matter, even for a stat.
	before, _ := fingerprintPath(filepath.Join(d1, "f"), false)
	os.WriteFile(filepath.Join(d1, "f"), []byte("DATA"), 0644)
	after, _ := fingerprintPath(filepath.Join(d1, "f"), false)
	if before.FingerprintSHA256 == after.FingerprintSHA256 {
		t.Error("same-size content change not detected for stat")
	}
}
