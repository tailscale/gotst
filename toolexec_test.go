// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestToolExecWrapper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "compile")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s' \"$*\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	oldArgs := os.Args
	oldReport := reportToolExecFunc
	oldStdout := os.Stdout
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOOLEXEC_IMPORTPATH", "example.com/p")
	t.Setenv(cacheShimSocketEnv, "endpoint")
	os.Args = []string{"gotst", toolExecArg, tool, "-buildid", "action/content", "source.go"}
	os.Stdout = devNull
	var got toolExecEvent
	reportToolExecFunc = func(endpoint string, ev toolExecEvent) {
		if endpoint != "endpoint" {
			t.Errorf("endpoint = %q", endpoint)
		}
		got = ev
	}
	t.Cleanup(func() {
		os.Args = oldArgs
		os.Stdout = oldStdout
		devNull.Close()
		reportToolExecFunc = oldReport
	})
	if err := runToolExec(); err != nil {
		t.Fatal(err)
	}
	if got.ImportPath != "example.com/p" || got.Tool != "compile" || got.BuildID != "action/content" {
		t.Fatalf("event = %+v", got)
	}
}

func TestToolExecPassesVersionProbeWithoutReporting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	tool, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	oldArgs := os.Args
	oldReport := reportToolExecFunc
	os.Args = []string{"gotst", toolExecArg, tool, "-V=full"}
	reportToolExecFunc = func(string, toolExecEvent) { t.Error("reported version probe") }
	t.Cleanup(func() {
		os.Args = oldArgs
		reportToolExecFunc = oldReport
	})
	if err := runToolExec(); err != nil {
		t.Fatal(err)
	}
}
