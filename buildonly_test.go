// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBuildOnlyDoesNotRunTestBinary(t *testing.T) {
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
	write("go.mod", "module buildonlytest\n\ngo 1.25.1\n")
	write("buildonly.go", "package buildonlytest\n")
	write("buildonly_test.go", `package buildonlytest

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if err := os.WriteFile("test-binary-ran", nil, 0600); err != nil { panic(err) }
	os.Exit(m.Run())
}

func TestNeverRuns(t *testing.T) { t.Fatal("test ran") }
`)

	exe := filepath.Join(t.TempDir(), "gotst")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.Command(goCmd(), "build", "-o", exe, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building gotst: %v\n%s", err, out)
	}

	cmd := exec.Command(exe, "-listen=", "-build-only", "-progress=0")
	cmd.Dir = dir
	cacheDir := filepath.Join(t.TempDir(), "cache")
	cmd.Args = append(cmd.Args, "-cache-dir="+cacheDir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("gotst -build-only: %v\n%s", err, &out)
	}
	if _, err := os.Stat(filepath.Join(dir, "test-binary-ran")); !os.IsNotExist(err) {
		t.Fatalf("test binary ran; stat error = %v\noutput:\n%s", err, &out)
	}

	cmd = exec.Command(exe, "-listen=", "-build-only", "-progress=0", "-vlog", "-cache-dir="+cacheDir)
	cmd.Dir = dir
	out.Reset()
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("second gotst -build-only: %v\n%s", err, &out)
	}
	if !bytes.Contains(out.Bytes(), []byte("linked executable cache: 1 hit(s), 0 put(s)")) {
		t.Fatalf("second build did not reuse linked executable:\n%s", &out)
	}
	if !bytes.Contains(out.Bytes(), []byte("1/1 test pkgs; 1/1 built (1 cached, 100.0%)")) {
		t.Fatalf("second build progress did not report cached executable:\n%s", &out)
	}
}
