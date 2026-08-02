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
	"time"
)

func TestLoadRunProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, profileConfigName)
	config := `
version: 1
profiles:
  base:
    packages: [./...]
    exclude_packages: [./slow/...]
    tags: [one]
    short: true
    timeout: 2m
    test_flags: [-custom]
  platform:
    tags: [two, one]
    timeout: 3m
  default:
    include: [base, platform]
    packages: [./extra]
    short: false
`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := loadRunProfile(path, "default")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "default" || p.Root != dir {
		t.Fatalf("identity = %q, %q; want default, %q", p.Name, p.Root, dir)
	}
	if !slices.Equal(p.Packages, []string{"./...", "./extra"}) {
		t.Errorf("Packages = %q", p.Packages)
	}
	if !slices.Equal(p.ExcludePackages, []string{"./slow/..."}) {
		t.Errorf("ExcludePackages = %q", p.ExcludePackages)
	}
	if !slices.Equal(p.Tags, []string{"one", "two"}) {
		t.Errorf("Tags = %q", p.Tags)
	}
	if p.Short || !p.shortSet {
		t.Errorf("Short = %v, shortSet = %v; want false, true", p.Short, p.shortSet)
	}
	if p.Timeout != 3*time.Minute {
		t.Errorf("Timeout = %v; want 3m", p.Timeout)
	}
	if !slices.Equal(p.TestFlags, []string{"-custom"}) {
		t.Errorf("TestFlags = %q", p.TestFlags)
	}
}

func TestLoadRunProfileErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config string
		want   string
	}{
		{
			name: "unknown include",
			config: `version: 1
profiles:
  default: {include: [missing]}
`,
			want: `unknown profile "missing"`,
		},
		{
			name: "cycle",
			config: `version: 1
profiles:
  default: {include: [other]}
  other: {include: [default]}
`,
			want: "default -> other -> default",
		},
		{
			name: "unknown field",
			config: `version: 1
profiles:
  default: {wat: true}
`,
			want: "field wat not found",
		},
		{
			name: "timeout",
			config: `version: 1
profiles:
  default: {timeout: forever}
`,
			want: "invalid timeout",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, profileConfigName)
			if err := os.WriteFile(path, []byte(tt.config), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := loadRunProfile(path, "default")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v; want substring %q", err, tt.want)
			}
		})
	}
}

func TestDefaultProfileWithoutConfig(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })

	p, err := loadRunProfile("", "default")
	if err != nil {
		t.Fatal(err)
	}
	if p.Root != dir || !slices.Equal(p.Packages, []string{"./..."}) {
		t.Fatalf("fallback profile = %+v", p)
	}
	if _, err := loadRunProfile("", "other"); err == nil {
		t.Fatal("non-default profile unexpectedly succeeded without a config")
	}
}

func TestProfileEndToEnd(t *testing.T) {
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
	write("go.mod", "module profiletest\n\ngo 1.25.1\n")
	write("profiletest.go", "package profiletest\n")
	write("profiletest_test.go", `//go:build profiletag

package profiletest

import (
	"flag"
	"testing"
	"time"
)

var custom = flag.String("custom-profile-flag", "", "profile integration test flag")

func TestProfileValues(t *testing.T) {
	if !testing.Short() {
		t.Error("testing.Short is false")
	}
	if *custom != "yes" {
		t.Errorf("custom flag = %q; want yes", *custom)
	}
	if got := flag.Lookup("test.timeout").Value.String(); got != (37 * time.Second).String() {
		t.Errorf("timeout = %q; want 37s", got)
	}
}
`)
	write(profileConfigName, `version: 1
profiles:
  base:
    packages: [./...]
    tags: [profiletag]
  default:
    include: [base]
    short: true
    timeout: 37s
    test_flags: [-custom-profile-flag=yes]
`)

	exe := filepath.Join(t.TempDir(), "gotst")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.Command(goCmd(), "build", "-o", exe, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building gotst: %v\n%s", err, out)
	}
	cmd := exec.Command(exe, "-listen=")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("running profile: %v\n%s", err, &out)
	}
	if !strings.Contains(out.String(), "ok  \tprofiletest") {
		t.Fatalf("output does not contain package success:\n%s", &out)
	}
}
