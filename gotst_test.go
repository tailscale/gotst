// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"maps"
	"os"
	"strings"
	"testing"
	"time"
)

func TestGoListPackageHasTests(t *testing.T) {
	for _, tt := range []struct {
		name string
		pkg  goListPackage
		want bool
	}{
		{name: "none"},
		{name: "internal", pkg: goListPackage{TestGoFiles: []string{"x_test.go"}}, want: true},
		{name: "external", pkg: goListPackage{XTestGoFiles: []string{"x_test.go"}}, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pkg.hasTests(); got != tt.want {
				t.Fatalf("hasTests() = %v; want %v", got, tt.want)
			}
		})
	}
}

func TestDirectTestArgs(t *testing.T) {
	in := []string{"-test.paniconexit0", "-test.v=test2json", "-test.timeout=10m0s"}
	want := []string{"-test.paniconexit0", "-test.timeout=10m0s"}
	got := directTestArgs(in)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("directTestArgs(%q) = %q; want %q", in, got, want)
	}
}

func TestTestAttrs(t *testing.T) {
	got := testAttrs(strings.Join([]string{
		"=== RUN   TestFlake",
		"=== ATTR  TestFlake issue-url https://example.com/issues/123",
		"\x16=== ATTR  TestFlake note value with spaces",
		"not an attribute",
	}, "\n"))
	want := map[string]string{
		"issue-url": "https://example.com/issues/123",
		"note":      "value with spaces",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("testAttrs() = %v; want %v", got, want)
	}
}

func TestProfileTestArgs(t *testing.T) {
	in := []string{"-test.timeout=10m0s", "-test.paniconexit0"}
	p := runProfile{
		Short:     true,
		shortSet:  true,
		Timeout:   30 * time.Second,
		TestFlags: []string{"-testing.v=true", "-custom-flag"},
	}
	got := profileTestArgs(in, p)
	want := []string{
		"-test.paniconexit0",
		"-test.short=true",
		"-test.timeout=30s",
		"-test.v=true",
		"-custom-flag",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("profileTestArgs(%q) = %q; want %q", in, got, want)
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{max: 5}
	for _, s := range []string{"abc", "def"} {
		n, err := b.Write([]byte(s))
		if n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	got := b.String()
	if !strings.HasPrefix(got, "abcde\n") || !strings.Contains(got, "output truncated after 5 bytes") {
		t.Fatalf("String() = %q", got)
	}
}

func Test(t *testing.T) {
	// TODO: write actual tests. For now this file
	// exists just for manual testing.
}

func TestAttr(t *testing.T) {
	t.Attr("attr-foo", "value-bar")
}

// TestReadFile exists for testing -test.testlogfile.
func TestReadFile(t *testing.T) {
	_, err := os.ReadFile("README.md")
	if err != nil {
		t.Errorf("failed to read README.md: %v", err)
	}
	os.ReadFile("../README.md")
}

// TestCheckEnvVar exists for testing -test.testlogfile.
func TestCheckEnvVar(t *testing.T) {
	if os.Getenv("GOTST_TEST_ENV_VAR") == "fail" {
		t.Fatal("fail")
	}
}
