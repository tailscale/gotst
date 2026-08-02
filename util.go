// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
)

func processCmdOutput(cmd *exec.Cmd, fn func(r io.Reader) error) (err error) {
	var errBuf bytes.Buffer
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer out.Close()
	cmd.Stderr = &errBuf
	defer func() {
		if werr := cmd.Wait(); err == nil && werr != nil {
			err = fmt.Errorf("%v: %s", werr, errBuf.String())
		}
	}()
	if err := cmd.Start(); err != nil {
		return err
	}
	return fn(out)
}

var goCmd = sync.OnceValue(func() string {
	v, err := findGo()
	if err != nil {
		log.Fatalf("error finding 'go' binary: %v\n", err)
	}
	return v
})

func findGo() (string, error) {
	var cands []string
	if mod, err := goModuleRoot(); err == nil {
		if strings.HasSuffix(mod, "/src") {
			cands = append(cands, filepath.Join(mod, "..", "bin", "go"))
		} else if strings.HasSuffix(mod, "/src/cmd") {
			cands = append(cands, filepath.Join(mod, "..", "..", "bin", "go"))
		} else {
			toolGo := filepath.Join(mod, "tool", "go")
			cands = append(cands, toolGo)
		}
	}
	cands = append(cands,
		filepath.Join(os.Getenv("HOME"), "sdk", "go", "bin", "go"),
		"/usr/local/go/bin/go",
		"/usr/local/bin/go",
		"/usr/bin/go",
	)
	if path, err := exec.LookPath("go"); err == nil {
		cands = append(cands, path)
	}
	for _, cand := range cands {
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	return "", fmt.Errorf("no go found in any of %q", cands)
}

func goModuleRoot() (string, error) {
	pwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(pwd, "go.mod")); err == nil {
			return pwd, nil
		}
		if pwd == "/" {
			break
		}
		pwd = filepath.Dir(pwd)
	}
	return "", errors.New("no go.mod found")
}

func pidStillrunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		// If we can FindProcess it on Windows, it's running.
		return true
	}
	// On Unix, we can send signal 0 to test if it's running.
	err = proc.Signal(os.Signal(syscall.Signal(0)))
	return err == nil
}
