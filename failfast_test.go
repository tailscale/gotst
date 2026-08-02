// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFailFastEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a nested test module")
	}
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module failfasttest\n\ngo 1.25.1\n")
	write("failfast.go", "package failfasttest\n")
	write("failfast_test.go", `package failfasttest

import (
	"os"
	"testing"
	"time"
)

func TestAFail(t *testing.T) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat("hang-started"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for sibling test to start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("intentional failure")
}

func TestBHangs(t *testing.T) {
	if err := os.WriteFile("hang-started", nil, 0600); err != nil { t.Fatal(err) }
	time.Sleep(30 * time.Second)
}

func TestCNeverStarts(t *testing.T) {
	if err := os.WriteFile("queued-started", nil, 0600); err != nil { t.Fatal(err) }
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

	t0 := time.Now()
	cmd := exec.Command(exe, "-listen=", "-cache=false", "-failfast", "-j=2", "-progress=0")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		t.Fatalf("gotst succeeded; output:\n%s", &out)
	}
	if d := time.Since(t0); d > 10*time.Second {
		t.Fatalf("fail-fast run took %v; output:\n%s", d, &out)
	}
	if !strings.Contains(out.String(), "TestAFail") || !strings.Contains(out.String(), "1 test(s) failed") {
		t.Fatalf("output does not report first failure:\n%s", &out)
	}
	if _, err := os.Stat(filepath.Join(dir, "queued-started")); !os.IsNotExist(err) {
		t.Fatalf("queued test started; stat error = %v", err)
	}
}
