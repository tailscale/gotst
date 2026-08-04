// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestRetryAndCountEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs nested test modules")
	}
	exe := filepath.Join(t.TempDir(), "gotst")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.Command(goCmd(), "build", "-o", exe, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building gotst: %v\n%s", err, out)
	}

	t.Run("flake passes on retry", func(t *testing.T) {
		dir := makeAttemptModule(t, 2)
		out, err := runGotstFixture(exe, dir, "-max-retries=3")
		if err != nil {
			t.Fatalf("gotst failed: %v\n%s", err, out)
		}
		for _, want := range []string{
			"1 flaky",
			"FLAKY TESTS (1):",
			"attempttest TestAttempts: passed after 3 attempts (2 failed)",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
		if got := readAttemptCount(t, dir); got != 3 {
			t.Fatalf("attempt count = %d; want 3", got)
		}
	})

	t.Run("retry limit exhausted", func(t *testing.T) {
		dir := makeAttemptModule(t, 2)
		out, err := runGotstFixture(exe, dir, "-max-retries=1")
		if err == nil {
			t.Fatalf("gotst succeeded:\n%s", out)
		}
		if !strings.Contains(out, "1 test(s) failed") {
			t.Fatalf("output missing failure summary:\n%s", out)
		}
		if strings.Contains(out, "FLAKY TESTS") {
			t.Fatalf("failed test reported as flaky:\n%s", out)
		}
		if got := readAttemptCount(t, dir); got != 2 {
			t.Fatalf("attempt count = %d; want 2", got)
		}
	})

	t.Run("failfast waits for retries", func(t *testing.T) {
		dir := makeAttemptModule(t, 1)
		out, err := runGotstFixture(exe, dir, "-failfast", "-max-retries=1")
		if err != nil {
			t.Fatalf("gotst failed before successful retry: %v\n%s", err, out)
		}
		if !strings.Contains(out, "FLAKY TESTS (1):") {
			t.Fatalf("output missing flaky summary:\n%s", out)
		}
		if got := readAttemptCount(t, dir); got != 2 {
			t.Fatalf("attempt count = %d; want 2", got)
		}
	})

	t.Run("explicit count disables cache", func(t *testing.T) {
		dir := makeAttemptModule(t, 0)
		cacheDir := t.TempDir()
		out, err := runGotstFixture(exe, dir, "-cache-dir="+cacheDir, "-count=3", "-max-retries=0")
		if err != nil {
			t.Fatalf("count=3 failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "cache off") {
			t.Fatalf("explicit count did not disable cache:\n%s", out)
		}
		if got := readAttemptCount(t, dir); got != 3 {
			t.Fatalf("attempt count = %d; want 3", got)
		}
		for range 2 {
			if out, err := runGotstFixture(exe, dir, "-cache-dir="+cacheDir, "-count=1", "-max-retries=0"); err != nil {
				t.Fatalf("explicit count=1 failed: %v\n%s", err, out)
			}
		}
		if got := readAttemptCount(t, dir); got != 5 {
			t.Fatalf("attempt count after two explicit count=1 runs = %d; want 5", got)
		}
	})

	t.Run("omitted count uses cache", func(t *testing.T) {
		dir := makeAttemptModule(t, 0)
		cacheDir := t.TempDir()
		if out, err := runGotstFixture(exe, dir, "-cache-dir="+cacheDir); err != nil {
			t.Fatalf("cold run failed: %v\n%s", err, out)
		}
		out, err := runGotstFixture(exe, dir, "-cache-dir="+cacheDir)
		if err != nil {
			t.Fatalf("warm run failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "cache hits 1/1 (100.0%)") {
			t.Fatalf("warm run did not hit cache:\n%s", out)
		}
		if got := readAttemptCount(t, dir); got != 1 {
			t.Fatalf("attempt count after warm run = %d; want 1", got)
		}
	})
}

func makeAttemptModule(t *testing.T, failUntil int) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module attempttest\n\ngo 1.25.1\n")
	write("attempt.go", "package attempttest\n")
	write("fail-until", strconv.Itoa(failUntil))
	write("attempt_test.go", `package attempttest

import (
	"os"
	"strconv"
	"testing"
)

func TestAttempts(t *testing.T) {
	n := 0
	if data, err := os.ReadFile("attempt-count"); err == nil {
		n, _ = strconv.Atoi(string(data))
	}
	n++
	if err := os.WriteFile("attempt-count", []byte(strconv.Itoa(n)), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("fail-until")
	if err != nil { t.Fatal(err) }
	failUntil, _ := strconv.Atoi(string(data))
	if n <= failUntil { t.Fatalf("intentional failure %d", n) }
}
`)
	return dir
}

func runGotstFixture(exe, dir string, args ...string) (string, error) {
	args = append([]string{"-listen=", "-progress=0"}, args...)
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func readAttemptCount(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "attempt-count"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return n
}
