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

	t.Run("JSON summary includes test attributes", func(t *testing.T) {
		dir := makeAttemptModule(t, 1)
		out, err := runGotstFixture(exe, dir, "-json-summary", "-max-output=0", "-max-retries=1")
		if err != nil {
			t.Fatalf("gotst failed: %v\n%s", err, out)
		}
		for _, want := range []string{
			`gotst flaky tests JSON:`,
			`"Package":"attempttest"`,
			`"Test":"TestAttempts"`,
			`"Attrs":{"issue-url":"https://example.com/issues/123"}`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("JSON summary includes failed attempt diagnostics", func(t *testing.T) {
		dir := makeAttemptModule(t, 1)
		out, err := runGotstFixture(exe, dir, "-json-summary", "-max-retries=1")
		if err != nil {
			t.Fatalf("gotst failed: %v\n%s", err, out)
		}
		for _, want := range []string{
			"FLAKY TEST DIAGNOSTICS:",
			"[gotst: attempttest TestAttempts failed attempt 1/1",
			"intentional failure 1",
			"--- FAIL: TestAttempts",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
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
		if !strings.Contains(out, "FAILED: attempttest.TestAttempts") {
			t.Fatalf("output missing immediate failure marker:\n%s", out)
		}
		if marker, diagnostics := strings.Index(out, "FAILED: attempttest.TestAttempts"), strings.Index(out, "intentional failure"); diagnostics < 0 || marker > diagnostics {
			t.Fatalf("failure marker did not precede diagnostics:\n%s", out)
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

	t.Run("debug uncached verifies stable test", func(t *testing.T) {
		dir := makeAttemptModule(t, 0)
		out, err := runGotstFixture(exe, dir, "-debug-uncached", "-cache-dir="+t.TempDir())
		if err != nil {
			t.Fatalf("debug-uncached failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "debug-uncached: all 1 tests reused the seeded result cache") {
			t.Fatalf("debug-uncached did not verify the cache hit:\n%s", out)
		}
		if got := readAttemptCount(t, dir); got != 1 {
			t.Fatalf("test process ran %d times, want once plus one cached verification", got)
		}
	})

	t.Run("debug uncached diagnoses changed input", func(t *testing.T) {
		dir := makeUnstableCacheModule(t)
		out, err := runGotstFixture(exe, dir, "-debug-uncached", "-j=1", "-cache-dir="+t.TempDir())
		if err == nil {
			t.Fatalf("debug-uncached unexpectedly succeeded:\n%s", out)
		}
		for _, want := range []string{
			"debug-uncached: MISS unstablecache/TestRead: recorded inputs changed",
			"open ",
			"shared-input\" changed",
			"1/2 tests did not reuse the result cache (1 hits)",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("debug output missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("debug uncached rejects count", func(t *testing.T) {
		dir := makeAttemptModule(t, 0)
		out, err := runGotstFixture(exe, dir, "-debug-uncached", "-count=1")
		if err == nil || !strings.Contains(out, "-debug-uncached and -count are mutually exclusive") {
			t.Fatalf("debug-uncached with count: err=%v\n%s", err, out)
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
	t.Attr("issue-url", "https://example.com/issues/123")
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

func makeUnstableCacheModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module unstablecache\n\ngo 1.25.1\n")
	write("unstable.go", "package unstablecache\n")
	write("shared-input", "before")
	write("unstable_test.go", `package unstablecache

import (
	"os"
	"testing"
)

func TestRead(t *testing.T) {
	if _, err := os.ReadFile("shared-input"); err != nil { t.Fatal(err) }
}

func TestWrite(t *testing.T) {
	if err := os.WriteFile("shared-input", []byte("after"), 0600); err != nil { t.Fatal(err) }
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
