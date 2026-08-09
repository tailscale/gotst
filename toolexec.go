// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const toolExecArg = "-gotst-toolexec"

var reportToolExecFunc = reportToolExec

// runToolExec transparently invokes the tool selected by cmd/go's -toolexec
// hook. Compile invocations are reported to the parent cache broker so it can
// associate package identities with tool actions that actually ran.
func runToolExec() error {
	if len(os.Args) < 3 {
		return errors.New("toolexec wrapper missing tool argument")
	}
	tool, args := os.Args[2], os.Args[3:]
	toolName := strings.TrimSuffix(filepath.Base(tool), ".exe")
	isCompile := toolName == "compile" && !slicesContains(args, "-V=full")
	ev := toolExecEvent{}
	if isCompile {
		ev = toolExecEvent{
			ImportPath: os.Getenv("TOOLEXEC_IMPORTPATH"),
			Tool:       toolName,
			BuildID:    flagValue(args, "-buildid"),
		}
	}
	cmd := exec.Command(tool, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		os.Exit(exitErr.ExitCode())
	}
	if err == nil && isCompile {
		reportToolExecFunc(os.Getenv(cacheShimSocketEnv), ev)
	}
	return err
}

func flagValue(args []string, name string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
		if value, ok := strings.CutPrefix(arg, name+"="); ok {
			return value
		}
	}
	return ""
}

func slicesContains(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}
