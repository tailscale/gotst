// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestParseInvocation(t *testing.T) {
	defs := &profileDefinitions{
		root: t.TempDir(),
		profiles: map[string]rawProfile{
			"default": {Packages: []string{"./default/..."}, Tags: []string{"defaulttag"}},
			"full":    {Packages: []string{"./full/..."}},
		},
	}
	for _, tt := range []struct {
		name         string
		args         []string
		wantProfile  string
		wantPackages []string
		wantTests    []string
	}{
		{name: "default", wantProfile: "default", wantPackages: []string{"./default/..."}},
		{name: "packages", args: []string{"./foo/...", "tailscale.io/bar"}, wantProfile: "default", wantPackages: []string{"./foo/...", "tailscale.io/bar"}},
		{name: "profile", args: []string{"full"}, wantProfile: "full", wantPackages: []string{"./full/..."}},
		{name: "tests", args: []string{"TestFoo", "TestBar"}, wantProfile: "default", wantPackages: []string{"./default/..."}, wantTests: []string{"TestFoo", "TestBar"}},
		{name: "qualified", args: []string{"tailscale.io/foo/bar.TestBaz", "./foo/bar.TestLocal"}, wantProfile: "default", wantPackages: []string{"./default/..."}, wantTests: []string{"tailscale.io/foo/bar.TestBaz", "./foo/bar.TestLocal"}},
		{name: "profile subset", args: []string{"full", "TestFreeBSDSubnetRouter"}, wantProfile: "full", wantPackages: []string{"./full/..."}, wantTests: []string{"TestFreeBSDSubnetRouter"}},
		{name: "package subset", args: []string{"./foo/...", "TestFoo"}, wantProfile: "default", wantPackages: []string{"./foo/..."}, wantTests: []string{"TestFoo"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseInvocation(defs, tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if got.profile.Name != tt.wantProfile {
				t.Errorf("profile = %q; want %q", got.profile.Name, tt.wantProfile)
			}
			if !slices.Equal(got.profile.Packages, tt.wantPackages) {
				t.Errorf("packages = %q; want %q", got.profile.Packages, tt.wantPackages)
			}
			if names := got.tests.requestedNames(); !slices.Equal(names, tt.wantTests) {
				t.Errorf("tests = %q; want %q", names, tt.wantTests)
			}
		})
	}
}

func TestInvocationEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a nested test module")
	}
	exe := filepath.Join(t.TempDir(), "gotst")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.Command(goCmd(), "build", "-o", exe, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building gotst: %v\n%s", err, out)
	}

	dir := t.TempDir()
	writeInvocationFixture(t, dir, "go.mod", "module invocation.test\n\ngo 1.25.1\n")
	writeInvocationFixture(t, dir, profileConfigName, `version: 1
profiles:
  default:
    packages: [./...]
    test_flags: [-test.v]
  full:
    packages: [./a, ./b]
    test_flags: [-test.v]
`)
	writeInvocationPackage(t, dir, "a", `package a

import (
	"fmt"
	"testing"
)

func TestAlpha(t *testing.T) { fmt.Println("RAN_ALPHA") }
func TestShared(t *testing.T) { fmt.Println("RAN_SHARED_A") }
`)
	writeInvocationPackage(t, dir, "b", `package b

import (
	"fmt"
	"testing"
)

func TestBeta(t *testing.T) { fmt.Println("RAN_BETA") }
func TestShared(t *testing.T) { fmt.Println("RAN_SHARED_B") }
`)
	// This package proves the source scan avoids linking packages that cannot
	// contain an unqualified requested test: its tests intentionally do not build.
	writeInvocationFixture(t, dir, "broken/broken.go", "package broken\n")
	writeInvocationFixture(t, dir, "broken/broken_test.go", `package broken

import "testing"

func TestUnrelated(t *testing.T) { doesNotCompile }
`)
	writeInvocationFixture(t, dir, "hidden/hidden.go", "package hidden\n")
	writeInvocationFixture(t, dir, "hidden/hidden_test.go", `//go:build hidden

package hidden

import "testing"

func TestHidden(t *testing.T) {}
`)

	for _, tt := range []struct {
		name    string
		args    []string
		want    []string
		notWant []string
	}{
		{name: "unqualified", args: []string{"TestAlpha"}, want: []string{"ok  \tinvocation.test/a\tTestAlpha"}, notWant: []string{"invocation.test/b", "TestShared"}},
		{name: "unqualified in multiple packages", args: []string{"TestShared"}, want: []string{"ok  \tinvocation.test/a\tTestShared", "ok  \tinvocation.test/b\tTestShared"}, notWant: []string{"TestAlpha", "TestBeta"}},
		{name: "profile subset", args: []string{"full", "TestBeta"}, want: []string{"ok  \tinvocation.test/b\tTestBeta"}, notWant: []string{"invocation.test/a", "TestShared"}},
		{name: "relative qualified", args: []string{"./b.TestShared"}, want: []string{"ok  \tinvocation.test/b\tTestShared"}, notWant: []string{"invocation.test/a", "TestBeta"}},
		{name: "import qualified", args: []string{"invocation.test/a.TestShared"}, want: []string{"ok  \tinvocation.test/a\tTestShared"}, notWant: []string{"invocation.test/b", "TestAlpha"}},
		{name: "explicit package", args: []string{"./a", "TestShared"}, want: []string{"ok  \tinvocation.test/a\tTestShared"}, notWant: []string{"invocation.test/b", "TestAlpha"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"-listen=", "-progress=0", "-cache=false", "-vlog", "-max-retries=0"}, tt.args...)
			cmd := exec.Command(exe, args...)
			cmd.Dir = dir
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			if err := cmd.Run(); err != nil {
				t.Fatalf("gotst failed: %v\n%s", err, &out)
			}
			for _, want := range tt.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output missing %q:\n%s", want, &out)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(out.String(), notWant) {
					t.Errorf("output unexpectedly contains %q:\n%s", notWant, &out)
				}
			}
		})
	}

	t.Run("missing after build tags", func(t *testing.T) {
		cmd := exec.Command(exe, "-listen=", "-progress=0", "-cache=false", "TestHidden")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("gotst unexpectedly succeeded:\n%s", out)
		}
		if !strings.Contains(string(out), "requested test(s) not found: TestHidden") {
			t.Fatalf("unexpected error:\n%s", out)
		}
	})
}

func writeInvocationPackage(t *testing.T, root, name, testFile string) {
	t.Helper()
	writeInvocationFixture(t, root, name+"/package.go", "package "+name+"\n")
	writeInvocationFixture(t, root, name+"/package_test.go", testFile)
}

func writeInvocationFixture(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
